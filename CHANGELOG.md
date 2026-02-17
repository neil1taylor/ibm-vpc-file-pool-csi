# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [v0.5.0] — 2026-02-17

### Added

- **Cross-region disaster recovery (Phase 4d)** — `ReplicationPolicy` CRD and background replication controller for replicating SubVolume data to a remote region's NFS pool. Configurable schedule (Go duration), label-based SubVolume selector, consecutive failure tracking with auto-pause after max retries exceeded. Per-SubVolume and per-policy replication status in CRD. Prometheus metrics: `pool_csi_replication_sync_total`, `pool_csi_replication_sync_duration_seconds`, `pool_csi_replication_lag_seconds`, `pool_csi_replication_consecutive_failures`, `pool_csi_replication_subvolume_count`. K8s client interface extended with `GetReplicationPolicy`, `ListReplicationPolicies`, `CreateReplicationPolicy`, `UpdateReplicationPolicyStatus`, `DeleteReplicationPolicy`. 20+ unit tests with race detector coverage

## [v0.4.0] — 2026-02-17

### Added

- **Volume snapshots (Phase 4a)** — CSI `CreateSnapshot`, `DeleteSnapshot`, `ListSnapshots` with directory-level copy-based snapshots on NFS. Includes `RestoreSnapshot` for creating new volumes from snapshots. Snapshot CRD tracks phase, readiness, and size. Full idempotency, capacity tracking, and Prometheus metrics
- **Volume cloning (Phase 4b)** — CSI `CreateVolume` with `VolumeContentSource` of type `VOLUME` creates a copy of an existing SubVolume. Dual-path design: synchronous `cp -a` for volumes under 10 GB, async background worker for larger volumes with `NodePublishVolume` gating until clone completes
- **Background clone worker** (`pkg/pool/clone_worker.go`) — ticker-based goroutine that discovers Pending/InProgress clone SubVolumes and processes them. Handles crash recovery, concurrent dedup via `sync.Map`, partial copy cleanup on failure, and Prometheus metrics
- **Volume group snapshots (Phase 4c)** — `CreateVolumeGroupSnapshot` and `DeleteVolumeGroupSnapshot` for coordinated multi-PVC snapshots. Sequential copy with configurable copy order, inconsistency window tracking, and Abort/Continue failure policies. VolumeGroupSnapshot CRD with per-member status tracking
- **Share draining** — `FileSharePool.spec.drainShares` marks shares for graceful evacuation. Draining shares are excluded from new allocations. Reconciler tracks progress in `status.drainStatus` and sets `DrainComplete` condition when all SubVolumes are removed
- **PVC migration CLI** (`cmd/migrate/`) — `kubectl migrate` plugin with `plan`, `execute`, and `status` subcommands for migrating PVCs from the stock IBM VPC File CSI driver to the pool driver. Creates temporary rsync pods for data transfer with progress streaming and file count verification
- **Integration test suite** (`test/integration/`) — 35+ tests covering pool lifecycle, concurrent allocation, capacity management, error recovery, and snapshot lifecycle. Wires real `pool.Manager` + `driver.Driver` + `Reconciler` with fake infrastructure
- **VPC API client test coverage** — increased from 22% to 90% with 57 test functions using `httptest` servers
- **GitHub Actions CI/CD** — `ci.yml` workflow with lint, test, build, and generate-check jobs; `release.yml` for tagged container image builds
- **Helm validation** — `make helm-lint` and `make helm-template` targets; fixed node DaemonSet template for resources/nodeSelector/affinity
- **Phase 4 design documents** — `VOLUME-CLONING.md` (Phase 4b), `VOLUME-GROUP-SNAPSHOTS.md` (Phase 4c), `CROSS-REGION-DR.md` (Phase 4d) with CRD designs, consistency analysis, and architecture diagrams

### Fixed

- SubVolume CRD YAML now includes clone fields (`sourceVolume`, `sourceShareID`, `cloneStatus`, `cloneProgress`, `Cloning` phase)
- Helm `values.yaml` missing `nameOverride`/`fullnameOverride` defaults
- Helm `NOTES.txt` updated for Snapshot CRD and VolumeSnapshotClass

## [v0.3.0] — 2026-02-16

### Added

- **Cross-zone accessor binding support** — `FileSharePool.spec.accessorZones` enables mount targets in additional VPC zones, allowing nodes in non-home zones to mount shares via zone-local NFS IPs. PV volumeAttributes include `server.<zone>` keys for zone-aware node agent mounting
- **`ZoneMountTarget` in pool status** — `status.shares[].mountTargets` records mount target IPs per zone for full observability
- **Idempotent `CreateFileShare`** — when the VPC API returns HTTP 400 "already exists" (e.g., reconciler retry after status update conflict), the client looks up the existing share by name via `getShareByName` and returns it
- **`CreateShareMountTarget`** — new VPC client method to add mount targets to existing shares (used for accessor zone mount target creation)
- **E2E test framework** (`test/e2e/`) — automated end-to-end tests using `//go:build e2e` tag with `make test-e2e` target. Tests: `TestBasicPool`, `TestCrossZonePool`, `TestCrossZonePool_CRDValidation`
- **nsenter mount wrapper** — Dockerfile injects `/usr/local/bin/mount` that routes NFS mounts through `nsenter --mount=/proc/1/ns/mnt` (host namespace) and bind mounts through container's `/usr/bin/mount`
- **`.dockerignore`** — excludes `.git`, `docs`, `site`, `test`, `charts`, `config` from build context for faster container builds

### Fixed

- SubVolume status now correctly persisted after creation — saves `desiredStatus` before `Create()` (which strips status due to subresource) and writes via `Status().Update()`
- Pool reconciler no longer gets stuck in Initializing when share name conflict occurs on retry
- Node DaemonSet requires `hostPID: true` for nsenter mount wrapper to access host mount namespace

## [v0.2.0] — 2026-02-16

### Added

- **Share tiering** for multi-performance pools — assign `dp2`, `custom`, or other VPC file share profiles to tiers within a single FileSharePool, enabling mixed-performance workloads
- **Per-PVC usage reporting** in `NodeGetVolumeStats` — returns `du`-based per-subdirectory usage via the CSI `NodeGetVolumeStats` RPC, surfacing storage consumption in `kubectl describe pvc`
- **Prometheus metrics and soft quota alerting** — exports pool utilization, allocation latency, share count, and PVC count metrics on the `/metrics` endpoint; includes PrometheusRule alerts at configurable utilization thresholds
- **Auto-discovery of VPC config** — region, VPC ID, and subnet ID are automatically read from the `ibm-cloud-provider-data` ConfigMap and secret provider endpoint on managed ROKS/IKS clusters, eliminating manual Helm value overrides
- **Troubleshooting guide** (`TROUBLESHOOTING.md`) — comprehensive runbook covering mount failures, stuck PVCs, capacity issues, VPC API errors, and node problems
- **Performance tuning guide** (`PERFORMANCE-TUNING.md`) — NFS mount option tuning, IOPS planning by profile, benchmarking procedures, and kernel parameter recommendations
- **Contributing guide** (`CONTRIBUTING.md`) — developer setup, PR process, coding standards, and DCO sign-off requirements
- **Ready-to-use examples** (`examples/`) — YAML manifests for basic PVC, StatefulSet, multi-pool, tiered storage, and monitoring configurations
- **MkDocs Material documentation site** with GitHub Pages deployment — full docs site with search, dark mode, and tabbed navigation
- **Phase 4 roadmap** — OpenShift Virtualization Ready milestones: snapshots, volume cloning, group snapshots, and cross-region DR

### Changed

- Centralized glossary into README for single-source definitions of Pool, Share, SubVolume, and related terms
- "Why Pooling?" section added to README explaining the cost, quota, and speed advantages over stock 1:1 CSI mapping

## [v0.1.0] — 2026-02-15

### Added

- **CSI Controller** (`pkg/driver/controller.go`) — gRPC server implementing `CreateVolume`, `DeleteVolume`, `ValidateVolumeCapabilities`, `ControllerExpandVolume`, and capability RPCs; delegates all allocation logic to the Pool Manager
- **CSI Node Agent** (`pkg/driver/node.go`) — `NodeStageVolume` mounts NFS shares once per node, `NodePublishVolume` bind-mounts subdirectories into pod paths, with mount caching to avoid redundant NFS mounts
- **Pool Manager** (`pkg/pool/manager.go`) — core allocation engine with `Allocate()` and `Deallocate()` methods; supports `spread` (most free space) and `binpack` (least free space) share selection strategies; tracks share capacity via SubVolume CRs
- **FileSharePool reconciler** (`pkg/pool/reconciler.go`) — controller-runtime reconciler watching FileSharePool CRs for background pool management: creates initial shares, handles auto-expansion when utilization exceeds threshold, and updates pool status
- **IBM VPC API client** (`pkg/ibmcloud/client.go`) — thin wrapper around `vpc-go-sdk` for file share CRUD, mount target management, and share capacity expansion; all operations idempotent with context-based timeouts
- **FileSharePool CRD** — cluster-scoped custom resource defining pool configuration: zone, profile, share size, max shares, allocation strategy, auto-expansion, and tier definitions
- **SubVolume CRD** — cluster-scoped custom resource tracking individual PVC allocations: share reference, subdirectory path, requested size, and pool membership
- **Helm chart** (`charts/ibm-vpc-file-pool-csi/`) — deploys controller Deployment, node DaemonSet, CSI sidecars (provisioner, resizer, registrar, liveness probe), RBAC, and StorageClass
- **Storage-secret-sidecar integration** — automatic IBM Cloud API key injection on managed ROKS/IKS clusters using `armada-storage-secret` sidecar
- **Path validation** — all subdirectory operations validate against `^/pvcs/pvc-[a-f0-9-]{36}$` regex and reject path traversal attempts
- **Leader election** — controller uses controller-runtime leader election to prevent concurrent pool state corruption

### Fixed

- VPC API endpoint construction for production IBM Cloud regions
- RBAC permissions for FileSharePool and SubVolume CR watch/update
- Subdirectory creation race condition during concurrent `NodePublishVolume` calls
- Mount target IP resolution when share has multiple mount targets across zones
- Makefile targets for end-to-end build pipeline

[v0.5.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.4.0...v0.5.0
[v0.4.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.3.0...v0.4.0
[v0.3.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.2.0...v0.3.0
[v0.2.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/releases/tag/v0.1.0
