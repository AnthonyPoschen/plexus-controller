package exporter

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExportUploadsLatestHostedFactorioArchiveWithoutNesting(t *testing.T) {
	root := t.TempDir()
	older := filepath.Join(root, "_autosave1.zip")
	wanted := filepath.Join(root, "hosted-world.zip")
	writeSaveArchive(t, older, map[string]string{"old/level.dat": "old", "old/level-init.dat": "old-init"})
	writeSaveArchive(t, wanted, map[string]string{"hosted/level.dat": "level", "hosted/level-init.dat": "init"})
	oldTime := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	if err := os.Chtimes(older, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(wanted, oldTime.Add(time.Minute), oldTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	original, err := os.ReadFile(wanted)
	if err != nil {
		t.Fatal(err)
	}

	var uploaded []byte
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		uploaded, err = io.ReadAll(r.Body)
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(nil)), Header: make(http.Header)}, err
	})}

	var progress []Progress
	size, err := (Exporter{Client: client, Progress: func(update Progress) { progress = append(progress, update) }}).Export(context.Background(), root, ArchiveDirectory, LatestModifiedArchive, "https://objects.example/export.zip?signature=redacted")
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(original)) || bytes.Equal(uploaded, original) == false {
		t.Fatalf("export nested or changed the hosted archive: size=%d want=%d", size, len(original))
	}
	want := []Progress{{Stage: StageArchive, Percent: 5}, {Stage: StageArchive, Percent: 20}, {Stage: StageValidation, Percent: 30}, {Stage: StageValidation, Percent: 50}, {Stage: StageUpload, Percent: 60}, {Stage: StageUpload, Percent: 70}, {Stage: StageUpload, Percent: 80}, {Stage: StageUpload, Percent: 90}, {Stage: StageUpload, Percent: 95}}
	if len(progress) != len(want) {
		t.Fatalf("progress updates = %#v, want %#v", progress, want)
	}
	for index := range want {
		if progress[index] != want[index] {
			t.Fatalf("progress[%d] = %#v, want %#v", index, progress[index], want[index])
		}
	}
}

func TestExportRejectsAmbiguousOrSymlinkedHostedArchives(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "one.zip")
	second := filepath.Join(root, "two.zip")
	writeSaveArchive(t, first, map[string]string{"one/level.dat": "level", "one/level-init.dat": "init"})
	writeSaveArchive(t, second, map[string]string{"two/level.dat": "level", "two/level-init.dat": "init"})
	sameTime := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	for _, name := range []string{first, second} {
		if err := os.Chtimes(name, sameTime, sameTime); err != nil {
			t.Fatal(err)
		}
	}
	_, err := (Exporter{Client: noRequestClient(t)}).Export(context.Background(), root, ArchiveDirectory, LatestModifiedArchive, "https://objects.example/export")
	stage, _ := Diagnostic(err)
	if err == nil || stage != StageArchive || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("ambiguous adapter selection was not rejected: %v", err)
	}

	if err := os.Remove(first); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(second); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(t.TempDir(), "outside.zip")
	writeSaveArchive(t, outside, map[string]string{"outside/level.dat": "level", "outside/level-init.dat": "init"})
	if err := os.Symlink(outside, filepath.Join(root, "escape.zip")); err != nil {
		t.Fatal(err)
	}
	_, err = (Exporter{Client: noRequestClient(t)}).Export(context.Background(), root, ArchiveDirectory, LatestModifiedArchive, "https://objects.example/export")
	if err == nil || !strings.Contains(err.Error(), "no hosted") {
		t.Fatalf("symlinked archive was not ignored safely: %v", err)
	}
}

func TestExportReportsValidationAndUploadStagesWithoutAuthorization(t *testing.T) {
	root := t.TempDir()
	writeSaveArchive(t, filepath.Join(root, "invalid.zip"), map[string]string{"notes.txt": "not-a-save"})
	_, err := (Exporter{Client: noRequestClient(t)}).Export(context.Background(), root, ArchiveDirectory, LatestModifiedArchive, "https://objects.example/export?signature=must-not-leak")
	stage, message := Diagnostic(err)
	if stage != StageValidation || !strings.Contains(message, "missing required entry") || strings.Contains(message, "must-not-leak") {
		t.Fatalf("validation diagnostic stage=%q message=%q", stage, message)
	}

	root = t.TempDir()
	writeSaveArchive(t, filepath.Join(root, "valid.zip"), map[string]string{"world/level.dat": "level", "world/level-init.dat": "init"})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return nil, &urlEchoError{value: r.URL.String()}
	})}
	const authorization = "https://objects.example/export.zip?signature=must-not-leak"
	_, err = (Exporter{Client: client}).Export(context.Background(), root, ArchiveDirectory, LatestModifiedArchive, authorization)
	stage, message = Diagnostic(err)
	if stage != StageUpload || strings.Contains(message, "must-not-leak") || strings.Contains(message, authorization) {
		t.Fatalf("upload diagnostic stage=%q message=%q", stage, message)
	}
}

func TestExportRejectsNonFileZIPEntries(t *testing.T) {
	root := t.TempDir()
	name := filepath.Join(root, "unsafe.zip")
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for _, entryName := range []string{"world/level.dat", "world/level-init.dat"} {
		entry, createErr := writer.Create(entryName)
		if createErr != nil {
			t.Fatal(createErr)
		}
		_, _ = io.WriteString(entry, "data")
	}
	header := &zip.FileHeader{Name: "world/escape"}
	header.SetMode(os.ModeSymlink | 0o777)
	entry, err := writer.CreateHeader(header)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(entry, "../../outside")
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}

	_, err = (Exporter{Client: noRequestClient(t)}).Export(context.Background(), root, ArchiveDirectory, LatestModifiedArchive, "https://objects.example/export")
	stage, _ := Diagnostic(err)
	if err == nil || stage != StageValidation {
		t.Fatalf("non-file ZIP entry was accepted: stage=%q error=%v", stage, err)
	}
}

func writeSaveArchive(t *testing.T, name string, entries map[string]string) {
	t.Helper()
	file, err := os.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	writer := zip.NewWriter(file)
	for entryName, contents := range entries {
		entry, err := writer.Create(entryName)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, contents); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func noRequestClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected upload")
		return nil, errors.New("unreachable")
	})}
}

type urlEchoError struct{ value string }

func (err *urlEchoError) Error() string { return err.value }

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
