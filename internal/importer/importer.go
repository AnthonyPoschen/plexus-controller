// Package importer downloads one validated Factorio save archive and replaces
// only adapter-declared save archives while leaving the Server stopped.
package importer

import (
	"archive/zip"
	"context"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"

	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
)

const (
	ArchiveDirectory = "archive-directory"
	ReplaceArchives  = "replace-archives"
	incomingName     = ".incoming-plexus.zip"
)

type Stage string

const (
	StageDownload   Stage = "download"
	StageValidation Stage = "validation"
	StageReplace    Stage = "replace"
)

type stageError struct {
	stage Stage
	err   error
}

type Progress struct {
	Stage   Stage `json:"stage"`
	Percent int32 `json:"progressPercent"`
}

func (err *stageError) Error() string { return err.err.Error() }
func (err *stageError) Unwrap() error { return err.err }

type Importer struct {
	Client   *http.Client
	Progress func(Progress)
}

func Import(ctx context.Context, workRoot string, targetRoot string, targetLayout string, replacement string, archiveName string, downloadURL string) (int64, error) {
	return (Importer{}).Import(ctx, workRoot, targetRoot, targetLayout, replacement, archiveName, downloadURL)
}

func (importer Importer) Import(ctx context.Context, workRoot string, targetRoot string, targetLayout string, replacement string, archiveName string, downloadURL string) (int64, error) {
	if err := validateTransferURL(downloadURL); err != nil {
		return 0, fail(StageDownload, err)
	}
	safeName, err := sanitizeArchiveName(archiveName)
	if err != nil {
		return 0, fail(StageValidation, err)
	}
	if targetLayout != ArchiveDirectory || replacement != ReplaceArchives {
		return 0, fail(StageReplace, fmt.Errorf("unsupported adapter save replacement"))
	}
	importer.report(StageDownload, 10)
	incoming, size, err := downloadArchive(ctx, importer.client(), workRoot, downloadURL)
	if err != nil {
		return 0, fail(StageDownload, err)
	}
	defer os.Remove(incoming)
	importer.report(StageDownload, 40)
	importer.report(StageValidation, 50)
	entries, err := inspectArchive(incoming, size)
	if err != nil {
		return 0, fail(StageValidation, err)
	}
	if err := factorio.ValidateSaveArchive(safeName, "application/zip", size, entries); err != nil {
		return 0, fail(StageValidation, fmt.Errorf("uploaded Factorio save archive is invalid: %w", err))
	}
	importer.report(StageValidation, 60)
	importer.report(StageReplace, 70)
	if err := replaceArchives(targetRoot, incoming, safeName); err != nil {
		return 0, fail(StageReplace, err)
	}
	importer.report(StageReplace, 95)
	return size, nil
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

func fail(stage Stage, err error) error { return &stageError{stage: stage, err: err} }

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
