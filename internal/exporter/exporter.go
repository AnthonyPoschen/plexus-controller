// Package exporter validates and uploads one adapter-selected hosted Factorio
// archive from a read-only saves directory.
package exporter

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
	"path/filepath"
	"strings"
	"time"

	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
)

const (
	ArchiveDirectory      = "archive-directory"
	LatestModifiedArchive = "latest-modified-archive"
)

type Stage string

const (
	StageArchive    Stage = "archive"
	StageValidation Stage = "validation"
	StageUpload     Stage = "upload"
)

type stageError struct {
	stage Stage
	err   error
}

// Progress is deliberately coarse: it describes completed exporter work, not
// bytes durably stored by the object service.
type Progress struct {
	Stage   Stage `json:"stage"`
	Percent int32 `json:"progressPercent"`
}

func (err *stageError) Error() string { return err.err.Error() }
func (err *stageError) Unwrap() error { return err.err }

type Exporter struct {
	Client   *http.Client
	Progress func(Progress)
}

func Export(ctx context.Context, sourceRoot string, sourceLayout string, selection string, uploadURL string) (int64, error) {
	return (Exporter{}).Export(ctx, sourceRoot, sourceLayout, selection, uploadURL)
}

func (exporter Exporter) Export(ctx context.Context, sourceRoot string, sourceLayout string, selection string, uploadURL string) (int64, error) {
	if err := validateUploadURL(uploadURL); err != nil {
		return 0, fail(StageUpload, err)
	}
	exporter.report(StageArchive, 5)
	archive, archiveName, size, err := openSelectedArchive(sourceRoot, sourceLayout, selection)
	if err != nil {
		return 0, fail(StageArchive, err)
	}
	defer archive.Close()
	exporter.report(StageArchive, 20)
	exporter.report(StageValidation, 30)
	entries, err := inspectArchive(archive, size)
	if err != nil {
		return 0, fail(StageValidation, err)
	}
	if err := factorio.ValidateSaveArchive(archiveName, "application/zip", size, entries); err != nil {
		return 0, fail(StageValidation, fmt.Errorf("hosted Factorio save archive is invalid: %w", err))
	}
	exporter.report(StageValidation, 50)
	if _, err := archive.Seek(0, io.SeekStart); err != nil {
		return 0, fail(StageArchive, fmt.Errorf("rewind hosted Factorio save archive"))
	}
	exporter.report(StageUpload, 60)
	if err := upload(ctx, exporter.client(), archive, uploadURL, size, exporter.Progress); err != nil {
		return 0, fail(StageUpload, err)
	}
	exporter.report(StageUpload, 95)
	return size, nil
}

func (exporter Exporter) report(stage Stage, percent int32) {
	if exporter.Progress != nil {
		exporter.Progress(Progress{Stage: stage, Percent: percent})
	}
}

// Diagnostic returns a bounded stage-specific message suitable for a Pod
// termination message and SaveExport status. Upload authorizations are never
// included in errors produced by this package.
func Diagnostic(err error) (Stage, string) {
	stage := StageArchive
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

func (exporter Exporter) client() *http.Client {
	if exporter.Client != nil {
		return exporter.Client
	}
	return &http.Client{CheckRedirect: func(_ *http.Request, _ []*http.Request) error { return http.ErrUseLastResponse }}
}

func openSelectedArchive(sourceRoot string, sourceLayout string, selection string) (*os.File, string, int64, error) {
	if sourceLayout != ArchiveDirectory || selection != LatestModifiedArchive {
		return nil, "", 0, fmt.Errorf("unsupported adapter save selection")
	}
	rootPath, err := filepath.Abs(sourceRoot)
	if err != nil {
		return nil, "", 0, fmt.Errorf("resolve hosted saves directory")
	}
	root, err := os.OpenRoot(rootPath)
	if err != nil {
		return nil, "", 0, fmt.Errorf("open hosted saves directory")
	}
	defer root.Close()
	entries, err := fs.ReadDir(root.FS(), ".")
	if err != nil {
		return nil, "", 0, fmt.Errorf("read hosted saves directory")
	}
	type candidate struct {
		name     string
		info     os.FileInfo
		modified time.Time
	}
	var selected candidate
	var found bool
	var tied bool
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || strings.EqualFold(filepath.Ext(entry.Name()), ".zip") == false {
			continue
		}
		info, err := root.Lstat(entry.Name())
		if err != nil || info.Mode().IsRegular() == false {
			continue
		}
		if found == false || info.ModTime().After(selected.modified) {
			selected = candidate{name: entry.Name(), info: info, modified: info.ModTime()}
			found, tied = true, false
			continue
		}
		if info.ModTime().Equal(selected.modified) {
			tied = true
		}
	}
	if found == false {
		return nil, "", 0, fmt.Errorf("no hosted Factorio save archive was found")
	}
	if tied {
		return nil, "", 0, fmt.Errorf("hosted Factorio save selection is ambiguous")
	}
	archive, err := root.Open(selected.name)
	if err != nil {
		return nil, "", 0, fmt.Errorf("open selected hosted Factorio save archive")
	}
	openedInfo, err := archive.Stat()
	if err != nil || openedInfo.Mode().IsRegular() == false || os.SameFile(selected.info, openedInfo) == false {
		archive.Close()
		return nil, "", 0, fmt.Errorf("selected hosted Factorio save archive changed during selection")
	}
	return archive, selected.name, openedInfo.Size(), nil
}

func inspectArchive(archive *os.File, size int64) ([]factorio.ArchiveEntry, error) {
	reader, err := zip.NewReader(archive, size)
	if err != nil {
		return nil, fmt.Errorf("open hosted Factorio save ZIP")
	}
	entries := make([]factorio.ArchiveEntry, 0, len(reader.File))
	for _, file := range reader.File {
		mode := file.Mode()
		if mode.IsRegular() == false && mode.IsDir() == false {
			return nil, fmt.Errorf("hosted Factorio save ZIP contains a non-file entry")
		}
		if file.UncompressedSize64 > uint64(^uint64(0)>>1) {
			return nil, fmt.Errorf("hosted Factorio save ZIP contains an oversized entry")
		}
		entries = append(entries, factorio.ArchiveEntry{
			Name: file.Name, UncompressedBytes: int64(file.UncompressedSize64), Directory: mode.IsDir(),
		})
	}
	return entries, nil
}

func upload(ctx context.Context, client *http.Client, archive io.Reader, uploadURL string, size int64, report func(Progress)) error {
	body := &uploadProgressReader{reader: archive, size: size, report: report, milestones: []uploadMilestone{{fraction: 25, percent: 70}, {fraction: 50, percent: 80}, {fraction: 75, percent: 90}}}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, uploadURL, body)
	if err != nil {
		return fmt.Errorf("create object upload request")
	}
	request.ContentLength = size
	request.Header.Set("Content-Type", "application/zip")
	response, err := client.Do(request)
	if err != nil {
		return fmt.Errorf("object storage request failed")
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("object storage returned status %d", response.StatusCode)
	}
	return nil
}

type uploadMilestone struct {
	fraction int64
	percent  int32
}

type uploadProgressReader struct {
	reader     io.Reader
	size       int64
	read       int64
	report     func(Progress)
	milestones []uploadMilestone
}

func (reader *uploadProgressReader) Read(buffer []byte) (int, error) {
	count, err := reader.reader.Read(buffer)
	reader.read += int64(count)
	for len(reader.milestones) > 0 && reader.size > 0 && reader.read*100 >= reader.size*reader.milestones[0].fraction {
		milestone := reader.milestones[0]
		reader.milestones = reader.milestones[1:]
		if reader.report != nil {
			reader.report(Progress{Stage: StageUpload, Percent: milestone.percent})
		}
	}
	return count, err
}

func validateUploadURL(value string) error {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return fmt.Errorf("valid object storage upload URL is required")
	}
	if parsed.Scheme == "http" {
		host := strings.TrimSpace(parsed.Hostname())
		address := net.ParseIP(host)
		if strings.EqualFold(host, "localhost") == false && (address == nil || address.IsLoopback() == false) {
			return fmt.Errorf("object storage upload URL must use HTTPS")
		}
	}
	return nil
}
