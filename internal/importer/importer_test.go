package importer

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
)

func TestImportReplacesOnlyAdapterDeclaredSaveArchives(t *testing.T) {
	target := t.TempDir()
	work := t.TempDir()
	writeSaveArchive(t, filepath.Join(target, "_autosave1.zip"), map[string]string{"old/level.dat": "old", "old/level-init.dat": "old-init"})
	writeSaveArchive(t, filepath.Join(target, "hosted-world.zip"), map[string]string{"hosted/level.dat": "hosted", "hosted/level-init.dat": "hosted-init"})
	if err := os.WriteFile(filepath.Join(target, "notes.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "keep-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	replacement := zipBytes(t, map[string]string{"copper-works/level.dat": "level", "copper-works/level-init.dat": "init"})
	client := &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if r.Method != http.MethodGet {
			t.Fatalf("unexpected method %s", r.Method)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(replacement)), Header: make(http.Header)}, nil
	})}

	var progress []Progress
	size, err := (Importer{Client: client, Progress: func(update Progress) { progress = append(progress, update) }}).Import(
		context.Background(), work, target, ArchiveDirectory, ReplaceArchives, "copper-works.zip", "https://objects.example/import.zip?signature=redacted",
	)
	if err != nil {
		t.Fatal(err)
	}
	if size != int64(len(replacement)) {
		t.Fatalf("imported size = %d, want %d", size, len(replacement))
	}
	installed, err := os.ReadFile(filepath.Join(target, "copper-works.zip"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(installed, replacement) == false {
		t.Fatal("replacement archive was rewritten or nested")
	}
	if _, err := os.Stat(filepath.Join(target, "_autosave1.zip")); err == nil {
		t.Fatal("previous hosted save archive was retained")
	}
	if _, err := os.Stat(filepath.Join(target, "hosted-world.zip")); err == nil {
		t.Fatal("previous hosted save archive was retained")
	}
	if notes, err := os.ReadFile(filepath.Join(target, "notes.txt")); err != nil || string(notes) != "keep" {
		t.Fatalf("non-save data was changed: %v %q", err, notes)
	}
	if info, err := os.Stat(filepath.Join(target, "keep-dir")); err != nil || info.IsDir() == false {
		t.Fatalf("non-save directory was changed: %v", err)
	}
	want := []Progress{
		{Stage: StageDownload, Percent: 10}, {Stage: StageDownload, Percent: 40},
		{Stage: StageValidation, Percent: 50}, {Stage: StageValidation, Percent: 60},
		{Stage: StageReplace, Percent: 70}, {Stage: StageReplace, Percent: 95},
	}
	if len(progress) != len(want) {
		t.Fatalf("progress updates = %#v, want %#v", progress, want)
	}
	for index := range want {
		if progress[index] != want[index] {
			t.Fatalf("progress[%d] = %#v, want %#v", index, progress[index], want[index])
		}
	}
}

func TestImportRejectsInvalidArchivesWithoutTouchingHostedSaves(t *testing.T) {
	target := t.TempDir()
	hosted := filepath.Join(target, "hosted-world.zip")
	writeSaveArchive(t, hosted, map[string]string{"hosted/level.dat": "hosted", "hosted/level-init.dat": "hosted-init"})
	original, err := os.ReadFile(hosted)
	if err != nil {
		t.Fatal(err)
	}
	invalid := zipBytes(t, map[string]string{"notes.txt": "not-a-save"})
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(invalid)), Header: make(http.Header)}, nil
	})}
	_, err = (Importer{Client: client}).Import(context.Background(), t.TempDir(), target, ArchiveDirectory, ReplaceArchives, "copper-works.zip", "https://objects.example/import?signature=must-not-leak")
	stage, message := Diagnostic(err)
	if err == nil || stage != StageValidation || !strings.Contains(message, "missing required entry") || strings.Contains(message, "must-not-leak") {
		t.Fatalf("invalid archive diagnostic stage=%q message=%q", stage, message)
	}
	retained, err := os.ReadFile(hosted)
	if err != nil || bytes.Equal(retained, original) == false {
		t.Fatal("invalid import mutated the hosted save")
	}
}

func TestImportDownloadFailureDoesNotClaimRecoverySnapshot(t *testing.T) {
	target := t.TempDir()
	writeSaveArchive(t, filepath.Join(target, "hosted-world.zip"), map[string]string{"hosted/level.dat": "hosted", "hosted/level-init.dat": "hosted-init"})
	client := &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("https://objects.example/import.zip?signature=must-not-leak")
	})}
	_, err := (Importer{Client: client}).Import(context.Background(), t.TempDir(), target, ArchiveDirectory, ReplaceArchives, "copper-works.zip", "https://objects.example/import.zip?signature=must-not-leak")
	stage, message := Diagnostic(err)
	if err == nil || stage != StageDownload || strings.Contains(message, "must-not-leak") || strings.Contains(strings.ToLower(message), "snapshot") {
		t.Fatalf("download diagnostic stage=%q message=%q", stage, message)
	}
}

func TestImportRejectsUnsafeArchiveNames(t *testing.T) {
	_, err := (Importer{Client: noRequestClient(t)}).Import(context.Background(), t.TempDir(), t.TempDir(), ArchiveDirectory, ReplaceArchives, "../escape.zip", "https://objects.example/import")
	if err == nil || !strings.Contains(err.Error(), "safe Factorio save archive name") {
		t.Fatalf("unsafe archive name was accepted: %v", err)
	}
}

func writeSaveArchive(t *testing.T, name string, entries map[string]string) {
	t.Helper()
	if err := os.WriteFile(name, zipBytes(t, entries), 0o600); err != nil {
		t.Fatal(err)
	}
}

func zipBytes(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
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
	return buffer.Bytes()
}

func noRequestClient(t *testing.T) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		t.Fatal("unexpected download")
		return nil, errors.New("unreachable")
	})}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
