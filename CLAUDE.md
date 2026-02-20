# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

IBM VPC File Pool CSI Driver (`ibm-vpc-file-pool-csi`) — a Kubernetes CSI driver that pools multiple PVCs as subdirectories within shared VPC file shares, instead of the traditional 1:1 PVC-to-share mapping. Think of it like VMware: one NFS datastore holds many VMDKs.

**Current state:** The Go implementation is built. The repository contains both the specification documents and the working code.

## Reference Documents

Read the relevant doc before implementing any component:

| Document | Covers |
|----------|--------|
| `SKILL.md` | Task-specific build guidance — read this first for any implementation task |
| `ARCHITECTURE.md` | System design, component diagram, data flows |
| `CRD-SPEC.md` | FileSharePool and SubVolume CRD definitions |
| `CSI-INTERFACE.md` | CSI gRPC method implementations |
| `IBM-VPC-API.md` | VPC API client wrapper design |
| `CODING-GUIDELINES.md` | Go conventions, error handling, concurrency, metrics |
| `TESTING.md` | Testing strategy, fakes/mocks, coverage targets |
| `API-KEY-SETUP.md` | Authentication via secret-common-lib |
| `INSTALL.md` | Build, deploy, Helm chart |

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
```

Run a single test: `go test ./pkg/pool/ -v -race -run TestPoolManager_Allocate`

## Architecture

**CSI Controller** (Deployment) — gRPC server receiving CreateVolume/DeleteVolume from CSI sidecars. Delegates to Pool Manager. Never calls VPC API directly.

**Pool Manager** — Core brain. Runs as a controller-runtime reconciler watching FileSharePool CRs. Exposes synchronous `Allocate()` for the CSI controller. Handles share selection (spread vs binpack strategy), capacity tracking, auto-expansion.

**CSI Node Agent** (DaemonSet) — Mounts NFS shares once per node, bind-mounts subdirectories into pod paths. Maintains mount cache.

**IBM VPC Client** (`pkg/ibmcloud/`) — Thin wrapper around `vpc-go-sdk`. Isolated from K8s/CSI concerns. All operations idempotent with context-based timeouts.

**CRDs:** `FileSharePool` (cluster-scoped, pool config + status) and `SubVolume` (cluster-scoped, tracks individual PVC allocations).

## Project Layout

```
cmd/           → main.go only (flags, wiring, no business logic)
pkg/driver/    → CSI gRPC handlers (thin, delegate to pkg/pool/)
pkg/pool/      → Core business logic (allocation, share selection, capacity)
pkg/ibmcloud/  → IBM VPC API client, isolated from K8s
pkg/ibmcloud/fake/ → Fake client for testing
pkg/k8s/       → Kubernetes client and CRD reconcilers
pkg/util/      → Mount helpers, path validation, quota tracking
api/v1alpha1/  → CRD Go types and generated code
config/        → Kubernetes manifests (CRDs, RBAC, deployments)
charts/        → Helm chart
test/          → Integration and e2e tests (unit tests live next to code)
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

## Logging

Use `klog/v2` structured logging: V(2) normal ops, V(4) detailed, V(6) trace. Use `klog.InfoS`/`klog.ErrorS` with key-value pairs.
