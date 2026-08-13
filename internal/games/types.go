package games

import (
	"fmt"

	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
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

	// DefaultEnv are environment variables the controller will set unless
	// the user overrides them.
	DefaultEnv map[string]string

	// Ports the game listens on (used for Service creation).
	Ports []GamePort
}

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
		DefaultImage:            "factoriotools/factorio:stable",
		MinDiskGiB:              10,
		RecommendedDiskGiB:      50,
		RawDiskPaths:            []string{"/saves", "/mods"},
		DefaultEnv: map[string]string{
			"FACTORIO_SERVER_NAME": "Plexus Factorio Server",
		},
		Ports: []GamePort{
			{Name: "game", Port: 34197, Protocol: "UDP"},
			{Name: "rcon", Port: 27015, Protocol: "TCP"},
		},
		ConfigLayer: ConfigLayer{
			Templates: []ConfigTemplate{
				{
					TargetPath: "/config/server-settings.json",
					Format:     "json",
					Mappings: map[string]string{
						"server-name":       "name",
						"max-players":       "max_players",
						"autosave-interval": "autosave_interval",
					},
				},
			},
		},
	},

	"project-zomboid": {
		ID:                 "project-zomboid",
		DisplayName:        "Project Zomboid",
		DefaultImage:       "docker.io/renegademaster/zomboid-dedicated-server:latest",
		MinDiskGiB:         15,
		RecommendedDiskGiB: 60,
		RawDiskPaths:       []string{"/home/steam/Zomboid"},
		// ... more fields as we implement
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
