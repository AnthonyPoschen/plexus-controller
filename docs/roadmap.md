# Controller Roadmap

This document tracks the implementation plan for the Plexus GameServer controller. It is the runtime-focused companion to the backend repository's `docs/roadmap.md`.

For the overall product direction and why we chose the CRD + separate controller model, see the backend's `docs/game-deployment.md`.

## Product Direction (Controller View)

- Reconcile `GameServer` CRs authored by the backend into real Kubernetes workloads.
- Support a clean split: ConfigMaps for most settings (restart-required), editor pods for raw disk when the game server is stopped.
- Make game-specific behavior (images, paths, probes, backup hooks, editor pod requirements) evolvable without touching the customer-facing backend.
- Provide accurate status so the backend dashboard can show customers real runtime state.

## Core Responsibilities

- Watch `GameServer` resources.
- Render game-specific Kubernetes objects using `GameRuntimeProfile` data.
- Manage ConfigMaps for the config layer.
- Provision and tear down short-lived editor pods for raw disk file management.
- Handle safe archive ingest/export (object storage ↔ PVC) only while the server is stopped.
- Maintain rich runtime status (phase, active setup, observed generations,
  endpoint, players, conditions, and observation time).

## Implementation Phases

### Phase 0: Foundation & Scaffolding (Current)

- Set up Go module with controller-runtime + client-go.
- Basic `cmd/manager` with leader election and health endpoints.
- Initial `plexus.gg/v1alpha1` `GameServer` CRD types (spec + status).
- Minimal kustomize manifests (CRD, RBAC, sample).
- Dockerfile + basic Makefile.
- CI (lint + build).
- Initial `docs/` with architecture, roadmap, and strong cross-links back to the backend repo.
- Prod overlay ships a manager Deployment. CI publishes
  `ghcr.io/anthonyposchen/plexus-controller` with
  `master-<sha>-<timestamp>` tags for Flux ImagePolicy.

Exit criteria:
- Controller can be deployed to a cluster (even if it does nothing useful yet).
- Agents and humans can easily discover the relationship to the backend repository.

### Phase 1: Basic Reconciliation (MVP Game)

The first Factorio tracer now reconciles a Deployment, LoadBalancer Service,
and persistent volume; reports Starting, Running, Stopping, Stopped, and Failed
observations with periodic freshness updates; and removes the workload while
retaining the Service and storage on stop. Factorio configuration
rendering is integrated. The published runtime is the Plexus Factorio
supervisor image from this repository: the supervisor is PID 1, boots from
disk, recovers unexpected process exits in-pod, and runs the adapter graceful
stop on SIGTERM. Desired power Stopped still removes the pod. Explicit Force
stop bypasses pod grace, and replacement readiness is
fenced to the new Deployment revision.

The first live runtime-output slice is also released for Factorio: the shared
game-management profile declares one read-only current-container stdout
channel, and controller-owned server, owner, game, active-setup, and primary
component labels let the backend bind that stream to the current workload and
verify its Pod → ReplicaSet → Deployment ownership chain. The backend exposes
only a bounded current-run tail plus live follow; retained history, stdin, pod
identity, arbitrary container selection, and general Kubernetes log access are
not part of this release.

- Implement a `GameServerReconciler` that can create a Deployment + Service + PVC for one game (start with Factorio or Project Zomboid).
- Use a simple embedded or ConfigMap-sourced `GameRuntimeProfile`.
- Basic status updates (phase, observedGeneration).
- Proper owner references / finalizers.
- RBAC that allows the controller to manage the necessary resources in game server namespaces.

Exit criteria:
- Creating a `GameServer` CR results in a running (or pending) game pod with persistent storage.

### Phase 2: Config Layer (ConfigMaps)

The controller-owned `factorio/v1` game-management contract provides the typed
configuration/secret boundary and serialized schema consumed by both
repositories. The first management release marks Factorio configuration,
native mod discovery, save import/export, interactive RCON, and live container
logs as released; the Factorio file log channel remains unreleased. Factorio
reconciliation now validates that contract, renders its
settings ConfigMap and Secret-backed environment on Start, rejects incompatible
schema revisions with migration-required status, and acknowledges the active
configuration generation and Secret revision only after rollout availability.
Derived ConfigMaps and runtime Secrets are immutable and revision-scoped so an
old pod template cannot restart against replacement inputs. Customer mutation
work remains in this phase and must use that contract rather than introduce a
second Factorio schema.

- Define the first `GameRuntimeProfile` entries with explicit config file mappings.
- [x] Controller renders ConfigMaps (and mounts them into the game pod).
- [x] Support backend-driven versioned structured configuration and referenced
  setup-scoped Secrets from the shared game-adapter contract.
- Document and enforce the "confirm stopped → save configuration → next Start
  applies it" flow.

Exit criteria:
- Common server settings can be changed via the backend without hand-editing files inside the pod.

### Phase 3: Editor Pod Lifecycle (Managed Disk Operations)

The Factorio provider-ID tracer installs bounded, backend-validated immutable
artifacts with one managed disk Job while the Server is stopped, or while Start
waits for that Job. Observed versions are reported after the Job exits. The
supervisor pod is not scheduled until installed mods match and the Job has
released the PVC. Stop does not cancel the Job.
Native Mod Portal discovery is released for Factorio. It intentionally does not
provide arbitrary uploads, customer filesystem access, general editor sessions,
or large-artifact transfer; those remain in this and Phase 4.

- Add support for on-demand editor pod creation when the main game server is stopped.
- Editor pod mounts the same PVC(s) as the game server.
- Lightweight process inside the editor pod for narrowly scoped save, backup,
  mod, and allowlisted advanced-config operations.
- Backend can request a managed operation/session (via CR status/subresource or
  narrow API); customers do not receive a general filesystem browser.
- Controller cleans up editor pods on session end or timeout.
- Editor-session state is reported through a dedicated condition or future
  supporting resource without replacing the observed runtime phase.

Exit criteria:
- A stopped server can have an editor pod spun up on demand.
- Backend can complete an authorized managed operation against the PVC through
  the editor pod without exposing arbitrary paths to the browser.

### Phase 4: Archive Ingest / Export

Factorio export is delivered as the first tracer: `SaveExport` drives a
short-lived, adapter-path-scoped Job and expiring object-storage upload while
both backend and controller enforce fresh stopped/setup identity. Factorio
import is the matching destructive tracer: `SaveImport` downloads a validated
archive, snapshots the current hosted save, replaces only adapter-declared
save data, restores or retains that snapshot if apply fails, and leaves the
Server stopped. Other game adapters remain in this phase.

- Object storage handoff support (backend provides pre-signed URLs).
- Controller-driven jobs or steps inside the editor pod context that safely copy archives between object storage and the PVC.
- Gating behind "server must be stopped" state.
- Progress/status reporting back to the CR.

Exit criteria:
- Customers can import/export supported save archives, and approved managed
  workflows can transfer other adapter-declared assets through the backend →
  object storage → controller path while the server is stopped.

### Phase 5: Multiple Games + Rich Profiles

Project Zomboid is the second released runtime adapter. It shares the generic
GameServer contract for workload, configuration, secrets, graceful shutdown,
endpoint, and status. Configuration, native Steam Workshop mods, and live
container logs are released; saves and console stay adapter-gated. Workshop
updates apply during the next startup through SteamCMD and never restart a
GameServer automatically.

- Expand `GameRuntimeProfile` coverage for all supported games.
- Game-specific probes, graceful shutdown, backup hooks, mod loading behavior.
- Per-game editor pod requirements (extra tools, security context, etc.).
- Versioning strategy for profiles.

Exit criteria:
- Adding or updating a game is primarily a profile + controller change, not a backend change.

### Phase 6: Operations & Hardening

- Quotas, resource limits derived from compute plan.
- Resource requests derived from the base compute plan and High performance entitlement.
- Node selection / affinity based on configured region and location.
- Better finalizers and cleanup (even on controller crashes).
- Rich conditions and events.
- Metrics for the controller itself.
- Integration with backend `server_actions` audit (via the backend API).

Exit criteria:
- The system is safe and observable enough for production game servers.

## Architecture Boundaries (Summary)

**Backend (never does this):**
- Create Deployments, Pods, PVCs, ConfigMaps for game servers
- Exec into pods or mount PVCs directly
- Contain game-specific runtime logic

**Controller (never does this):**
- Talk to Stripe, Clerk, or billing tables
- Authorize end users
- Own the product catalog or pricing

The `GameServer` CR (plus narrow supporting mechanisms for editor sessions) is the contract.

## Cross-Repo Linkage

When making changes here, also consider impact on (and required updates in) the
`github.com/AnthonyPoschen/plexus` backend repository, using its matching issue
worktree for coordinated changes.

See the backend's `docs/game-deployment.md`, `docs/roadmap.md`, and the disk proxy implementation plan for full context.

## References

- Backend `docs/game-deployment.md` (source design doc)
- Backend `docs/server-management-ux.md` (resolved customer model, lifecycle,
  state matrix, API direction, and proposed CR handoff)
- Backend disk proxy / editor pod plan (detailed file management design)
- `GameRuntimeProfile` sketch in backend docs
