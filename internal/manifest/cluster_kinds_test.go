package manifest

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOverlayListsGameServerCRDAsClusterScoped(t *testing.T) {
	root := findRepoRoot(t)
	found := map[string][]string{}
	err := filepath.WalkDir(filepath.Join(root, "kustomization"), func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.HasSuffix(path, ".yaml") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			rel = path
		}
		for _, document := range strings.Split(string(raw), "\n---\n") {
			kind := yamlKind(document)
			if !IsClusterScopedKind(kind) {
				continue
			}
			found[kind] = append(found[kind], rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := found["CustomResourceDefinition"]; !ok {
		t.Fatal("controller overlay must still ship the GameServer CRD; tenant Flux SA needs cluster scope in k8s repos/plexus-controller.yaml")
	}
}

func yamlKind(document string) string {
	for _, line := range strings.Split(document, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "kind:") {
			return strings.TrimSpace(strings.TrimPrefix(trimmed, "kind:"))
		}
	}
	return ""
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found")
		}
		dir = parent
	}
}
