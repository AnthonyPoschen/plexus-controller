package kustomization

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"sigs.k8s.io/yaml"
)

func TestGeneratedGameServerCRDRemainsProduction(t *testing.T) {
	matches, err := filepath.Glob(filepath.Join("base", "crds", "*"))
	if err != nil {
		t.Fatal(err)
	}
	for _, match := range matches {
		base := filepath.Base(match)
		if strings.Contains(base, "dev.plexus.gg") {
			t.Fatalf("Dev CRD must be an overlay rewrite, found committed file %s", base)
		}
	}

	crd := loadCRD(t, filepath.Join("base", "crds", "plexus.gg_gameservers.yaml"))
	if crd.Name != "gameservers.plexus.gg" {
		t.Fatalf("committed CRD name = %q", crd.Name)
	}
	if crd.Spec.Group != "plexus.gg" {
		t.Fatalf("committed CRD group = %q", crd.Spec.Group)
	}
	if !containsString(crd.Spec.Names.ShortNames, "gs") {
		t.Fatalf("committed CRD shortNames = %v, want gs", crd.Spec.Names.ShortNames)
	}
}

func TestProdOverlayLeavesGeneratedGameServerCRDUntouched(t *testing.T) {
	output := kustomizeBuild(t, "overlays/prod")
	crd := crdNamed(t, output, "gameservers.plexus.gg")
	if crd.Spec.Group != "plexus.gg" {
		t.Fatalf("prod overlay CRD group = %q", crd.Spec.Group)
	}
	if !containsString(crd.Spec.Names.ShortNames, "gs") {
		t.Fatalf("prod overlay shortNames = %v, want gs", crd.Spec.Names.ShortNames)
	}
	if strings.Contains(output, "gameservers.dev.plexus.gg") {
		t.Fatal("prod overlay must not emit the Dev GameServer CRD")
	}
}

func TestDevOverlayRewritesGeneratedGameServerCRD(t *testing.T) {
	output := kustomizeBuild(t, "overlays/dev")
	if strings.Contains(output, "name: gameservers.plexus.gg") {
		t.Fatal("Dev overlay must not emit the production GameServer CRD name")
	}

	crd := crdNamed(t, output, "gameservers.dev.plexus.gg")
	if crd.Spec.Group != "dev.plexus.gg" {
		t.Fatalf("Dev overlay CRD group = %q", crd.Spec.Group)
	}
	if containsString(crd.Spec.Names.ShortNames, "gs") {
		t.Fatalf("Dev overlay claimed shared shortName gs: %v", crd.Spec.Names.ShortNames)
	}
	if len(crd.Spec.Names.ShortNames) != 0 {
		t.Fatalf("Dev overlay shortNames = %v, want none", crd.Spec.Names.ShortNames)
	}

	for _, name := range []string{"saveexports.dev.plexus.gg", "saveimports.dev.plexus.gg"} {
		other := crdNamed(t, output, name)
		if other.Spec.Group != "dev.plexus.gg" {
			t.Fatalf("%s group = %q", name, other.Spec.Group)
		}
	}
}

func TestDevOverlayRewritesControllerRBAC(t *testing.T) {
	output := kustomizeBuild(t, "overlays/dev")
	if !strings.Contains(output, "name: plexus-controller-crd-reader-dev") {
		t.Fatal("Dev overlay must rename the Dev CRD reader ClusterRole")
	}
	if strings.Contains(output, "name: plexus-controller-crd-reader\n") {
		t.Fatal("Dev overlay must not keep the production CRD reader name")
	}
	if strings.Contains(output, "apiGroups:\n  - plexus.gg\n") {
		t.Fatal("Dev Role still authorizes the production API group")
	}
}

func loadCRD(t *testing.T, rel string) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	manifest, err := os.ReadFile(rel)
	if err != nil {
		t.Fatal(err)
	}
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(manifest, &crd); err != nil {
		t.Fatal(err)
	}
	return crd
}

func crdNamed(t *testing.T, output string, name string) apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	for _, document := range strings.Split(output, "\n---\n") {
		if !strings.Contains(document, "kind: CustomResourceDefinition") {
			continue
		}
		var crd apiextensionsv1.CustomResourceDefinition
		if err := yaml.Unmarshal([]byte(document), &crd); err != nil {
			t.Fatal(err)
		}
		if crd.Name == name {
			return crd
		}
	}
	t.Fatalf("CRD %s not found in kustomize output", name)
	return apiextensionsv1.CustomResourceDefinition{}
}

func kustomizeBuild(t *testing.T, path string) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	cmd := exec.Command("kubectl", "kustomize", path)
	cmd.Dir = filepath.Dir(thisFile)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl kustomize %s: %v\n%s", path, err, output)
	}
	return string(output)
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}
