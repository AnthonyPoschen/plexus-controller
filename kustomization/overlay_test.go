package kustomization

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"

	rbacv1 "k8s.io/api/rbac/v1"
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
	roleDoc := documentKindNamed(output, "Role", "plexus-controller")
	if roleDoc == "" {
		t.Fatal("Dev overlay must emit the namespace-scoped Role")
	}
	var role rbacv1.Role
	if err := yaml.Unmarshal([]byte(roleDoc), &role); err != nil {
		t.Fatal(err)
	}
	if !roleAllows(role, "apps", "replicasets", "get", "list", "watch") {
		t.Fatalf("Dev Role missing ReplicaSet get/list/watch:\n%s", roleDoc)
	}
	if documentKindNamed(output, "Deployment", "plexus-controller") != "" {
		t.Fatal("Dev CRD overlay must not emit the manager Deployment")
	}
}

func TestProdOverlayIncludesManagerDeployment(t *testing.T) {
	output := kustomizeBuild(t, "overlays/prod")
	deployDoc := documentKindNamed(output, "Deployment", "plexus-controller")
	if deployDoc == "" {
		t.Fatal("prod overlay must emit a plexus-controller Deployment")
	}
	var deploy struct {
		Metadata struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Spec struct {
			Template struct {
				Spec struct {
					ServiceAccountName string `json:"serviceAccountName"`
					Containers         []struct {
						Name  string `json:"name"`
						Image string `json:"image"`
						Env   []struct {
							Name  string `json:"name"`
							Value string `json:"value"`
						} `json:"env"`
					} `json:"containers"`
				} `json:"spec"`
			} `json:"template"`
		} `json:"spec"`
	}
	if err := yaml.Unmarshal([]byte(deployDoc), &deploy); err != nil {
		t.Fatal(err)
	}
	if deploy.Metadata.Namespace != "app-plexus-controller" {
		t.Fatalf("Deployment namespace = %q", deploy.Metadata.Namespace)
	}
	if deploy.Spec.Template.Spec.ServiceAccountName != "plexus-controller" {
		t.Fatalf("serviceAccountName = %q", deploy.Spec.Template.Spec.ServiceAccountName)
	}
	if len(deploy.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("containers = %#v", deploy.Spec.Template.Spec.Containers)
	}
	container := deploy.Spec.Template.Spec.Containers[0]
	if !regexp.MustCompile(`^ghcr\.io/anthonyposchen/plexus-controller:master-[a-f0-9]{7}-[0-9]{14}$`).MatchString(container.Image) {
		t.Fatalf("manager image = %q", container.Image)
	}
	if !strings.Contains(output, "name: docker-secret") {
		t.Fatal("prod overlay must pull GHCR with docker-secret")
	}
	env := map[string]string{}
	for _, item := range container.Env {
		env[item.Name] = item.Value
	}
	if env["PLEXUS_API_GROUP"] != "plexus.gg" || env["PLEXUS_RUNTIME_NAMESPACE"] != "app-plexus" {
		t.Fatalf("manager env = %#v", env)
	}
}

func TestProdOverlayKeepsControllerRoleInRuntimeNamespace(t *testing.T) {
	output := kustomizeBuild(t, "overlays/prod")
	roleDoc := documentKindNamed(output, "Role", "plexus-controller")
	if roleDoc == "" {
		t.Fatal("prod overlay must still emit the namespace-scoped Role")
	}
	var role rbacv1.Role
	if err := yaml.Unmarshal([]byte(roleDoc), &role); err != nil {
		t.Fatal(err)
	}
	if role.Namespace != "app-plexus" {
		t.Fatalf("Role namespace = %q, want app-plexus", role.Namespace)
	}
	if !roleAllows(role, "apps", "replicasets", "get", "list", "watch") {
		t.Fatalf("prod Role missing ReplicaSet get/list/watch:\n%s", roleDoc)
	}

	bindingDoc := documentKindNamed(output, "RoleBinding", "plexus-controller")
	if bindingDoc == "" {
		t.Fatal("prod overlay must emit a RoleBinding for the manager")
	}
	var binding struct {
		Metadata struct {
			Namespace string `json:"namespace"`
		} `json:"metadata"`
		Subjects []struct {
			Kind      string `json:"kind"`
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		} `json:"subjects"`
	}
	if err := yaml.Unmarshal([]byte(bindingDoc), &binding); err != nil {
		t.Fatal(err)
	}
	if binding.Metadata.Namespace != "app-plexus" {
		t.Fatalf("RoleBinding namespace = %q, want app-plexus", binding.Metadata.Namespace)
	}
	if len(binding.Subjects) != 1 || binding.Subjects[0].Kind != "ServiceAccount" || binding.Subjects[0].Name != "plexus-controller" || binding.Subjects[0].Namespace != "app-plexus-controller" {
		t.Fatalf("RoleBinding subjects = %#v", binding.Subjects)
	}
	if documentKindNamed(output, "ClusterRoleBinding", "plexus-controller-crd-reader") == "" {
		t.Fatal("prod overlay must bind the CRD reader ClusterRole")
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

func documentKindNamed(output string, kind string, name string) string {
	for _, document := range strings.Split(output, "\n---\n") {
		if yamlKind(document) != kind {
			continue
		}
		var meta struct {
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal([]byte(document), &meta); err != nil {
			continue
		}
		if meta.Metadata.Name == name {
			return document
		}
	}
	return ""
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

func roleAllows(role rbacv1.Role, apiGroup, resource string, verbs ...string) bool {
	for _, rule := range role.Rules {
		if !containsString(rule.APIGroups, apiGroup) || !containsString(rule.Resources, resource) {
			continue
		}
		for _, verb := range verbs {
			if !containsString(rule.Verbs, verb) && !containsString(rule.Verbs, "*") {
				return false
			}
		}
		return true
	}
	return false
}
