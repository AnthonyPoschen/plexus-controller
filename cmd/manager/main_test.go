package main

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/yaml"

	plexusv1alpha1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
	"github.com/AnthonyPoschen/plexus-controller/pkg/runtimeapi"
)

func TestRuntimeManagerOptionsAreNamespaceScoped(t *testing.T) {
	t.Setenv(runtimeapi.EnvAPIGroup, "slot.plexus.gg")
	t.Setenv(runtimeapi.EnvNamespace, "slot-a")
	contract, err := runtimeapi.Load()
	if err != nil {
		t.Fatal(err)
	}

	options := runtimeManagerOptions(contract, ":8080", ":8081", true)
	if len(options.Cache.DefaultNamespaces) != 1 {
		t.Fatalf("cache namespaces = %#v", options.Cache.DefaultNamespaces)
	}
	if _, ok := options.Cache.DefaultNamespaces["slot-a"]; !ok {
		t.Fatalf("cache is not scoped to the runtime namespace: %#v", options.Cache.DefaultNamespaces)
	}
	if options.LeaderElectionNamespace != "slot-a" {
		t.Fatalf("leader election namespace = %q", options.LeaderElectionNamespace)
	}
}

func TestAddGroupToSchemeRegistersNonProductionGroup(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := plexusv1alpha1.AddGroupToScheme("slot.plexus.gg")(scheme); err != nil {
		t.Fatal(err)
	}
	gvks, _, err := scheme.ObjectKinds(&plexusv1alpha1.GameServer{})
	if err != nil {
		t.Fatal(err)
	}
	if len(gvks) != 1 || gvks[0].Group != "slot.plexus.gg" || gvks[0].Version != runtimeapi.Version || gvks[0].Kind != "GameServer" {
		t.Fatalf("registered GameServer GVK = %#v", gvks)
	}
}

func TestGameServerCRDReadyzUsesNamedNonProductionCRD(t *testing.T) {
	contract := runtimeapi.Contract{Group: "slot.plexus.gg", Version: runtimeapi.Version, Namespace: "slot-a"}
	scheme := runtime.NewScheme()
	if err := apiextensionsv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	crd := loadRewrittenGameServerCRD(t, contract.Group)
	reader := fake.NewClientBuilder().WithScheme(scheme).WithObjects(crd).Build()

	check := gameServerCRDReadyz(reader, contract)
	if err := check(&http.Request{}); err != nil {
		t.Fatal(err)
	}

	unserved := crd.DeepCopy()
	unserved.Spec.Versions[0].Served = false
	if err := reader.Update(context.Background(), unserved); err != nil {
		t.Fatal(err)
	}
	if err := check(&http.Request{}); err == nil || !strings.Contains(err.Error(), "does not serve compiled version") {
		t.Fatalf("unserved CRD ready error = %v", err)
	}

	missing := gameServerCRDReadyz(fake.NewClientBuilder().WithScheme(scheme).Build(), contract)
	if err := missing(&http.Request{}); err == nil || !strings.Contains(err.Error(), "gameservers.slot.plexus.gg") {
		t.Fatalf("missing CRD ready error = %v", err)
	}
}

func TestControllerRBACIsNamespaceScoped(t *testing.T) {
	manifest, err := os.ReadFile(filepath.Join("..", "..", "kustomization", "base", "role.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifest), "\nkind: Role\n") || strings.Contains(string(manifest), "kind: ClusterRole") {
		t.Fatalf("controller RBAC is not namespace-scoped:\n%s", manifest)
	}
}

func TestManagerDockerfileBuildsControllerBinary(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "Dockerfile"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "go build -mod=vendor") || !strings.Contains(body, "./cmd/manager") {
		t.Fatal("Dockerfile must build the vendored manager binary")
	}
	if !strings.Contains(body, `ENTRYPOINT ["/manager"]`) {
		t.Fatal("Dockerfile must run the manager as PID 1")
	}
}

func loadRewrittenGameServerCRD(t *testing.T, group string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()
	manifest, err := os.ReadFile(filepath.Join("..", "..", "kustomization", "base", "crds", "plexus.gg_gameservers.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	manifest = []byte(strings.ReplaceAll(string(manifest), "gameservers.plexus.gg", runtimeapi.GameServerCRDName(group)))
	manifest = []byte(strings.ReplaceAll(string(manifest), "\n  group: plexus.gg\n", "\n  group: "+group+"\n"))
	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(manifest, &crd); err != nil {
		t.Fatal(err)
	}
	crd.ResourceVersion = "1"
	if crd.Name != runtimeapi.GameServerCRDName(group) || crd.Spec.Group != group {
		t.Fatalf("rewritten CRD = %s group=%s", crd.Name, crd.Spec.Group)
	}
	if len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Name != runtimeapi.Version || !crd.Spec.Versions[0].Served {
		t.Fatalf("rewritten CRD versions = %#v", crd.Spec.Versions)
	}
	return &crd
}

func TestNonProductionGroupCreateUsesCompiledVersion(t *testing.T) {
	contract := runtimeapi.Contract{Group: "slot.plexus.gg", Version: runtimeapi.Version, Namespace: "slot-a"}
	scheme := runtime.NewScheme()
	if err := plexusv1alpha1.AddGroupToScheme(contract.Group)(scheme); err != nil {
		t.Fatal(err)
	}
	gameServer := &plexusv1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{Name: "server-1", Namespace: contract.Namespace},
		Spec: plexusv1alpha1.GameServerSpec{
			ServerID:     "server-1",
			OwnerUserID:  "owner-1",
			DesiredPower: plexusv1alpha1.DesiredPowerStopped,
			ShutdownMode: plexusv1alpha1.ShutdownModeGraceful,
		},
	}
	gameServer.SetGroupVersionKind(contract.GroupVersion().WithKind("GameServer"))
	if gameServer.APIVersion != "slot.plexus.gg/v1alpha1" {
		t.Fatalf("GameServer APIVersion = %q", gameServer.APIVersion)
	}
	gvks, _, err := scheme.ObjectKinds(gameServer)
	if err != nil || len(gvks) != 1 || gvks[0].Group != contract.Group {
		t.Fatalf("in-process GameServer GVK = %#v err=%v", gvks, err)
	}
}
