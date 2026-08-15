package runtimeapi

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestLoadDefaultsToProductionGroupAndNamespace(t *testing.T) {
	t.Setenv(EnvAPIGroup, "")
	t.Setenv(EnvNamespace, "")

	contract, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if contract.Group != DefaultGroup || contract.Version != Version || contract.Namespace != DefaultNamespace {
		t.Fatalf("default contract = %#v", contract)
	}
	if contract.GameServerCRDName() != "gameservers.plexus.gg" {
		t.Fatalf("default CRD name = %q", contract.GameServerCRDName())
	}
}

func TestLoadAcceptsNonProductionGroupAndNamespace(t *testing.T) {
	t.Setenv(EnvAPIGroup, "slot.plexus.gg")
	t.Setenv(EnvNamespace, "slot-a")

	contract, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if contract.Group != "slot.plexus.gg" || contract.Namespace != "slot-a" || contract.Version != Version {
		t.Fatalf("slot contract = %#v", contract)
	}
	if contract.APIVersion() != "slot.plexus.gg/v1alpha1" {
		t.Fatalf("API version = %q", contract.APIVersion())
	}
	if contract.GameServerCRDName() != "gameservers.slot.plexus.gg" {
		t.Fatalf("slot CRD name = %q", contract.GameServerCRDName())
	}
	if contract.NamespacedAPIPath("gameservers") != "/apis/slot.plexus.gg/v1alpha1/namespaces/slot-a/gameservers" {
		t.Fatalf("namespaced path = %q", contract.NamespacedAPIPath("gameservers"))
	}
	if contract.GroupVersion() != (schema.GroupVersion{Group: "slot.plexus.gg", Version: Version}) {
		t.Fatalf("group version = %#v", contract.GroupVersion())
	}
}

func TestLoadRejectsInvalidGroupAndNamespace(t *testing.T) {
	t.Setenv(EnvAPIGroup, "SLOT.plexus.gg")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvAPIGroup) {
		t.Fatalf("invalid group error = %v", err)
	}
	t.Setenv(EnvAPIGroup, "slot.plexus.gg")
	t.Setenv(EnvNamespace, "Slot_A")
	if _, err := Load(); err == nil || !strings.Contains(err.Error(), EnvNamespace) {
		t.Fatalf("invalid namespace error = %v", err)
	}
}

func TestCheckGameServerCRDRequiresNamedCRDToServeCompiledVersion(t *testing.T) {
	contract := Contract{Group: "dev.plexus.gg", Version: Version, Namespace: "app-plexus-dev"}
	served := CRDDocument{
		Metadata: CRDMetadata{Name: "gameservers.dev.plexus.gg"},
		Spec:     CRDSpec{Group: "dev.plexus.gg", Versions: []CRDVersion{{Name: Version, Served: true}}},
	}
	if err := contract.CheckGameServerCRD(served); err != nil {
		t.Fatal(err)
	}

	unserved := served
	unserved.Spec.Versions[0].Served = false
	if err := contract.CheckGameServerCRD(unserved); err == nil || !strings.Contains(err.Error(), "does not serve compiled version") {
		t.Fatalf("unserved version error = %v", err)
	}

	wrongGroup := served
	wrongGroup.Spec.Group = DefaultGroup
	if err := contract.CheckGameServerCRD(wrongGroup); err == nil || !strings.Contains(err.Error(), "has group") {
		t.Fatalf("wrong group error = %v", err)
	}

	wrongName := served
	wrongName.Metadata.Name = "gameservers.plexus.gg"
	if err := contract.CheckGameServerCRD(wrongName); err == nil || !strings.Contains(err.Error(), "name is") {
		t.Fatalf("wrong CRD name error = %v", err)
	}
}
