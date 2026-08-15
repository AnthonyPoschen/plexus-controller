package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// NOTE: This file is the central place for the GameServer CRD definition.
// Run `go generate ./api/...` to regenerate deepcopy and CRD manifests.

// DesiredPower is the backend's durable intent for the selected setup.
// +kubebuilder:validation:Enum=Running;Stopped
type DesiredPower string

const (
	DesiredPowerRunning DesiredPower = "Running"
	DesiredPowerStopped DesiredPower = "Stopped"
)

// ShutdownMode selects the shutdown behavior for the current stop intent.
// +kubebuilder:validation:Enum=Graceful;Force
type ShutdownMode string

const (
	ShutdownModeGraceful ShutdownMode = "Graceful"
	ShutdownModeForce    ShutdownMode = "Force"
)

// GameServerPhase is the controller's latest observed runtime lifecycle phase.
// +kubebuilder:validation:Enum=Unknown;Provisioning;Stopped;Starting;Running;Stopping;Failed
type GameServerPhase string

const (
	GameServerPhaseUnknown      GameServerPhase = "Unknown"
	GameServerPhaseProvisioning GameServerPhase = "Provisioning"
	GameServerPhaseStopped      GameServerPhase = "Stopped"
	GameServerPhaseStarting     GameServerPhase = "Starting"
	GameServerPhaseRunning      GameServerPhase = "Running"
	GameServerPhaseStopping     GameServerPhase = "Stopping"
	GameServerPhaseFailed       GameServerPhase = "Failed"
)

// GameServerSpec defines the desired runtime state of one stable customer
// Server. A Server may be unloaded by omitting SelectedSetup, but its desired
// power must then remain Stopped.
// +kubebuilder:validation:XValidation:rule="has(self.selectedSetup) || self.desiredPower == 'Stopped'",message="desiredPower must be Stopped when selectedSetup is omitted"
type GameServerSpec struct {
	// ServerID is the stable identifier from the backend database.
	// +kubebuilder:validation:MinLength=1
	ServerID string `json:"serverID"`

	// OwnerUserID is the backend user who owns this Server.
	// +kubebuilder:validation:MinLength=1
	OwnerUserID string `json:"ownerUserID"`

	// DesiredPower is the customer's durable Running or Stopped intent.
	DesiredPower DesiredPower `json:"desiredPower"`

	// ComputePlanID references the purchased base node plan.
	ComputePlanID string `json:"computePlanID,omitempty"`

	// HighPerformance enables the plan's additional CPU scheduling priority and
	// memory entitlement without changing its burst CPU ceiling.
	HighPerformance bool `json:"highPerformance,omitempty"`

	// Region is the desired broad deployment region (for example, "australia").
	Region string `json:"region,omitempty"`

	// Location is the desired configured location within Region (for example,
	// "sydney").
	Location string `json:"location,omitempty"`

	// SelectedSetup is the one saved setup this Server will run. It is omitted
	// when the Server is unloaded; retained setup data remains independent.
	SelectedSetup *SelectedSetupSpec `json:"selectedSetup,omitempty"`

	// RestartGeneration is incremented to request another restart of the same
	// selected setup while leaving DesiredPower Running.
	// +kubebuilder:validation:Minimum=0
	RestartGeneration int64 `json:"restartGeneration,omitempty"`

	// ShutdownMode selects graceful shutdown or an explicit force-stop intent.
	ShutdownMode ShutdownMode `json:"shutdownMode"`
}

// SelectedSetupSpec identifies the saved setup selected for this Server and
// carries only its versioned desired configuration contract.
type SelectedSetupSpec struct {
	// ID is the stable backend identifier of the selected saved setup.
	// +kubebuilder:validation:MinLength=1
	ID string `json:"id"`

	// GameID selects the controller-owned game adapter.
	// +kubebuilder:validation:MinLength=1
	GameID string `json:"gameID"`

	// Configuration contains structured, non-sensitive values and a reference
	// to this setup's sensitive values.
	Configuration GameConfiguration `json:"configuration"`

	// Mods is the provider-validated enabled selection for the next runtime.
	// Artifact-backed adapters stage an immutable archive; Steam Workshop
	// adapters carry only provider identity and load IDs. Customers cannot
	// provide a URL, filename, or filesystem path directly.
	// +kubebuilder:validation:MaxItems=1
	Mods []ModSpec `json:"mods,omitempty"`
}

// ModSpec identifies one provider release. Artifact fields are required only
// for archive-backed adapters. Steam Workshop selections leave them empty and
// rely on startup download. All filesystem interpretation remains owned by the
// game adapter.
type ModSpec struct {
	ProviderID      string   `json:"providerID"`
	ProviderModID   string   `json:"providerModID"`
	Name            string   `json:"name"`
	Version         string   `json:"version"`
	GameVersion     string   `json:"gameVersion"`
	Dependencies    []string `json:"dependencies"`
	LoadIDs         []string `json:"loadIDs,omitempty"`
	ArchiveFileName string   `json:"archiveFileName"`
	ArchiveSHA256   string   `json:"archiveSHA256"`
	ArtifactRef     string   `json:"artifactRef"`
}

// InstalledMod reports a release observed after the managed disk job
// installed it on the retained PVC.
type InstalledMod struct {
	ProviderID    string `json:"providerID"`
	ProviderModID string `json:"providerModID"`
	Name          string `json:"name"`
	Version       string `json:"version"`
}

// GameConfiguration is the stable envelope around adapter-specific structured
// values. The backend and controller validate Values against SchemaVersion.
type GameConfiguration struct {
	// SchemaVersion selects the versioned game-management configuration schema.
	// +kubebuilder:validation:MinLength=1
	SchemaVersion string `json:"schemaVersion"`

	// Values contains structured JSON/YAML, not a rendered configuration file.
	// Sensitive values must never be placed here.
	Values runtime.RawExtension `json:"values"`

	// SecretRef names a Secret in the GameServer namespace that belongs only to
	// this selected setup and conforms to the adapter's versioned secret schema.
	SecretRef SetupSecretReference `json:"secretRef"`
}

// SetupSecretReference identifies a setup-scoped Secret in the GameServer's
// namespace. Namespace is intentionally absent to prevent cross-namespace use.
type SetupSecretReference struct {
	// Name is the Kubernetes Secret name.
	// +kubebuilder:validation:MinLength=1
	Name string `json:"name"`
}

// GameServerStatus defines the controller's latest observed runtime state.
type GameServerStatus struct {
	// Phase is the observed lifecycle phase, independent of desired power.
	Phase GameServerPhase `json:"phase,omitempty"`

	// ActiveSetupID is the setup the runtime has actually activated.
	ActiveSetupID string `json:"activeSetupID,omitempty"`

	// ObservedGeneration is the latest GameServer metadata generation observed.
	// +kubebuilder:validation:Minimum=0
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// ObservedRestartGeneration is the latest restart request observed.
	// +kubebuilder:validation:Minimum=0
	ObservedRestartGeneration int64 `json:"observedRestartGeneration,omitempty"`

	// ObservedConfigurationGeneration is the GameServer generation whose
	// selected setup configuration is active in the available workload.
	// +kubebuilder:validation:Minimum=0
	ObservedConfigurationGeneration int64 `json:"observedConfigurationGeneration,omitempty"`

	// ObservedSecretRevision is the setup Secret revision active in the
	// available workload. It never contains Secret material.
	// +kubebuilder:validation:Minimum=0
	ObservedSecretRevision int64 `json:"observedSecretRevision,omitempty"`

	// InstalledMods is runtime observation, never inferred by the backend from
	// the desired enabled selection. InstalledModsGeneration identifies the
	// configuration generation whose available workload produced the list.
	// +listType=map
	// +listMapKey=providerID
	// +listMapKey=providerModID
	InstalledMods []InstalledMod `json:"installedMods,omitempty"`

	// +kubebuilder:validation:Minimum=0
	InstalledModsGeneration int64 `json:"installedModsGeneration,omitempty"`

	// Endpoint is the public address players can connect to when available.
	Endpoint string `json:"endpoint,omitempty"`

	// Players is the latest observed connected-player count. Nil means the
	// controller did not observe a count.
	// +kubebuilder:validation:Minimum=0
	Players *int32 `json:"players,omitempty"`

	// Conditions represent the latest observations of individual runtime
	// concerns.
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastObservedAt is when this status was last refreshed from runtime state.
	LastObservedAt *metav1.Time `json:"lastObservedAt,omitempty"`

	// Message is a human-readable explanation of the current state.
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Game",type=string,JSONPath=`.spec.selectedSetup.gameID`
// +kubebuilder:printcolumn:name="Desired",type=string,JSONPath=`.spec.desiredPower`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Endpoint",type=string,JSONPath=`.status.endpoint`
// +kubebuilder:resource:shortName=gs

// GameServer is the Schema for the gameservers API.
type GameServer struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GameServerSpec   `json:"spec,omitempty"`
	Status GameServerStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GameServerList contains a list of GameServer.
type GameServerList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GameServer `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GameServer{}, &GameServerList{})
}
