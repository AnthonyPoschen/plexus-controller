# plexus-controller

The Plexus GameServer controller.

This repository contains the Kubernetes operator that reconciles `GameServer` custom resources (created by the Plexus backend) into actual Kubernetes workloads: Deployments/StatefulSets, Services, PersistentVolumeClaims, ConfigMaps, Secrets, and short-lived "editor" pods for safe file management.

## Purpose

Plexus sells compute entitlements (via Stripe subscriptions). Each purchased server maps to a `GameServer` CR. This controller is responsible for turning that desired state into running game servers on the cluster, including:

- Game-specific pod and volume configuration
- ConfigMap-driven configuration (most settings)
- Raw disk access via temporary editor pods (only when the main game server is stopped)
- Safe ingest/export of large archives (worlds, mod packs) from object storage
- Status reporting so the backend can show accurate runtime state to customers

Game-specific behavior and safety rules live here, not in the customer-facing backend API.

## Relationship to the Backend Repository

This controller is the runtime half of a two-repo architecture:

- **Backend** (`github.com/AnthonyPoschen/plexus`): Product, billing, entitlements, catalog, user-facing APIs, authentication, desired state authoring, object storage coordination, and audit.
- **Controller** (this repo): Kubernetes reconciliation, pod/volume lifecycle, editor pods, ConfigMap rendering, and all game-specific runtime details.

When working on provisioning, runtime behavior, file management, or the `GameServer` CRD, **consult both repositories**.

See the backend's [docs/game-deployment.md](https://github.com/AnthonyPoschen/plexus/blob/master/backend/docs/game-deployment.md) for the original design rationale and the "Related Repositories" section in the backend's AGENTS.md.

## Key Design Principles

- The `servers` table in the backend SQLite database is the durable product source of truth.
- The `GameServer` CR is the desired-state handoff to Kubernetes.
- Most configuration is delivered via ConfigMaps (editable while the server runs; restart required to apply).
- Raw disk mutation (mods, world saves, arbitrary files on the PVC) requires the main game server to be stopped. A temporary editor pod that mounts the same PVC is provisioned for interactive browser-based file management.
- Large archive transfers use short-lived pre-signed object storage URLs; the controller performs the safe copy only in a stopped state.
- The backend never talks directly to pods or PVCs. It goes through the controller (CRs + editor sessions).

## Getting Started (Development)

```bash
# TODO: add make targets once scaffolding is complete
make manifests
make install
make run
```

## Links

- Backend repository (source of truth for catalog, billing, entitlements, and user APIs)
- Backend `docs/game-deployment.md` — core architecture and runtime interface guidance
- Backend plan for disk proxy / editor pods (internal session doc)

## License

TBD
