# AGENTS.md

This file captures repository conventions and guidance for the Plexus GameServer controller.

## Relationship to the Backend Repository

This controller is part of a two-repo system:

- Backend: `/home/zanven/.grok/worktrees/anthonyposchen-plexus/backend`
  - Owns product, billing, entitlements, catalog, user APIs, desired state, auth, and object storage coordination.

- Controller (this repo): Owns Kubernetes reconciliation, pod/volume/ConfigMap/editor pod lifecycle, and all game-specific runtime behavior.

**When working on anything related to provisioning, runtime, GameServer CRs, editor pods, ConfigMaps for games, or file management on disk, you must also consult the backend repository.**

Key backend documents to read:
- `docs/game-deployment.md` — core architecture and the original recommendation for the CRD + controller model.
- `docs/roadmap.md` — overall product phases.
- The current implementation plan for disk proxy / editor pods (in the backend's session docs).
- `internal/catalog/catalog.go` — source of game profiles that evolve into `GameRuntimeProfile`.

## Key Design Rules (Controller Side)

- The `GameServer` CR is the desired state handoff. The backend writes it; we reconcile it.
- Config layer (most settings) → ConfigMaps. Changes are restart-required in most cases.
- Raw disk layer (mods, world saves, arbitrary files) → only while the main game server is stopped, via short-lived editor pods that mount the same PVCs.
- Never mutate raw disk while the game pod is running.
- Editor pods are temporary and must be reliably cleaned up.
- Game-specific logic lives here (images, paths, probes, backup hooks, editor pod requirements). Keep it out of the backend.
- Status in the CR is how we communicate runtime reality back to the backend dashboard.

## Development Conventions

- Follow standard controller-runtime patterns.
- Keep the controller focused. Do not add product/billing logic.
- When adding support for a new game or new runtime feature, update (or help update) the corresponding `GameRuntimeProfile` definition and the backend catalog where appropriate.
- Prefer clear separation: one game profile change should not require backend API changes.

## Documentation

Keep `docs/architecture.md` and `docs/roadmap.md` up to date. They are the primary way other agents and humans understand the split between the two repos.

## Testing

- Unit tests for reconcilers and profile rendering.
- Integration tests against kind/minikube are highly valuable (create a GameServer, observe pods/ConfigMaps, exercise editor pod creation, etc.).

## Cross-Repo Changes

Any change that affects the `GameServer` spec/status contract, editor pod behavior, or the `GameRuntimeProfile` shape should be coordinated with the backend team/repo. Update linkage docs in both places.
