package v1alpha1

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestSaveExportContractDoesNotAcceptCustomerFilesystemPathsOrURLs(t *testing.T) {
	expiresAt := metav1.NewTime(time.Date(2026, 8, 15, 0, 10, 0, 0, time.UTC))
	export := SaveExport{Spec: SaveExportSpec{
		ServerID: "server-1", OwnerUserID: "owner-1", SetupID: "setup-1", GameID: "factorio",
		UploadURLSecretRef: "save-export-1-upload", ExpiresAt: expiresAt,
	}}
	encoded, err := json.Marshal(export.Spec)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"path", "url", "pvc"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) && forbidden != "url" {
			t.Fatalf("managed export contract exposed forbidden %s input: %s", forbidden, encoded)
		}
	}
	if strings.Contains(string(encoded), "https://") {
		t.Fatalf("managed export contract exposed transfer authorization: %s", encoded)
	}
}
