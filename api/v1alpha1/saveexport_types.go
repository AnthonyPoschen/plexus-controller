package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

type SaveExportPhase string

const (
	SaveExportPending   SaveExportPhase = "Pending"
	SaveExportRunning   SaveExportPhase = "Running"
	SaveExportSucceeded SaveExportPhase = "Succeeded"
	SaveExportFailed    SaveExportPhase = "Failed"
	SaveExportExpired   SaveExportPhase = "Expired"
)

// SaveExportSpec is the backend-authorized request for one managed save
// export. No filesystem path is accepted here; the game adapter owns it.
type SaveExportSpec struct {
	// +kubebuilder:validation:MinLength=1
	ServerID string `json:"serverID"`
	// +kubebuilder:validation:MinLength=1
	OwnerUserID string `json:"ownerUserID"`
	// +kubebuilder:validation:MinLength=1
	SetupID string `json:"setupID"`
	// +kubebuilder:validation:MinLength=1
	GameID string `json:"gameID"`
	// +kubebuilder:validation:MinLength=1
	UploadURLSecretRef string      `json:"uploadURLSecretRef"`
	ExpiresAt          metav1.Time `json:"expiresAt"`
}

type SaveExportStatus struct {
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Expired
	Phase SaveExportPhase `json:"phase,omitempty"`
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
	Message    string       `json:"message,omitempty"`
	StartedAt  *metav1.Time `json:"startedAt,omitempty"`
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=se
type SaveExport struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              SaveExportSpec   `json:"spec,omitempty"`
	Status            SaveExportStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type SaveExportList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SaveExport `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SaveExport{}, &SaveExportList{})
}
