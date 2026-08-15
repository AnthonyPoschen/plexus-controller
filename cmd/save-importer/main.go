package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/AnthonyPoschen/plexus-controller/internal/importer"
)

func main() {
	downloadURL := os.Getenv("PLEXUS_DOWNLOAD_URL")
	targetLayout := os.Getenv("PLEXUS_SAVE_TARGET_LAYOUT")
	replacement := os.Getenv("PLEXUS_SAVE_REPLACEMENT")
	archiveName := os.Getenv("PLEXUS_ARCHIVE_NAME")
	importID := os.Getenv("PLEXUS_SAVE_IMPORT_ID")
	runner := importer.Importer{Progress: writeProgress}
	result, err := runner.Import(context.Background(), "/work", "/target", targetLayout, replacement, archiveName, downloadURL, importID)
	if err != nil {
		stage, message := importer.Diagnostic(err)
		writeTermination(terminationStatus{Stage: string(stage), Message: message, Recovery: importer.RecoveryOf(err)})
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	writeTermination(terminationStatus{Stage: "complete", ArchiveBytes: result.ArchiveBytes, Recovery: result.Recovery})
}

func writeProgress(progress importer.Progress) {
	data, err := json.Marshal(progress)
	if err == nil {
		fmt.Println(string(data))
	}
}

type terminationStatus struct {
	Stage        string `json:"stage"`
	ArchiveBytes int64  `json:"archiveBytes,omitempty"`
	Message      string `json:"message,omitempty"`
	Recovery     string `json:"recovery,omitempty"`
}

func writeTermination(status terminationStatus) {
	data, err := json.Marshal(status)
	if err == nil {
		_ = os.WriteFile("/dev/termination-log", data, 0o600)
	}
}
