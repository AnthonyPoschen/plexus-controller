package manifest

// ClusterScopedKinds are Kubernetes kinds a tenant Flux ServiceAccount
// cannot apply with only a namespaced RoleBinding.
var ClusterScopedKinds = map[string]struct{}{
	"APIService":                       {},
	"ClusterRole":                      {},
	"ClusterRoleBinding":               {},
	"CustomResourceDefinition":         {},
	"MutatingWebhookConfiguration":     {},
	"Namespace":                        {},
	"PersistentVolume":                 {},
	"PriorityClass":                    {},
	"StorageClass":                     {},
	"ValidatingAdmissionPolicy":        {},
	"ValidatingAdmissionPolicyBinding": {},
	"ValidatingWebhookConfiguration":   {},
}

func IsClusterScopedKind(kind string) bool {
	_, ok := ClusterScopedKinds[kind]
	return ok
}
