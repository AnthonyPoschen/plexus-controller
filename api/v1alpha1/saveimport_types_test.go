package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSaveImportContractDoesNotAcceptCustomerFilesystemPathsOrURLs(t *testing.T) {
	expiresAt := metav1.NewTime(time.Date(2026, 8, 15, 0, 10, 0, 0, time.UTC))
	replacement := SaveImport{Spec: SaveImportSpec{
		ServerID: "server-1", OwnerUserID: "owner-1", SetupID: "setup-1", GameID: "factorio",
		DownloadURLSecretRef: "save-import-1-download", ArchiveName: "copper-works.zip", ExpiresAt: expiresAt,
	}}
	encoded, err := json.Marshal(replacement.Spec)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(strings.ToLower(string(encoded)), "path") || strings.Contains(strings.ToLower(string(encoded)), "pvc") {
		t.Fatalf("managed import contract exposed forbidden path input: %s", encoded)
	}
	if strings.Contains(string(encoded), "https://") {
		t.Fatalf("managed import contract exposed transfer authorization: %s", encoded)
	}
}
