# Architecture Reference

## System Overview

```
┌──────────────────────────────────────────────────────────────────────┐
│                        Kubernetes Cluster                            │
│                                                                      │
│  ┌─────────────┐    PVC     ┌──────────────────────────────────┐    │
│  │  Workload   │──created──▶│       CSI Controller Pod          │    │
│  │   Pod       │            │                                    │    │
│  │             │            │  ┌────────────────┐                │    │
│  │  /data ◄───bind-mount───│──│  CSI Controller │                │    │
│  │             │            │  │  (gRPC server)  │                │    │
│  └─────────────┘            │  └───────┬────────┘                │    │
│        ▲                    │          │ Allocate()              │    │
│        │                    │  ┌───────▼────────┐                │    │
│        │                    │  │  Pool Manager   │                │    │
│  ┌─────┴───────┐            │  │                 │                │    │
│  │  CSI Node   │            │  │  - Pick share   │                │    │
│  │  Agent      │            │  │  - Record CR    │                │    │
│  │  (DaemonSet)│            │  │  - Track alloc  │                │    │
│  │             │            │  │  - Auto-expand  │                │    │
│  │  NFS mount  │            │  └───────┬────────┘                │    │
│  │  + bind     │            │          │                          │    │
│  └─────────────┘            │  ┌───────▼────────┐                │    │
│                             │  │  IBM VPC Client │                │    │
│                             │  │  (share CRUD)   │                │    │
│                             │  └───────┬────────┘                │    │
│                             └──────────┼────────────────────────┘    │
│                                        │                              │
└────────────────────────────────────────┼──────────────────────────────┘
                                         │ HTTPS (VPC API)
                                         ▼
                              ┌─────────────────────┐
                              │  IBM Cloud VPC API   │
                              │  File Share Service  │
                              └──────────┬──────────┘
                                         │
                                         ▼
                              ┌─────────────────────┐
                              │  VPC File Shares     │
                              │  (NFS exports)       │
                              │                      │
                              │  ┌────────────────┐  │
                              │  │ Share pool-1-a │  │
                              │  │ /pvcs/pvc-aaa  │  │
                              │  │ /pvcs/pvc-bbb  │  │
                              │  │ /pvcs/pvc-ccc  │  │
                              │  └────────────────┘  │
                              │  ┌────────────────┐  │
                              │  │ Share pool-1-b │  │
                              │  │ /pvcs/pvc-ddd  │  │
                              │  │ /pvcs/pvc-eee  │  │
                              │  └────────────────┘  │
                              └─────────────────────┘
```

## Components

### 1. CSI Controller (Deployment)

The gRPC server that implements CSI Controller RPCs. Runs as a Deployment with 1–2 replicas using leader election.

**Responsibilities:**
- Receive `CreateVolume` / `DeleteVolume` / `ControllerExpandVolume` calls from the Kubernetes CSI sidecar containers.
- Delegate to the Pool Manager for allocation decisions.
- Return volume info (NFS server IP, share path, subdir path) to the kubelet via the CSI protocol.

**Sidecar containers** (standard CSI sidecars, deployed alongside the controller):
- `csi-provisioner` — watches PVCs, calls CreateVolume/DeleteVolume
- `csi-resizer` — watches PVC size changes, calls ControllerExpandVolume
- `liveness-probe` — health checks

**Does NOT:**
- Mount anything.
- Call IBM VPC API directly (goes through Pool Manager → IBM VPC Client).
- Create VPC file shares in the CreateVolume hot path.

### 2. CSI Node Agent (DaemonSet)

Runs on every worker node. Implements CSI Node RPCs.

**Responsibilities:**
- `NodeStageVolume`: Mount the VPC file share (NFS) to a staging directory on the node. If the share is already mounted (another PVC on the same share), reuse the existing mount.
- `NodePublishVolume`: Create the subdirectory on the share if it does not already exist (with uid/gid/permissions from VolumeContext), then bind-mount it into the pod's volume path.
- `NodeUnpublishVolume`: Unmount the bind-mount from the pod path.
- `NodeUnstageVolume`: Unmount the NFS share from the staging directory (only when no more PVCs reference it on this node).
- `NodeGetVolumeStats`: Return capacity/usage stats for the subdirectory (via `du` or `statfs`).

**Mount caching strategy:**

```
Node filesystem:
  /var/lib/kubelet/plugins/vpc-file-pool.csi.ibm.io/staging/
    └── {share-id}/                    ← NFS mount of the whole share
        └── pvcs/
            ├── pvc-aaa/              ← subdirectory for PVC aaa
            ├── pvc-bbb/              ← subdirectory for PVC bbb
            └── pvc-ccc/              ← subdirectory for PVC ccc

  /var/lib/kubelet/pods/{pod-uid}/volumes/kubernetes.io~csi/{pv-name}/mount/
    └── (bind mount of /staging/{share-id}/pvcs/pvc-aaa/)
```

The node agent maintains an in-memory reference count per share mount. When the last PVC using a share on that node is unpublished, the NFS mount is unmounted.

**NFS mount via nsenter wrapper:**

The node agent container includes a `/usr/local/bin/mount` wrapper that routes NFS mounts through the host's mount namespace using `nsenter --mount=/proc/1/ns/mnt --root=/proc/1/root`. Bind mounts use the container's local `/usr/bin/mount`. This requires `hostPID: true` on the DaemonSet so `/proc/1` refers to the host's PID 1.

**Cross-zone server selection:**

When volumeContext contains `server.<zone>` keys (from pools with accessor zones), the node agent selects the IP matching its own zone (`topology.kubernetes.io/zone` label). This ensures NFS traffic stays within the zone for lower latency. Falls back to the default `server` key if no zone match is found.

### 3. Pool Manager

The core logic component. Runs within the controller pod but is architecturally distinct.

**Two modes of operation:**

#### A. Synchronous allocation (called from CSI Controller)

```go
type PoolManager interface {
    // Allocate finds a share with room, records the SubVolume CR, and returns
    // the share's mount info. Subdirectory creation is deferred to NodePublishVolume.
    Allocate(ctx context.Context, req AllocationRequest) (*AllocationResult, error)

    // Deallocate deletes the SubVolume CR and updates pool tracking.
    // Does not remove the subdirectory on the NFS share (nfsOps is nil in controller mode).
    Deallocate(ctx context.Context, subVolumeName string) error

    // Expand updates the allocation size for an existing SubVolume.
    Expand(ctx context.Context, subVolumeName string, newSizeGB int64) error

    // CreateSnapshot creates a directory-level copy of a SubVolume.
    CreateSnapshot(ctx context.Context, snapshotName, sourceVolumeID string, params map[string]string) (*SnapshotResult, error)

    // DeleteSnapshot removes a snapshot directory and its CR.
    DeleteSnapshot(ctx context.Context, snapshotName string) error

    // ListSnapshots lists snapshots, optionally filtered by source volume.
    ListSnapshots(ctx context.Context, sourceVolumeID string) ([]SnapshotResult, error)

    // RestoreSnapshot creates a new SubVolume from a snapshot.
    RestoreSnapshot(ctx context.Context, snapshotName string, req AllocationRequest) (*AllocationResult, error)

    // CloneVolume creates a new SubVolume that is a copy of an existing one.
    // For small volumes (below syncThreshold), the copy completes before returning.
    // For large volumes, the copy runs asynchronously.
    CloneVolume(ctx context.Context, sourceVolumeID string, req AllocationRequest, syncThresholdGB int64) (*AllocationResult, error)

    // CreateVolumeGroupSnapshot creates snapshots for multiple SubVolumes in sequence.
    // Does NOT execute hooks -- the CSI controller handles hook orchestration.
    CreateVolumeGroupSnapshot(ctx context.Context, req GroupSnapshotRequest) (*GroupSnapshotResult, error)

    // DeleteVolumeGroupSnapshot deletes all member snapshots and the group CR.
    DeleteVolumeGroupSnapshot(ctx context.Context, groupName string) error
}

type AllocationRequest struct {
    PVName        string
    PVCName       string
    PVCNamespace  string
    PoolName      string
    RequestedGB   int64
    Zone          string
}

type AllocationResult struct {
    ShareID         string            // VPC share ID
    MountTargetIP   string            // Primary mount target IP
    SubPath         string            // Subdirectory path, e.g., "/pvcs/pvc-abc123"
    SharePath       string            // NFS export root, e.g., "/"
    UID             *int64            // Unix user ID for the subdirectory
    GID             *int64            // Unix group ID
    Permissions     string            // Unix permissions string (e.g., "755")
    MountTargets    map[string]string // Zone -> IP mapping (nil for single-zone)
    AccessibleZones []string          // All zones this volume is accessible from
}
```

#### B. Asynchronous reconciliation (controller-runtime reconciler)

Watches `FileSharePool` CRs and reconciles pool state:
- If total allocated capacity exceeds `expandThreshold` percentage, create a new VPC file share and add it to the pool.
- If a share is unhealthy (VPC API reports degraded), mark it as draining — no new allocations, but existing PVCs remain.
- Periodically scan SubVolume CRs and reconcile actual share state.
- Update `FileSharePool.Status` with current share inventory and capacity.

### 4. IBM VPC Client

Thin wrapper around the `vpc-go-sdk`. Isolates all IBM Cloud API calls.

**Operations:**
- `CreateFileShare(ctx, spec)` → share ID, mount target IP
- `DeleteFileShare(ctx, shareID)` → error
- `ExpandFileShare(ctx, shareID, newSizeGB)` → error
- `GetFileShare(ctx, shareID)` → share details (state, size, IOPS, mount targets)
- `ListFileShares(ctx, filters)` → list of shares (for discovery/reconciliation)

All operations are idempotent or check-before-act. All have context-based timeouts.

### 5. Clone Worker

Background component within the controller pod that handles asynchronous clone operations for large SubVolumes.

**Responsibilities:**
- Process SubVolumes with `cloneStatus=Pending` or `cloneStatus=InProgress`.
- Mount source and target shares (same-share or cross-share) and perform `cp -a` data copy.
- Update SubVolume CR status as clone progresses: `Pending` → `InProgress` → `Complete` or `Failed`.
- Handle crash recovery by restarting incomplete clones on controller restart.

**Does NOT:**
- Block `CreateVolume` — the CSI controller returns immediately for async clones.
- Allow pod access before clone completes — the node agent gates `NodePublishVolume` on `cloneStatus=Complete`.

### 6. Replication Controller

Standalone reconciler that handles cross-region disaster recovery by copying SubVolume data between pools.

**Responsibilities:**
- Watch `ReplicationPolicy` CRs and execute replication cycles on a configurable schedule.
- Mount source shares (read-only) and destination shares (read-write, cross-region via Transit Gateway).
- Copy each matching SubVolume's subdirectory from source to destination using `CopyDir`.
- Track replication progress, RPO, bytes transferred, and consecutive failures in `ReplicationPolicy.Status`.
- Expose Prometheus metrics for replication lag, RPO, and error counts.

**Does NOT:**
- Create or delete VPC file shares (destination pool must already exist).
- Interfere with the CSI controller, node agent, or pool manager.
- Provide crash consistency or cross-file transactional consistency (file-level consistency only).

### 7. Migration CLI

Utility package (`pkg/migrate/`) for migrating PVCs from the stock IBM VPC file CSI driver to the pool-based driver.

**Components:**
- **Planner** (`planner.go`) — analyzes existing PVCs and generates a migration plan.
- **Executor** (`executor.go`) — executes the migration plan: creates SubVolume CRs, spawns data-copy pods, and rebinds PVCs.
- **Pod** (`pod.go`) — manages ephemeral pods used for data transfer during migration.

### 8. CRD Controllers

Five CRD types, managed by controller-runtime:

- **FileSharePool** — defines a pool of VPC file shares; reconciler ensures the pool has enough capacity.
- **SubVolume** — tracks a single PVC's allocation: which share, subdirectory path, requested size, and clone/snapshot state.
- **Snapshot** — tracks a point-in-time directory copy of a SubVolume, stored under `/pvcs/.snapshots/`.
- **VolumeGroupSnapshot** — coordinates multiple Snapshot CRs for multi-PVC consistent snapshots, with optional quiesce hooks.
- **ReplicationPolicy** — defines a cross-region replication relationship between a source pool and a destination pool.

See `CRD-SPEC.md` for full type definitions.

## Data Flow: CreateVolume

```
1. User creates PVC with StorageClass "ibm-vpc-file-pool"
       │
2. csi-provisioner sidecar calls CreateVolume gRPC
       │
3. CSI Controller receives CreateVolume
       │
       ├─ Extract pool name from StorageClass parameters
       ├─ Extract requested size from PVC spec
       │
4. Call PoolManager.Allocate(ctx, request)
       │
       ├─ 4a. Read FileSharePool CR for the named pool
       ├─ 4b. Read all SubVolume CRs for that pool (or use cached state)
       ├─ 4c. Calculate per-share allocated capacity
       ├─ 4d. Pick a share with enough remaining capacity
       │       (strategy: spread or bin-pack, per pool config)
       │
       │   If no share has room:
       │       ├─ If pool.spec.autoExpand and shares < maxShares:
       │       │   Create new VPC file share via IBM VPC Client (SLOW: 30-90s)
       │       │   Wait for share to become "stable"
       │       │   Add to FileSharePool.Status.Shares
       │       │   Use this new share
       │       └─ Else: return codes.ResourceExhausted error (retriable)
       │
       ├─ 4e. Create SubVolume CR recording the allocation
       │
       └─ 4f. Return AllocationResult (share IP, subpath, uid, gid, permissions)
       │
       │   NOTE: Subdirectory creation is deferred to NodePublishVolume on the node agent.
       │   The controller does NOT create directories on the NFS share (nfsOps is nil).
       │
5. CSI Controller returns CreateVolumeResponse with:
       - volume_id: "{pool}/{share-id}/{subdir-name}"
       - volume_context:
           server: "10.240.1.5"                  # Primary (home zone) IP
           server.us-south-1: "10.240.1.5"       # Zone-keyed IPs (when accessorZones configured)
           server.us-south-2: "10.240.2.10"      # Accessor zone IP
           share: "/"
           subDir: "/pvcs/pvc-abc123"
           pool: "general-purpose"
           shareID: "r006-a1b2c3d4-..."
           permissions: "0755"   # optional
           uid: "1000"           # optional
           gid: "1000"           # optional
       - accessible_topology: [zone]
       │
6. Kubernetes creates PV, binds it to the PVC
       │
7. Pod scheduled → kubelet calls NodeStageVolume + NodePublishVolume
       │
8. Node agent mounts share (if not cached), creates subdirectory if it
       does not exist (with uid/gid/permissions from VolumeContext), then
       bind-mounts subdir into pod
```

## Data Flow: DeleteVolume

```
1. PVC deleted (reclaimPolicy: Delete)
       │
2. csi-provisioner calls DeleteVolume gRPC
       │
3. CSI Controller receives DeleteVolume
       │
       ├─ Parse volume_id to extract pool, share ID, subdir name
       │
4. Call PoolManager.Deallocate(ctx, subVolumeName)
       │
       ├─ 4a. Read SubVolume CR to get share details
       ├─ 4b. Update FileSharePool.Status (decrement allocated capacity)
       ├─ 4c. Delete SubVolume CR
       │
       │   NOTE: The controller does NOT remove subdirectories on the NFS share
       │   (nfsOps is nil in controller mode). Subdirectory cleanup is a future
       │   enhancement or handled by an external garbage collection process.
       │
5. CSI Controller returns success
```

## Data Flow: Pool Reconciliation (Background)

```
Every 60 seconds (or on FileSharePool CR change):

1. Reconciler reads FileSharePool CR
       │
2. For each share in Status.Shares:
       ├─ Call IBM VPC API to verify share health
       ├─ If share is "degraded" or "failed":
       │   Mark share as draining in Status
       │
3. Calculate total pool utilization:
       ├─ Sum all SubVolume allocations
       ├─ Compare to total pool capacity
       │
4. If utilization > expandThreshold:
       ├─ If shares.count < maxShares:
       │   Create new VPC file share (IBM VPC Client)
       │   Add to Status.Shares when share becomes "stable"
       └─ Else: emit Warning event ("pool at capacity")
       │
5. Update FileSharePool.Status with current metrics
       │
6. Emit Prometheus metrics:
       - pool_total_capacity_gb
       - pool_allocated_capacity_gb
       - pool_pvc_count
       - pool_share_count
       - share_allocated_capacity_gb (per share)
       - share_pvc_count (per share)
```

## Data Flow: CreateSnapshot

```
1. User creates VolumeSnapshot referencing a PVC
       │
2. csi-snapshotter sidecar calls CreateSnapshot gRPC
       │
3. CSI Controller receives CreateSnapshot
       │
       ├─ Parse source volume ID to extract pool, share ID, subdir name
       │
4. Call PoolManager.CreateSnapshot(ctx, snapshotName, sourceVolumeID, params)
       │
       ├─ 4a. Read source SubVolume CR
       ├─ 4b. Create snapshot directory: /pvcs/.snapshots/{snap-name}/
       ├─ 4c. cp -a from source subdir to snapshot directory
       ├─ 4d. Create Snapshot CR recording the snapshot metadata
       │
5. CSI Controller returns CreateSnapshotResponse
```

## Data Flow: CloneVolume

```
1. User creates PVC with dataSource referencing an existing PVC
       │
2. csi-provisioner detects dataSource, sets VolumeContentSource
       │
3. CSI Controller receives CreateVolume with VolumeContentSource.Volume
       │
4. Call PoolManager.CloneVolume(ctx, sourceVolumeID, req, syncThresholdGB)
       │
       ├─ Source size <= syncThreshold?
       │
       ├─ YES (synchronous): cp -a during call, return with cloneStatus=Complete
       │
       └─ NO (asynchronous):
           ├─ Create SubVolume CR with cloneStatus=Pending
           ├─ Return immediately (pod will wait)
           │
           │  (background Clone Worker)
           ├─ Update CR: cloneStatus=InProgress
           ├─ cp -a from source to target
           └─ Update CR: cloneStatus=Complete or Failed
       │
5. NodePublishVolume gates pod access on cloneStatus=Complete
       (kubelet retries with backoff until clone finishes)
```

## Volume ID Format

The CSI volume ID encodes everything needed to locate the subvolume:

```
Format: {pool-name}/{share-vpc-id}/{subdir-name}

Example: general-purpose/r006-a1b2c3d4-5678-90ab-cdef-1234567890ab/pvc-abc123

Parsing:
  pool-name: general-purpose
  share-vpc-id: r006-a1b2c3d4-5678-90ab-cdef-1234567890ab
  subdir-name: pvc-abc123
```

This format is:
- Deterministic (same inputs → same ID)
- Parseable without external lookups
- Human-readable in `kubectl get pv` output

## Security Model

### RBAC
The controller ServiceAccount needs:
- FileSharePool, SubVolume: full CRUD
- PersistentVolumes: get, list, watch, create, update, patch, delete (CSI provisioner creates PVs)
- PersistentVolumeClaims: get, list, watch, update, patch
- PersistentVolumeClaims/status: patch
- Nodes: get, list, watch (for topology)
- Secrets: get, list, watch
- ConfigMaps: get, list, watch
- StorageClasses: get, list, watch
- Events: create, patch
- Leases: get, create, update (for leader election)
- CSINode: get, list, watch

### Secrets
- On managed clusters (ROKS/IKS), authentication is provided by the `storage-secret-sidecar` which populates the `storage-secret-store` secret. No standalone API key secret is needed.
- On self-managed clusters, an IBM Cloud API key is stored in a Kubernetes Secret (`ibm-cloud-credentials`).
- Never logged, never included in CRD status or events.

### NFS Security
- Shares use VPC security group rules (TCP 2049).
- Mount options: `nfsvers=4.1,sec=sys` (or `sec=krb5p` if encryption-in-transit is enabled).
- Subdirectories created with specific UID/GID/permissions per StorageClass config.
- Node agent validates all paths before mount/unmount to prevent traversal attacks.

### Pod Isolation
- Pods get a bind-mount of their specific subdirectory, NOT the whole share.
- A pod cannot traverse above its subdirectory because the bind-mount is the filesystem root from the pod's perspective.

## Failure Modes

| Failure | Impact | Recovery |
|---------|--------|----------|
| Controller pod crash | New PVCs queue; existing PVCs unaffected | Pod restarts; leader election transfers |
| Node agent crash | New pod mounts fail on that node | DaemonSet restarts; existing mounts survive (kernel-level) |
| VPC file share unavailable | All PVCs on that share lose access | IBM HA (99.999%); pool manager marks share as draining |
| IBM VPC API down | No new shares can be created; existing PVCs unaffected | Pool manager retries with backoff; allocations to existing shares still work |
| SubVolume CR accidentally deleted | Allocation tracking lost for that PVC | Data still on share; operator can recreate CR from share contents |
| FileSharePool CR deleted | Pool manager stops reconciling | Don't delete it; use finalizers to prevent accidental deletion |
| Disk full on a share | Writes fail for all PVCs on that share | Monitoring alerts; auto-expand or manual expand |
