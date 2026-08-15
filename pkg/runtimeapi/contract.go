package runtimeapi

import (
	"fmt"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation"
)

const (
	// DefaultGroup is the production GameServer API group stored on master.
	DefaultGroup = "plexus.gg"
	// Version is the compiled GameServer API version. Deploy-time overlays
	// may rewrite the group, never this version string.
	Version = "v1alpha1"
	// GameServerPlural is the CRD plural used to name gameservers.<group>.
	GameServerPlural = "gameservers"
	// DefaultNamespace is the production runtime namespace.
	DefaultNamespace = "app-plexus"
	// EnvAPIGroup is the process environment that selects the runtime API group.
	EnvAPIGroup = "PLEXUS_API_GROUP"
	// EnvNamespace is the process environment that selects the single runtime namespace.
	EnvNamespace = "PLEXUS_RUNTIME_NAMESPACE"
)

// Contract is the runtime API group and namespace both the website and
// controller pin for one tenant. Dev, slots, and production all use this.
type Contract struct {
	Group     string
	Version   string
	Namespace string
}

// CRDDocument is the subset of a CustomResourceDefinition required to prove
// the named GameServer CRD exists and serves the compiled version.
type CRDDocument struct {
	Metadata CRDMetadata `json:"metadata"`
	Spec     CRDSpec     `json:"spec"`
}

type CRDMetadata struct {
	Name string `json:"name"`
}

type CRDSpec struct {
	Group    string       `json:"group"`
	Versions []CRDVersion `json:"versions"`
}

type CRDVersion struct {
	Name   string `json:"name"`
	Served bool   `json:"served"`
}

// Load reads PLEXUS_API_GROUP and PLEXUS_RUNTIME_NAMESPACE, defaulting to the
// production group and namespace when unset.
func Load() (Contract, error) {
	group, err := LoadAPIGroup()
	if err != nil {
		return Contract{}, err
	}
	namespace, err := LoadNamespace()
	if err != nil {
		return Contract{}, err
	}
	return Contract{Group: group, Version: Version, Namespace: namespace}, nil
}

// LoadAPIGroup returns PLEXUS_API_GROUP or the production default.
func LoadAPIGroup() (string, error) {
	return NormalizeAPIGroup(os.Getenv(EnvAPIGroup))
}

// LoadNamespace returns PLEXUS_RUNTIME_NAMESPACE or the production default.
func LoadNamespace() (string, error) {
	return NormalizeNamespace(os.Getenv(EnvNamespace))
}

// NormalizeAPIGroup accepts an empty value as the production default.
func NormalizeAPIGroup(group string) (string, error) {
	group = strings.TrimSpace(group)
	if group == "" {
		return DefaultGroup, nil
	}
	if messages := validation.IsDNS1123Subdomain(group); len(messages) != 0 {
		return "", fmt.Errorf("%s %q is not a valid API group: %s", EnvAPIGroup, group, strings.Join(messages, "; "))
	}
	return group, nil
}

// NormalizeNamespace accepts an empty value as the production default.
func NormalizeNamespace(namespace string) (string, error) {
	namespace = strings.TrimSpace(namespace)
	if namespace == "" {
		return DefaultNamespace, nil
	}
	if messages := validation.IsDNS1123Label(namespace); len(messages) != 0 {
		return "", fmt.Errorf("%s %q is not a valid namespace: %s", EnvNamespace, namespace, strings.Join(messages, "; "))
	}
	return namespace, nil
}

func (contract Contract) GroupVersion() schema.GroupVersion {
	return schema.GroupVersion{Group: contract.Group, Version: contract.Version}
}

func (contract Contract) APIVersion() string {
	return contract.GroupVersion().String()
}

func (contract Contract) GameServerCRDName() string {
	return GameServerCRDName(contract.Group)
}

func (contract Contract) NamespacedAPIPath(resource string) string {
	return "/apis/" + contract.Group + "/" + contract.Version + "/namespaces/" + contract.Namespace + "/" + resource
}

func (contract Contract) GameServerCRDPath() string {
	return "/apis/apiextensions.k8s.io/v1/customresourcedefinitions/" + contract.GameServerCRDName()
}

// GameServerCRDName is the cluster-scoped CRD object name for a group.
func GameServerCRDName(group string) string {
	if group == "" {
		group = DefaultGroup
	}
	return GameServerPlural + "." + group
}

// CheckGameServerCRD fails unless the named CRD exists for this group and
// serves the compiled API version.
func (contract Contract) CheckGameServerCRD(document CRDDocument) error {
	name := contract.GameServerCRDName()
	if document.Metadata.Name != "" && document.Metadata.Name != name {
		return fmt.Errorf("GameServer CRD name is %q, want %q", document.Metadata.Name, name)
	}
	if document.Spec.Group != contract.Group {
		return fmt.Errorf("GameServer CRD %s has group %q, want %q", name, document.Spec.Group, contract.Group)
	}
	for _, version := range document.Spec.Versions {
		if version.Name == contract.Version && version.Served {
			return nil
		}
	}
	return fmt.Errorf("GameServer CRD %s does not serve compiled version %s", name, contract.Version)
}
