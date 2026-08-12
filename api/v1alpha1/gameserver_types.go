package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// NOTE: This file is the central place for the GameServer CRD definition.
// Run `go generate` (after installing controller-gen) to regenerate
// deepcopy and CRD manifests.

// GameServerSpec defines the desired state of GameServer.
//
// Most game-specific defaults (image, environment, minimum disk, config templates,
// which paths are raw-disk-only, etc.) live in the controller's internal game
// definitions (see internal/games in the controller repo). The controller applies
// these at reconciliation / creation time.
//
// The CRD stays relatively small and generic. Game-specific user choices go
// into GameConfig.
type GameServerSpec struct {
	// ServerID is the stable identifier from the backend database.
	// +kubebuilder:validation:Required
	ServerID string `json:"serverID"`

	// OwnerUserID is the backend user who owns this server.
	// +kubebuilder:validation:Required
	OwnerUserID string `json:"ownerUserID"`

	// GameID identifies which game (e.g. "factorio", "project-zomboid").
	// +kubebuilder:validation:Required
	GameID string `json:"gameID"`

	// ComputePlanID references the purchased base node plan.
	// The controller uses this + HighPerformance + the game definition to
	// determine final resource requests (including disk at spin-up time).
	ComputePlanID string `json:"computePlanID,omitempty"`

	// HighPerformance enables the plan's additional CPU scheduling priority and
	// memory entitlement without changing its burst CPU ceiling.
	HighPerformance bool `json:"highPerformance,omitempty"`

	// Region is the desired broad deployment region (for example, "australia").
	Region string `json:"region,omitempty"`

	// Location is the desired configured location within Region (for example,
	// "sydney"). Admin assignments may use configured testing locations before
	// they become publicly available.
	Location string `json:"location,omitempty"`

	// Image allows overriding the container image the controller would normally
	// choose for this gameID.
	Image string `json:"image,omitempty"`

	// Storage allows the user (or backend) to request a specific persistent
	// volume size. The controller will ensure the final size meets the game's
	// minimums + the compute plan (see internal/games.CalculateDiskSize).
	Storage *StorageSpec `json:"storage,omitempty"`

	// GameConfig holds user-provided, game-specific settings.
	// The controller looks up the GameDefinition for spec.gameID and uses its
	// ConfigLayer templates to turn these values into ConfigMaps (the preferred
	// "config layer").
	GameConfig map[string]string `json:"gameConfig,omitempty"`
}

// StorageSpec describes storage requirements for the game server.
type StorageSpec struct {
	Size string `json:"size,omitempty"` // e.g. "50Gi"
}

// GameServerStatus defines the observed state of GameServer.
type GameServerStatus struct {
	// Phase is a high-level summary of where the GameServer is in its lifecycle.
	// +kubebuilder:validation:Enum=Pending;Running;Stopped;Failed;EditorActive
	Phase string `json:"phase,omitempty"`

	// Endpoint is the public address players can connect to (if running).
	Endpoint string `json:"endpoint,omitempty"`

	// ObservedGeneration is the most recent generation the controller has observed.
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions represent the latest available observations of the GameServer's state.
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastReadyAt is when the game server last reported as ready/healthy.
	LastReadyAt *metav1.Time `json:"lastReadyAt,omitempty"`

	// Message is a human-readable explanation of the current state.
	Message string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Game",type=string,JSONPath=`.spec.gameID`
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
