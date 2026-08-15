package gamemanagement

import (
	"encoding/json"
	"testing"

	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
	zomboid "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/projectzomboid/v1"
)

func TestSchemaRegistryIncludesReleasedAdapters(t *testing.T) {
	factorioSchema, ok := Schema(factorio.GameID)
	if !ok || factorioSchema.Version != factorio.SchemaVersion {
		t.Fatalf("Factorio schema = %#v ok=%t", factorioSchema, ok)
	}
	zomboidSchema, ok := Schema(zomboid.GameID)
	if !ok || zomboidSchema.Version != zomboid.SchemaVersion {
		t.Fatalf("Project Zomboid schema = %#v ok=%t", zomboidSchema, ok)
	}
	if zomboidSchema.Saves.ExportReleased || zomboidSchema.Saves.ImportReleased {
		t.Fatalf("Project Zomboid advertised unreleased save surfaces: %#v", zomboidSchema.Saves)
	}
	if !zomboidSchema.Mods.Released || zomboidSchema.Mods.ProviderID != zomboid.ModProviderID || zomboidSchema.Mods.AutomaticRestart {
		t.Fatalf("Project Zomboid Workshop contract = %#v", zomboidSchema.Mods)
	}
}

func TestConfigurationEnvCarriesFactorioChannel(t *testing.T) {
	env, err := ConfigurationEnv(factorio.GameID, []byte(`{"channel":"experimental","name":"Copper Works"}`))
	if err != nil {
		t.Fatal(err)
	}
	if env[factorio.ChannelEnv] != factorio.ChannelExperimental {
		t.Fatalf("Factorio channel env = %#v", env)
	}
	defaults, err := ConfigurationEnv(factorio.GameID, []byte(`{"name":"Copper Works"}`))
	if err != nil || defaults[factorio.ChannelEnv] != factorio.ChannelStable {
		t.Fatalf("default Factorio channel env = %#v err=%v", defaults, err)
	}
}

func TestNormalizeConfigurationIsGameSpecificAndGeneric(t *testing.T) {
	factorioValues, err := NormalizeConfiguration(factorio.GameID, []byte(`{"name":"Copper Works"}`))
	if err != nil {
		t.Fatal(err)
	}
	var factorioConfig factorio.Configuration
	if err := json.Unmarshal(factorioValues, &factorioConfig); err != nil || factorioConfig.Name != "Copper Works" {
		t.Fatalf("Factorio normalized = %s err=%v", factorioValues, err)
	}

	zomboidValues, err := NormalizeConfiguration(zomboid.GameID, []byte(`{"name":"Knox County","maxPlayers":12}`))
	if err != nil {
		t.Fatal(err)
	}
	var zomboidConfig zomboid.Configuration
	if err := json.Unmarshal(zomboidValues, &zomboidConfig); err != nil || zomboidConfig.Name != "Knox County" || zomboidConfig.MaxPlayers != 12 {
		t.Fatalf("Project Zomboid normalized = %s err=%v", zomboidValues, err)
	}
}

func TestApplySecretPatchGeneratesRequiredCredentials(t *testing.T) {
	factorioSecrets, err := ApplySecretPatch(factorio.GameID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	decodedFactorio, err := factorio.DecodeSecrets(factorioSecrets)
	if err != nil || decodedFactorio.RCONPassword == "" {
		t.Fatalf("Factorio generated secrets = %s err=%v", factorioSecrets, err)
	}

	zomboidSecrets, err := ApplySecretPatch(zomboid.GameID, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	decodedZomboid, err := zomboid.DecodeSecrets(zomboidSecrets)
	if err != nil || decodedZomboid.AdminPassword == "" || decodedZomboid.RCONPassword == "" {
		t.Fatalf("Project Zomboid generated secrets = %s err=%v", zomboidSecrets, err)
	}
	admin := "adminpass1"
	patched, err := ApplySecretPatch(zomboid.GameID, zomboidSecrets, map[string]*string{"adminPassword": &admin})
	if err != nil {
		t.Fatal(err)
	}
	decodedPatched, err := zomboid.DecodeSecrets(patched)
	if err != nil || decodedPatched.AdminPassword != admin || decodedPatched.RCONPassword != decodedZomboid.RCONPassword {
		t.Fatalf("Project Zomboid patched secrets = %#v", decodedPatched)
	}
}
