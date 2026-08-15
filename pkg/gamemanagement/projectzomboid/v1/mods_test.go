package v1

import (
	"strings"
	"testing"
)

func TestWorkshopUpdatePolicyAppliesOnNextStartNeverAutomatically(t *testing.T) {
	policy := Schema().Mods
	if policy.ProviderID != ModProviderID || policy.ApplyPolicy != ModApplyPolicyNextStart || policy.AutomaticRestart || policy.ClientSynchronization != ModClientSyncRestart || !policy.Released || !policy.NativeDiscovery || !policy.DirectReference {
		t.Fatalf("Project Zomboid Workshop policy drifted: %#v", policy)
	}
	if ModUpdateCustomerMessage(true) != "Restart to apply" || ModUpdateCustomerMessage(false) != "Start to apply" {
		t.Fatal("Project Zomboid update messaging must stay policy-derived and restart-free")
	}
	if !strings.Contains(ModUpdateCustomerDetail(), "mismatch may require Restart") || !strings.Contains(ModUpdateCustomerDetail(), "will not restart") {
		t.Fatalf("Project Zomboid update detail omitted client mismatch guidance: %q", ModUpdateCustomerDetail())
	}
	if Schema().Mods.AutomaticRestart {
		t.Fatal("Project Zomboid Workshop apply must never restart automatically")
	}
}

func TestValidateModReleaseAcceptsStandaloneWorkshopItem(t *testing.T) {
	if err := ValidateModRelease(testWorkshopRelease()); err != nil {
		t.Fatal(err)
	}
}

func TestValidateModReleaseRejectsForeignAppRemovedAndInvalidDependencies(t *testing.T) {
	for name, mutate := range map[string]func(*ModRelease){
		"provider":      func(release *ModRelease) { release.ProviderID = "factorio-mod-portal" },
		"workshop id":   func(release *ModRelease) { release.ProviderModID = "not-a-workshop-id" },
		"app":           func(release *ModRelease) { release.GameVersion = "4000" },
		"empty load":    func(release *ModRelease) { release.Name = "A Workshop Item"; release.LoadIDs = nil },
		"dependency":    func(release *ModRelease) { release.Dependencies = []string{"bad"} },
		"duplicate":     func(release *ModRelease) { release.Dependencies = []string{release.ProviderModID} },
		"empty version": func(release *ModRelease) { release.Version = "" },
	} {
		t.Run(name, func(t *testing.T) {
			release := testWorkshopRelease()
			mutate(&release)
			if err := ValidateModRelease(release); err == nil {
				t.Fatal("expected invalid Workshop release to be rejected")
			}
		})
	}
}

func TestRuntimeWorkshopEnvClearsAndJoinsStartupSelection(t *testing.T) {
	empty := RuntimeWorkshopEnv(nil)
	if empty[WorkshopItemsEnv] != "" || empty[WorkshopModNamesEnv] != "" {
		t.Fatalf("empty selection should clear startup env: %#v", empty)
	}
	release := testWorkshopRelease()
	release.Dependencies = []string{"2685168362"}
	release.LoadIDs = []string{"Brita_2", "tsarslib"}
	env := RuntimeWorkshopEnv(&release)
	if env[WorkshopItemsEnv] != "2160432461;2685168362" || env[WorkshopModNamesEnv] != "Brita_2;tsarslib" {
		t.Fatalf("Workshop startup env = %#v", env)
	}
}

func testWorkshopRelease() ModRelease {
	return ModRelease{
		ProviderID:    ModProviderID,
		ProviderModID: "2160432461",
		Name:          "Brita_2",
		Version:       "2026-08-01T12:00:00Z",
		GameVersion:   WorkshopAppIDString,
		LoadIDs:       []string{"Brita_2"},
	}
}
