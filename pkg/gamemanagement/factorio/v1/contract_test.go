package v1_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
)

func TestManagementSchemaSerializationMatchesContract(t *testing.T) {
	got, err := json.MarshalIndent(factorio.Schema(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')

	want, err := os.ReadFile(filepath.Join("testdata", "management-schema.json"))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, want) == false {
		t.Fatalf("management schema drifted; review compatibility and update testdata/management-schema.json when intentional\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

func TestManagementSchemaExposesStableAndExperimentalChannelsFirst(t *testing.T) {
	schema := factorio.Schema()
	if len(schema.Configuration.Sections) == 0 {
		t.Fatal("expected Factorio configuration sections")
	}
	section := schema.Configuration.Sections[0]
	if section.ID != "channel" || len(section.Fields) != 1 || section.Fields[0].Path != "channel" {
		t.Fatalf("channel control must be first: %#v", section)
	}
	field := section.Fields[0]
	if field.Default != factorio.ChannelStable || field.Type != "string" || !field.Required {
		t.Fatalf("channel field = %#v", field)
	}
	if len(field.Options) != 2 || field.Options[0].Value != factorio.ChannelStable || field.Options[1].Value != factorio.ChannelExperimental {
		t.Fatalf("channel options = %#v", field.Options)
	}
}

func TestDecodeConfigurationDefaultsToStableChannelAndRejectsPinnedPatches(t *testing.T) {
	configuration, err := factorio.DecodeConfiguration([]byte(`{"name":"Copper Works"}`))
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Channel != factorio.ChannelStable {
		t.Fatalf("default channel = %q", configuration.Channel)
	}
	env, err := factorio.ConfigurationEnv(configuration)
	if err != nil || env[factorio.ChannelEnv] != factorio.ChannelStable {
		t.Fatalf("stable channel env = %#v err=%v", env, err)
	}

	experimental, err := factorio.DecodeConfiguration([]byte(`{"channel":"experimental","name":"Copper Works"}`))
	if err != nil {
		t.Fatal(err)
	}
	env, err = factorio.ConfigurationEnv(experimental)
	if err != nil || env[factorio.ChannelEnv] != factorio.ChannelExperimental {
		t.Fatalf("experimental channel env = %#v err=%v", env, err)
	}

	for _, raw := range []string{
		`{"channel":"2.0.77","name":"Copper Works"}`,
		`{"channel":"latest","name":"Copper Works"}`,
		`{"channel":"","name":"Copper Works"}`,
	} {
		if _, err := factorio.DecodeConfiguration([]byte(raw)); err == nil {
			t.Fatalf("expected pinned or unknown channel to be rejected: %s", raw)
		}
	}
}

func TestDecodeConfigurationRejectsUnknownFields(t *testing.T) {
	_, err := factorio.DecodeConfiguration([]byte(`{"name":"Copper Works","futureSetting":true}`))
	if err == nil {
		t.Fatal("expected an unknown configuration field to be rejected")
	}
}

func TestDecodeConfigurationValidatesValues(t *testing.T) {
	_, err := factorio.DecodeConfiguration([]byte(`{"name":"Copper Works","maxPlayers":70000}`))
	if err == nil {
		t.Fatal("expected maxPlayers outside the contract range to be rejected")
	}
}

func TestValidateSaveArchiveAcceptsFactorioSave(t *testing.T) {
	err := factorio.ValidateSaveArchive("copper-works.zip", "application/zip", 1024, []factorio.ArchiveEntry{
		{Name: "copper-works/", Directory: true},
		{Name: "copper-works/level.dat", UncompressedBytes: 2048},
		{Name: "copper-works/level-init.dat", UncompressedBytes: 128},
		{Name: "copper-works/script.dat", UncompressedBytes: 256},
	})
	if err != nil {
		t.Fatalf("expected a compatible Factorio save, got %v", err)
	}
}

func TestValidateSaveArchiveAcceptsRequiredEntriesAtArchiveRootForCompatibility(t *testing.T) {
	err := factorio.ValidateSaveArchive("copper-works.zip", "application/zip", 1024, []factorio.ArchiveEntry{
		{Name: "level.dat", UncompressedBytes: 2048},
		{Name: "level-init.dat", UncompressedBytes: 128},
	})
	if err != nil {
		t.Fatalf("expected archive-root required entries to remain compatible, got %v", err)
	}
}

func TestValidateSaveArchiveRejectsInvalidLayoutOrExpansion(t *testing.T) {
	tests := map[string][]factorio.ArchiveEntry{
		"parent traversal": {
			{Name: "../level.dat", UncompressedBytes: 2048},
			{Name: "../level-init.dat", UncompressedBytes: 128},
		},
		"embedded traversal": {
			{Name: "copper-works/../level.dat", UncompressedBytes: 2048},
			{Name: "copper-works/../level-init.dat", UncompressedBytes: 128},
		},
		"absolute path": {
			{Name: "/level.dat", UncompressedBytes: 2048},
			{Name: "/level-init.dat", UncompressedBytes: 128},
		},
		"Windows drive absolute path": {
			{Name: "C:/level.dat", UncompressedBytes: 2048},
			{Name: "C:/level-init.dat", UncompressedBytes: 128},
		},
		"backslash": {
			{Name: `copper-works\level.dat`, UncompressedBytes: 2048},
			{Name: "copper-works/level-init.dat", UncompressedBytes: 128},
		},
		"split required entries": {
			{Name: "copper-works/level.dat", UncompressedBytes: 2048},
			{Name: "iron-works/level-init.dat", UncompressedBytes: 128},
		},
		"nested save root": {
			{Name: "exports/copper-works/level.dat", UncompressedBytes: 2048},
			{Name: "exports/copper-works/level-init.dat", UncompressedBytes: 128},
		},
		"duplicate required entry": {
			{Name: "copper-works/level.dat", UncompressedBytes: 2048},
			{Name: "copper-works/level.dat", UncompressedBytes: 2048},
			{Name: "copper-works/level-init.dat", UncompressedBytes: 128},
		},
		"unrelated archive root": {
			{Name: "copper-works/level.dat", UncompressedBytes: 2048},
			{Name: "copper-works/level-init.dat", UncompressedBytes: 128},
			{Name: "iron-works/notes.txt", UncompressedBytes: 64},
		},
		"negative expansion": {
			{Name: "copper-works/level.dat", UncompressedBytes: -1},
			{Name: "copper-works/level-init.dat", UncompressedBytes: 128},
		},
		"oversized expansion": {
			{Name: "copper-works/level.dat", UncompressedBytes: factorio.MaximumSaveExpandedBytes},
			{Name: "copper-works/level-init.dat", UncompressedBytes: 1},
		},
		"missing required entry": {
			{Name: "copper-works/level.dat", UncompressedBytes: 2048},
		},
	}

	for name, entries := range tests {
		t.Run(name, func(t *testing.T) {
			err := factorio.ValidateSaveArchive("copper-works.zip", "application/zip", 1024, entries)
			if err == nil {
				t.Fatal("expected invalid Factorio save archive to be rejected")
			}
		})
	}
}

func TestDecodeSecretsRequiresPairedFactorioCredentials(t *testing.T) {
	_, err := factorio.DecodeSecrets([]byte(`{"username":"plexus","rconPassword":"generated"}`))
	if err == nil {
		t.Fatal("expected a username without a Factorio token to be rejected")
	}
}
