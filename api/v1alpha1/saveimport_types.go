package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type SaveImportPhase string

const (
	SaveImportPending   SaveImportPhase = "Pending"
	SaveImportRunning   SaveImportPhase = "Running"
	SaveImportSucceeded SaveImportPhase = "Succeeded"
	SaveImportFailed    SaveImportPhase = "Failed"
	SaveImportExpired   SaveImportPhase = "Expired"
)

// SaveImportSpec is the backend-authorized request for one managed save
// replacement. No filesystem path is accepted here; the game adapter owns it.
type SaveImportSpec struct {
	// +kubebuilder:validation:MinLength=1
	ServerID string `json:"serverID"`
	// +kubebuilder:validation:MinLength=1
	OwnerUserID string `json:"ownerUserID"`
	// +kubebuilder:validation:MinLength=1
	SetupID string `json:"setupID"`
	// +kubebuilder:validation:MinLength=1
	GameID string `json:"gameID"`
	// +kubebuilder:validation:MinLength=1
	DownloadURLSecretRef string `json:"downloadURLSecretRef"`
	// +kubebuilder:validation:MinLength=1
	ArchiveName string      `json:"archiveName"`
	ExpiresAt   metav1.Time `json:"expiresAt"`
}

type SaveImportStatus struct {
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Expired
	Phase SaveImportPhase `json:"phase,omitempty"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	ProgressPercent int32 `json:"progressPercent,omitempty"`
	// Stage is a bounded operational stage, not byte-level progress.
	// +kubebuilder:validation:MaxLength=32
	Stage       string `json:"stage,omitempty"`
	ArchiveName string `json:"archiveName,omitempty"`
	// +kubebuilder:validation:Minimum=0
	ArchiveBytes int64 `json:"archiveBytes,omitempty"`
	// +kubebuilder:validation:MaxLength=512
	Message string `json:"message,omitempty"`
	// Recovery is a bounded recovery outcome, never a filesystem path.
	// +kubebuilder:validation:Enum=none;snapshot-created;restored;rollback-failed
	Recovery   string       `json:"recovery,omitempty"`
	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=si
type SaveImport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SaveImportSpec   `json:"spec,omitempty"`
	Status            SaveImportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SaveImportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SaveImport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SaveImport{}, &SaveImportList{})
}
