// Package v1alpha1 contains API Schema definitions for the plexus.gg v1alpha1 API group
// +kubebuilder:object:generate=true
// +groupName=plexus.gg
package v1alpha1

//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen object:headerFile=../../hack/boilerplate.go.txt paths=./...

//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen crd:crdVersions=v1 paths=./... output:crd:artifacts:config=../../kustomization/base/crds

import (
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/scheme"

	"github.com/AnthonyPoschen/plexus-controller/pkg/runtimeapi"
)

var (
	// GroupVersion is the generated production group version. Runtime
	// processes register the configured group with AddGroupToScheme.
	GroupVersion = schema.GroupVersion{Group: runtimeapi.DefaultGroup, Version: runtimeapi.Version}

	// SchemeBuilder is used to add go types to the GroupVersionKind scheme
	SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}

	// AddToScheme adds the types in this group-version to the given scheme.
	AddToScheme = SchemeBuilder.AddToScheme
)

// AddGroupToScheme registers GameServer types under the supplied API group so
// Dev, slots, and production can share the compiled version on different CRDs.
func AddGroupToScheme(group string) func(*runtime.Scheme) error {
	return func(target *runtime.Scheme) error {
		normalized, err := runtimeapi.NormalizeAPIGroup(group)
		if err != nil {
			return err
		}
		if normalized == runtimeapi.DefaultGroup {
			return AddToScheme(target)
		}
		builder := &scheme.Builder{GroupVersion: schema.GroupVersion{Group: normalized, Version: runtimeapi.Version}}
		builder.Register(
			&GameServer{}, &GameServerList{},
			&SaveExport{}, &SaveExportList{},
			&SaveImport{}, &SaveImportList{},
		)
		return builder.AddToScheme(target)
	}
}
