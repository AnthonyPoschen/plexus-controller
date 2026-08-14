package v1alpha1

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"testing"

	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

func TestGameServerDeepCopyCopiesStructuredConfiguration(t *testing.T) {
	original := &GameServer{
		Spec: GameServerSpec{
			DesiredPower: DesiredPowerRunning,
			SelectedSetup: &SelectedSetupSpec{
				ID:     "setup-1",
				GameID: "factorio",
				Configuration: GameConfiguration{
					SchemaVersion: "factorio/v1",
					Values:        runtime.RawExtension{Raw: []byte(`{"maxPlayers":20,"autosave":{"intervalMinutes":10}}`)},
					SecretRef:     SetupSecretReference{Name: "setup-1-secrets-v2"},
				},
			},
		},
	}

	copy := original.DeepCopy()
	copy.Spec.SelectedSetup.Configuration.Values.Raw[0] = '['
	copy.Spec.SelectedSetup.Configuration.SecretRef.Name = "replacement"

	if string(original.Spec.SelectedSetup.Configuration.Values.Raw) != `{"maxPlayers":20,"autosave":{"intervalMinutes":10}}` {
		t.Fatalf("deep copy mutated original structured configuration: %s", original.Spec.SelectedSetup.Configuration.Values.Raw)
	}
	if original.Spec.SelectedSetup.Configuration.SecretRef.Name != "setup-1-secrets-v2" {
		t.Fatalf("deep copy mutated original Secret reference: %#v", original.Spec.SelectedSetup.Configuration.SecretRef)
	}
}

func TestUnloadedGameServerHasStoppedDesiredPower(t *testing.T) {
	spec := GameServerSpec{
		ServerID:     "server-1",
		OwnerUserID:  "user-1",
		DesiredPower: DesiredPowerStopped,
		ShutdownMode: ShutdownModeGraceful,
	}

	encoded, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}

	var document map[string]any
	if err := json.Unmarshal(encoded, &document); err != nil {
		t.Fatal(err)
	}
	if document["desiredPower"] != string(DesiredPowerStopped) {
		t.Fatalf("unloaded GameServer desired power = %v, want Stopped", document["desiredPower"])
	}
	if _, present := document["selectedSetup"]; present {
		t.Fatalf("unloaded GameServer unexpectedly selected a setup: %s", encoded)
	}
}

func TestGeneratedCRDMatchesGameServerContract(t *testing.T) {
	manifest, err := os.ReadFile("../../kustomization/base/crds/plexus.gg_gameservers.yaml")
	if err != nil {
		t.Fatal(err)
	}

	var crd apiextensionsv1.CustomResourceDefinition
	if err := yaml.Unmarshal(manifest, &crd); err != nil {
		t.Fatal(err)
	}
	if len(crd.Spec.Versions) != 1 || crd.Spec.Versions[0].Schema == nil {
		t.Fatalf("unexpected GameServer CRD versions: %#v", crd.Spec.Versions)
	}

	root := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	spec := root.Properties["spec"]
	wantSpecFields := []string{"computePlanID", "desiredPower", "highPerformance", "location", "ownerUserID", "region", "restartGeneration", "selectedSetup", "serverID", "shutdownMode"}
	if got := sortedPropertyNames(spec.Properties); !reflect.DeepEqual(got, wantSpecFields) {
		t.Fatalf("spec fields = %v, want %v", got, wantSpecFields)
	}
	if !contains(spec.Required, "desiredPower") || !contains(spec.Required, "shutdownMode") {
		t.Fatalf("desired power and shutdown mode must be required: %v", spec.Required)
	}
	if !hasValidation(spec.XValidations, "has(self.selectedSetup) || self.desiredPower == 'Stopped'") {
		t.Fatalf("CRD does not require unloaded Servers to remain stopped: %#v", spec.XValidations)
	}

	selectedSetup := spec.Properties["selectedSetup"]
	if got := sortedPropertyNames(selectedSetup.Properties); !reflect.DeepEqual(got, []string{"configuration", "gameID", "id"}) {
		t.Fatalf("selected setup fields = %v, want only identity and configuration", got)
	}
	configuration := selectedSetup.Properties["configuration"]
	if got := sortedPropertyNames(configuration.Properties); !reflect.DeepEqual(got, []string{"schemaVersion", "secretRef", "values"}) {
		t.Fatalf("configuration fields = %v, want versioned values and Secret reference", got)
	}
	values := configuration.Properties["values"]
	if values.XPreserveUnknownFields == nil || !*values.XPreserveUnknownFields {
		t.Fatalf("configuration.values is not a structured RawExtension: %#v", values)
	}
	secretRef := configuration.Properties["secretRef"]
	if got := sortedPropertyNames(secretRef.Properties); !reflect.DeepEqual(got, []string{"name"}) {
		t.Fatalf("Secret reference exposes fields other than its setup-scoped name: %v", got)
	}

	status := root.Properties["status"]
	wantStatusFields := []string{"activeSetupID", "conditions", "endpoint", "lastObservedAt", "message", "observedConfigurationGeneration", "observedGeneration", "observedRestartGeneration", "observedSecretRevision", "phase", "players"}
	if got := sortedPropertyNames(status.Properties); !reflect.DeepEqual(got, wantStatusFields) {
		t.Fatalf("status fields = %v, want %v", got, wantStatusFields)
	}
}

func sortedPropertyNames(properties map[string]apiextensionsv1.JSONSchemaProps) []string {
	names := make([]string, 0, len(properties))
	for name := range properties {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func hasValidation(validations apiextensionsv1.ValidationRules, rule string) bool {
	for _, validation := range validations {
		if validation.Rule == rule {
			return true
		}
	}
	return false
}
