# Volume Cloning Design (Phase 4b)

**Status:** Implemented (v0.4.0)

## Overview

Volume cloning creates a new SubVolume that is a copy of an existing one. In the pool model, a SubVolume is a subdirectory under `/pvcs/pvc-{uuid}` on a shared VPC NFS file share. Cloning is therefore a directory copy (`cp -a`) from one subdirectory to another.

Unlike snapshots (Phase 4a), which are immutable point-in-time copies stored under `/pvcs/.snapshots/`, a clone is a fully writable, independent SubVolume. The clone has its own PVC, PV, and SubVolume CR. After creation, the clone and source are completely independent -- writes to one do not affect the other.

Key characteristics:

- **Full data copy** -- NFS has no copy-on-write. Every byte of the source is copied to the clone.
- **Not instant** -- Copy time is proportional to data size. A 100 GB subdirectory takes minutes.
- **No crash consistency guarantee** if the source is being actively written to.
- **Same-share or cross-share** -- The clone can land on the same VPC file share as the source or a different share in the pool.

```
VPC File Share (pool-1-a)
  /pvcs/
    /pvc-aaaa (source, 50 GB)  ──cp -a──>  /pvc-bbbb (clone, 50 GB)
    /pvc-cccc
    /pvc-dddd

  OR cross-share:

VPC File Share (pool-1-a)               VPC File Share (pool-1-b)
  /pvcs/                                  /pvcs/
    /pvc-aaaa (source) ──cp via NFS──>      /pvc-bbbb (clone)
    /pvc-cccc                               /pvc-eeee
```

---

## CSI Interface

The CSI specification defines volume cloning through `CreateVolume` with a `volume_content_source` of type `VOLUME`. No separate RPC exists for cloning.

### Request Flow

```
1. User creates PVC with dataSource referencing an existing PVC
       │
       ▼
2. csi-provisioner detects dataSource, sets volume_content_source
       │
       ▼
3. CSI Controller receives CreateVolume with:
       - req.Name = new PV name (pvc-bbbb)
       - req.VolumeContentSource.Type = VOLUME
       - req.VolumeContentSource.Volume.VolumeId = "pool/share-id/pvc-aaaa"
       - req.CapacityRange.RequiredBytes >= source size
       │
       ▼
4. CSI Controller delegates to PoolManager.CloneVolume()
       │
       ▼
5. PoolManager:
       - Validates source SubVolume exists
       - Selects target share (prefer same share)
       - Creates target SubVolume CR with clone tracking fields
       - Initiates data copy (sync or async based on size)
       - Returns AllocationResult
       │
       ▼
6. CSI Controller returns CreateVolumeResponse with content_source set
```

### PVC Definition (User-Facing)

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-clone
  namespace: default
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ibm-vpc-file-pool
  resources:
    requests:
      storage: 50Gi          # Must be >= source PVC size
  dataSource:
    kind: PersistentVolumeClaim
    name: my-original-pvc     # Source PVC in the same namespace
```

### CSI CreateVolume Request (from csi-provisioner)

The provisioner translates the PVC `dataSource` into a `VolumeContentSource`:

```go
req := &csi.CreateVolumeRequest{
    Name: "pvc-bbbb-uuid",
    VolumeContentSource: &csi.VolumeContentSource{
        Type: &csi.VolumeContentSource_Volume{
            Volume: &csi.VolumeContentSource_VolumeSource{
                VolumeId: "general-purpose/r006-xxxx/pvc-aaaa-uuid",
            },
        },
    },
    CapacityRange: &csi.CapacityRange{
        RequiredBytes: 50 * (1 << 30), // 50 GB
    },
    Parameters: map[string]string{
        "pool": "general-purpose",
    },
}
```

### CSI CreateVolume Response

```go
resp := &csi.CreateVolumeResponse{
    Volume: &csi.Volume{
        VolumeId:      "general-purpose/r006-xxxx/pvc-bbbb-uuid",
        CapacityBytes: 50 * (1 << 30),
        VolumeContext: map[string]string{
            "server":  "10.240.1.5",
            "share":   "/",
            "subDir":  "/pvcs/pvc-bbbb-uuid",
            "pool":    "general-purpose",
            "shareID": "r006-xxxx",
        },
        ContentSource: &csi.VolumeContentSource{
            Type: &csi.VolumeContentSource_Volume{
                Volume: &csi.VolumeContentSource_VolumeSource{
                    VolumeId: "general-purpose/r006-xxxx/pvc-aaaa-uuid",
                },
            },
        },
    },
}
```

---

## Consistency Guarantees

Volume cloning copies data via `cp -a` over NFS. The consistency of the clone depends entirely on what the source is doing during the copy.

### Source Quiesced (Not Being Written To)

Full consistency. The clone is a byte-for-byte copy of the source. This is the recommended approach for any workload where consistency matters.

**Best practice:** Scale down the application or pause writes before cloning.

### Source Actively Written To (General Files)

File-level consistency only. NFS close-to-open semantics mean each individual file is internally consistent (the copy sees a complete version of each file), but there are no cross-file guarantees. If a write modifies files A and B, the clone might capture the new A and old B.

This is the same consistency level as `rsync` against a live NFS mount.

**Acceptable for:** Log files, static assets, web content, data that tolerates minor inconsistency.

### VM Disk Images

**NOT safe to clone while the VM is running.** VM disk images (QCOW2, VMDK, raw) are single large files with internal structure. `cp` of a file being actively written to can produce an internally inconsistent disk image -- partial block writes, torn metadata, corrupted filesystem structures.

```
  WARNING: Cloning a SubVolume containing a running VM's disk image
  will almost certainly produce an unusable clone. Stop the VM first.

  Safe:    Stop VM  -->  Clone  -->  Start VM on clone
  Unsafe:  Clone while VM running  -->  Corrupted disk image
```

### Consistency Matrix

| Source State | Consistency | Safe? |
|-------------|-------------|-------|
| Quiesced (no writes) | Full (byte-for-byte) | Yes |
| Active writes (many small files) | File-level (NFS close-to-open) | Depends on workload |
| Active writes (single large file, e.g., DB) | None (partial file state) | No |
| Running VM disk image | None (intra-file inconsistency) | No |

---

## Architecture

The core challenge is that `cp -a` of a large SubVolume can take minutes, but the CSI `CreateVolume` call should not block the provisioner indefinitely. The design uses a dual-path approach: synchronous for small volumes, asynchronous for large ones.

### Sync vs Async Decision

```
CreateVolume with VolumeContentSource (clone)
       │
       ├── Source size <= syncThreshold (default 10 GB)?
       │       │
       │       ├── YES: Synchronous path
       │       │   - cp -a during CreateVolume
       │       │   - Block until copy completes
       │       │   - Return success with clone ready
       │       │
       │       └── NO: Asynchronous path
       │           - Create SubVolume CR with cloneStatus=Pending
       │           - Return success immediately
       │           - Background goroutine performs cp -a
       │           - SubVolume CR updated: Pending -> InProgress -> Complete
       │           - NodeStageVolume waits for cloneStatus=Complete
       │
       └── configurable via StorageClass parameter: cloneSyncThresholdGB
```

The threshold is configurable per StorageClass:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ibm-vpc-file-pool
provisioner: vpc-file-pool.csi.ibm.io
parameters:
  pool: general-purpose
  cloneSyncThresholdGB: "10"    # Volumes <= 10 GB clone synchronously (default)
```

### Synchronous Path (Small Volumes)

For SubVolumes at or below the threshold, cloning happens inline during `CreateVolume`:

```
CreateVolume (clone, source <= 10 GB)
  │
  ├── 1. Validate source SubVolume exists
  ├── 2. Select target share
  ├── 3. cp -a from source subdir to target subdir
  │      (blocks for seconds to ~1 minute)
  ├── 4. Create SubVolume CR (phase=Bound, cloneStatus=Complete)
  ├── 5. Update pool capacity tracking
  └── 6. Return CreateVolumeResponse
```

The synchronous path is simple and predictable. The csi-provisioner has a configurable timeout (default 300s in our deployment), which is sufficient for copies up to ~10 GB on VPC file shares (~100-200 MB/s throughput).

### Asynchronous Path (Large Volumes)

For SubVolumes above the threshold, `CreateVolume` returns immediately and the copy runs in a background goroutine:

```
CreateVolume (clone, source > 10 GB)
  │
  ├── 1. Validate source SubVolume exists
  ├── 2. Select target share
  ├── 3. Create SubVolume CR (phase=Creating, cloneStatus=Pending)
  ├── 4. Update pool capacity tracking
  ├── 5. Launch background clone goroutine
  └── 6. Return CreateVolumeResponse immediately
         │
         │  (background)
         │
         ├── 7. Update SubVolume CR: cloneStatus=InProgress
         ├── 8. cp -a from source to target
         │      (may take minutes for hundreds of GB)
         ├── 9. On success:
         │      SubVolume CR: cloneStatus=Complete, phase=Bound
         └── 10. On failure:
                SubVolume CR: cloneStatus=Failed, phase=Failed
                Clean up partial target directory

  Later, when pod is scheduled:

NodeStageVolume (standard NFS mount)
  │
NodePublishVolume
  │
  ├── 1. Read SubVolume CR cloneStatus
  ├── 2. If cloneStatus != Complete:
  │      Return codes.Unavailable ("clone in progress, retry")
  │      (kubelet retries with backoff)
  └── 3. If cloneStatus == Complete:
         Bind-mount subdirectory into pod (normal path)
```

The node agent gates pod access on clone completion. Kubelet retries `NodePublishVolume` with exponential backoff, so the pod simply waits until the clone finishes. This is the same pattern used by CSI drivers that support asynchronous volume provisioning.

### Architecture Diagram

```
┌──────────────────────────────────────────────────────────────────────┐
│                          CSI Controller                               │
│                                                                       │
│   CreateVolume(VolumeContentSource=VOLUME)                           │
│       │                                                               │
│       ├── Parse source volume ID                                     │
│       ├── Fetch source SubVolume CR                                  │
│       │                                                               │
│       ├── source.RequestedGB <= syncThreshold?                       │
│       │       │                                                       │
│       │   ┌───┴───┐                                                  │
│       │   │ YES   │  Sync path: cp -a, create CR, return            │
│       │   └───────┘                                                  │
│       │       │                                                       │
│       │   ┌───┴───┐                                                  │
│       │   │  NO   │  Async path: create CR (Pending), return        │
│       │   └───┬───┘                                                  │
│       │       │                                                       │
│       │       ▼                                                       │
│       │   Background Clone Worker                                    │
│       │       │                                                       │
│       │       ├── Update CR: cloneStatus=InProgress                  │
│       │       ├── cp -a source → target                              │
│       │       └── Update CR: cloneStatus=Complete or Failed          │
│       │                                                               │
└───────┼───────────────────────────────────────────────────────────────┘
        │
        ▼
┌──────────────────────────────────────────────────────────────────────┐
│                          CSI Node Agent                               │
│                                                                       │
│   NodePublishVolume                                                  │
│       │                                                               │
│       ├── Fetch SubVolume CR                                         │
│       ├── cloneStatus == Complete?                                   │
│       │       │                                                       │
│       │   ┌───┴───┐                                                  │
│       │   │ YES   │  Normal bind-mount path                         │
│       │   └───────┘                                                  │
│       │       │                                                       │
│       │   ┌───┴───┐                                                  │
│       │   │  NO   │  Return codes.Unavailable (kubelet retries)     │
│       │   └───────┘                                                  │
│       │                                                               │
└───────────────────────────────────────────────────────────────────────┘
```

---

## CRD Changes

### SubVolume Spec Additions

```go
type SubVolumeSpec struct {
    // ... existing fields ...

    // SourceVolume is the name of the source SubVolume this was cloned from.
    // Empty for non-clone SubVolumes.
    // +optional
    SourceVolume string `json:"sourceVolume,omitempty"`

    // SourceShareID is the VPC file share ID of the source SubVolume.
    // Populated when the clone source is on a different share than the target.
    // +optional
    SourceShareID string `json:"sourceShareID,omitempty"`
}
```

### SubVolume Status Additions

```go
type SubVolumeStatus struct {
    // ... existing fields ...

    // CloneStatus tracks the progress of an asynchronous clone operation.
    // Empty for non-clone SubVolumes and completed synchronous clones.
    // +kubebuilder:validation:Enum=Pending;InProgress;Complete;Failed
    // +optional
    CloneStatus string `json:"cloneStatus,omitempty"`

    // CloneProgress tracks bytes copied during a clone operation.
    // +optional
    CloneProgress *CloneProgress `json:"cloneProgress,omitempty"`
}

// CloneProgress tracks the data copy progress for a clone operation.
type CloneProgress struct {
    // BytesCopied is the number of bytes copied so far.
    BytesCopied int64 `json:"bytesCopied"`

    // TotalBytes is the total size of the source data to copy.
    TotalBytes int64 `json:"totalBytes"`

    // StartedAt is when the copy operation started.
    // +optional
    StartedAt *metav1.Time `json:"startedAt,omitempty"`

    // CompletedAt is when the copy operation finished (success or failure).
    // +optional
    CompletedAt *metav1.Time `json:"completedAt,omitempty"`

    // Error records the failure reason if cloneStatus is Failed.
    // +optional
    Error string `json:"error,omitempty"`
}
```

### SubVolume Phase Updates

The existing `Phase` enum needs the `Cloning` value for the async path:

```go
// +kubebuilder:validation:Enum=Creating;Cloning;Bound;Expanding;Deleting;Retained;Archived;Failed
Phase string `json:"phase,omitempty"`
```

### Example SubVolume CR (Clone, Async In-Progress)

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: SubVolume
metadata:
  name: pvc-bbbb-uuid
  labels:
    storage.ibmcloud.io/pool: general-purpose
    storage.ibmcloud.io/share-id: r006-xxxx-1
    storage.ibmcloud.io/clone-source: pvc-aaaa-uuid
spec:
  poolName: general-purpose
  shareID: r006-xxxx-1
  shareMountTargetIP: "10.240.1.5"
  subPath: /pvcs/pvc-bbbb-uuid
  requestedGB: 50
  pvName: pvc-bbbb-uuid
  pvcName: my-clone
  pvcNamespace: default
  sourceVolume: pvc-aaaa-uuid
  reclaimPolicy: Delete
status:
  phase: Cloning
  cloneStatus: InProgress
  cloneProgress:
    bytesCopied: 21474836480     # 20 GB copied so far
    totalBytes: 53687091200      # 50 GB total
    startedAt: "2026-02-16T10:00:00Z"
```

### Example SubVolume CR (Clone, Complete)

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: SubVolume
metadata:
  name: pvc-bbbb-uuid
  labels:
    storage.ibmcloud.io/pool: general-purpose
    storage.ibmcloud.io/share-id: r006-xxxx-1
spec:
  poolName: general-purpose
  shareID: r006-xxxx-1
  shareMountTargetIP: "10.240.1.5"
  subPath: /pvcs/pvc-bbbb-uuid
  requestedGB: 50
  pvName: pvc-bbbb-uuid
  pvcName: my-clone
  pvcNamespace: default
  sourceVolume: pvc-aaaa-uuid
  reclaimPolicy: Delete
status:
  phase: Bound
  cloneStatus: Complete
  cloneProgress:
    bytesCopied: 53687091200
    totalBytes: 53687091200
    startedAt: "2026-02-16T10:00:00Z"
    completedAt: "2026-02-16T10:04:30Z"
  createdAt: "2026-02-16T10:00:00Z"
```

---

## Same-Share vs Cross-Share Cloning

A clone can be placed on the same VPC file share as the source or on a different share in the pool. Both have tradeoffs.

### Same-Share Clone

```
VPC File Share (pool-1-a)
  /pvcs/
    /pvc-aaaa (source)  ──cp -a──>  /pvc-bbbb (clone)
```

**Advantages:**
- Faster copy. Data stays on the same NFS server; no network transfer between shares.
- Simpler implementation. Both source and target are under the same NFS mount.
- The node agent already has the share mounted if the source is in use.

**Disadvantages:**
- Consumes capacity on the same share. A 50 GB clone of a 50 GB source requires 100 GB on that share.
- Increases blast radius. Source and clone share the same NFS server failure domain.
- May conflict with the pool's allocation strategy (e.g., spread wants to distribute across shares).

### Cross-Share Clone

```
VPC File Share (pool-1-a)               VPC File Share (pool-1-b)
  /pvcs/                                  /pvcs/
    /pvc-aaaa (source)  ──cp via NFS──>     /pvc-bbbb (clone)
```

**Advantages:**
- Better capacity distribution. The clone uses free space on a different share.
- Lower blast radius. Source and clone on different NFS servers.
- Consistent with spread allocation strategy.

**Disadvantages:**
- Slower copy. Data must traverse the NFS client, flow through the node's network stack, and write to a different NFS server.
- Requires both shares to be mounted on the controller node (or whichever node performs the copy).
- More complex implementation. The controller must handle mounting two shares and copying between them.

### Design Decision

The clone operation uses the **standard share selection logic** (same as a normal `Allocate`), with a **preference for the source's share** when it has sufficient capacity. This means:

1. If the source share has enough free space, the clone goes there (fast, local copy).
2. If the source share is full, the clone goes to another share selected by the pool's allocation strategy (spread or binpack).
3. The user cannot force same-share or cross-share placement -- it is an automatic decision based on capacity.

This approach is consistent with how the pool model works for all allocations: the pool manager picks the best share, and the user does not need to care which share their data lands on.

---

## Share Selection for Clones

The clone share selection algorithm extends the existing `selectShare` function with a same-share preference:

```
selectShareForClone(strategy, shares, requestedGB, tier, sourceShareID):
  │
  ├── 1. Check if source share has capacity
  │      source = shares[sourceShareID]
  │      if source.State == "stable" && source.FreeGB >= requestedGB:
  │          return source   (same-share clone, fastest)
  │
  ├── 2. Fall back to normal selection
  │      return selectShare(strategy, shares, requestedGB, tier)
  │      (may return a different share -- cross-share clone)
  │
  └── 3. If all shares full:
         Auto-expand if allowed, else return ErrPoolExhausted
```

### Cross-Share Copy Mechanics

When the clone lands on a different share than the source, the controller must mount both shares to perform the copy. The controller node agent approach:

```
Controller (has NFS access via nfsOps):
  │
  ├── Source share already mounted at:
  │     /staging/{source-share-id}/pvcs/pvc-aaaa
  │
  ├── Target share mounted at:
  │     /staging/{target-share-id}/pvcs/pvc-bbbb
  │
  └── cp -a /staging/{source-share-id}/pvcs/pvc-aaaa  \
            /staging/{target-share-id}/pvcs/pvc-bbbb
```

If the controller does not have both shares mounted (controller mode runs without `nfsOps`), the cross-share copy must be deferred to a node agent that can mount both. This is handled by:

1. The controller creates the SubVolume CR with `cloneStatus=Pending` and both `sourceShareID` and `shareID` set.
2. A clone worker (running as part of the pool reconciler or a dedicated DaemonSet pod) picks up the pending clone, mounts both shares, performs the copy, and updates the CR.

For the initial implementation, cross-share cloning uses the same async path as large-volume cloning: the controller creates the CR, and the reconciler background loop performs the copy on the next reconcile cycle.

---

## Controller Capabilities

Add `CLONE_VOLUME` to the controller capabilities:

```go
func (d *Driver) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
    return &csi.ControllerGetCapabilitiesResponse{
        Capabilities: []*csi.ControllerServiceCapability{
            newControllerCap(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
            newControllerCap(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME),
            newControllerCap(csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT),
            newControllerCap(csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS),
            newControllerCap(csi.ControllerServiceCapability_RPC_CLONE_VOLUME),  // NEW
        },
    }, nil
}
```

This capability tells the csi-provisioner that the driver supports volume cloning via `CreateVolume` with `VolumeContentSource` of type `VOLUME`.

---

## Pool Manager Interface Changes

Add a `CloneVolume` method to `PoolManager`:

```go
type PoolManager interface {
    // ... existing methods ...

    // CloneVolume creates a new SubVolume that is a copy of an existing one.
    // For small volumes (below syncThreshold), the copy completes before returning.
    // For large volumes, the copy runs asynchronously -- the SubVolume CR is
    // created with cloneStatus=Pending and the background worker completes the copy.
    CloneVolume(ctx context.Context, sourceVolumeID string, req AllocationRequest, syncThresholdGB int64) (*AllocationResult, error)
}
```

### CloneVolume Implementation (Pseudocode)

```go
func (m *Manager) CloneVolume(ctx context.Context, sourceVolumeID string, req AllocationRequest, syncThresholdGB int64) (*AllocationResult, error) {
    m.mu.Lock()
    defer m.mu.Unlock()

    // 1. Parse source volume ID
    _, _, pvName, err := parseManagerVolumeID(sourceVolumeID)
    if err != nil {
        return nil, fmt.Errorf("invalid source volume ID: %w", err)
    }

    // 2. Fetch source SubVolume CR
    sourceSV, err := m.k8sClient.GetSubVolume(ctx, pvName)
    if err != nil {
        return nil, fmt.Errorf("%w: %s", ErrSourceNotFound, pvName)
    }

    // 3. Validate: clone size must be >= source size
    if req.RequestedGB < sourceSV.Spec.RequestedGB {
        return nil, fmt.Errorf("clone size (%d GB) must be >= source size (%d GB)",
            req.RequestedGB, sourceSV.Spec.RequestedGB)
    }

    // 4. Fetch pool and select target share (prefer source share)
    pool, err := m.k8sClient.GetFileSharePool(ctx, req.PoolName)
    if err != nil {
        return nil, fmt.Errorf("%w: %s", ErrPoolNotFound, req.PoolName)
    }

    share, err := selectShareForClone(
        pool.Spec.AllocationStrategy, pool.Status.Shares,
        req.RequestedGB, req.Tier, sourceSV.Spec.ShareID,
    )
    if err != nil && errors.Is(err, ErrPoolExhausted) {
        share, err = m.tryAutoExpand(ctx, pool, req.RequestedGB, req.Tier)
        if err != nil {
            return nil, err
        }
    } else if err != nil {
        return nil, err
    }

    // 5. Determine sync vs async
    isSameShare := share.ShareID == sourceSV.Spec.ShareID
    isSmallEnough := sourceSV.Spec.RequestedGB <= syncThresholdGB
    doSync := isSameShare && isSmallEnough && m.nfsOps != nil

    subPath := fmt.Sprintf("/pvcs/%s", req.PVName)

    if doSync {
        // 6a. Synchronous clone
        srcPath, _ := util.SafeJoin(m.stagingBasePath, sourceSV.Spec.SubPath)
        dstPath, _ := util.SafeJoin(m.stagingBasePath, subPath)
        if err := m.nfsOps.CopyDir(srcPath, dstPath); err != nil {
            return nil, fmt.Errorf("clone copy: %w", err)
        }
        // Create SubVolume CR with cloneStatus=Complete
        // ...
    } else {
        // 6b. Asynchronous clone
        // Create SubVolume CR with cloneStatus=Pending
        // Launch background worker
        // ...
    }

    // 7. Update pool capacity tracking
    // 8. Return AllocationResult
}
```

---

## CSI Controller Changes

The `CreateVolume` method is extended to handle `VolumeContentSource` of type `VOLUME`:

```go
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
    // ... existing validation ...

    // Check for content source
    if req.GetVolumeContentSource() != nil {
        if snapSource := req.GetVolumeContentSource().GetSnapshot(); snapSource != nil {
            return d.createVolumeFromSnapshot(ctx, req, snapSource.GetSnapshotId(), poolName, requiredGB, zone)
        }
        if volSource := req.GetVolumeContentSource().GetVolume(); volSource != nil {
            return d.createVolumeFromClone(ctx, req, volSource.GetVolumeId(), poolName, requiredGB, zone)
        }
    }

    // ... normal allocation path ...
}

func (d *Driver) createVolumeFromClone(ctx context.Context, req *csi.CreateVolumeRequest, sourceVolumeID, poolName string, requiredGB int64, zone string) (*csi.CreateVolumeResponse, error) {
    params := req.GetParameters()

    syncThreshold := int64(10) // default 10 GB
    if t := params["cloneSyncThresholdGB"]; t != "" {
        if parsed, err := strconv.ParseInt(t, 10, 64); err == nil {
            syncThreshold = parsed
        }
    }

    allocReq := pool.AllocationRequest{
        PVName:       req.GetName(),
        PVCName:      params["csi.storage.k8s.io/pvc/name"],
        PVCNamespace: params["csi.storage.k8s.io/pvc/namespace"],
        PoolName:     poolName,
        RequestedGB:  requiredGB,
        Zone:         zone,
        Tier:         params["tier"],
        UID:          parseOptionalInt64(params["uid"]),
        GID:          parseOptionalInt64(params["gid"]),
        Permissions:  params["permissions"],
    }

    result, err := d.poolManager.CloneVolume(ctx, sourceVolumeID, allocReq, syncThreshold)
    if err != nil {
        switch {
        case errors.Is(err, pool.ErrSourceNotFound):
            return nil, status.Errorf(codes.NotFound, "source volume not found")
        case errors.Is(err, pool.ErrPoolExhausted):
            return nil, status.Errorf(codes.ResourceExhausted, "pool %q has no available capacity", poolName)
        default:
            return nil, status.Errorf(codes.Internal, "clone failed: %v", err)
        }
    }

    volumeID := fmt.Sprintf("%s/%s/%s", poolName, result.ShareID, req.GetName())

    return &csi.CreateVolumeResponse{
        Volume: &csi.Volume{
            VolumeId:      volumeID,
            CapacityBytes: requiredGB * (1 << 30),
            VolumeContext: buildVolumeContext(result, poolName),
            ContentSource: &csi.VolumeContentSource{
                Type: &csi.VolumeContentSource_Volume{
                    Volume: &csi.VolumeContentSource_VolumeSource{
                        VolumeId: sourceVolumeID,
                    },
                },
            },
        },
    }, nil
}
```

---

## Node Agent Changes

### NodePublishVolume Gate

The node agent must check clone status before allowing a pod to mount a cloned SubVolume:

```go
func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
    // ... existing path validation ...

    // Check if this is a clone that is still in progress
    if d.k8sClient != nil {
        _, _, pvName, err := parseVolumeID(req.GetVolumeId())
        if err == nil {
            sv, err := d.k8sClient.GetSubVolume(ctx, pvName)
            if err == nil && sv.Status.CloneStatus != "" && sv.Status.CloneStatus != "Complete" {
                return nil, status.Errorf(codes.Unavailable,
                    "clone is %s, not ready for mount (retry later)", sv.Status.CloneStatus)
            }
            if err == nil && sv.Status.CloneStatus == "Failed" {
                return nil, status.Errorf(codes.Internal,
                    "clone failed: %s", sv.Status.CloneProgress.Error)
            }
        }
    }

    // ... existing bind-mount logic ...
}
```

When `NodePublishVolume` returns `codes.Unavailable`, the kubelet retries with exponential backoff (starting at ~1s, capped at ~5 minutes). The pod stays in `ContainerCreating` until the clone completes.

---

## Background Clone Worker

The clone worker runs as part of the pool reconciler and processes SubVolumes with `cloneStatus=Pending` or `cloneStatus=InProgress` (for crash recovery):

```go
func (m *Manager) reconcileClones(ctx context.Context) error {
    // List SubVolumes with pending or in-progress clones
    svList, err := m.k8sClient.ListSubVolumesByCloneStatus(ctx, "Pending", "InProgress")
    if err != nil {
        return err
    }

    for _, sv := range svList {
        if sv.Status.CloneStatus == "Pending" || sv.Status.CloneStatus == "InProgress" {
            go m.performClone(ctx, &sv)
        }
    }
    return nil
}

func (m *Manager) performClone(ctx context.Context, sv *v1alpha1.SubVolume) {
    // 1. Update status to InProgress
    sv.Status.CloneStatus = "InProgress"
    now := metav1.Now()
    sv.Status.CloneProgress = &v1alpha1.CloneProgress{
        TotalBytes: sv.Spec.RequestedGB * (1 << 30),
        StartedAt:  &now,
    }
    m.k8sClient.UpdateSubVolumeStatus(ctx, sv)

    // 2. Mount source and target shares if needed

    // 3. Perform copy
    srcPath := m.resolvePath(sv.Spec.SourceShareID, sv.Spec.SourceVolume)
    dstPath := m.resolvePath(sv.Spec.ShareID, sv.Spec.SubPath)
    err := m.nfsOps.CopyDir(srcPath, dstPath)

    // 4. Update status
    completedAt := metav1.Now()
    if err != nil {
        sv.Status.CloneStatus = "Failed"
        sv.Status.Phase = "Failed"
        sv.Status.CloneProgress.Error = err.Error()
        sv.Status.CloneProgress.CompletedAt = &completedAt
    } else {
        sv.Status.CloneStatus = "Complete"
        sv.Status.Phase = "Bound"
        sv.Status.CloneProgress.BytesCopied = sv.Status.CloneProgress.TotalBytes
        sv.Status.CloneProgress.CompletedAt = &completedAt
    }
    m.k8sClient.UpdateSubVolumeStatus(ctx, sv)
}
```

### Clone Progress Tracking

For fine-grained progress tracking (optional enhancement), the clone worker can use `du` on the destination directory periodically to update `bytesCopied`. The initial implementation sets `bytesCopied = totalBytes` only on completion, which is sufficient for correctness.

### Crash Recovery

If the controller crashes during an async clone:

1. On restart, the reconciler lists SubVolumes with `cloneStatus=InProgress`.
2. It checks whether the target directory exists and has data.
3. If the target directory exists but the copy is incomplete, it removes the partial directory and restarts the copy from scratch. `cp -a` is not resumable, so starting over is the only safe option.
4. If the target directory does not exist, it restarts from `Pending`.

This is safe because:
- The SubVolume CR records the intent; the data copy is idempotent (remove and redo).
- The node agent will not mount the SubVolume until `cloneStatus=Complete`, so no pod sees partial data.

---

## Error Handling

### Source Not Found

```
Condition: Source volume ID references a SubVolume CR that does not exist.
Response:  codes.NotFound ("source volume not found")
Cause:     Source PVC was deleted, or volume ID is malformed.
Recovery:  User must recreate the PVC or fix the dataSource reference.
```

### Source on Unhealthy Share

```
Condition: Source SubVolume's share has state "degraded" or "draining".
Response:  codes.Unavailable ("source share is not healthy, retry later")
Cause:     The VPC file share is experiencing issues.
Recovery:  If degraded: wait for VPC to recover. If draining: clone is blocked
           (draining shares should not be sources for new operations).
```

### Insufficient Capacity

```
Condition: No share in the pool has enough free space for the clone.
Response:  codes.ResourceExhausted ("pool has no available capacity")
Cause:     Pool is full and auto-expand is disabled or maxShares reached.
Recovery:  Increase pool maxShares, or delete unused volumes to free space.
```

### Copy Failure Mid-Way (Async)

```
Condition: cp -a fails partway through (NFS error, disk full on share, etc.)
Response:  SubVolume CR updated: cloneStatus=Failed, cloneProgress.error set.
           NodePublishVolume returns codes.Internal with error message.
Cleanup:   Background worker removes the partial target directory.
           Pool capacity tracking is NOT decremented until the SubVolume CR
           is explicitly deleted (to prevent double-allocation of that space).
Recovery:  User deletes the failed clone PVC. DeleteVolume cleans up the CR
           and frees the capacity tracking. User can retry the clone.
```

### Clone Size Smaller Than Source

```
Condition: Requested clone size is smaller than source SubVolume size.
Response:  codes.InvalidArgument ("clone size must be >= source size")
Cause:     PVC spec requests less storage than the source PVC.
Recovery:  Increase the clone PVC's storage request to match or exceed source.
```

### Idempotency

Clone operations are idempotent, following the same pattern as `Allocate`:

1. Before starting a clone, check if a SubVolume CR with the target PV name already exists.
2. If it exists and `cloneStatus=Complete`, return the existing allocation (success).
3. If it exists and `cloneStatus=InProgress` or `Pending`, return the existing allocation (the background worker will complete it).
4. If it exists and `cloneStatus=Failed`, return an error (user must delete and retry).

---

## Metrics

New Prometheus metrics for clone operations:

```go
// Clone operation counters
pool_csi_clones_total{pool, status}           // Total clone operations (success/error)
pool_csi_clone_duration_seconds{pool}          // Time taken for synchronous clones
pool_csi_clone_async_duration_seconds{pool}    // Time taken for async clones (start to complete)

// Clone state gauges
pool_csi_clones_pending{pool}                  // Number of clones in Pending state
pool_csi_clones_in_progress{pool}              // Number of clones in InProgress state
pool_csi_clones_failed{pool}                   // Number of clones in Failed state
```

---

## Testing Strategy

### Unit Tests

**`pkg/pool/manager_test.go`** -- Clone allocation logic:

| Test Case | Description |
|-----------|-------------|
| `TestCloneVolume_SyncPath` | Clone of a small volume completes synchronously, SubVolume CR has `cloneStatus=Complete` |
| `TestCloneVolume_AsyncPath` | Clone of a large volume returns immediately, SubVolume CR has `cloneStatus=Pending` |
| `TestCloneVolume_SameShare` | Clone lands on the same share when source share has capacity |
| `TestCloneVolume_CrossShare` | Clone lands on a different share when source share is full |
| `TestCloneVolume_SourceNotFound` | Returns `ErrSourceNotFound` when source SubVolume does not exist |
| `TestCloneVolume_InsufficientCapacity` | Returns `ErrPoolExhausted` when no share has room |
| `TestCloneVolume_SizeTooSmall` | Returns error when requested size < source size |
| `TestCloneVolume_Idempotent` | Second call with same PV name returns existing SubVolume |
| `TestCloneVolume_CopyFailure` | Copy failure sets `cloneStatus=Failed` and cleans up |
| `TestCloneVolume_SourceOnDrainingShare` | Returns error when source is on a draining share |

**`pkg/driver/controller_test.go`** -- CSI handler tests:

| Test Case | Description |
|-----------|-------------|
| `TestCreateVolume_WithVolumeSource` | CreateVolume with `VolumeContentSource.Volume` delegates to CloneVolume |
| `TestCreateVolume_CloneNotFound` | Returns `codes.NotFound` when source does not exist |
| `TestCreateVolume_CloneExhausted` | Returns `codes.ResourceExhausted` when pool is full |
| `TestControllerGetCapabilities_IncludesClone` | Capabilities include `CLONE_VOLUME` |

**`pkg/driver/node_test.go`** -- Node publish gate:

| Test Case | Description |
|-----------|-------------|
| `TestNodePublishVolume_CloneInProgress` | Returns `codes.Unavailable` when clone is not complete |
| `TestNodePublishVolume_CloneComplete` | Normal bind-mount when clone is complete |
| `TestNodePublishVolume_CloneFailed` | Returns `codes.Internal` when clone has failed |
| `TestNodePublishVolume_NonClone` | Normal path when SubVolume has no clone status |

**`pkg/pool/clone_worker_test.go`** -- Background worker:

| Test Case | Description |
|-----------|-------------|
| `TestCloneWorker_CompletesSuccessfully` | Worker copies data and sets `cloneStatus=Complete` |
| `TestCloneWorker_CopyFailure` | Worker sets `cloneStatus=Failed` on copy error |
| `TestCloneWorker_CrashRecovery` | Worker restarts incomplete clone on reconcile |
| `TestCloneWorker_CleansUpPartialCopy` | Worker removes partial target directory on failure |

All tests use `FakeNFSOperations` and the fake K8s client. The `FakeNFSOperations.CopyDir` method simulates copy behavior (success, failure, partial) without actual filesystem operations.

### Integration Tests (E2E)

These require a live ROKS cluster with the driver deployed:

| Test Case | Description |
|-----------|-------------|
| `TestE2E_CloneSmallVolume` | Create PVC, write data, clone via dataSource, verify data in clone |
| `TestE2E_CloneLargeVolume` | Create PVC > threshold, clone, verify pod waits then mounts |
| `TestE2E_CloneAndModify` | Clone a PVC, modify data in clone, verify source is unchanged |
| `TestE2E_CloneExpand` | Clone with larger size than source, verify extra capacity available |
| `TestE2E_CloneDeleteSource` | Clone a PVC, delete source PVC, verify clone still works |
| `TestE2E_CloneCrossShare` | Fill source share, clone, verify clone lands on different share |

---

## Dependencies

Volume cloning is **independent of Phase 4a (snapshots)**. Cloning does not require snapshots as a prerequisite. The two features share some infrastructure:

| Component | Snapshots (4a) | Cloning (4b) | Shared? |
|-----------|---------------|--------------|---------|
| `NFSOperations.CopyDir` | Yes | Yes | Yes -- same `cp -a` mechanism |
| `SubVolume.Status.CloneStatus` | No | Yes | No -- new field |
| `Snapshot` CRD | Yes | No | No |
| Background worker | No (snapshots are sync) | Yes (for async clones) | No |
| `CLONE_VOLUME` capability | No | Yes | No |
| Node publish gate | No | Yes | No |
| Share selection preference | No | Yes (prefer source share) | No |

The following must exist before implementing cloning:

1. **`NFSOperations.CopyDir`** -- Already implemented in `pkg/pool/nfs.go` (used by snapshots). No changes needed.
2. **`SubVolume` CRD** -- Already exists. Needs new fields (`sourceVolume`, `cloneStatus`, `cloneProgress`).
3. **Pool Manager** -- Already exists. Needs the new `CloneVolume` method.
4. **CSI Controller** -- Already handles `VolumeContentSource` for snapshots. Needs the volume source branch.
5. **Node Agent** -- Already exists. Needs the clone status gate in `NodePublishVolume`.

### Implementation Order

```
1. CRD changes (SubVolume spec/status additions, run make generate)
2. Pool Manager: CloneVolume() method + selectShareForClone()
3. CSI Controller: createVolumeFromClone() handler
4. CSI Controller: Add CLONE_VOLUME capability
5. Node Agent: Clone status gate in NodePublishVolume
6. Background clone worker (for async path)
7. Unit tests for all components
8. E2E tests
```

---

## StorageClass Parameters

New parameters for clone behavior:

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `cloneSyncThresholdGB` | No | `10` | SubVolumes at or below this size (in GB) are cloned synchronously. Above this size, cloning is asynchronous. Set to `0` to force all clones async. Set to `32000` to force all clones sync (use with caution). |

---

## Limitations

1. **No crash consistency for active sources.** Cloning an actively-written SubVolume produces file-level consistency at best. There is no quiesce mechanism built into the driver. Applications must coordinate their own quiesce if consistency is required.

2. **Full data copy, not copy-on-write.** Every clone doubles the storage consumption. NFS does not support reflinks, snapshots, or any deduplication mechanism that would make cloning space-efficient.

3. **Async clones delay pod startup.** When a large SubVolume is cloned asynchronously, the pod referencing the clone PVC will stay in `ContainerCreating` until the copy completes. This can be minutes for large data sets.

4. **Cross-share clones are slower.** Data must traverse the network twice (NFS read from source share, NFS write to target share). Performance depends on VPC network bandwidth between the NFS servers and the node performing the copy.

5. **No progress visibility in PVC status.** Kubernetes PVC status does not expose driver-specific clone progress. Users must inspect the SubVolume CR directly (`kubectl get subvolumes`) to see `cloneProgress`.

6. **Clone of a clone is supported** but not optimized. The second clone copies all data from the first clone -- there is no chain or deduplication.
