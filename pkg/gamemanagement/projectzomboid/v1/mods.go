package v1

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/model"
)

const (
	ModProviderID           = "steam-workshop"
	ModProviderName         = "Steam Workshop"
	ModProviderURL          = "https://steamcommunity.com/app/108600/workshop/"
	WorkshopAppID           = 108600
	WorkshopAppIDString     = "108600"
	ModApplyPolicyNextStart = "next-start"
	ModClientSyncRestart    = "restart-required"
	MaximumWorkshopItems    = 24
	WorkshopItemsEnv        = "MOD_WORKSHOP_IDS"
	WorkshopModNamesEnv     = "MOD_NAMES"
)

var (
	workshopIDPattern    = regexp.MustCompile(`^[1-9][0-9]{0,19}$`)
	workshopModIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_().+-]{0,79}$`)
)

type ModRelease struct {
	ProviderID    string
	ProviderModID string
	Name          string
	Version       string
	GameVersion   string
	Dependencies  []string
	LoadIDs       []string
}

// ModUpdateCustomerMessage is the Project Zomboid adapter policy for a
// detected Workshop update. SteamCMD applies the revision on the next Start
// or customer Restart and never restarts a Server automatically.
func ModUpdateCustomerMessage(runtimeRunning bool) string {
	if runtimeRunning {
		return "Restart to apply"
	}
	return "Start to apply"
}

// ModUpdateCustomerDetail explains that a client/server Workshop mismatch may
// require Restart and that Plexus will not restart automatically.
func ModUpdateCustomerDetail() string {
	return "Project Zomboid clients must match the server's Workshop mods. A mismatch may require Restart. Plexus applies Workshop updates during the next startup and will not restart this Server automatically."
}

func ModProviderPolicy() model.ModProviderPolicy {
	return model.ModProviderPolicy{
		ProviderID:            ModProviderID,
		ProviderName:          ModProviderName,
		ProviderURL:           ModProviderURL,
		Released:              true,
		NativeDiscovery:       true,
		DirectReference:       true,
		DependencyResolution:  "workshop-children",
		VersionSelection:      "latest",
		Compatibility:         "workshop-app",
		ApplyPolicy:           ModApplyPolicyNextStart,
		RequiresStopped:       true,
		AutomaticRestart:      false,
		ClientSynchronization: ModClientSyncRestart,
	}
}

// ValidateModRelease accepts one Steam Workshop item or collection for the
// hosted Project Zomboid app. Required Workshop children are declared as
// dependencies and must themselves be valid Workshop IDs.
func ValidateModRelease(release ModRelease) error {
	if release.ProviderID != ModProviderID {
		return fmt.Errorf("mod provider identity is invalid")
	}
	if !ValidWorkshopID(release.ProviderModID) {
		return fmt.Errorf("Workshop ID %q is invalid", release.ProviderModID)
	}
	if strings.TrimSpace(release.Name) == "" {
		return fmt.Errorf("Workshop item name is required")
	}
	if strings.TrimSpace(release.Version) == "" {
		return fmt.Errorf("Workshop item version is required")
	}
	if release.GameVersion != "" && release.GameVersion != WorkshopAppIDString {
		return fmt.Errorf("Workshop item targets app %q; hosted Project Zomboid uses app %s", release.GameVersion, WorkshopAppIDString)
	}
	if len(release.Dependencies) > MaximumWorkshopItems-1 {
		return fmt.Errorf("Workshop selection exceeds %d required items", MaximumWorkshopItems)
	}
	seen := map[string]struct{}{release.ProviderModID: {}}
	for _, dependency := range release.Dependencies {
		if !ValidWorkshopID(dependency) {
			return fmt.Errorf("Workshop dependency %q is invalid", dependency)
		}
		if _, exists := seen[dependency]; exists {
			return fmt.Errorf("Workshop dependency %q is duplicated", dependency)
		}
		seen[dependency] = struct{}{}
	}
	loadIDs := release.LoadIDs
	if len(loadIDs) == 0 && workshopModIDPattern.MatchString(release.Name) {
		loadIDs = []string{release.Name}
	}
	if len(loadIDs) == 0 {
		return fmt.Errorf("Workshop item does not declare a Project Zomboid Mod ID")
	}
	if len(loadIDs) > MaximumWorkshopItems {
		return fmt.Errorf("Workshop selection exceeds %d loadable mods", MaximumWorkshopItems)
	}
	seenLoad := map[string]struct{}{}
	for _, loadID := range loadIDs {
		if !workshopModIDPattern.MatchString(loadID) {
			return fmt.Errorf("Project Zomboid Mod ID %q is invalid", loadID)
		}
		if _, exists := seenLoad[loadID]; exists {
			return fmt.Errorf("Project Zomboid Mod ID %q is duplicated", loadID)
		}
		seenLoad[loadID] = struct{}{}
	}
	return nil
}

func ValidWorkshopID(value string) bool {
	return workshopIDPattern.MatchString(strings.TrimSpace(value))
}

func ValidWorkshopModID(value string) bool {
	return workshopModIDPattern.MatchString(strings.TrimSpace(value))
}

// RuntimeWorkshopEnv maps one enabled Workshop selection onto the dedicated
// server image variables consumed at startup. SteamCMD downloads and updates
// those items before the game process starts. An empty selection clears both
// variables so a later start cannot keep a removed item.
func RuntimeWorkshopEnv(release *ModRelease) map[string]string {
	items, loadIDs := WorkshopSelection(release)
	return map[string]string{
		WorkshopItemsEnv:    strings.Join(items, ";"),
		WorkshopModNamesEnv: strings.Join(loadIDs, ";"),
	}
}

// WorkshopSelection returns the Steam Workshop IDs and Project Zomboid load
// IDs that the next startup should install.
func WorkshopSelection(release *ModRelease) ([]string, []string) {
	if release == nil || strings.TrimSpace(release.ProviderModID) == "" {
		return nil, nil
	}
	items := []string{strings.TrimSpace(release.ProviderModID)}
	for _, dependency := range release.Dependencies {
		dependency = strings.TrimSpace(dependency)
		if dependency == "" || dependency == items[0] {
			continue
		}
		items = append(items, dependency)
	}
	loadIDs := append([]string(nil), release.LoadIDs...)
	if len(loadIDs) == 0 && workshopModIDPattern.MatchString(strings.TrimSpace(release.Name)) {
		loadIDs = []string{strings.TrimSpace(release.Name)}
	}
	return items, loadIDs
}
