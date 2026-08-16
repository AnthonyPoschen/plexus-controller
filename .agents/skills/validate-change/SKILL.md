---
name: validate-change
description: >
  Validate a proposed plexus-controller change before calling it good. Use
  when reviewing a diff, finishing a feature, or checking whether Flux can
  apply CRDs and other cluster-scoped Kubernetes kinds.
---

# Validate a controller change

Run this before treating a controller or CRD change as done.

## Cluster-scoped kinds

The GameServer CRD is cluster-scoped. The prod tenant Kustomization
(`app-plexus-controller`) uses a ServiceAccount. A **RoleBinding** to
`cluster-admin` is not enough to patch CRDs or ClusterRoles. The k8s
sandbox must bind that SA with a **ClusterRoleBinding**
(`repos/plexus-controller.yaml`). Dev CRDs are applied by the
cluster-level `app-plexus-dev-crd` Kustomization (no tenant SA).

If the overlay gains a `ClusterRole` / webhook / CRD, Flux will
`Forbidden` until the sandbox has cluster scope.

## Checks

```sh
go test ./...
```

`internal/manifest/cluster_kinds_test.go` walks `kustomization/` and
fails if a new cluster-scoped kind appears without being listed.

## Slot path

Website slot deploy rewrites this CRD onto `gameservers.<slot>.plexus.gg`.
Keep the CRD cluster-scoped. Do not add namespaced-only assumptions.
