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
	"time"
)

func TestImportSnapshotsThenReplacesOnlyAdapterDeclaredSaveArchives(t *testing.T) {
	target := t.TempDir()
	work := t.TempDir()
	writeSaveArchive(t, filepath.Join(target, "_autosave1.zip"), map[string]string{"old/level.dat": "old", "old/level-init.dat": "old-init"})
	originalHosted := writeSaveArchive(t, filepath.Join(target, "hosted-world.zip"), map[string]string{"hosted/level.dat": "hosted", "hosted/level-init.dat": "hosted-init"})
	if err := os.WriteFile(filepath.Join(target, "notes.txt"), []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(target, "keep-dir"), 0o755); err != nil {
		t.Fatal(err)
	}

	replacement := zipBytes(t, map[string]string{"copper-works/level.dat": "level", "copper-works/level-init.dat": "init"})
	var progress []Progress
	result, err := (Importer{Client: downloadClient(t, replacement), Progress: func(update Progress) { progress = append(progress, update) }}).Import(
		context.Background(), work, target, ArchiveDirectory, ReplaceArchives, "copper-works.zip", "https://objects.example/import.zip?signature=redacted", "import-1",
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.ArchiveBytes != int64(len(replacement)) || result.Recovery != RecoverySnapshotCreated {
		t.Fatalf("imported result = %#v", result)
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
	snapshotted, err := os.ReadFile(filepath.Join(target, recoveryDirName, "import-1", "hosted-world.zip"))
	if err != nil || bytes.Equal(snapshotted, originalHosted) == false {
		t.Fatalf("current hosted save was not snapshotted: %v", err)
	}
	want := []Progress{
		{Stage: StageDownload, Percent: 10}, {Stage: StageDownload, Percent: 25},
		{Stage: StageValidation, Percent: 30}, {Stage: StageValidation, Percent: 40},
		{Stage: StageSnapshot, Percent: 50}, {Stage: StageSnapshot, Percent: 60},
		{Stage: StageReplace, Percent: 70}, {Stage: StageReplace, Percent: 85},
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

func TestImportRestoresSnapshotWhenReplacementFails(t *testing.T) {
	target := t.TempDir()
	hosted := filepath.Join(target, "hosted-world.zip")
	original := writeSaveArchive(t, hosted, map[string]string{"hosted/level.dat": "hosted", "hosted/level-init.dat": "hosted-init"})
	replacement := zipBytes(t, map[string]string{"copper-works/level.dat": "level", "copper-works/level-init.dat": "init"})
	importer := Importer{
		Client: downloadClient(t, replacement),
		replace: func(targetRoot string, _ string, _ string) error {
			if err := os.Remove(filepath.Join(targetRoot, "hosted-world.zip")); err != nil {
				t.Fatal(err)
			}
			return errors.New("install failed")
		},
	}
	result, err := importer.Import(context.Background(), t.TempDir(), target, ArchiveDirectory, ReplaceArchives, "copper-works.zip", "https://objects.example/import", "import-restore")
	stage, message := Diagnostic(err)
	if err == nil || stage != StageReplace || result.Recovery != RecoveryRestored || RecoveryOf(err) != RecoveryRestored {
		t.Fatalf("failed replace was not restored: result=%#v stage=%q err=%v", result, stage, err)
	}
	if !strings.Contains(message, "restored from the automatic recovery snapshot") || strings.Contains(strings.ToLower(message), recoveryDirName) {
		t.Fatalf("restore diagnostic leaked or was incomplete: %q", message)
	}
	retained, err := os.ReadFile(hosted)
	if err != nil || bytes.Equal(retained, original) == false {
		t.Fatal("failed replace did not restore the previous hosted save")
	}
}

func TestImportReportsRecoverableSnapshotWhenRollbackFails(t *testing.T) {
	target := t.TempDir()
	original := writeSaveArchive(t, filepath.Join(target, "hosted-world.zip"), map[string]string{"hosted/level.dat": "hosted", "hosted/level-init.dat": "hosted-init"})
	replacement := zipBytes(t, map[string]string{"copper-works/level.dat": "level", "copper-works/level-init.dat": "init"})
	importer := Importer{
		Client: downloadClient(t, replacement),
		replace: func(targetRoot string, _ string, _ string) error {
			if err := os.Remove(filepath.Join(targetRoot, "hosted-world.zip")); err != nil {
				t.Fatal(err)
			}
			return errors.New("install failed")
		},
		restore: func(string, string) error { return errors.New("copy failed") },
	}
	result, err := importer.Import(context.Background(), t.TempDir(), target, ArchiveDirectory, ReplaceArchives, "copper-works.zip", "https://objects.example/import", "import-rollback")
	stage, message := Diagnostic(err)
	if err == nil || stage != StageRollback || result.Recovery != RecoveryRollbackFailed || RecoveryOf(err) != RecoveryRollbackFailed {
		t.Fatalf("failed rollback was not reported: result=%#v stage=%q err=%v", result, stage, err)
	}
	if !strings.Contains(message, "recoverable safety snapshot is retained") {
		t.Fatalf("rollback failure hid the recoverable snapshot: %q", message)
	}
	snapshotted, err := os.ReadFile(filepath.Join(target, recoveryDirName, "import-rollback", "hosted-world.zip"))
	if err != nil || bytes.Equal(snapshotted, original) == false {
		t.Fatalf("failed rollback discarded the recovery snapshot: %v", err)
	}
}

func TestImportRetryReusesExistingSnapshot(t *testing.T) {
	target := t.TempDir()
	original := writeSaveArchive(t, filepath.Join(target, "hosted-world.zip"), map[string]string{"hosted/level.dat": "hosted", "hosted/level-init.dat": "hosted-init"})
	replacement := zipBytes(t, map[string]string{"copper-works/level.dat": "level", "copper-works/level-init.dat": "init"})
	failing := Importer{
		Client: downloadClient(t, replacement),
		replace: func(targetRoot string, _ string, _ string) error {
			_ = os.WriteFile(filepath.Join(targetRoot, "partial.zip"), []byte("corrupt"), 0o600)
			_ = os.Remove(filepath.Join(targetRoot, "hosted-world.zip"))
			return errors.New("install failed")
		},
		restore: func(string, string) error { return errors.New("copy failed") },
	}
	if _, err := failing.Import(context.Background(), t.TempDir(), target, ArchiveDirectory, ReplaceArchives, "copper-works.zip", "https://objects.example/import", "import-retry"); err == nil {
		t.Fatal("expected first replacement to fail")
	}
	if err := os.WriteFile(filepath.Join(target, "partial.zip"), []byte("still-corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	retried, err := (Importer{Client: downloadClient(t, replacement)}).Import(
		context.Background(), t.TempDir(), target, ArchiveDirectory, ReplaceArchives, "copper-works.zip", "https://objects.example/import", "import-retry",
	)
	if err != nil || retried.Recovery != RecoverySnapshotCreated {
		t.Fatalf("retry did not complete: %#v %v", retried, err)
	}
	if _, err := os.Stat(filepath.Join(target, "partial.zip")); err == nil {
		t.Fatal("retry left a partial hosted archive")
	}
	installed, err := os.ReadFile(filepath.Join(target, "copper-works.zip"))
	if err != nil || bytes.Equal(installed, replacement) == false {
		t.Fatal("retry did not install the replacement archive")
	}
	snapshotted, err := os.ReadFile(filepath.Join(target, recoveryDirName, "import-retry", "hosted-world.zip"))
	if err != nil || bytes.Equal(snapshotted, original) == false {
		t.Fatal("retry overwrote the original recovery snapshot")
	}
}

func TestImportPrunesOlderRecoverySnapshots(t *testing.T) {
	target := t.TempDir()
	writeSaveArchive(t, filepath.Join(target, "hosted-world.zip"), map[string]string{"hosted/level.dat": "hosted", "hosted/level-init.dat": "hosted-init"})
	oldest := filepath.Join(target, recoveryDirName, "import-old")
	older := filepath.Join(target, recoveryDirName, "import-older")
	if err := os.MkdirAll(oldest, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(older, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(oldest, "stale.zip"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(older, "stale.zip"), []byte("older"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTime := time.Now().Add(-2 * time.Hour)
	olderTime := time.Now().Add(-3 * time.Hour)
	if err := os.Chtimes(oldest, oldTime, oldTime); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(older, olderTime, olderTime); err != nil {
		t.Fatal(err)
	}
	replacement := zipBytes(t, map[string]string{"copper-works/level.dat": "level", "copper-works/level-init.dat": "init"})
	if _, err := (Importer{Client: downloadClient(t, replacement)}).Import(
		context.Background(), t.TempDir(), target, ArchiveDirectory, ReplaceArchives, "copper-works.zip", "https://objects.example/import", "import-new",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(older); err == nil {
		t.Fatal("oldest recovery snapshot was retained")
	}
	if _, err := os.Stat(oldest); err != nil {
		t.Fatal("retention removed a snapshot still inside the keep window")
	}
	if _, err := os.Stat(filepath.Join(target, recoveryDirName, "import-new")); err != nil {
		t.Fatal("current recovery snapshot was pruned")
	}
}

func TestImportRejectsInvalidArchivesWithoutTouchingHostedSaves(t *testing.T) {
	target := t.TempDir()
	hosted := filepath.Join(target, "hosted-world.zip")
	original := writeSaveArchive(t, hosted, map[string]string{"hosted/level.dat": "hosted", "hosted/level-init.dat": "hosted-init"})
	invalid := zipBytes(t, map[string]string{"notes.txt": "not-a-save"})
	_, err := (Importer{Client: downloadClient(t, invalid)}).Import(context.Background(), t.TempDir(), target, ArchiveDirectory, ReplaceArchives, "copper-works.zip", "https://objects.example/import?signature=must-not-leak", "import-invalid")
	stage, message := Diagnostic(err)
	if err == nil || stage != StageValidation || !strings.Contains(message, "missing required entry") || strings.Contains(message, "must-not-leak") {
		t.Fatalf("invalid archive diagnostic stage=%q message=%q", stage, message)
	}
	if RecoveryOf(err) != RecoveryNone {
		t.Fatalf("invalid archive claimed recovery: %q", RecoveryOf(err))
	}
	if _, err := os.Stat(filepath.Join(target, recoveryDirName)); err == nil {
		t.Fatal("invalid archive created a recovery snapshot")
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
	_, err := (Importer{Client: client}).Import(context.Background(), t.TempDir(), target, ArchiveDirectory, ReplaceArchives, "copper-works.zip", "https://objects.example/import.zip?signature=must-not-leak", "import-download")
	stage, message := Diagnostic(err)
	if err == nil || stage != StageDownload || strings.Contains(message, "must-not-leak") || strings.Contains(strings.ToLower(message), "snapshot") {
		t.Fatalf("download diagnostic stage=%q message=%q", stage, message)
	}
	if RecoveryOf(err) != RecoveryNone {
		t.Fatalf("download failure claimed recovery: %q", RecoveryOf(err))
	}
}

func TestImportRejectsUnsafeArchiveNames(t *testing.T) {
	_, err := (Importer{Client: noRequestClient(t)}).Import(context.Background(), t.TempDir(), t.TempDir(), ArchiveDirectory, ReplaceArchives, "../escape.zip", "https://objects.example/import", "import-1")
	if err == nil || !strings.Contains(err.Error(), "safe Factorio save archive name") {
		t.Fatalf("unsafe archive name was accepted: %v", err)
	}
}

func writeSaveArchive(t *testing.T, name string, entries map[string]string) []byte {
	t.Helper()
	payload := zipBytes(t, entries)
	if err := os.WriteFile(name, payload, 0o600); err != nil {
		t.Fatal(err)
	}
	return payload
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

func downloadClient(t *testing.T, archive []byte) *http.Client {
	t.Helper()
	return &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.Method != http.MethodGet {
			t.Fatalf("unexpected method %s", request.Method)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(archive)), Header: make(http.Header)}, nil
	})}
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
