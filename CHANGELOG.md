# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [v0.11.0] — 2026-02-21

### Added

- **Golden image syncer for KubeVirt one-click VM creation** — Background worker discovers CDI `DataImportCron` resources and provisions ready-to-boot golden image PVCs on pool storage. Two modes: native CDI (pool SC is default) and custom syncer (pool SC is not default). Custom syncer creates converter Jobs that download OCI images from container registries (e.g., `quay.io/containerdisks/`), decompress gzip-compressed OCI layers, detect qcow2 vs raw format, convert to raw, and write `disk.img` to golden PVCs. Creates `DataSource` CRs for InstanceTypes tab and OpenShift `Template` CRs for the VM catalog
- **`GoldenImageConfig` in FileSharePool spec** — `spec.goldenImages` configures target namespaces, image filters, converter image, and PVC size for the golden image syncer
- **`GoldenImageStatus` in FileSharePool status** — `status.goldenImages` tracks per-image, per-namespace sync state (Pending/Syncing/Ready/Failed)
- **Mount target recovery in health check** — When a stable share has no mount target IP and no VPC mount targets, the reconciler attempts to create one. If creation fails (e.g., share uses `security_group` access mode incompatible with VPC mount targets), the share is marked as `draining` to prevent further allocations
- **`selectShare` mount target IP guard** — Share selection for both normal allocations and clones now skips shares with empty `MountTargetIP`, preventing PVs with empty NFS server fields that cause mount failures
- **RBAC for CDI, Jobs, and Templates** — ClusterRole extended with `cdi.kubevirt.io` (dataimportcrons, datasources), `batch/jobs` (create/delete), and `template.openshift.io/templates` (CRUD)

### Fixed

- **Shares without mount targets selected for allocation** — `selectShare` did not filter by `MountTargetIP`. Shares created before VPC config was available (no inline mount target) could be selected, producing PVs with an empty `server` field. Fixed by adding `MountTargetIP == ""` check to both `selectShare` and `selectShareForClone`
- **No recovery for shares missing mount targets** — Stable shares with no mount target IP were stuck permanently. Added health check recovery that creates a mount target or marks the share as draining if creation fails
- **Golden image converter OOMKilled on large images** — Converter Jobs wrote decompressed OCI layers to staging emptyDir (tmpfs, backed by pod memory). Large images (2+ GB decompressed) caused OOMKill at the 2Gi memory limit. Fixed by decompressing directly to the NFS PVC mount (`/data/`) instead of staging
- **Golden image converter produced gzip files instead of raw disk images** — OCI image layers are gzip-compressed. `qemu-img info` doesn't understand gzip and reported "raw" format, so the script copied the gzip file as-is. VMs failed with "timed out waiting for domain to be defined". Fixed by adding `file` + `gunzip` decompression step before format detection

## [v0.10.0] — 2026-02-20

### Added

- **CDI `StorageProfile` auto-patching with `cloneStrategy: copy`** — The controller patches CDI's StorageProfile for pool StorageClasses with `claimPropertySets` (accessModes, volumeMode) and `cloneStrategy: copy` (host-assisted cloning). CDI's default snapshot-based cloning does not work with the pool CSI driver; `copy` mode uses CDI's built-in host-assisted clone path
- **NFS `sec=sys` default mount option** — Added `sec=sys` to the driver's default NFS mount options. VPC file shares default-negotiate to `sec=null` (anonymous auth) unless the client explicitly requests `sec=sys`. Without it, all files are UID 99, chown fails, and KubeVirt VMs cannot start

### Fixed

- **KubeVirt VMs fail to start: chown operation not permitted** — VPC file shares negotiate `sec=null` by default, making all files anonymous (UID 99) and chown impossible. virt-handler's `chown(107, 107)` failed. Fixed by adding `sec=sys` to default mount options, enabling standard Unix UID/GID auth where chown works for non-root UIDs
- **CDI clones fail between StorageClasses** — CDI's default clone strategy uses VolumeSnapshots, which fails when cloning between different CSI drivers. Set `cloneStrategy: copy` in the StorageProfile to use host-assisted cloning

## [v0.9.0] — 2026-02-19

### Fixed

- **NFS mount used server root instead of share export path** — VPC access mode shares all resolve to the same FQDN; individual shares are differentiated by their NFS export path (e.g., `server:/share_abc123`). The driver was mounting `server:/` (the NFS server root, owned by UID 99, mode 0711), causing `permission denied` when creating subdirectories. Fixed by parsing the export path from the VPC API's `MountPath` field and propagating it through `MountTargetInfo` → `PoolShareStatus` → `SubVolumeSpec` → `AllocationResult` → PV volume context → NFS mount command
- **Export path lost during mount target IP polling** — When `CreateFileShare` polled for the mount target IP to become available, it copied the IP address but discarded the export path. Fixed to copy both fields

### Added

- **ExportPath fields in CRDs** — `PoolShareStatus.ExportPath`, `ZoneMountTarget.ExportPath`, and `SubVolumeSpec.ShareExportPath` fields to persist the NFS export path for each share. Backward-compatible: empty value defaults to `"/"`
- **Export path backfill in health checks** — Existing shares without an export path are automatically backfilled during the reconciler's periodic health check, enabling seamless upgrade from older versions
- **Configurable registrar health port** — The node-driver-registrar's `--http-endpoint` port is now configurable via `node.registrarHealthPort` (default 9809). Avoids conflicts with the stock IBM VPC file CSI driver (port 9808) or other hostNetwork services

## [v0.8.0] — 2026-02-18

### Fixed

- **Hardcoded staging path ignores kubeletDir** — The controller's `stagingBasePath` was hardcoded to `/var/lib/kubelet/...` even when `node.kubeletDir` was set to `/var/data/kubelet` for ROKS. Added `--kubelet-dir` flag to the controller binary and wired it through the Helm chart so the staging path matches the node DaemonSet's kubelet directory
- **Mount flags override replaces safe NFS defaults** — Custom mount flags from the StorageClass completely replaced the safe defaults (`soft`, `timeo=600`, `retrans=3`). A user adding `noatime` would lose `soft` mount protection. Changed to merge custom flags with defaults: same-key options are overridden (e.g., `timeo=300` replaces `timeo=600`), and `soft` is always preserved unless `hard` is explicitly specified
- **No resource requests/limits on any container** — All 8 containers ran with zero resource requests/limits (BestEffort QoS). Added sensible defaults: controller 50m/128Mi (512Mi limit), node 50m/64Mi (256Mi limit), sidecars 10m/32Mi (128Mi limit), registrar/liveness 10m/16Mi (64Mi limit)
- **No probes on sidecar containers** — Added liveness probes to all CSI sidecars (provisioner, resizer, snapshotter, liveness-probe, secret-sidecar) and the node-driver-registrar using their `--http-endpoint` health check support
- **No readiness probe on node main container** — Added readiness probe (socket existence check) so the node is not marked Ready before the CSI driver is initialized
- **csi-resizer and csi-snapshotter missing `--timeout`** — Only the provisioner had `--timeout=300s`. Added the same 300s timeout to the resizer and snapshotter to accommodate VPC API calls that take 30-90s
- **Duplicate ConfigMap fetch** — Two separate API calls for the same `ibm-cloud-provider-data` ConfigMap during auto-discovery. Consolidated into a single fetch
- **Webhook cert volume permissions** — The `webhook-certs` secret volume defaulted to `0644`, exposing TLS private keys to all containers in the pod. Set `defaultMode: 0400`

## [v0.7.0] — 2026-02-17

### Added

- **Automatic StorageClass creation** — When a `FileSharePool` is created, the reconciler now auto-creates matching `StorageClass`(es) so users no longer need to create them manually. Non-tiered pools get one SC named after the pool; tiered pools get one SC per tier (e.g., `my-pool-standard`, `my-pool-premium`). StorageClasses include the correct provisioner, pool/tier parameters, UID/GID/permissions from pool defaults, NFS mount options (falling back to `nfsvers=4.1,soft,timeo=600,retrans=3`), and `allowVolumeExpansion: true`
- **WaitForFirstConsumer for multi-zone pools** — Auto-created StorageClasses use `volumeBindingMode: WaitForFirstConsumer` when the pool has `accessorZones`, ensuring PVCs are scheduled to the correct zone
- **OwnerReference GC** — Auto-created StorageClasses have an OwnerReference pointing to the FileSharePool, so they are garbage-collected when the pool is deleted
- **Opt-out annotation** — Set `storage.ibmcloud.io/skip-storageclass: "true"` on a FileSharePool to skip automatic StorageClass creation (for users who prefer to manage their own)
- **RBAC: StorageClass create** — Controller ClusterRole now includes `create` verb for `storageclasses` (in addition to existing `get`, `list`, `watch`)
- **storagev1 scheme registration** — `k8s.io/api/storage/v1` types registered in both controller and node scheme for StorageClass operations

## [v0.6.0] — 2026-02-17

### Added

- **Lifecycle hooks (Phase 5)** — `Hook` CRD types (`api/v1alpha1/hook_types.go`) supporting `exec` (pod command execution) and `http` (webhook callback) hook types. Configurable `OnError` policy per hook: `Abort` halts the operation on failure, `Continue` logs and proceeds. Timeout enforcement via context deadline (default 30s). `HookResult` type tracks per-hook success, message, timing, and phase
- **Hook orchestrator** (`pkg/hooks/`) — Sequential hook execution engine with `RunPreHooks()` and `RunPostHooks()` methods. `ExecHook` executor finds pods via label selector and runs commands via Kubernetes exec API (SPDY). `HTTPHook` executor sends HTTP requests with custom headers and validates 2xx responses. `Orchestrator` dispatches to the correct executor based on hook type. Output truncated to 256 chars for CRD status safety
- **Pre/post-sync hooks for replication** — `ReplicationPolicy.spec.preSyncHooks` and `postSyncHooks` fields enable application-consistent replication. Hook orchestrator wired into the replication controller: pre-sync hooks execute before each cycle (abort on failure), post-sync hooks execute after successful sync (log-only on failure). Resolves the "quiesce hooks deferred" limitation from Phase 4d
- **Pre/post-snapshot hooks for group snapshots** — `VolumeGroupSnapshot.spec.preSnapshotHooks` and `postSnapshotHooks` fields with `HookResult` status tracking. Resolves the "hooks deferred" limitation from Phase 4c
- **Incremental sync (rsync)** — `NFSOperations.SyncDir()` method using `rsync -a --delete` for delta-only data transfer. `ReplicationPolicy.spec.incrementalSync` field (default `true`) enables rsync-based replication instead of full `cp -a` copies. Reduces replication time and bandwidth for large volumes with few changes
- **Validating admission webhooks** (`pkg/webhook/`) — Five webhook validators registered with the controller-runtime manager, enforcing CRD field constraints at admission time:
  - `FileSharePoolValidator` — zone/profile required, shareSizeGB 10-32000, maxShares 1-1000, initialShares <= maxShares, expandThresholdPercent 1-99, allocationStrategy enum, zone/profile immutable on update
  - `SubVolumeValidator` — poolName/pvcName/pvcNamespace required, requestedGB > 0, subPath validated against `^/pvcs/pvc-[a-f0-9-]{36}$` (path traversal protection)
  - `ReplicationPolicyValidator` — source pool/destination/schedule required, schedule validated as Go duration, maxRetries >= 0
  - `SnapshotValidator` — sourceSubVolume and poolName required
  - `VolumeGroupSnapshotValidator` — poolName required, sourcePVCs non-empty, failurePolicy enum
- **Webhook registration in controller** — All five validators registered via `ctrl.NewWebhookManagedBy()` in `cmd/main.go` using controller-runtime v0.23 typed generic `admission.Validator[T]` interface
- **DeepCopy generation for hook types** — `make generate` produces DeepCopy methods for `Hook`, `ExecHookSpec`, `HTTPHookSpec`, `HookResult` types. Updated CRD YAML manifests for ReplicationPolicy and VolumeGroupSnapshot with hook fields
- **Test coverage** — 15 hook tests (executor, HTTP hook, orchestrator) and 37 webhook validator tests, all with race detector. Fake NFS operations updated with `SyncDir` across all test packages

### Fixed

- **VPC auto-discovery silent failure** — `mgr.GetClient()` returns a cached client whose cache is not started until `mgr.Start()`, causing ConfigMap lookups for `ibm-cloud-provider-data` (VPC ID, subnet ID) to silently fail. Pools were created without mount targets because the VPC ID and subnet ID were empty. Fixed by using the standard `kubernetes.Clientset` for pre-start ConfigMap reads instead of the controller-runtime cached client (`cmd/main.go`)
- **CSI provisioner missing `--extra-create-metadata`** — The `csi-provisioner` sidecar was not passing PVC name and namespace to `CreateVolume` parameters, causing the `SubVolume` webhook to reject every provision request with `spec.pvcName is required`. Added the `--extra-create-metadata` flag to provisioner args in both the Helm chart template and the static deployment manifest
- **Node DaemonSet overlapping Bidirectional mounts** — The `staging-dir` volume was a redundant hostPath mount of the same path as `kubelet-dir`'s subdirectory; both had `mountPropagation: Bidirectional`. On CRI-O (ROKS), this caused "No space left on device" errors. Removed the `staging-dir` volume entirely from both the Helm template and raw manifests
- **Node DaemonSet missing hostNetwork** — NFS TCP connections established in the pod network namespace become stale after pod restart, causing mount failures. Added `hostNetwork: true` so NFS mounts persist across container restarts
- **Liveness-probe port conflict with hostNetwork** — With `hostNetwork: true`, the liveness-probe sidecar binds port 9809 on the host. During rolling updates, old and new pods overlap causing "address already in use" CrashLoopBackOff. Removed the `liveness-probe` sidecar (CSI node health is better monitored via the Unix socket) and added `updateStrategy` with `maxSurge: 0` to prevent pod overlap during rolling updates
- **CSIDriver fsGroupPolicy causing chown failures** — `fsGroupPolicy: File` causes kubelet to recursively `chown` the NFS mount. With VPC file share `root_squash` enabled, `chown` fails with "operation not permitted", causing slow or failed pod startups. Changed to `fsGroupPolicy: None`
- **VPC file share root_squash** — VPC file shares always have `root_squash` enabled (cannot be disabled in VPC access mode), mapping UID 0 to nobody (65534). Root-owned share directories were not writable by the CSI node agent. Fixed by setting `InitialOwner{UID: 65534, GID: 65534}` on share creation so the mapped nobody user owns the root directory
- **Secret provider panic on startup** — `secret-common-lib` panics with a nil gRPC connection dereference if the `storage-secret-sidecar` is not yet ready when the controller starts. Added panic recovery with a retry loop in `cmd/main.go` `runController()`

### Cluster Testing Validated

- Full end-to-end deployment on ROKS eu-de cluster (3-node VPC Gen2)
- FileSharePool reconciliation with VPC share creation and mount target binding
- PVC provisioning (10Gi, 20Gi) via `ibm-vpc-file-pool` StorageClass — binds in seconds
- Pod mount with NFS write/read verification
- VolumeSnapshot creation and readiness
- PVC deletion with capacity reclamation
- All five admission webhooks rejecting invalid resources (FileSharePool, SubVolume path traversal, ReplicationPolicy, Snapshot, VolumeGroupSnapshot)

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

[v0.11.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.10.0...v0.11.0
[v0.10.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.9.0...v0.10.0
[v0.9.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.8.0...v0.9.0
[v0.8.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.7.0...v0.8.0
[v0.7.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.6.0...v0.7.0
[v0.6.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.5.0...v0.6.0
[v0.5.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.4.0...v0.5.0
[v0.4.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.3.0...v0.4.0
[v0.3.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.2.0...v0.3.0
[v0.2.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/compare/v0.1.0...v0.2.0
[v0.1.0]: https://github.com/IBM/ibm-vpc-file-pool-csi/releases/tag/v0.1.0
