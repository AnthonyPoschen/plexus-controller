package games

import (
	"fmt"

	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
	"github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/model"
	zomboid "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/projectzomboid/v1"
)

// GameDefinition contains all the controller-owned defaults and behavior
// for a specific game. This is the source of truth for runtime details
// inside the controller.
type GameDefinition struct {
	ID                      string
	ManagementSchemaVersion string

	// Display is mostly for logs / status
	DisplayName string

	// DefaultImage is the container image the controller uses for this game.
	DefaultImage string

	// MinDiskGiB is the minimum storage this game requires. The controller will
	// ensure the PVC is at least this large (taking into account the compute plan).
	MinDiskGiB int

	// RecommendedDiskGiB is what the controller suggests for a comfortable experience
	// on top of the base compute plan.
	RecommendedDiskGiB int

	// ConfigLayer defines files that should be managed via ConfigMaps (the
	// preferred, restart-required path).
	ConfigLayer ConfigLayer

	// RawDiskPaths are paths that live on the PVC and are only safely mutable
	// via an editor pod when the main game server is stopped.
	RawDiskPaths []string

	// SaveExport declares the only PVC path a managed save export may read.
	// It is controller-owned and is never supplied by a customer request.
	SaveExport *SaveExportDefinition

	// SaveImport declares the only PVC path a managed save replacement may write.
	// It is controller-owned and is never supplied by a customer request.
	SaveImport *SaveImportDefinition

	// DefaultEnv are environment variables the controller will set unless
	// the user overrides them.
	DefaultEnv map[string]string

	// Ports the game listens on (used for Service creation).
	Ports []GamePort

	// Shutdown is the adapter-owned graceful shutdown contract used when a
	// workload is stopped or replaced.
	Shutdown model.ShutdownPolicy

	// Workload describes adapter-owned container mounts and rendering.
	Workload WorkloadSpec
}

// WorkloadSpec is the controller-owned runtime layout for one game adapter.
type WorkloadSpec struct {
	ContainerName    string
	DataMountPath    string
	Config           ConfigRuntime
	SecretEnvKeys    []string
	SupportsMods     bool
	WorkshopStartup  bool
	Supervisor       bool
	AdditionalMounts []VolumeMount
}

type ConfigRuntime struct {
	VolumeName      string
	SourceName      string
	MountPath       string
	SourcePath      string
	FileName        string
	InitName        string
	InitCopyCommand string
}

type VolumeMount struct {
	Name      string
	MountPath string
	SubPath   string
}

type SaveExportDefinition struct {
	PVCSubPath   string
	SourceLayout SaveExportSourceLayout
	Selection    SaveExportSelection
	ArchiveName  string
}

type SaveExportSourceLayout string

const SaveExportSourceArchiveDirectory SaveExportSourceLayout = "archive-directory"

type SaveExportSelection string

const SaveExportSelectLatestModifiedArchive SaveExportSelection = "latest-modified-archive"

type SaveImportDefinition struct {
	PVCSubPath   string
	TargetLayout SaveImportTargetLayout
	Replacement  SaveImportReplacement
}

type SaveImportTargetLayout string

const SaveImportTargetArchiveDirectory SaveImportTargetLayout = "archive-directory"

type SaveImportReplacement string

const SaveImportReplaceArchives SaveImportReplacement = "replace-archives"

type GamePort struct {
	Name     string
	Port     int32
	Protocol string // TCP or UDP
}

type ConfigLayer struct {
	// Templates describe how to turn the selected setup's decoded, versioned
	// configuration values into actual files mounted as ConfigMaps.
	Templates []ConfigTemplate
}

// ConfigTemplate is a simple way for the controller to render game config
// without the CRD knowing the exact shape of every game.
type ConfigTemplate struct {
	// TargetPath inside the container (e.g. /config/server.properties)
	TargetPath string

	// Format hints how to render (json, properties, ini, etc.)
	Format string

	// Mappings: adapter configuration key -> key in the rendered file.
	Mappings map[string]string
}

// Registry holds all known games. This is where defaults live in the controller.
var Registry = map[string]GameDefinition{
	factorio.GameID: {
		ID:                      factorio.GameID,
		ManagementSchemaVersion: factorio.SchemaVersion,
		DisplayName:             "Factorio",
		DefaultImage:            factorio.RuntimeImage,
		MinDiskGiB:              10,
		RecommendedDiskGiB:      50,
		RawDiskPaths:            []string{"/saves", "/mods"},
		SaveExport: &SaveExportDefinition{
			PVCSubPath: "saves", SourceLayout: SaveExportSourceArchiveDirectory,
			Selection: SaveExportSelectLatestModifiedArchive, ArchiveName: "factorio-save.zip",
		},
		SaveImport: &SaveImportDefinition{
			PVCSubPath: "saves", TargetLayout: SaveImportTargetArchiveDirectory,
			Replacement: SaveImportReplaceArchives,
		},
		DefaultEnv: map[string]string{
			"FACTORIO_SERVER_NAME": "Plexus Factorio Server",
		},
		Ports: []GamePort{
			{Name: "game", Port: 34197, Protocol: "UDP"},
			{Name: "rcon", Port: 27015, Protocol: "TCP"},
		},
		Shutdown: factorio.Schema().Shutdown,
		ConfigLayer: ConfigLayer{
			Templates: []ConfigTemplate{
				{
					TargetPath: "/factorio/config/server-settings.json",
					Format:     "json",
					Mappings: map[string]string{
						"server-name":       "name",
						"max-players":       "max_players",
						"autosave-interval": "autosave_interval",
					},
				},
			},
		},
		Workload: WorkloadSpec{
			ContainerName: factorio.GameID,
			DataMountPath: "/factorio",
			Config: ConfigRuntime{
				VolumeName:      "factorio-config",
				SourceName:      "factorio-config-source",
				MountPath:       "/factorio/config",
				SourcePath:      "/plexus/config",
				FileName:        factorio.ConfigFileName,
				InitName:        "factorio-config-init",
				InitCopyCommand: "cp /plexus/config/server-settings.json /factorio/config/server-settings.json",
			},
			SecretEnvKeys: []string{"GAME_PASSWORD", "RCON_PASSWORD", "TOKEN", "USERNAME"},
			SupportsMods:  true,
			Supervisor:    true,
		},
	},

	zomboid.GameID: {
		ID:                      zomboid.GameID,
		ManagementSchemaVersion: zomboid.SchemaVersion,
		DisplayName:             "Project Zomboid",
		DefaultImage:            "docker.io/renegademaster/zomboid-dedicated-server:" + zomboid.SupportedImageTag,
		MinDiskGiB:              15,
		RecommendedDiskGiB:      60,
		RawDiskPaths:            []string{"/home/steam/Zomboid", "/home/steam/ZomboidDedicatedServer"},
		DefaultEnv: map[string]string{
			"ADMIN_USERNAME": zomboid.AdminUsername,
			"USE_STEAM":      "true",
		},
		Ports: []GamePort{
			{Name: "game", Port: 16261, Protocol: "UDP"},
			{Name: "direct", Port: 16262, Protocol: "UDP"},
		},
		Shutdown: zomboid.Schema().Shutdown,
		ConfigLayer: ConfigLayer{
			Templates: []ConfigTemplate{
				{
					TargetPath: "/home/steam/Zomboid/Server/" + zomboid.ConfigIdentity + ".ini",
					Format:     "ini",
					Mappings: map[string]string{
						"PublicName": "name",
						"MaxPlayers": "maxPlayers",
					},
				},
			},
		},
		Workload: WorkloadSpec{
			ContainerName: zomboid.GameID,
			DataMountPath: "/home/steam/Zomboid",
			Config: ConfigRuntime{
				SourceName:      "zomboid-config-source",
				SourcePath:      "/plexus/config",
				FileName:        zomboid.ConfigFileName,
				InitName:        "zomboid-config-init",
				InitCopyCommand: "mkdir -p /home/steam/Zomboid/Server && cp /plexus/config/server.ini /home/steam/Zomboid/Server/" + zomboid.ConfigIdentity + ".ini",
			},
			SecretEnvKeys:   []string{"ADMIN_PASSWORD", "RCON_PASSWORD", "SERVER_PASSWORD"},
			WorkshopStartup: true,
			AdditionalMounts: []VolumeMount{
				{Name: "game-data", MountPath: "/home/steam/ZomboidDedicatedServer", SubPath: "install"},
			},
		},
	},
}

// Get returns the definition for a gameID. The controller should treat
// unknown games as an error (or have a very minimal fallback).
func Get(gameID string) (GameDefinition, error) {
	def, ok := Registry[gameID]
	if !ok {
		return GameDefinition{}, fmt.Errorf("unknown gameID %q - no definition registered in controller", gameID)
	}
	return def, nil
}

// CalculateDiskSize is an example of controller-owned logic that runs at
// GameServer creation/reconciliation time. It combines user/compute plan
// requests with the game's own minimums.
func CalculateDiskSize(gameID string, requestedGiB int, computePlanStorageGiB int) int {
	def, err := Get(gameID)
	if err != nil {
		// Very defensive - fall back to something safe
		return max(requestedGiB, computePlanStorageGiB, 4)
	}

	base := max(requestedGiB, computePlanStorageGiB, def.MinDiskGiB)
	return max(base, def.RecommendedDiskGiB, base) // ensure we meet the game's recommended size
}

func max(a, b, c int) int {
	if a > b {
		if a > c {
			return a
		}
		return c
	}
	if b > c {
		return b
	}
	return c
}
