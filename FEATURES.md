# Features

## Overview

The IBM VPC File Pool CSI Driver replaces the traditional one-PVC-per-share model with a pooled architecture: a small number of large VPC file shares are pre-provisioned, and each PVC gets a subdirectory on an existing share. This eliminates the 30-90 second wait for VPC share creation, reduces the number of shares needed (VPC accounts cap at 300), and consolidates billing since each share has a 10 GB minimum.

For cluster administrators, the driver manages pool lifecycle automatically — expanding capacity when usage crosses a threshold, monitoring share health via the VPC API, and draining degraded shares without manual intervention. State lives entirely in Kubernetes CRDs (FileSharePool, SubVolume, Snapshot), so there is no external database or ConfigMap to manage.

For workload teams, the driver is invisible: they create standard PVCs and the CSI sidecar routes requests to the pool manager. PVC creation completes in under a second. Volume snapshots, clones, and group snapshots work through the standard CSI interfaces. KubeVirt users get first-class support with correct directory ownership and raw disk image handling.

The driver is purpose-built for IBM Cloud ROKS and ROVS clusters running on VPC infrastructure. It integrates with the VPC file share API, creates one mount target per share that serves pods in every zone, and authenticates through the same `secret-common-lib` used by the stock IBM CSI drivers.

### Maturity Legend

| Badge | Meaning |
|-------|---------|
| **GA** | Production-ready, fully tested, stable API |
| **Beta** | Implemented and tested, API may evolve |
| **Planned** | Code exists but not yet validated in production |

---

## Feature Categories

### Core: Pooled Storage Model — GA

Subdirectory-based PVC provisioning on shared VPC file shares. Each PVC maps to a directory (`/pvcs/pvc-<uuid>`) on an existing share, tracked by a SubVolume CR.

- **CRD-based state** — FileSharePool and SubVolume CRDs hold all allocation state. No external database or ConfigMaps.
- **Background reconciliation** — VPC API calls (share creation, health checks, mount target discovery) happen in a background reconciler, never in the CSI CreateVolume hot path.
- **Instant PVC creation** — CreateVolume records a SubVolume CR and returns immediately (<1s vs 30-90s for the stock driver).
- **Idempotent operations** — All CSI methods are idempotent with crash recovery. Duplicate requests return the same result.

### Allocation & Capacity — GA

Share selection and capacity management across the pool.

- **Spread strategy** — Distributes PVCs evenly across shares for lower blast radius (default).
- **Binpack strategy** — Fills shares before using the next one, minimizing active share count.
- **Automatic pool expansion** — When allocation exceeds a configurable threshold (default 80%), a new VPC share is created in the background. Proactive expansion adds buffer before the threshold is reached.
- **Per-PVC quota tracking** — Each SubVolume records its requested size. Pool capacity accounting prevents overcommit.
- **Configurable reclaim policies** — Delete (remove subdirectory on PVC deletion) or Retain (keep data for manual recovery).

### Volume Snapshots — Beta

Directory-level point-in-time copies via the standard CSI snapshot interface.

- **CreateSnapshot** — A Job in the driver's namespace copies the SubVolume directory to a snapshot path on the same share. The Snapshot CR reads `readyToUse: false` until the copy has finished.
- **DeleteSnapshot** — A Job removes the snapshot directory; then the CR is deleted and the pool capacity is given back.
- **ListSnapshots** — Lists snapshots filtered by source volume with pagination support.
- **Restore from snapshot** — A Job copies the snapshot directory into the new volume and sets the pool's owner and permissions. The claim stays Pending until the copy has finished.
- **Validating webhook** — Ensures Snapshot CRs have required fields (sourceSubVolume, poolName).

### Volume Cloning — Beta

Asynchronous, Job-based cloning for every volume size.

- **Async copy** — CreateVolume creates a SubVolume CR with `CloneStatus=Pending` and returns. The clone worker runs a Kubernetes Job that copies the data (the controller pod has no NFS access).
- **Same-share preference** — Clones prefer the same share as the source for speed. Falls back to cross-share when capacity requires it.
- **Clone status gate** — NodePublishVolume returns `Unavailable` for SubVolumes with an in-progress clone, preventing pods from mounting incomplete data.
- **Crash recovery** — The clone worker recovers incomplete clones on restart by scanning for SubVolumes in Pending/InProgress state.
- **Progress tracking** — CloneProgress tracks BytesCopied, TotalBytes, StartedAt, CompletedAt, and Error.

### Volume Group Snapshots — Beta

Multi-PVC coordinated snapshots for application-consistent backups.

- **Coordinated snapshot creation** — CreateVolumeGroupSnapshot takes a list of PVCs and snapshots them in order, tracking per-member state (Pending, Creating, Ready, Failed).
- **Failure policies** — `Abort` (default) rolls back completed snapshots on first failure. `Continue` finishes all members and marks the group as PartialFailure.
- **Copy order** — Configurable ordering of member snapshots. Defaults to alphabetical PV name sorting.
- **Inconsistency window tracking** — Measures the time delta between first and last member copy in milliseconds.
- **Pre/post-snapshot hooks** — Exec and HTTP webhook hooks via the hook orchestrator for application quiescing.
- **Validating webhook** — Ensures VolumeGroupSnapshot CRs have poolName and sourcePVCs.

### Cross-Region Replication — Beta

Disaster recovery with configurable sync schedules. Two replication modes are supported.

- **Direct NFS mode** — rsync-based replication between source and destination NFS shares mounted over Transit Gateway or VPN. Uses incremental delta transfer for efficiency.
- **Driver-to-driver mode** — HTTPS-based replication from sync-client Jobs on the source cluster to a receiver service on the destination cluster. Streams tar archives over HTTPS with bearer token auth. No cross-region NFS connectivity required.
- **Schedule-based sync** — ReplicationPolicy CR defines sync interval (e.g., `15m`, `1h`, `6h`) as a duration string.
- **Per-SubVolume incremental sync** — Optional label selector filters which SubVolumes replicate. Incremental mode (default) uses rsync for efficient delta transfer (direct NFS mode).
- **Bandwidth limiting and parallel syncs** — `bandwidthLimitMbps` caps rsync throughput (direct NFS mode); `maxParallelSyncs` controls concurrent SubVolume syncs in both modes.
- **Extra rsync options** — `rsyncOptions` field passes additional rsync flags (e.g., `--compress`, `--checksum`). Dangerous flags (`--daemon`, `--server`, `--rsh`, `--rsync-path`) are blocked by the webhook. Only applies to direct NFS mode.
- **Retry with backoff** — Configurable max retries (default 3). Policy pauses on consecutive failures.
- **Running VM disks** — Experimental — The controller copies a running VM's disks with QEMU, all at one moment, after a short guest freeze (0.15 s in a lab test). After the first copy, each copy holds only the changed blocks. The destination saves the old contents of every changed block first, so a copy that stops half way is rolled back, and it keeps those as restore points for 24 hours by default (`restorePointKeepHours`). The copy is file-system consistent, or crash consistent if the guest cannot freeze, and the status says which. A live migration cancels the copy. In receiver mode the receiver at the destination applies each copy, and after a VM move only the blocks that differ are sent. Red Hat does not support the QEMU commands this uses.
- **Pause/resume via annotation** — Annotate a ReplicationPolicy with `storage.ibmcloud.io/paused=true` to pause; remove the annotation to resume. The controller updates `status.phase` accordingly.
- **Pre/post-sync hooks** — Exec and HTTP hooks for application quiescing before sync and notification after.
- **Metadata sidecar** — Writes `.subvolume-metadata.json` alongside each replicated SubVolume directory, enabling the failover CLI to reconstruct resources without querying the source cluster.
- **Failover CLI** — `kubectl failover` plugin with `plan` (scan destination NFS, compute RPO), `execute` (create SubVolume CRs, PVs, PVCs on DR cluster), and `status` (report PVC binding) subcommands. For running VMs it picks the newest moment all of a VM's disks can show, rolls back a copy that stopped half way, and with `--rewind-to` goes back to an earlier restore point (`--list-moments` lists them). Supports `--dry-run` and is idempotent.
- **Status tracking** — Per-policy LastSyncTime and LastSyncDuration. Per-SubVolume sync state with BytesSynced and LastError.
- **Destination config** — NFS server IP and base path (direct NFS mode), or HTTPS receiver endpoint with bearer token auth (driver-to-driver mode).
- **Prometheus metrics** — Dedicated metrics for sync total, duration, lag, failures, and SubVolume count.

### NFS & Mount Management — GA

Efficient NFS mount handling with security enforcement.

- **Mount caching** — One NFS mount per share per node. Multiple PVCs on the same share reuse the cached mount, avoiding redundant NFS connections.
- **Bind-mount publishing** — NodePublishVolume bind-mounts the SubVolume's subdirectory from the staged NFS mount into the pod's volume path.
- **`sec=sys` enforcement** — All NFS mounts include `sec=sys` to ensure proper UID/GID authentication. Without it, VPC shares negotiate `sec=null` and all files appear as UID 99.
- **Soft mount defaults** — Mount options `nfsvers=4.1,soft,timeo=600,retrans=3` prevent pods from hanging on NFS failures.
- **Mount target address** — Each share has one mount target, in the pool's zone. Nodes in every zone mount it.
- **Path traversal validation** — All subdirectory paths validated against `^/pvcs/pvc-[a-f0-9-]{36}$` before any filesystem operation.

### KubeVirt Integration — Beta

First-class support for running virtual machines on pooled storage.

- **UID 107 directory ownership** — Pool `defaultUID: 107` and `defaultGID: 107` ensure subdirectories are pre-owned for QEMU (virt-handler chowns to 107:107).
- **Raw disk image support** — KubeVirt requires raw format for filesystem PVCs. Cloud images (distributed as qcow2) must be converted to raw. CDI does this when it imports an image, and the pool's golden images are made by CDI. Convert with `qemu-img convert -f qcow2 -O raw` only if you write images to a pool yourself.
- **CDI registry import** — CDI imports an image from a registry onto a pool StorageClass. This was measured on rovslab (CentOS Stream 9 in about 40 s, RHEL 9 in about 2.5 min). A copy of a catalog image from other storage (such as Ceph) onto the pool uses CDI's copy pods. Their default memory limit is too small, so the controller raises it (see the CDI copy limit feature below).
- **`sec=sys` enables chown** — With `sec=sys`, virt-handler's chown to UID 107 succeeds. Without it, chown fails for all non-root UIDs.

### Multi-Tenancy & Performance Tiers — Beta

Multiple independent pools with per-tier storage profiles.

- **Multiple pools per cluster** — Each FileSharePool CR is independent with its own shares, capacity, and allocation strategy.
- **Tiered storage** — Pools can define multiple ShareTiers, each with a distinct IOPS value, share size, and max share count.
- **Auto-generated StorageClasses** — Non-tiered pools generate one StorageClass (`{pool-name}`). Tiered pools generate one per tier (`{pool-name}-{tier-name}`). Skip with the `storage.ibmcloud.io/skip-storageclass` annotation.
- **Tier-aware allocation** — CreateVolume requests pass the tier name via StorageClass parameters. Share selection filters to the requested tier.
- **Resource group & tag support** — Shares can be tagged with VPC resource group and user-defined tags for cost tracking.

### Pool Lifecycle & Resilience — GA

Automated pool management with health monitoring and safe shutdown.

- **Pool phases** — Initializing (no shares ready) → Ready (stable capacity) → Expanding (creating shares) → Degraded (draining/unhealthy shares) → Full (at max capacity).
- **Share health monitoring** — Background reconciler polls VPC API for share lifecycle state. Promotes creating shares to stable when mount targets are available. Detects VPC-reported degradation.
- **Share draining** — Marking a share for drain excludes it from new allocations. ShareDrainStatus tracks remaining SubVolumes and drain start time. Share is removed when empty.
- **Pool finalizer protection** — Pools cannot be deleted while SubVolumes exist.
- **Leader election** — Single-writer safety for the controller. Concurrent controllers would corrupt pool state.
- **Crash recovery** — All state in CRDs. Controller restart re-reads CRs and resumes from last known state.

### IBM VPC Platform Integration — GA

Thin wrapper around the VPC file share API with operational guardrails.

- **VPC file share CRUD** — Create, get, list, delete shares with idempotent retry. All operations use context-based timeouts.
- **Mount target management** — Creates one mount target per share, in the pool's zone. Tracks its IP address.
- **API rate limiting** — 5 requests/second default to stay within VPC API limits.
- **`secret-common-lib` authentication** — API key and pod identity auth via the same library used by the stock IBM CSI drivers. Runs as a sidecar with panic recovery.
- **Config auto-discovery** — Reads cluster zone, resource group, and VPC ID from standard IBM Cloud ConfigMaps.
- **Initial owner handling** — Sets `initial_owner: {uid: 65534, gid: 65534}` on share creation so root (squashed to 65534 by NFS root_squash) can manage the share root.

### Security — GA

Defense-in-depth controls for a storage driver running with host access.

- **Path traversal validation** — All mkdir/rm operations validate paths against `^/pvcs/pvc-[a-f0-9-]{36}$` and reject traversal attempts.
- **Validating admission webhooks** — Beta — Five webhook validators cover all CRD types (FileSharePool, SubVolume, Snapshot, VolumeGroupSnapshot, ReplicationPolicy).
- **RBAC minimal permissions** — Controller and node service accounts use least-privilege roles.
- **No secrets in logs** — API keys and tokens are never logged. Share IDs and mount target IPs are safe to log.

### Observability — GA

Prometheus metrics and structured logging for operational visibility.

- **Prometheus metrics** — 21+ metrics covering allocation (count, duration, capacity), VPC API calls (count, latency), snapshots, clones, group snapshots, and replication.
- **Structured logging** — `klog/v2` with `InfoS`/`ErrorS` and key-value pairs. V(2) for normal operations, V(4) for detailed flow, V(6) for trace-level.
- **Key metrics:**
  - `vpc_file_pool_allocations_total` — allocation/deallocation counts by pool and status
  - `vpc_file_pool_allocation_duration_seconds` — allocation latency histogram
  - `vpc_file_pool_capacity_gb` / `vpc_file_pool_allocated_gb` — pool utilization gauges
  - `vpc_file_pool_api_calls_total` / `vpc_file_pool_api_call_duration_seconds` — VPC API health
  - `vpc_file_pool_snapshots_total` / `vpc_file_pool_clones_total` — data protection operation counts
  - `pool_csi_replication_lag_seconds` — replication RPO monitoring

### Deployment — GA

Production-ready packaging for ROKS and ROVS clusters.

- **Helm chart** — Full customization via `values.yaml`. Configurable image tags, resource limits, node selectors, tolerations, and all driver parameters.
- **ROKS/ROVS kubelet path handling** — ROKS and ROVS use `/var/data/kubelet` (not `/var/lib/kubelet`). The chart's `node.kubeletDir` defaults to the correct path.
- **UBI9-based container image** — Built on Red Hat Universal Base Image 9 for enterprise compliance.
- **cert-manager webhook TLS** — Admission webhooks use cert-manager for automatic certificate provisioning and rotation. ROVS has no cert-manager; on ROKS and ROVS set `webhook.certProvider=openshift` (see [INSTALL.md — Webhook Certificates](INSTALL.md#webhook-certificates)).
- **hostNetwork for NFS persistence** — Node DaemonSet runs with `hostNetwork: true` so NFS TCP connections survive container restarts.

### Migration — Beta

CLI tool for migrating PVCs from the stock IBM VPC File CSI driver.

- **`kubectl migrate plan`** — Discovers PVCs in a namespace by StorageClass, calculates total size, detects pod attachments.
- **`kubectl migrate execute`** — Creates a migration pod that copies data from the source PVC to a new SubVolume in the target pool.
- **`kubectl migrate status`** — Checks migration pod progress.
- **Dry-run mode** — Preview migration plan without making changes.
- **Idempotent and resumable** — Safe to re-run after interruption.

### OpenShift Console Integration — Beta

Visual management of file share pools through the OpenShift Console.

- **Dynamic console plugin** — Registers as an OpenShift ConsolePlugin (v1 API, requires OpenShift 4.14+). Gated by `consolePlugin.enabled` in the Helm chart (default `false`).
- **Single tabbed navigation** — One "IBM VPC File Pools" item under Storage in the console sidebar opens a tabbed interface (Overview, Pools, SubVolumes, Snapshots, Group Snapshots, Replication, Monitoring). Detail and create pages render standalone without tabs.
- **Pool dashboard** — Overview tab showing all FileSharePools with status, capacity gauges, share count, SubVolume count, and recent activity table.
- **CRUD for all 5 CRDs** — List, detail, create, edit, and delete views for FileSharePool, SubVolume, Snapshot, VolumeGroupSnapshot, and ReplicationPolicy resources. Create buttons route to custom form wizards (not the YAML editor).
- **FileSharePool creation wizard** — 6-step guided wizard for creating pools: zone/profile selection, capacity configuration, allocation strategy, directory ownership, and review. The form no longer offers golden images.
- **IOPS column in pool table** — Displays `spec.iops`, or "VPC default" when it is not set.
- **Monitoring tab** — Dedicated metrics page with time range selector (1h/6h/24h/7d), stat cards (allocation rate, P95 latency, VPC API health, replication status), and time-series charts (allocation rate, latency, API call rate by status, replication lag).
- **Prometheus metrics panels** — Stat cards and time-series charts using the driver's 21+ Prometheus metrics with `usePrometheusRange` hook for range queries.
- **TLS via service-ca** — Plugin Service uses OpenShift's service-ca operator for automatic TLS certificate provisioning (no cert-manager dependency).

### CDI Copy Limit — Beta

Lets VMs from the standard OpenShift Virtualization catalog use a pool for their boot disk.

- **Standard catalog** — Pick a catalog image and choose the pool's StorageClass for the boot disk. CDI copies the image onto the pool. See [USER-GUIDE.md](USER-GUIDE.md#create-a-vm-with-its-boot-disk-on-a-pool).
- **Copy pod memory limit** — The controller keeps the memory limit of CDI's copy pods at least 8Gi, through `HyperConverged` `spec.resourceRequirements.storageWorkloads`. It checks at start-up and every 5 minutes (leader only), and it only raises the limit. Helm value `cdi.copyPodMemoryLimit`; empty turns the check off.
- **Visible resets** — Each raise logs a warning and adds 1 to `vpc_file_pool_cdi_copy_limit_raised_total`.
- **Measured** — On rovslab, a 25 GB copy finished in 134 s with 8Gi and never finished with 2Gi. A RHEL 9 catalog VM had its guest agent after 103 s.
- **ROVS** — IBM says the virtualization add-on cannot be modified. See [KNOWN-LIMITATIONS.md](KNOWN-LIMITATIONS.md#cdi-containerized-data-importer-compatibility).

### Golden Image Syncer — Hidden

**Hidden.** The code is kept for possible later use. The console pool form no longer offers golden images, and the "VM storage" preset no longer turns them on. A pool that sets `spec.goldenImages` in YAML still works. The standard catalog is the recommended way to make VMs. The features below stay listed for that reason.

VM image caching for KubeVirt workloads.

- **CDI DataImportCron discovery** — Finds golden images defined by CDI DataImportCrons and syncs them to target namespaces.
- **CDI import** — Makes a CDI DataVolume `golden-<image>` in each target namespace, on the pool's StorageClass, from the registry URL of the DataImportCron (`pullMethod: node`). CDI converts the image to raw. For ImageStream-based crons (RHEL), the syncer uses the external address the ImageStream imports from. The image is ready when the DataVolume phase is Succeeded. Target namespaces need no `anyuid` grant.
- **Configurable refresh interval** — Default 24-hour sync cycle. Per-pool configuration via `GoldenImageConfig` in the FileSharePool spec.
- **CDI DataSource creation** — Creates DataSource CRs in `openshift-virtualization-os-images`, named `<image>-<namespace>-nfs-pool` (one per golden claim), for InstanceTypes catalog visibility. Each points to the golden PVC in its target namespace. Creating a VM from the InstanceTypes tab with its boot disk on the pool is being tested.
- **Pool deletion clean-up** — Deleting a pool deletes its golden DataVolumes, PVCs, Templates and DataSources first. The syncer skips pools being deleted. The syncer refuses a `golden-<image>` DataVolume that the pool did not make.
- **Per-namespace status** — Tracks sync phase (Pending, Syncing, Ready, Failed) per target namespace.

---

## Comparison Table

How this driver compares to the two most common alternatives for NFS-based Kubernetes storage on IBM Cloud.

| Feature | IBM VPC File Pool CSI | IBM VPC File CSI | K8s NFS CSI |
|---------|:---------------------:|:----------------:|:-----------:|
| **Architecture** | | | |
| Pooled storage model | ✅ | ❌ | ❌ |
| 1:1 PVC-to-share mapping | ❌ | ✅ | ✅ |
| State in CRDs | ✅ | ❌ | ❌ |
| PVC creation time | <1s | 30-90s | <1s ¹ |
| Background reconciliation | ✅ | ❌ | ❌ |
| **CSI Operations** | | | |
| CreateVolume / DeleteVolume | ✅ | ✅ | ✅ |
| ExpandVolume | ✅ (pool-level) | ✅ (share-level) | ❌ |
| Volume snapshots | ✅ (directory copy) | ❌ | ❌ |
| Volume clones | ✅ (sync + async) | ❌ | ❌ |
| Volume group snapshots | ✅ (beta) | ❌ | ❌ |
| Clone status gate | ✅ | — | — |
| **NFS** | | | |
| `sec=sys` enforcement | ✅ | ✅ | ❌ |
| Mount caching (per-share) | ✅ | — | ❌ |
| Soft mount defaults | ✅ | ✅ | Configurable |
| One mount target serves every zone | ✅ | ✅ | — |
| Bind-mount subdirectories | ✅ | ❌ | ❌ |
| **Capacity Management** | | | |
| Per-PVC quota tracking | ✅ | ❌ (share-level) | ❌ |
| Automatic pool expansion | ✅ | — | — |
| Spread / binpack strategies | ✅ | — | — |
| Proactive expansion | ✅ | — | — |
| Share consolidation | ✅ | ❌ | — |
| **Resilience** | | | |
| Idempotent operations | ✅ | ✅ | ✅ |
| Crash recovery | ✅ (CRD state) | ✅ | ❌ |
| Share health monitoring | ✅ | ❌ | — |
| Share draining | ✅ | — | — |
| Cross-region replication | ✅ (beta) | ❌ | ❌ |
| Leader election | ✅ | ✅ | ✅ |
| **Platform** | | | |
| IBM VPC API integration | ✅ | ✅ | ❌ |
| `secret-common-lib` auth | ✅ | ✅ | — |
| ROKS/ROVS kubelet path handling | ✅ | ✅ | ❌ |
| KubeVirt UID 107 support | ✅ | ❌ | ❌ |
| Raw disk image conversion | ✅ | ❌ | ❌ |
| Multi-tenancy / tiers | ✅ | ❌ | ❌ |
| Config auto-discovery | ✅ | ✅ | ❌ |
| API rate limiting | ✅ | ✅ | — |
| **Observability** | | | |
| Prometheus metrics | ✅ (21+ metrics) | ✅ | ❌ |
| Structured logging (klog) | ✅ | ✅ | ✅ |
| OpenShift Console plugin | ✅ (beta) | ❌ | ❌ |
| **Deployment** | | | |
| Helm chart | ✅ | ✅ | ✅ |
| RBAC minimal permissions | ✅ | ✅ | ✅ |
| Validating webhooks | ✅ (beta) | ❌ | ❌ |
| UBI9 container image | ✅ | ✅ | ❌ |
| cert-manager TLS | ✅ | ❌ | ❌ |
| Migration from stock driver | ✅ (beta) | — | — |

¹ K8s NFS CSI requires a pre-existing NFS server; provisioning time depends on the NFS server setup, not the CSI driver.

**Legend:** ✅ = supported, ❌ = not supported, — = not applicable

**Driver links:**
- [IBM VPC File CSI Driver](https://github.com/IBM/ibm-vpc-file-csi-driver)
- [Kubernetes NFS CSI Driver](https://github.com/kubernetes-csi/csi-driver-nfs)
