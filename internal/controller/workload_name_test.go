package controller

import (
	"strings"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	plexusv1alpha1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
)

func TestWorkloadDeploymentNameShape(t *testing.T) {
	gameServer := testGameServerForGame(plexusv1alpha1.DesiredPowerRunning, "factorio", "factorio/v1")
	gameServer.Spec.CustomerSlug = "zanven42@gmail.com"
	gameServer.Spec.DisplayName = "Test"
	name, err := workloadDeploymentName(gameServer)
	if err != nil {
		t.Fatal(err)
	}
	if name != "zanven42-test-factorio" {
		t.Fatalf("workload name = %q", name)
	}
	if strings.Contains(name, gameServer.Spec.ServerID) || strings.Contains(name, gameServer.Name) {
		t.Fatalf("workload name must not include a server-id hash: %q", name)
	}
}

func TestWorkloadDeploymentNameFallsBackWithoutEmail(t *testing.T) {
	gameServer := testGameServerForGame(plexusv1alpha1.DesiredPowerRunning, "factorio", "factorio/v1")
	gameServer.Spec.OwnerUserID = "e47bf2af-0a08-46fe-a262-8088a7bd5d6e"
	gameServer.Spec.CustomerSlug = ""
	gameServer.Spec.DisplayName = ""
	name, err := workloadDeploymentName(gameServer)
	if err != nil {
		t.Fatal(err)
	}
	if name != "ue47bf2af-server-factorio" {
		t.Fatalf("fallback name = %q, want ue47bf2af-server-factorio", name)
	}
}

func TestWorkloadDeploymentNameSlugsAndCapsTokens(t *testing.T) {
	gameServer := testGameServerForGame(plexusv1alpha1.DesiredPowerRunning, "factorio", "factorio/v1")
	gameServer.Spec.CustomerSlug = "Zanven_42"
	gameServer.Spec.DisplayName = "My Super Amazing Server Name"
	name, err := workloadDeploymentName(gameServer)
	if err != nil {
		t.Fatal(err)
	}
	if name != "zanven-42-my-super-amazing-factorio" {
		t.Fatalf("slugged name = %q", name)
	}
}

func TestWorkloadDeploymentNameRejectsMissingGame(t *testing.T) {
	gameServer := &plexusv1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{Name: "server-1", Namespace: "games"},
		Spec:       plexusv1alpha1.GameServerSpec{ServerID: "server-1", OwnerUserID: "user-1"},
	}
	if _, err := workloadDeploymentName(gameServer); err == nil {
		t.Fatal("expected error without selected setup")
	}
}
