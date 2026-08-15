package games

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
)

func TestFactorioUsesPlexusSupervisorImage(t *testing.T) {
	definition, err := Get(factorio.GameID)
	if err != nil {
		t.Fatal(err)
	}
	if definition.DefaultImage != factorio.RuntimeImage {
		t.Fatalf("Factorio image = %q, want %q", definition.DefaultImage, factorio.RuntimeImage)
	}
	if strings.Contains(definition.DefaultImage, "factoriotools") {
		t.Fatalf("Factorio GameDefinition still points at factoriotools: %q", definition.DefaultImage)
	}
	if !strings.HasPrefix(definition.DefaultImage, "ghcr.io/anthonyposchen/plexus-factorio:") {
		t.Fatalf("Factorio image is not the Plexus supervisor image: %q", definition.DefaultImage)
	}
	if !strings.HasSuffix(definition.DefaultImage, ":"+factorio.SupportedRuntimeVersion) {
		t.Fatalf("Factorio image is not pinned to %s: %q", factorio.SupportedRuntimeVersion, definition.DefaultImage)
	}
	if !definition.Workload.Supervisor {
		t.Fatal("Factorio workload must run the Plexus supervisor as PID 1")
	}
}

func TestFactorioDockerfilePinsSupportedRuntime(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile.factorio"))
	if err != nil {
		t.Fatal(err)
	}
	want := "FACTORIO_VERSION=" + factorio.SupportedRuntimeVersion
	if !strings.Contains(string(data), want) {
		t.Fatalf("Dockerfile.factorio must pin %s", want)
	}
	if !strings.Contains(string(data), `ENTRYPOINT ["/usr/local/bin/game-supervisor"]`) {
		t.Fatal("Dockerfile.factorio must run the game supervisor as PID 1")
	}
}
