// Package importer downloads one validated Factorio save archive and replaces
// only adapter-declared save archives while leaving the Server stopped.
package importer

import (
	"archive/zip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"time"

	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
)

const (
	ArchiveDirectory = "archive-directory"
	ReplaceArchives  = "replace-archives"
	incomingName     = ".incoming-plexus.zip"

	RecoveryNone            = "none"
	RecoverySnapshotCreated = "snapshot-created"
	RecoveryRestored        = "restored"
	RecoveryRollbackFailed  = "rollback-failed"

	recoveryDirName      = ".plexus-recovery"
	snapshotManifestName = "manifest.json"
	MaxRecoverySnapshots = 2
)

type Stage string

const (
	StageDownload   Stage = "download"
	StageValidation Stage = "validation"
	StageSnapshot   Stage = "snapshot"
	StageReplace    Stage = "replace"
	StageRollback   Stage = "rollback"
)

type stageError struct {
	stage    Stage
	recovery string
	err      error
}

type Progress struct {
	Stage   Stage `json:"stage"`
	Percent int32 `json:"progressPercent"`
}

type Result struct {
	ArchiveBytes int64
	Recovery     string
}

func (err *stageError) Error() string { return err.err.Error() }
func (err *stageError) Unwrap() error { return err.err }

type Importer struct {
	Client   *http.Client
	Progress func(Progress)
	// replace and restore are test hooks. Production uses the hosted-archive helpers.
	replace func(targetRoot string, incoming string, archiveName string) error
	restore func(targetRoot string, snapshotRoot string) error
}

func Import(ctx context.Context, workRoot string, targetRoot string, targetLayout string, replacement string, archiveName string, downloadURL string, importID string) (Result, error) {
	return (Importer{}).Import(ctx, workRoot, targetRoot, targetLayout, replacement, archiveName, downloadURL, importID)
}

func (importer Importer) Import(ctx context.Context, workRoot string, targetRoot string, targetLayout string, replacement string, archiveName string, downloadURL string, importID string) (Result, error) {
	if err := validateTransferURL(downloadURL); err != nil {
		return Result{}, fail(StageDownload, err)
	}
	safeName, err := sanitizeArchiveName(archiveName)
	if err != nil {
		return Result{}, fail(StageValidation, err)
	}
	safeImportID, err := sanitizeImportID(importID)
	if err != nil {
		return Result{}, fail(StageSnapshot, err)
	}
	if targetLayout != ArchiveDirectory || replacement != ReplaceArchives {
		return Result{}, fail(StageReplace, fmt.Errorf("unsupported adapter save replacement"))
	}
	importer.report(StageDownload, 10)
	incoming, size, err := downloadArchive(ctx, importer.client(), workRoot, downloadURL)
	if err != nil {
		return Result{}, fail(StageDownload, err)
	}
	defer os.Remove(incoming)
	importer.report(StageDownload, 25)
	importer.report(StageValidation, 30)
	entries, err := inspectArchive(incoming, size)
	if err != nil {
		return Result{}, fail(StageValidation, err)
	}
	if err := factorio.ValidateSaveArchive(safeName, "application/zip", size, entries); err != nil {
		return Result{}, fail(StageValidation, fmt.Errorf("uploaded Factorio save archive is invalid: %w", err))
	}
	importer.report(StageValidation, 40)
	importer.report(StageSnapshot, 50)
	snapshotRoot, err := snapshotHostedArchives(targetRoot, safeImportID)
	if err != nil {
		return Result{}, fail(StageSnapshot, err)
	}
	importer.report(StageSnapshot, 60)
	importer.report(StageReplace, 70)
	if err := importer.replaceArchives(targetRoot, incoming, safeName); err != nil {
		importer.report(StageRollback, 80)
		if restoreErr := importer.restoreArchives(targetRoot, snapshotRoot); restoreErr != nil {
			return Result{Recovery: RecoveryRollbackFailed}, failRecovery(StageRollback, RecoveryRollbackFailed, fmt.Errorf("hosted save replacement failed and automatic rollback failed; a recoverable safety snapshot is retained: %s", err.Error()))
		}
		return Result{Recovery: RecoveryRestored}, failRecovery(StageReplace, RecoveryRestored, fmt.Errorf("hosted save replacement failed; the previous save was restored from the automatic recovery snapshot: %w", err))
	}
	importer.report(StageReplace, 85)
	_ = pruneRecoverySnapshots(targetRoot, safeImportID)
	return Result{ArchiveBytes: size, Recovery: RecoverySnapshotCreated}, nil
}

func (importer Importer) replaceArchives(targetRoot string, incoming string, archiveName string) error {
	if importer.replace != nil {
		return importer.replace(targetRoot, incoming, archiveName)
	}
	return replaceArchives(targetRoot, incoming, archiveName)
}

func (importer Importer) restoreArchives(targetRoot string, snapshotRoot string) error {
	if importer.restore != nil {
		return importer.restore(targetRoot, snapshotRoot)
	}
	return restoreArchives(targetRoot, snapshotRoot)
}

func (importer Importer) report(stage Stage, percent int32) {
	if importer.Progress != nil {
		importer.Progress(Progress{Stage: stage, Percent: percent})
	}
}

func Diagnostic(err error) (Stage, string) {
	stage := StageDownload
	if staged, ok := err.(*stageError); ok {
		stage = staged.stage
	}
	message := strings.Join(strings.Fields(err.Error()), " ")
	if len(message) > 400 {
		message = message[:397] + "..."
	}
	return stage, message
}

func RecoveryOf(err error) string {
	if staged, ok := err.(*stageError); ok && staged.recovery != "" {
		return staged.recovery
	}
	return RecoveryNone
}

func fail(stage Stage, err error) error {
	return &stageError{stage: stage, recovery: RecoveryNone, err: err}
}

func failRecovery(stage Stage, recovery string, err error) error {
	return &stageError{stage: stage, recovery: recovery, err: err}
}

func (importer Importer) client() *http.Client {
	if importer.Client != nil {
		return importer.Client
	}
	return &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func downloadArchive(ctx context.Context, client *http.Client, workRoot string, downloadURL string) (string, int64, error) {
	workPath, err := filepath.Abs(workRoot)
	if err != nil {
		return "", 0, fmt.Errorf("resolve import workspace")
	}
	if err := os.MkdirAll(workPath, 0o700); err != nil {
		return "", 0, fmt.Errorf("create import workspace")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, downloadURL, nil)
	if err != nil {
		return "", 0, fmt.Errorf("create object download request")
	}
	response, err := client.Do(request)
	if err != nil {
		return "", 0, fmt.Errorf("object storage request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return "", 0, fmt.Errorf("object storage returned status %d", response.StatusCode)
	}
	destination := filepath.Join(workPath, incomingName)
	file, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return "", 0, fmt.Errorf("create staged import archive")
	}
	written, err := io.Copy(file, io.LimitReader(response.Body, factorio.MaximumSaveArchiveBytes+1))
	closeErr := file.Close()
	if err != nil {
		return "", 0, fmt.Errorf("download uploaded Factorio save archive")
	}
	if closeErr != nil {
		return "", 0, fmt.Errorf("finalize staged import archive")
	}
	if written <= 0 || written > factorio.MaximumSaveArchiveBytes {
		return "", 0, fmt.Errorf("uploaded Factorio save archive size must be between 1 and %d bytes", factorio.MaximumSaveArchiveBytes)
	}
	return destination, written, nil
}

func inspectArchive(name string, size int64) ([]factorio.ArchiveEntry, error) {
	reader, err := zip.OpenReader(name)
	if err != nil {
		return nil, fmt.Errorf("open uploaded Factorio save ZIP")
	}
	defer reader.Close()
	if size <= 0 {
		return nil, fmt.Errorf("open uploaded Factorio save ZIP")
	}
	entries := make([]factorio.ArchiveEntry, 0, len(reader.File))
	for _, file := range reader.File {
		mode := file.Mode()
		if mode.IsRegular() == false && mode.IsDir() == false {
			return nil, fmt.Errorf("uploaded Factorio save ZIP contains a non-file entry")
		}
		if file.UncompressedSize64 > uint64(^uint64(0)>>1) {
			return nil, fmt.Errorf("uploaded Factorio save ZIP contains an oversized entry")
		}
		entries = append(entries, factorio.ArchiveEntry{
			Name: file.Name, UncompressedBytes: int64(file.UncompressedSize64), Directory: mode.IsDir(),
		})
	}
	return entries, nil
}

type snapshotManifest struct {
	ImportID string   `json:"importID"`
	Files    []string `json:"files"`
}

func snapshotHostedArchives(targetRoot string, importID string) (string, error) {
	rootPath, err := filepath.Abs(targetRoot)
	if err != nil {
		return "", fmt.Errorf("resolve hosted saves directory")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return "", fmt.Errorf("open hosted saves directory")
	}
	defer root.Close()
	if err := mkdirAllRoot(root, recoveryDirName); err != nil {
		return "", fmt.Errorf("create recovery snapshot directory")
	}
	if err := mkdirAllRoot(root, filepath.Join(recoveryDirName, importID)); err != nil {
		return "", fmt.Errorf("create recovery snapshot directory")
	}
	snapshotPath := filepath.Join(rootPath, recoveryDirName, importID)
	snapshot, err := os.OpenRoot(snapshotPath)
	if err != nil {
		return "", fmt.Errorf("open recovery snapshot directory")
	}
	defer snapshot.Close()
	if existing, ok := readSnapshotManifest(snapshot, importID); ok {
		if err := verifySnapshotFiles(snapshot, existing.Files); err == nil {
			return snapshotPath, nil
		}
	}
	files, err := copyHostedArchives(root, snapshot)
	if err != nil {
		return "", err
	}
	if err := writeSnapshotManifest(snapshot, snapshotManifest{ImportID: importID, Files: files}); err != nil {
		return "", err
	}
	return snapshotPath, nil
}

func restoreArchives(targetRoot string, snapshotRoot string) error {
	rootPath, err := filepath.Abs(targetRoot)
	if err != nil {
		return fmt.Errorf("resolve hosted saves directory")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open hosted saves directory")
	}
	defer root.Close()
	snapshot, err := os.OpenRoot(snapshotRoot)
	if err != nil {
		return fmt.Errorf("open recovery snapshot directory")
	}
	defer snapshot.Close()
	manifest, ok := readSnapshotManifest(snapshot, path.Base(snapshotRoot))
	if !ok {
		return fmt.Errorf("recovery snapshot manifest is missing")
	}
	if err := verifySnapshotFiles(snapshot, manifest.Files); err != nil {
		return err
	}
	if err := removeHostedArchives(root); err != nil {
		return err
	}
	for _, name := range manifest.Files {
		if err := copyRootFile(snapshot, root, name); err != nil {
			return fmt.Errorf("restore previous hosted Factorio save archive")
		}
	}
	return nil
}

func pruneRecoverySnapshots(targetRoot string, keepImportID string) error {
	rootPath, err := filepath.Abs(targetRoot)
	if err != nil {
		return fmt.Errorf("resolve hosted saves directory")
	}
	recovery, err := os.OpenRoot(filepath.Join(rootPath, recoveryDirName))
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open recovery snapshot directory")
	}
	defer recovery.Close()
	entries, err := fs.ReadDir(recovery.FS(), ".")
	if err != nil {
		return fmt.Errorf("read recovery snapshot directory")
	}
	type snapshotDir struct {
		name    string
		modTime time.Time
	}
	kept := []snapshotDir{}
	for _, entry := range entries {
		if entry.IsDir() == false || strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, err := recovery.Lstat(entry.Name())
		if err != nil {
			continue
		}
		kept = append(kept, snapshotDir{name: entry.Name(), modTime: info.ModTime()})
	}
	sort.Slice(kept, func(i int, j int) bool {
		if kept[i].name == keepImportID {
			return true
		}
		if kept[j].name == keepImportID {
			return false
		}
		if kept[i].modTime.Equal(kept[j].modTime) {
			return kept[i].name > kept[j].name
		}
		return kept[i].modTime.After(kept[j].modTime)
	})
	for index, item := range kept {
		if index < MaxRecoverySnapshots || item.name == keepImportID {
			continue
		}
		if err := removeSnapshotDir(recovery, item.name); err != nil {
			return err
		}
	}
	return nil
}

func mkdirAllRoot(root *os.Root, name string) error {
	cleaned := filepath.ToSlash(name)
	if cleaned == "" || cleaned == "." {
		return nil
	}
	parts := strings.Split(cleaned, "/")
	current := ""
	for _, part := range parts {
		if part == "" || part == "." {
			continue
		}
		if current == "" {
			current = part
		} else {
			current = filepath.Join(current, part)
		}
		if err := root.Mkdir(current, 0o755); err != nil && !os.IsExist(err) {
			return err
		}
	}
	return nil
}

func copyHostedArchives(root *os.Root, snapshot *os.Root) ([]string, error) {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, fmt.Errorf("read hosted saves directory")
	}
	files := []string{}
	for _, entry := range entries {
		if !isHostedSaveArchive(entry) {
			continue
		}
		if err := copyRootFile(root, snapshot, entry.Name()); err != nil {
			return nil, fmt.Errorf("snapshot hosted Factorio save archive")
		}
		files = append(files, entry.Name())
	}
	sort.Strings(files)
	return files, nil
}

func removeHostedArchives(root *os.Root) error {
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("read hosted saves directory")
	}
	for _, entry := range entries {
		if !isHostedSaveArchive(entry) {
			continue
		}
		if err := root.Remove(entry.Name()); err != nil {
			return fmt.Errorf("remove hosted Factorio save archive")
		}
	}
	return nil
}

func isHostedSaveArchive(entry fs.DirEntry) bool {
	if entry.IsDir() || strings.HasPrefix(entry.Name(), ".") || strings.EqualFold(filepath.Ext(entry.Name()), ".zip") == false {
		return false
	}
	return entry.Type()&os.ModeSymlink == 0
}

func copyRootFile(source *os.Root, destination *os.Root, name string) error {
	input, err := source.Open(name)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil || info.Mode().IsRegular() == false {
		return fmt.Errorf("open hosted Factorio save archive")
	}
	output, err := destination.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func readSnapshotManifest(snapshot *os.Root, importID string) (snapshotManifest, bool) {
	file, err := snapshot.Open(snapshotManifestName)
	if err != nil {
		return snapshotManifest{}, false
	}
	defer file.Close()
	var manifest snapshotManifest
	if json.NewDecoder(file).Decode(&manifest) != nil || manifest.ImportID != importID {
		return snapshotManifest{}, false
	}
	return manifest, true
}

func writeSnapshotManifest(snapshot *os.Root, manifest snapshotManifest) error {
	file, err := snapshot.OpenFile(snapshotManifestName, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("write recovery snapshot manifest")
	}
	if err := json.NewEncoder(file).Encode(manifest); err != nil {
		file.Close()
		return fmt.Errorf("write recovery snapshot manifest")
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("write recovery snapshot manifest")
	}
	return nil
}

func verifySnapshotFiles(snapshot *os.Root, files []string) error {
	for _, name := range files {
		info, err := snapshot.Lstat(name)
		if err != nil || info.Mode().IsRegular() == false || info.Size() <= 0 {
			return fmt.Errorf("recovery snapshot is incomplete")
		}
	}
	return nil
}

func removeSnapshotDir(recovery *os.Root, name string) error {
	entries, err := fs.ReadDir(recovery.FS(), name)
	if err != nil {
		return fmt.Errorf("remove expired recovery snapshot")
	}
	for _, entry := range entries {
		if err := recovery.Remove(filepath.Join(name, entry.Name())); err != nil {
			return fmt.Errorf("remove expired recovery snapshot")
		}
	}
	if err := recovery.Remove(name); err != nil {
		return fmt.Errorf("remove expired recovery snapshot")
	}
	return nil
}

func replaceArchives(targetRoot string, incoming string, archiveName string) error {
	rootPath, err := filepath.Abs(targetRoot)
	if err != nil {
		return fmt.Errorf("resolve hosted saves directory")
	}
	if err := os.MkdirAll(rootPath, 0o755); err != nil {
		return fmt.Errorf("create hosted saves directory")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return fmt.Errorf("open hosted saves directory")
	}
	defer root.Close()
	if err := copyIntoRoot(root, incoming, incomingName); err != nil {
		return err
	}
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return fmt.Errorf("read hosted saves directory")
	}
	for _, entry := range entries {
		if entry.Name() == incomingName || strings.EqualFold(filepath.Ext(entry.Name()), ".zip") == false {
			continue
		}
		info, err := root.Lstat(entry.Name())
		if err != nil || info.Mode().IsRegular() == false || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if err := root.Remove(entry.Name()); err != nil {
			return fmt.Errorf("remove previous hosted Factorio save archive")
		}
	}
	if err := root.Rename(incomingName, archiveName); err != nil {
		return fmt.Errorf("install replacement Factorio save archive")
	}
	return nil
}

func copyIntoRoot(root *os.Root, source string, name string) error {
	input, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open staged import archive")
	}
	defer input.Close()
	output, err := root.OpenFile(name, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("stage replacement Factorio save archive")
	}
	if _, err := io.Copy(output, input); err != nil {
		output.Close()
		return fmt.Errorf("write replacement Factorio save archive")
	}
	if err := output.Close(); err != nil {
		return fmt.Errorf("finalize replacement Factorio save archive")
	}
	return nil
}

func sanitizeArchiveName(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	base := path.Base(trimmed)
	if trimmed == "" || trimmed != base || base == "." || base == ".." || strings.HasPrefix(base, ".") || strings.ContainsAny(trimmed, `/\`) {
		return "", fmt.Errorf("safe Factorio save archive name is required")
	}
	if strings.EqualFold(path.Ext(base), ".zip") == false {
		return "", fmt.Errorf("Factorio save must use the .zip extension")
	}
	return base, nil
}

func sanitizeImportID(name string) (string, error) {
	trimmed := strings.TrimSpace(name)
	base := path.Base(trimmed)
	if trimmed == "" || trimmed != base || base == "." || base == ".." || strings.HasPrefix(base, ".") || strings.ContainsAny(trimmed, `/\`) {
		return "", fmt.Errorf("safe save import identity is required")
	}
	return base, nil
}

func validateTransferURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("valid object storage download URL is required")
	}
	if parsed.Scheme == "http" {
		host := strings.TrimSpace(parsed.Hostname())
		address := net.ParseIP(host)
		if strings.EqualFold(host, "localhost") == false && (address == nil || address.IsLoopback() == false) {
			return fmt.Errorf("object storage download URL must use HTTPS")
		}
	}
	return nil
}
