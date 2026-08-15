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
	runner := importer.Importer{Progress: writeProgress}
	archiveBytes, err := runner.Import(context.Background(), "/work", "/target", targetLayout, replacement, archiveName, downloadURL)
	if err != nil {
		stage, message := importer.Diagnostic(err)
		writeTermination(terminationStatus{Stage: string(stage), Message: message})
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	writeTermination(terminationStatus{Stage: "complete", ArchiveBytes: archiveBytes})
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
}

func writeTermination(status terminationStatus) {
	data, err := json.Marshal(status)
	if err == nil {
		_ = os.WriteFile("/dev/termination-log", data, 0o600)
	}
}
