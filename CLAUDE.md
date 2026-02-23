# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

IBM VPC File Pool CSI Driver (`ibm-vpc-file-pool-csi`) — a Kubernetes CSI driver that pools multiple PVCs as subdirectories within shared VPC file shares, instead of the traditional 1:1 PVC-to-share mapping. Think of it like VMware: one NFS datastore holds many VMDKs.

**Current state:** The Go implementation is built and deployed on ROKS clusters. The repository contains specification documents, working code, and operational documentation.

## Reference Documents

Read the relevant doc before implementing any component:

### Design & Implementation

| Document | Covers |
|----------|--------|
| `SKILL.md` | Task-specific build guidance — read this first for any implementation task |
| `ARCHITECTURE.md` | System design, component diagram, data flows |
| `CRD-SPEC.md` | All CRD definitions (FileSharePool, SubVolume, Snapshot, VolumeGroupSnapshot, ReplicationPolicy) |
| `CSI-INTERFACE.md` | CSI gRPC method implementations |
| `IBM-VPC-API.md` | VPC API client wrapper design |
| `CODING-GUIDELINES.md` | Go conventions, error handling, concurrency, metrics |
| `TESTING.md` | Testing strategy, fakes/mocks, coverage targets |

### Features & Data Protection

| Document | Covers |
|----------|--------|
| `FEATURES.md` | Complete feature catalog with maturity badges and comparison table |
| `VOLUME-CLONING.md` | Sync/async clone design (Phase 4b) |
| `VOLUME-GROUP-SNAPSHOTS.md` | Coordinated multi-PVC snapshots (Phase 4c) |
| `CROSS-REGION-DR.md` | Replication architecture and consistency analysis (Phase 4d) |
| `CONSISTENCY-MODEL.md` | NFS consistency guarantees and limitations |
| `GOLDEN-IMAGE-WORKFLOW.md` | KubeVirt golden image syncer modes and setup |

### Operations & Configuration

| Document | Covers |
|----------|--------|
| `INSTALL.md` | Build, deploy, Helm chart |
| `UPGRADE-GUIDE.md` | Version upgrade procedures and compatibility matrix |
| `HELM-VALUES.md` | Complete Helm chart values reference |
| `API-KEY-SETUP.md` | Authentication via secret-common-lib |
| `MONITORING.md` | All 21 Prometheus metrics, alerting rules, Grafana dashboard |
| `DAY2-OPERATIONS.md` | Production runbook: health checks, draining, failover |
| `CAPACITY-PLANNING.md` | Sizing guidance, cost estimation, example configs |
| `BACKUP-RECOVERY.md` | Snapshot scheduling, DR setup, recovery runbooks |
| `TROUBLESHOOTING.md` | Comprehensive troubleshooting guide |
| `PERFORMANCE-TUNING.md` | NFS tuning, IOPS planning, benchmarking |

### Platform & Security

| Document | Covers |
|----------|--------|
| `VPC-NETWORKING.md` | VPC network configuration for NFS |
| `VM-DISK-FORMATS.md` | KubeVirt disk image format requirements |
| `SECURITY.md` | Security policy and hardening guide |
| `KNOWN-LIMITATIONS.md` | Platform constraints and workarounds |

## Skills (Slash Commands)

The project has custom Claude Code skills in `.claude/skills/`:

| Skill | Invocation | Purpose |
|-------|-----------|---------|
| `/test` | Auto or manual | Run Go tests with race detector, scoped by package/test name |
| `/pool-status` | Auto or manual | Run health checks on driver pods, pools, shares, replication |
| `/diagnose` | Auto or manual | Collect full diagnostics bundle to a timestamped file |
| `/deploy` | Auto or manual | Build image, push, apply CRDs, helm upgrade, verify rollout |
| `/bench` | Manual only | Create benchmark pool, run fio tests, report results, clean up |
| `/test-e2e` | Auto or manual | Pre-flight cleanup of stale resources + run E2E VM clone test |
| `/docs-audit` | Auto or manual | Audit docs for accuracy against current codebase |

## Build & Test Commands

```bash
make build              # Build binary (CGO_ENABLED=0)
make test               # Unit tests: go test ./... -v -race -count=1
make test-coverage      # Unit tests with coverage report
make lint               # golangci-lint run ./...
make generate           # controller-gen for CRD types and DeepCopy
make docker-build       # Container image (UBI9-based)
make deploy             # Apply CRDs + RBAC + manifests to cluster
make helm-install       # Helm chart installation
make test-e2e           # E2E tests (requires live ROKS cluster + env vars)
make run-local          # Run controller locally in dry-run mode

# Console Plugin (React/TypeScript — OpenShift Console dynamic plugin)
make console-plugin-install       # yarn install --frozen-lockfile
make console-plugin-build         # Production build (yarn build)
make console-plugin-dev           # Development server (yarn dev)
make console-plugin-lint          # Lint TypeScript/React code
make console-plugin-test          # Run plugin unit tests
make console-plugin-docker-build  # Container image for the console plugin
```

Run a single test: `go test ./pkg/pool/ -v -race -run TestPoolManager_Allocate`

## Architecture

**CSI Controller** (Deployment) — gRPC server receiving CreateVolume/DeleteVolume from CSI sidecars. Delegates to Pool Manager. Never calls VPC API directly.

**Pool Manager** — Core brain. Runs as a controller-runtime reconciler watching FileSharePool CRs. Exposes synchronous `Allocate()` for the CSI controller. Handles share selection (spread vs binpack strategy), capacity tracking, auto-expansion.

**Clone Worker** — Background goroutine that processes async clone operations for volumes >10 GB. Discovers pending clones, copies data, updates SubVolume status.

**Replication Controller** — Background controller that syncs SubVolume data to a remote region's NFS pool on a configurable schedule. Uses rsync for incremental transfer.

**Golden Image Syncer** — Discovers CDI DataImportCron resources and provisions ready-to-boot VM golden images on pool storage. Converts qcow2 to raw format.

**Hook Orchestrator** (`pkg/hooks/`) — Executes lifecycle hooks (exec commands or HTTP webhooks) before/after replication syncs and group snapshots.

**Admission Webhooks** (`pkg/webhook/`) — Five validating webhooks covering FileSharePool, SubVolume, Snapshot, VolumeGroupSnapshot, and ReplicationPolicy CRDs.

**CSI Node Agent** (DaemonSet) — Mounts NFS shares once per node, bind-mounts subdirectories into pod paths. Maintains mount cache.

**IBM VPC Client** (`pkg/ibmcloud/`) — Thin wrapper around `vpc-go-sdk`. Isolated from K8s/CSI concerns. All operations idempotent with context-based timeouts.

**CRDs:** `FileSharePool` (pool config + status), `SubVolume` (PVC allocation tracking), `Snapshot` (point-in-time copies), `VolumeGroupSnapshot` (coordinated multi-PVC snapshots), `ReplicationPolicy` (cross-region DR).

## Project Layout

```
cmd/               → main.go only (flags, wiring, no business logic)
cmd/migrate/       → kubectl-migrate CLI plugin for PVC migration
pkg/driver/        → CSI gRPC handlers (thin, delegate to pkg/pool/)
pkg/pool/          → Core business logic (allocation, share selection, capacity,
                     clone worker, replication controller, golden image syncer)
pkg/ibmcloud/      → IBM VPC API client, isolated from K8s
pkg/ibmcloud/fake/ → Fake client for testing
pkg/k8s/           → Kubernetes client and CRD reconcilers
pkg/util/          → Mount helpers, path validation, quota tracking
pkg/hooks/         → Lifecycle hook orchestrator (exec + HTTP)
pkg/webhook/       → Validating admission webhooks for all CRDs
pkg/metrics/       → Prometheus metric definitions (21 metrics)
pkg/migrate/       → PVC migration logic
api/v1alpha1/      → CRD Go types and generated code
config/            → Kubernetes manifests (CRDs, RBAC, deployments, webhooks)
charts/            → Helm chart
console-plugin/    → OpenShift Console dynamic plugin (React/TypeScript)
test/              → Integration and e2e tests (unit tests live next to code)
```

## Key Design Rules

1. **CreateVolume picks an existing share + records SubVolume CR** — never creates VPC shares in the hot path
2. **All state in CRDs** — no external DB, no ConfigMaps for state
3. **Node mounts cached** — one NFS mount per share per node, PVCs bind-mount subdirectories
4. **Fail safe** — return retriable gRPC errors, never silently overcommit
5. **VPC API calls are expensive (30-90s)** — belong in pool manager's background reconciliation, not CSI handlers

## Testing Conventions

- **Go 1.25+**, module path: `github.com/IBM/ibm-vpc-file-pool-csi`
- Unit tests live next to code (`_test.go`), use table-driven tests
- Race detector mandatory: `-race` flag
- Fake VPC client: `pkg/ibmcloud/fake/fake_client.go`
- Fake K8s client: `sigs.k8s.io/controller-runtime/pkg/client/fake`
- Fake mounter: `k8s.io/mount-utils/fake`
- Coverage targets: `pkg/pool/` 85%, `pkg/driver/` 75%, `pkg/ibmcloud/` 60%, `pkg/util/` 90%

## VPC NFS Platform Constraints

- **`sec=sys` required** — VPC file shares default-negotiate to `sec=null` (anonymous auth) unless the client explicitly requests `sec=sys`. Without `sec=sys`, all files are owned by UID 99, chown fails for all UIDs, and KubeVirt VMs cannot start. The driver includes `sec=sys` in its default NFS mount options (matching the stock IBM VPC File CSI driver)
- **NFS root_squash is always enabled** — root (UID 0) maps to nobody (UID 65534). The driver sets `initial_owner: {uid: 65534, gid: 65534}` on share creation so the mapped root can manage the share root
- **KubeVirt requires `defaultUID: 107`** — virt-handler chowns PVC mount directories to UID 107 (QEMU). With `sec=sys`, chown works and `defaultUID: 107` ensures directories are pre-owned correctly
- **Do not use setuid/MkdirAsUser approaches** — non-root UIDs cannot traverse the kubelet directory tree (`/var/data/kubelet/plugins/` is mode 0750). Create directories as root and chown afterwards

## Critical Safety Rules

- **Path validation required** before any mkdir/rm — validate against `^/pvcs/pvc-[a-f0-9-]{36}$` pattern and check for traversal
- **Never log** API keys or secrets; share IDs and mount target IPs are OK
- **Leader election required** for controller — concurrent controllers corrupt pool state
- **NFS mount options:** use `soft,timeo=600,retrans=3` — never `hard` (pods hang on NFS failures)
- **Run `make generate`** after changing CRD types in `api/v1alpha1/`

## After Making Changes

- **CRD type changes** (`api/v1alpha1/`) → run `make generate`, then update `CRD-SPEC.md`
- **New metrics** (`pkg/metrics/`) → update `MONITORING.md` metric reference table
- **Helm value changes** (`charts/*/values.yaml`) → update `HELM-VALUES.md`
- **New CLI flags** (`cmd/main.go`) → update `INSTALL.md`
- **New features** → update `FEATURES.md` maturity badge, add to `CHANGELOG.md`
- **Any code change** → run `/test` to verify tests pass
- **Before committing** → consider running `/docs-audit` to check doc accuracy

## Logging

Use `klog/v2` structured logging: V(2) normal ops, V(4) detailed, V(6) trace. Use `klog.InfoS`/`klog.ErrorS` with key-value pairs.
