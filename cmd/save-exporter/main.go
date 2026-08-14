package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/AnthonyPoschen/plexus-controller/internal/exporter"
)

func main() {
	uploadURL := os.Getenv("PLEXUS_UPLOAD_URL")
	sourceLayout := os.Getenv("PLEXUS_SAVE_SOURCE_LAYOUT")
	selection := os.Getenv("PLEXUS_SAVE_SELECTION")
	runner := exporter.Exporter{Progress: writeProgress}
	archiveBytes, err := runner.Export(context.Background(), "/source", sourceLayout, selection, uploadURL)
	if err != nil {
		stage, message := exporter.Diagnostic(err)
		writeTermination(terminationStatus{Stage: string(stage), Message: message})
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	writeTermination(terminationStatus{Stage: "complete", ArchiveBytes: archiveBytes})
}

func writeProgress(progress exporter.Progress) {
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
