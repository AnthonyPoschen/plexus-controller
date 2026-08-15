package v1_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	zomboid "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/projectzomboid/v1"
)

func TestManagementSchemaSerializationMatchesContract(t *testing.T) {
	got, err := json.MarshalIndent(zomboid.Schema(), "", "  ")
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

func TestDecodeConfigurationRejectsUnknownFields(t *testing.T) {
	_, err := zomboid.DecodeConfiguration([]byte(`{"name":"Knox County","futureSetting":true}`))
	if err == nil {
		t.Fatal("expected an unknown configuration field to be rejected")
	}
}

func TestDecodeConfigurationValidatesValues(t *testing.T) {
	_, err := zomboid.DecodeConfiguration([]byte(`{"name":"Knox County","maxPlayers":200}`))
	if err == nil {
		t.Fatal("expected maxPlayers outside the contract range to be rejected")
	}
}

func TestDecodeSecretsRequiresAdminAndRCONPasswords(t *testing.T) {
	_, err := zomboid.DecodeSecrets([]byte(`{"adminPassword":"adminpass"}`))
	if err == nil {
		t.Fatal("expected a missing RCON password to be rejected")
	}
}

func TestRenderConfigFilesAndSecretEnvStaySeparated(t *testing.T) {
	configuration, err := zomboid.DecodeConfiguration([]byte(`{"name":"Knox County","description":"A quiet town","maxPlayers":12,"public":true,"pvp":false,"pauseEmpty":true}`))
	if err != nil {
		t.Fatal(err)
	}
	files, err := zomboid.RenderConfigFiles(configuration)
	if err != nil {
		t.Fatal(err)
	}
	ini := files[zomboid.ConfigFileName]
	if !strings.Contains(ini, "PublicName=Knox County") || !strings.Contains(ini, "MaxPlayers=12") || !strings.Contains(ini, "PVP=false") {
		t.Fatalf("rendered INI = %q", ini)
	}
	if strings.Contains(ini, "Password=") || strings.Contains(ini, "admin") {
		t.Fatalf("rendered INI leaked secrets: %q", ini)
	}

	secrets, err := zomboid.DecodeSecrets([]byte(`{"adminPassword":"adminpass1","serverPassword":"joinpass1","rconPassword":"generatedrconpassword"}`))
	if err != nil {
		t.Fatal(err)
	}
	env, err := zomboid.RuntimeSecretEnv(secrets)
	if err != nil {
		t.Fatal(err)
	}
	if string(env["ADMIN_PASSWORD"]) != "adminpass1" || string(env["SERVER_PASSWORD"]) != "joinpass1" || string(env["RCON_PASSWORD"]) != "generatedrconpassword" {
		t.Fatalf("runtime secret env = %#v", env)
	}
}
