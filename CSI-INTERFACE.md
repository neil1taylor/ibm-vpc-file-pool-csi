# CSI Interface Reference

## Driver Registration

```go
// Driver name — must be unique across all CSI drivers in the cluster.
// Follows the reverse-DNS convention.
const DriverName = "vpc-file-pool.csi.ibm.io"

// CSIDriver object (applied to cluster):
// config/deploy/csidriver.yaml
```

```yaml
apiVersion: storage.k8s.io/v1
kind: CSIDriver
metadata:
  name: vpc-file-pool.csi.ibm.io
spec:
  attachRequired: false          # NFS doesn't need ControllerPublish/Unpublish
  podInfoOnMount: true           # Passes pod name/namespace to NodePublishVolume
  volumeLifecycleModes:
    - Persistent
  fsGroupPolicy: File            # Let kubelet chown the mount to the pod's fsGroup
  storageCapacity: false         # We don't report per-node capacity (NFS is network storage)
```

## Identity Service

Minimal implementation. Every CSI driver must implement this.

```go
// pkg/driver/identity.go

func (d *Driver) GetPluginInfo(_ context.Context, _ *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
    return &csi.GetPluginInfoResponse{
        Name:          DriverName,
        VendorVersion: d.version,
    }, nil
}

func (d *Driver) GetPluginCapabilities(_ context.Context, _ *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
    return &csi.GetPluginCapabilitiesResponse{
        Capabilities: []*csi.PluginCapability{
            {
                Type: &csi.PluginCapability_Service_{
                    Service: &csi.PluginCapability_Service{
                        Type: csi.PluginCapability_Service_CONTROLLER_SERVICE,
                    },
                },
            },
            {
                Type: &csi.PluginCapability_Service_{
                    Service: &csi.PluginCapability_Service{
                        Type: csi.PluginCapability_Service_VOLUME_ACCESSIBILITY_CONSTRAINTS,
                    },
                },
            },
        },
    }, nil
}

func (d *Driver) Probe(_ context.Context, _ *csi.ProbeRequest) (*csi.ProbeResponse, error) {
    return &csi.ProbeResponse{}, nil
}
```

**Plugin capabilities:**

| Capability | Purpose |
|-----------|---------|
| `CONTROLLER_SERVICE` | Driver has a controller component (not node-only) |
| `VOLUME_ACCESSIBILITY_CONSTRAINTS` | Driver reports topology (zone) for volume placement |

---

## Controller Service

### Capabilities

```go
func (d *Driver) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
    return &csi.ControllerGetCapabilitiesResponse{
        Capabilities: []*csi.ControllerServiceCapability{
            newControllerCap(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
            newControllerCap(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME),
            newControllerCap(csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT),
            newControllerCap(csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS),
            newControllerCap(csi.ControllerServiceCapability_RPC_CLONE_VOLUME),
            // We do NOT support:
            // - PUBLISH_UNPUBLISH_VOLUME (no attach needed for NFS)
            // - LIST_VOLUMES (optional, not needed)
        },
    }, nil
}
```

**Advertised capabilities:**

| Capability | Purpose |
|-----------|---------|
| `CREATE_DELETE_VOLUME` | Standard PVC create/delete via SubVolume CRs |
| `EXPAND_VOLUME` | Online resize of SubVolume quota |
| `CREATE_DELETE_SNAPSHOT` | Directory-level snapshots of SubVolumes |
| `LIST_SNAPSHOTS` | Enumerate snapshots with filtering and pagination |
| `CLONE_VOLUME` | Create a new volume pre-populated from an existing volume |

### CreateVolume

This is the most critical method. It MUST be idempotent. Supports three creation paths: fresh allocation, restore from snapshot, and clone from existing volume.

```go
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
    // 1. Validate request
    if req.GetName() == "" {
        return nil, status.Error(codes.InvalidArgument, "volume name is required")
    }

    params := req.GetParameters()
    poolName := params["pool"]
    if poolName == "" {
        return nil, status.Error(codes.InvalidArgument, "StorageClass parameter 'pool' is required")
    }

    // 2. Extract requested size
    requiredBytes := req.GetCapacityRange().GetRequiredBytes()
    requiredGB := (requiredBytes + (1<<30 - 1)) / (1 << 30)
    if requiredGB < 1 {
        requiredGB = 1
    }

    // 3. Check for idempotency — does a SubVolume CR already exist for this volume name?
    if d.k8sClient != nil {
        existing, err := d.k8sClient.GetSubVolume(ctx, req.GetName())
        if err == nil && existing != nil {
            return &csi.CreateVolumeResponse{
                Volume: subVolumeToCSIVolume(existing),
            }, nil
        }
    }

    // 4. Extract topology preference (zone)
    zone := ""
    if req.GetAccessibilityRequirements() != nil {
        for _, topo := range req.GetAccessibilityRequirements().GetPreferred() {
            if z, ok := topo.GetSegments()["topology.kubernetes.io/zone"]; ok {
                zone = z
                break
            }
        }
    }

    // 5. Check for content source (snapshot restore or volume clone)
    if req.GetVolumeContentSource() != nil {
        if snapSource := req.GetVolumeContentSource().GetSnapshot(); snapSource != nil {
            return d.createVolumeFromSnapshot(ctx, req, snapSource.GetSnapshotId(), poolName, requiredGB, zone)
        }
        if volSource := req.GetVolumeContentSource().GetVolume(); volSource != nil {
            return d.createVolumeFromClone(ctx, req, volSource.GetVolumeId(), poolName, requiredGB, zone)
        }
    }

    // 6. Delegate to Pool Manager (fresh allocation)
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

    result, err := d.poolManager.Allocate(ctx, allocReq)
    if err != nil {
        switch {
        case errors.Is(err, pool.ErrPoolNotFound):
            return nil, status.Errorf(codes.NotFound, "pool %q not found", poolName)
        case errors.Is(err, pool.ErrPoolExhausted):
            return nil, status.Errorf(codes.ResourceExhausted, "pool %q has no available capacity", poolName)
        case errors.Is(err, pool.ErrShareCreationPending):
            return nil, status.Errorf(codes.Unavailable, "pool %q is expanding, retry shortly", poolName)
        default:
            return nil, status.Errorf(codes.Internal, "allocation failed: %v", err)
        }
    }

    // 7. Build volume ID and context
    volumeID := fmt.Sprintf("%s/%s/%s", poolName, result.ShareID, req.GetName())

    volCtx := map[string]string{
        "server":  result.MountTargetIP,           // NFS server IP or FQDN
        "share":   result.SharePath,               // NFS export path (e.g. "/share_abc123")
        "subDir":  result.SubPath,                 // Subdirectory path within the export
        "pool":    poolName,                       // Pool name
        "shareID": result.ShareID,                 // VPC share ID
    }
    if result.Permissions != "" {
        volCtx["permissions"] = result.Permissions
    }
    if result.UID != nil {
        volCtx["uid"] = strconv.FormatInt(*result.UID, 10)
    }
    if result.GID != nil {
        volCtx["gid"] = strconv.FormatInt(*result.GID, 10)
    }
    // Cross-zone support: add server.<zone> keys for each zone with a mount target.
    // The node agent selects the IP matching its own zone for NFS mounts.
    for z, ip := range result.MountTargets {
        volCtx["server."+z] = ip               // e.g., "server.us-south-1": "10.240.1.5"
    }

    // 8. Build accessible topology
    var topologies []*csi.Topology
    if len(result.AccessibleZones) > 0 {
        for _, z := range result.AccessibleZones {
            topologies = append(topologies, &csi.Topology{
                Segments: map[string]string{
                    "topology.kubernetes.io/zone": z,
                },
            })
        }
    } else {
        topologies = []*csi.Topology{
            {
                Segments: map[string]string{
                    "topology.kubernetes.io/zone": zone,
                },
            },
        }
    }

    return &csi.CreateVolumeResponse{
        Volume: &csi.Volume{
            VolumeId:           volumeID,
            CapacityBytes:      requiredGB * (1 << 30),
            VolumeContext:      volCtx,
            AccessibleTopology: topologies,
        },
    }, nil
}
```

#### CreateVolume from Snapshot (Restore)

When `VolumeContentSource` contains a snapshot reference, the controller delegates to `poolManager.RestoreSnapshot`. This copies the snapshot directory contents into a new SubVolume allocation. The response includes a `ContentSource` pointing back to the source snapshot.

```go
func (d *Driver) createVolumeFromSnapshot(ctx context.Context, req *csi.CreateVolumeRequest, snapshotID, poolName string, requiredGB int64, zone string) (*csi.CreateVolumeResponse, error) {
    _, _, snapshotName, err := parseVolumeID(snapshotID)
    if err != nil {
        return nil, status.Errorf(codes.InvalidArgument, "invalid snapshot ID: %v", err)
    }

    params := req.GetParameters()
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

    result, err := d.poolManager.RestoreSnapshot(ctx, snapshotName, allocReq)
    if err != nil {
        switch {
        case errors.Is(err, pool.ErrSnapshotNotFound):
            return nil, status.Errorf(codes.NotFound, "snapshot %q not found", snapshotName)
        case errors.Is(err, pool.ErrPoolExhausted):
            return nil, status.Errorf(codes.ResourceExhausted, "pool %q has no available capacity", poolName)
        default:
            return nil, status.Errorf(codes.Internal, "restore snapshot failed: %v", err)
        }
    }

    volumeID := fmt.Sprintf("%s/%s/%s", poolName, result.ShareID, req.GetName())
    // Response includes ContentSource referencing the snapshot
    return &csi.CreateVolumeResponse{
        Volume: &csi.Volume{
            VolumeId:      volumeID,
            CapacityBytes: requiredGB * (1 << 30),
            VolumeContext: map[string]string{...},
            ContentSource: &csi.VolumeContentSource{
                Type: &csi.VolumeContentSource_Snapshot{
                    Snapshot: &csi.VolumeContentSource_SnapshotSource{
                        SnapshotId: snapshotID,
                    },
                },
            },
        },
    }, nil
}
```

**Error mapping:**

| Pool Error | gRPC Code | Meaning |
|-----------|-----------|---------|
| `ErrSnapshotNotFound` | `NOT_FOUND` | Source snapshot does not exist |
| `ErrPoolExhausted` | `RESOURCE_EXHAUSTED` | No capacity in pool for the new volume |

#### CreateVolume from Clone

When `VolumeContentSource` contains a volume reference, the controller delegates to `poolManager.CloneVolume`. The clone behavior depends on the source volume size relative to a configurable threshold:

- **Small volumes** (below `cloneSyncThresholdGB`, default 10 GB): The directory copy completes synchronously before CreateVolume returns.
- **Large volumes** (at or above the threshold): The SubVolume CR is created with `cloneStatus=Pending` and a background worker copies the data asynchronously. The node agent gates the mount until the clone completes (see [NodePublishVolume Clone Gate](#nodepublishvolume-clone-gate)).

```go
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
    // Response includes ContentSource referencing the source volume
    return &csi.CreateVolumeResponse{
        Volume: &csi.Volume{
            VolumeId:      volumeID,
            CapacityBytes: requiredGB * (1 << 30),
            VolumeContext: map[string]string{...},
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

**StorageClass parameters for cloning:**

| Parameter | Default | Description |
|-----------|---------|-------------|
| `cloneSyncThresholdGB` | `10` | Volumes smaller than this are cloned synchronously; larger ones use the async background worker |

**Error mapping:**

| Pool Error | gRPC Code | Meaning |
|-----------|-----------|---------|
| `ErrSourceNotFound` | `NOT_FOUND` | Source volume does not exist |
| `ErrPoolExhausted` | `RESOURCE_EXHAUSTED` | No capacity in pool for the clone |

### DeleteVolume

```go
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
    // 1. Validate
    volumeID := req.GetVolumeId()
    if volumeID == "" {
        return nil, status.Error(codes.InvalidArgument, "volume ID is required")
    }

    // 2. Parse volume ID
    poolName, shareID, pvName, err := parseVolumeID(volumeID)
    if err != nil {
        return nil, status.Errorf(codes.InvalidArgument, "invalid volume ID: %v", err)
    }

    // 3. Delegate to Pool Manager
    err = d.poolManager.Deallocate(ctx, pvName)
    if err != nil {
        if errors.Is(err, pool.ErrSubVolumeNotFound) {
            // Already deleted — idempotent success
            return &csi.DeleteVolumeResponse{}, nil
        }
        return nil, status.Errorf(codes.Internal, "deallocation failed: %v", err)
    }

    return &csi.DeleteVolumeResponse{}, nil
}
```

### ControllerExpandVolume

```go
func (d *Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
    volumeID := req.GetVolumeId()
    if volumeID == "" {
        return nil, status.Error(codes.InvalidArgument, "volume ID is required")
    }

    _, _, pvName, err := parseVolumeID(volumeID)
    if err != nil {
        return nil, status.Errorf(codes.InvalidArgument, "invalid volume ID: %v", err)
    }

    requiredBytes := req.GetCapacityRange().GetRequiredBytes()
    requiredGB := (requiredBytes + (1<<30 - 1)) / (1 << 30)

    err = d.poolManager.Expand(ctx, pvName, requiredGB)
    if err != nil {
        if errors.Is(err, pool.ErrInsufficientShareCapacity) {
            return nil, status.Errorf(codes.ResourceExhausted, "share does not have enough remaining capacity")
        }
        return nil, status.Errorf(codes.Internal, "expand failed: %v", err)
    }

    return &csi.ControllerExpandVolumeResponse{
        CapacityBytes:         requiredGB * (1 << 30),
        NodeExpansionRequired: false, // No node-side action needed for NFS subdirs
    }, nil
}
```

### ValidateVolumeCapabilities

```go
func (d *Driver) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
    // NFS supports all access modes
    supported := []*csi.VolumeCapability_AccessMode{
        {Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
        {Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY},
        {Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY},
        {Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER},
        {Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
    }

    for _, cap := range req.GetVolumeCapabilities() {
        found := false
        for _, s := range supported {
            if cap.GetAccessMode().GetMode() == s.Mode {
                found = true
                break
            }
        }
        if !found {
            return &csi.ValidateVolumeCapabilitiesResponse{}, nil
        }
    }

    return &csi.ValidateVolumeCapabilitiesResponse{
        Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
            VolumeCapabilities: req.GetVolumeCapabilities(),
        },
    }, nil
}
```

### CreateSnapshot

Creates a directory-level copy of a SubVolume's data. The snapshot is stored as a separate directory on the same NFS share and tracked by a Snapshot CR in Kubernetes. The snapshot ID follows the same `{pool}/{share}/{name}` format as volume IDs.

```go
func (d *Driver) CreateSnapshot(ctx context.Context, req *csi.CreateSnapshotRequest) (*csi.CreateSnapshotResponse, error) {
    if req.GetName() == "" {
        return nil, status.Error(codes.InvalidArgument, "snapshot name is required")
    }
    if req.GetSourceVolumeId() == "" {
        return nil, status.Error(codes.InvalidArgument, "source volume ID is required")
    }

    result, err := d.poolManager.CreateSnapshot(ctx, req.GetName(), req.GetSourceVolumeId(), req.GetParameters())
    if err != nil {
        switch {
        case errors.Is(err, pool.ErrSourceNotFound):
            return nil, status.Errorf(codes.NotFound, "source volume not found")
        default:
            return nil, status.Errorf(codes.Internal, "create snapshot failed: %v", err)
        }
    }

    snapshotID := fmt.Sprintf("%s/%s/%s", result.PoolName, result.ShareID, result.SnapshotName)

    return &csi.CreateSnapshotResponse{
        Snapshot: &csi.Snapshot{
            SnapshotId:     snapshotID,
            SourceVolumeId: req.GetSourceVolumeId(),
            SizeBytes:      result.SizeBytes,
            CreationTime:   timestamppb.New(result.CreationTime),
            ReadyToUse:     result.ReadyToUse,
        },
    }, nil
}
```

**Snapshot ID format:** `{pool-name}/{share-vpc-id}/{snapshot-name}` -- reuses the same 3-part format as volume IDs.

**Error mapping:**

| Pool Error | gRPC Code | Meaning |
|-----------|-----------|---------|
| `ErrSourceNotFound` | `NOT_FOUND` | Source volume does not exist |

### DeleteSnapshot

Removes a snapshot directory from the NFS share and deletes its Snapshot CR. Idempotent -- returns success if the snapshot is already gone.

```go
func (d *Driver) DeleteSnapshot(ctx context.Context, req *csi.DeleteSnapshotRequest) (*csi.DeleteSnapshotResponse, error) {
    snapshotID := req.GetSnapshotId()
    if snapshotID == "" {
        return nil, status.Error(codes.InvalidArgument, "snapshot ID is required")
    }

    _, _, snapshotName, err := parseVolumeID(snapshotID)
    if err != nil {
        return nil, status.Errorf(codes.InvalidArgument, "invalid snapshot ID: %v", err)
    }

    err = d.poolManager.DeleteSnapshot(ctx, snapshotName)
    if err != nil {
        if errors.Is(err, pool.ErrSnapshotNotFound) {
            return &csi.DeleteSnapshotResponse{}, nil  // idempotent
        }
        return nil, status.Errorf(codes.Internal, "delete snapshot failed: %v", err)
    }

    return &csi.DeleteSnapshotResponse{}, nil
}
```

### ListSnapshots

Lists snapshots with support for single-snapshot lookup by ID, filtering by source volume ID, and pagination via `MaxEntries` / `StartingToken`.

```go
func (d *Driver) ListSnapshots(ctx context.Context, req *csi.ListSnapshotsRequest) (*csi.ListSnapshotsResponse, error) {
    // Single snapshot lookup by ID
    if req.GetSnapshotId() != "" {
        poolName, shareID, snapshotName, err := parseVolumeID(req.GetSnapshotId())
        if err != nil {
            return &csi.ListSnapshotsResponse{}, nil
        }

        if d.k8sClient != nil {
            snap, err := d.k8sClient.GetSnapshot(ctx, snapshotName)
            if err != nil {
                return &csi.ListSnapshotsResponse{}, nil
            }

            sourceVolumeID := fmt.Sprintf("%s/%s/%s", snap.Spec.PoolName, snap.Spec.ShareID, snap.Spec.SourceSubVolume)
            return &csi.ListSnapshotsResponse{
                Entries: []*csi.ListSnapshotsResponse_Entry{
                    {
                        Snapshot: &csi.Snapshot{
                            SnapshotId:     fmt.Sprintf("%s/%s/%s", poolName, shareID, snapshotName),
                            SourceVolumeId: sourceVolumeID,
                            SizeBytes:      snap.Spec.SizeGB * (1 << 30),
                            CreationTime:   timestamppb.New(snap.Status.CreationTime.Time),
                            ReadyToUse:     snap.Status.ReadyToUse,
                        },
                    },
                },
            }, nil
        }
        return &csi.ListSnapshotsResponse{}, nil
    }

    // Filter by source volume
    results, err := d.poolManager.ListSnapshots(ctx, req.GetSourceVolumeId())
    if err != nil {
        return nil, status.Errorf(codes.Internal, "list snapshots failed: %v", err)
    }

    var entries []*csi.ListSnapshotsResponse_Entry
    for _, r := range results {
        snapshotID := fmt.Sprintf("%s/%s/%s", r.PoolName, r.ShareID, r.SnapshotName)
        entries = append(entries, &csi.ListSnapshotsResponse_Entry{
            Snapshot: &csi.Snapshot{
                SnapshotId:     snapshotID,
                SourceVolumeId: req.GetSourceVolumeId(),
                SizeBytes:      r.SizeBytes,
                CreationTime:   timestamppb.New(r.CreationTime),
                ReadyToUse:     r.ReadyToUse,
            },
        })
    }

    // Pagination
    maxEntries := int(req.GetMaxEntries())
    startIdx := 0
    if req.GetStartingToken() != "" {
        parsed, err := strconv.Atoi(req.GetStartingToken())
        if err == nil {
            startIdx = parsed
        }
    }
    if startIdx > len(entries) {
        startIdx = len(entries)
    }
    entries = entries[startIdx:]

    nextToken := ""
    if maxEntries > 0 && len(entries) > maxEntries {
        entries = entries[:maxEntries]
        nextToken = strconv.Itoa(startIdx + maxEntries)
    }

    return &csi.ListSnapshotsResponse{
        Entries:   entries,
        NextToken: nextToken,
    }, nil
}
```

**Pagination:** The `StartingToken` is an integer offset into the snapshot list. When `MaxEntries` is set and there are more results, `NextToken` returns the next offset for the caller.

### CreateVolumeGroupSnapshot

Creates coordinated snapshots for multiple volumes in a single operation. All member volumes must be in the same pool. Each member gets its own individual Snapshot CR, and a parent VolumeGroupSnapshot CR tracks the group.

```go
func (d *Driver) CreateVolumeGroupSnapshot(ctx context.Context, req *csi.CreateVolumeGroupSnapshotRequest) (*csi.CreateVolumeGroupSnapshotResponse, error) {
    if req.GetName() == "" {
        return nil, status.Error(codes.InvalidArgument, "group snapshot name is required")
    }
    if len(req.GetSourceVolumeIds()) == 0 {
        return nil, status.Error(codes.InvalidArgument, "at least one source volume ID is required")
    }

    params := req.GetParameters()
    poolName := params["pool"]
    // Falls back to extracting pool name from the first source volume ID
    failurePolicy := params["failurePolicy"]  // "Abort" (default) or "Continue"

    groupReq := pool.GroupSnapshotRequest{
        GroupName:       req.GetName(),
        PoolName:        poolName,
        SourceVolumeIDs: req.GetSourceVolumeIds(),
        CopyOrder:       parseCSV(params["copyOrder"]),
        FailurePolicy:   failurePolicy,
        Parameters:      params,
    }

    result, err := d.poolManager.CreateVolumeGroupSnapshot(ctx, groupReq)
    if err != nil {
        switch {
        case errors.Is(err, pool.ErrEmptySourceList):
            return nil, status.Error(codes.InvalidArgument, "source volume list is empty")
        case errors.Is(err, pool.ErrSourceNotFound):
            return nil, status.Errorf(codes.NotFound, "source volume not found: %v", err)
        case errors.Is(err, pool.ErrGroupSnapshotFailed):
            return nil, status.Errorf(codes.Internal, "group snapshot failed: %v", err)
        default:
            return nil, status.Errorf(codes.Internal, "create group snapshot failed: %v", err)
        }
    }

    groupSnapshotID := fmt.Sprintf("%s/%s", poolName, req.GetName())

    // Each member snapshot gets its own entry with GroupSnapshotId set
    var snapshots []*csi.Snapshot
    for _, member := range result.Members {
        snapshotID := fmt.Sprintf("%s/%s/%s", member.PoolName, member.ShareID, member.SnapshotName)
        snapshots = append(snapshots, &csi.Snapshot{
            SnapshotId:      snapshotID,
            SourceVolumeId:  member.SourceVolumeID,
            SizeBytes:       member.SizeBytes,
            CreationTime:    timestamppb.New(member.CreationTime),
            ReadyToUse:      member.ReadyToUse,
            GroupSnapshotId: groupSnapshotID,
        })
    }

    return &csi.CreateVolumeGroupSnapshotResponse{
        GroupSnapshot: &csi.VolumeGroupSnapshot{
            GroupSnapshotId: groupSnapshotID,
            Snapshots:       snapshots,
            CreationTime:    timestamppb.New(result.CreationTime),
            ReadyToUse:      result.ReadyToUse,
        },
    }, nil
}
```

**Group snapshot ID format:** `{pool-name}/{group-name}` -- a 2-part format (unlike the 3-part volume/snapshot IDs).

**Parameters:**

| Parameter | Default | Description |
|-----------|---------|-------------|
| `pool` | *(extracted from first source volume)* | Pool that contains all source volumes |
| `failurePolicy` | `Abort` | `Abort` rolls back all snapshots on first failure; `Continue` skips failed members |
| `copyOrder` | *(empty)* | Comma-separated SubVolume names specifying the order in which snapshots are created |

**Error mapping:**

| Pool Error | gRPC Code | Meaning |
|-----------|-----------|---------|
| `ErrEmptySourceList` | `INVALID_ARGUMENT` | No source volume IDs provided |
| `ErrSourceNotFound` | `NOT_FOUND` | One of the source volumes does not exist |
| `ErrGroupSnapshotFailed` | `INTERNAL` | One or more member snapshots failed (with `Abort` policy) |

### DeleteVolumeGroupSnapshot

Deletes a group snapshot and all its member snapshots. The group snapshot ID is parsed to extract the group name, and the pool manager removes the VolumeGroupSnapshot CR and all associated individual Snapshot CRs and directories.

```go
func (d *Driver) DeleteVolumeGroupSnapshot(ctx context.Context, req *csi.DeleteVolumeGroupSnapshotRequest) (*csi.DeleteVolumeGroupSnapshotResponse, error) {
    groupSnapshotID := req.GetGroupSnapshotId()
    if groupSnapshotID == "" {
        return nil, status.Error(codes.InvalidArgument, "group snapshot ID is required")
    }

    // Parse group snapshot ID: {pool-name}/{group-name}
    groupName := groupSnapshotID
    parts := splitVolumeID(groupSnapshotID)
    if len(parts) >= 2 {
        groupName = parts[len(parts)-1]
    }

    err := d.poolManager.DeleteVolumeGroupSnapshot(ctx, groupName)
    if err != nil {
        return nil, status.Errorf(codes.Internal, "delete group snapshot failed: %v", err)
    }

    return &csi.DeleteVolumeGroupSnapshotResponse{}, nil
}
```

### GetVolumeGroupSnapshot

Fetches group snapshot details from the VolumeGroupSnapshot CR in Kubernetes. Returns the group status and all member snapshots that are in the `Ready` phase.

```go
func (d *Driver) GetVolumeGroupSnapshot(ctx context.Context, req *csi.GetVolumeGroupSnapshotRequest) (*csi.GetVolumeGroupSnapshotResponse, error) {
    groupSnapshotID := req.GetGroupSnapshotId()
    if groupSnapshotID == "" {
        return nil, status.Error(codes.InvalidArgument, "group snapshot ID is required")
    }

    // Parse ID and look up the VolumeGroupSnapshot CR
    groupName := groupSnapshotID
    poolName := ""
    parts := splitVolumeID(groupSnapshotID)
    if len(parts) >= 2 {
        poolName = parts[0]
        groupName = parts[len(parts)-1]
    }

    vgs, err := d.k8sClient.GetVolumeGroupSnapshot(ctx, groupName)
    if err != nil {
        return nil, status.Errorf(codes.NotFound, "group snapshot %q not found", groupName)
    }

    var snapshots []*csi.Snapshot
    for _, member := range vgs.Status.Members {
        if member.Phase == "Ready" {
            snapshots = append(snapshots, &csi.Snapshot{
                SnapshotId:      member.SnapshotName,
                SourceVolumeId:  member.SubVolumeName,
                ReadyToUse:      true,
                GroupSnapshotId: groupSnapshotID,
            })
        }
    }

    return &csi.GetVolumeGroupSnapshotResponse{
        GroupSnapshot: &csi.VolumeGroupSnapshot{
            GroupSnapshotId: fmt.Sprintf("%s/%s", poolName, vgs.Name),
            Snapshots:       snapshots,
            ReadyToUse:      vgs.Status.Phase == "Complete",
        },
    }, nil
}
```

### Unimplemented Methods

These methods exist via the embedded `csi.UnimplementedControllerServer` and return `Unimplemented`:

```go
// Not needed — NFS doesn't require controller publish (no attach)
func (d *Driver) ControllerPublishVolume(ctx, req) { return Unimplemented }
func (d *Driver) ControllerUnpublishVolume(ctx, req) { return Unimplemented }

// Optional
func (d *Driver) ListVolumes(ctx, req) { return Unimplemented }
func (d *Driver) GetCapacity(ctx, req) { return Unimplemented }
func (d *Driver) ControllerGetVolume(ctx, req) { return Unimplemented }
func (d *Driver) ControllerModifyVolume(ctx, req) { return Unimplemented }
```

---

## Node Service

### Capabilities

```go
func (d *Driver) NodeGetCapabilities(ctx context.Context, req *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
    return &csi.NodeGetCapabilitiesResponse{
        Capabilities: []*csi.NodeServiceCapability{
            {
                Type: &csi.NodeServiceCapability_Rpc{
                    Rpc: &csi.NodeServiceCapability_RPC{
                        Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
                    },
                },
            },
            {
                Type: &csi.NodeServiceCapability_Rpc{
                    Rpc: &csi.NodeServiceCapability_RPC{
                        Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
                    },
                },
            },
        },
    }, nil
}
```

### NodeStageVolume

Mounts the whole NFS share to a staging directory. Called once per share per node (not once per PVC).

```go
func (d *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
    // 1. Extract volume context — prefer zone-local server IP
    vc := req.GetVolumeContext()
    server := vc["server"]                      // Default (home zone)
    if zoneIP, ok := vc["server."+d.nodeZone]; ok {
        server = zoneIP                          // Use zone-local mount target IP
    }
    sharePath := vc["share"]
    stagingPath := req.GetStagingTargetPath()

    // 2. Check if already mounted
    if d.mountCache.IsMounted(stagingPath) {
        return &csi.NodeStageVolumeResponse{}, nil
    }

    // 3. Build NFS mount source
    source := fmt.Sprintf("%s:%s", server, sharePath)

    // 4. Mount options — merge custom flags with safe NFS defaults.
    //    Custom flags override defaults with the same key (e.g. "timeo=300"
    //    overrides "timeo=600"). The "soft" default is always preserved unless
    //    "hard" is explicitly specified.
    mountOptions := mergeNFSMountOptions(
        []string{"nfsvers=4.1", "soft", "timeo=600", "retrans=3"},
        req.GetVolumeCapability().GetMount().GetMountFlags(),
    )

    // 5. Create staging directory if needed
    if err := os.MkdirAll(stagingPath, 0750); err != nil {
        return nil, status.Errorf(codes.Internal, "failed to create staging dir: %v", err)
    }

    // 6. Mount
    if err := d.mounter.Mount(source, stagingPath, "nfs4", mountOptions); err != nil {
        return nil, status.Errorf(codes.Internal, "NFS mount failed: %v", err)
    }

    // 7. Track in mount cache
    d.mountCache.Add(stagingPath, server, sharePath)

    return &csi.NodeStageVolumeResponse{}, nil
}
```

### NodePublishVolume

Bind-mounts the specific subdirectory into the pod's volume path. Includes a clone gate that prevents mounting volumes whose background clone is still in progress.

```go
func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
    // 1. Extract paths
    stagingPath := req.GetStagingTargetPath()
    targetPath := req.GetTargetPath()
    subDir := req.GetVolumeContext()["subDir"]

    // 2. SECURITY: Validate subDir path
    if err := util.ValidateSubDir(subDir); err != nil {
        return nil, status.Errorf(codes.InvalidArgument, "invalid subDir path: %s", subDir)
    }

    // 3. Clone gate — block mount until async clone completes
    if d.k8sClient != nil {
        _, _, pvName, parseErr := parseVolumeID(req.GetVolumeId())
        if parseErr == nil {
            sv, svErr := d.k8sClient.GetSubVolume(ctx, pvName)
            if svErr == nil && sv.Status.CloneStatus != "" {
                switch sv.Status.CloneStatus {
                case "Complete":
                    // Clone is done, proceed with normal mount
                case "Failed":
                    errMsg := "clone failed"
                    if sv.Status.CloneProgress != nil && sv.Status.CloneProgress.Error != "" {
                        errMsg = sv.Status.CloneProgress.Error
                    }
                    return nil, status.Errorf(codes.Internal, "clone failed: %s", errMsg)
                default:
                    // Pending or InProgress — tell kubelet to retry
                    return nil, status.Errorf(codes.Unavailable,
                        "clone is %s, not ready for mount (retry later)", sv.Status.CloneStatus)
                }
            }
        }
    }

    // 4. Build source path (staging mount + subdirectory)
    sourcePath := filepath.Join(stagingPath, subDir)

    // 5. Create the subdirectory if it does not exist (deferred from controller)
    if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
        perm := os.FileMode(0755)
        if p := req.GetVolumeContext()["permissions"]; p != "" {
            if parsed, err := strconv.ParseUint(p, 8, 32); err == nil {
                perm = os.FileMode(parsed)
            }
        }
        if err := os.MkdirAll(sourcePath, perm); err != nil {
            return nil, status.Errorf(codes.Internal, "failed to create subdirectory %s: %v", subDir, err)
        }
        // Set ownership if uid/gid provided in VolumeContext
        uidVal, gidVal := -1, -1
        if u := req.GetVolumeContext()["uid"]; u != "" {
            if v, err := strconv.Atoi(u); err == nil {
                uidVal = v
            }
        }
        if g := req.GetVolumeContext()["gid"]; g != "" {
            if v, err := strconv.Atoi(g); err == nil {
                gidVal = v
            }
        }
        if uidVal >= 0 || gidVal >= 0 {
            if err := os.Chown(sourcePath, uidVal, gidVal); err != nil {
                klog.ErrorS(err, "Failed to chown subdirectory", "path", sourcePath)
            }
        }
    }

    // 6. Create target directory
    if err := os.MkdirAll(targetPath, 0750); err != nil {
        return nil, status.Errorf(codes.Internal, "failed to create target dir: %v", err)
    }

    // 7. Bind mount
    mountOptions := []string{"bind"}
    if req.GetReadonly() {
        mountOptions = append(mountOptions, "ro")
    }

    if err := d.mounter.Mount(sourcePath, targetPath, "", mountOptions); err != nil {
        return nil, status.Errorf(codes.Internal, "bind mount failed: %v", err)
    }

    return &csi.NodePublishVolumeResponse{}, nil
}
```

#### NodePublishVolume Clone Gate

When a volume was created via `CLONE_VOLUME`, the background clone worker may still be copying data. The node agent checks the SubVolume CR's `status.cloneStatus` field before allowing the bind mount:

| CloneStatus | Behavior |
|-------------|----------|
| `""` (empty) | Normal volume, no clone gate. Proceed immediately. |
| `Complete` | Clone finished. Proceed with mount. |
| `Pending` / `InProgress` | Return `UNAVAILABLE` so kubelet retries later. |
| `Failed` | Return `INTERNAL` with the error message from `status.cloneProgress.error`. |

This prevents pods from seeing partially-copied data. The kubelet's exponential backoff retries the mount until the clone completes.

### NodeUnpublishVolume

```go
func (d *Driver) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
    targetPath := req.GetTargetPath()

    // Unmount the bind mount
    if err := d.mounter.Unmount(targetPath); err != nil {
        return nil, status.Errorf(codes.Internal, "unmount failed: %v", err)
    }

    // Clean up the directory
    if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
        return nil, status.Errorf(codes.Internal, "failed to remove target dir: %v", err)
    }

    return &csi.NodeUnpublishVolumeResponse{}, nil
}
```

### NodeUnstageVolume

```go
func (d *Driver) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
    stagingPath := req.GetStagingTargetPath()

    // Check if any PVCs still reference this staging mount
    // The kubelet only calls Unstage after all Unpublish calls for this volume,
    // so this should be safe to unmount.

    if err := d.mounter.Unmount(stagingPath); err != nil {
        return nil, status.Errorf(codes.Internal, "NFS unmount failed: %v", err)
    }

    d.mountCache.Remove(stagingPath)

    if err := os.Remove(stagingPath); err != nil && !os.IsNotExist(err) {
        return nil, status.Errorf(codes.Internal, "failed to remove staging dir: %v", err)
    }

    return &csi.NodeUnstageVolumeResponse{}, nil
}
```

### NodeGetVolumeStats

Reports per-PVC usage by looking up the SubVolume CR for the requested quota and walking the bind-mount directory to compute actual disk usage. Falls back to share-level `statfs` if the SubVolume lookup fails.

```go
func (d *Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
    volumePath := req.GetVolumePath()

    // Always get share-level stats for inode reporting and fallback
    var stat unix.Statfs_t
    if err := unix.Statfs(volumePath, &stat); err != nil {
        return nil, status.Errorf(codes.Internal, "statfs failed: %v", err)
    }

    totalInodes := int64(stat.Files)
    freeInodes := int64(stat.Ffree)
    usedInodes := totalInodes - freeInodes

    // Try per-PVC usage: look up SubVolume CR for RequestedGB, walk dir for actual usage
    volumeID := req.GetVolumeId()
    _, _, pvName, parseErr := parseVolumeID(volumeID)
    if parseErr == nil && d.k8sClient != nil {
        sv, svErr := d.k8sClient.GetSubVolume(ctx, pvName)
        if svErr == nil && sv.Spec.RequestedGB > 0 {
            usedBytes, walkErr := dirUsageBytes(volumePath)
            if walkErr == nil {
                totalBytes := sv.Spec.RequestedGB * (1024 * 1024 * 1024)
                available := totalBytes - usedBytes
                if available < 0 {
                    available = 0
                }
                return &csi.NodeGetVolumeStatsResponse{
                    Usage: []*csi.VolumeUsage{
                        {Available: available, Total: totalBytes, Used: usedBytes, Unit: csi.VolumeUsage_BYTES},
                        {Available: freeInodes, Total: totalInodes, Used: usedInodes, Unit: csi.VolumeUsage_INODES},
                    },
                }, nil
            }
        }
    }

    // Fallback: share-level statfs
    totalBytes := int64(stat.Blocks) * int64(stat.Bsize)
    freeBytes := int64(stat.Bfree) * int64(stat.Bsize)
    usedBytes := totalBytes - freeBytes

    return &csi.NodeGetVolumeStatsResponse{
        Usage: []*csi.VolumeUsage{
            {Available: freeBytes, Total: totalBytes, Used: usedBytes, Unit: csi.VolumeUsage_BYTES},
            {Available: freeInodes, Total: totalInodes, Used: usedInodes, Unit: csi.VolumeUsage_INODES},
        },
    }, nil
}
```

**Per-PVC stats strategy:** The preferred path looks up the SubVolume CR to get `RequestedGB` (the PVC's quota) and walks the subdirectory with `filepath.WalkDir` to compute actual bytes used. This gives accurate per-PVC numbers instead of the NFS share's aggregate stats.

**Fallback:** If the SubVolume CR lookup or directory walk fails, the method falls back to `statfs` which returns share-level numbers. This matches the behavior of the community NFS CSI driver.

**Note:** Inode stats always come from `statfs` and reflect the whole NFS share, since per-directory inode tracking is not supported by NFS.

### NodeGetInfo

```go
func (d *Driver) NodeGetInfo(ctx context.Context, req *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
    // Read zone from node labels (set by cloud provider)
    zone, err := d.getNodeZone()
    if err != nil {
        return nil, status.Errorf(codes.Internal, "failed to determine node zone: %v", err)
    }

    return &csi.NodeGetInfoResponse{
        NodeId: d.nodeID,
        AccessibleTopology: &csi.Topology{
            Segments: map[string]string{
                "topology.kubernetes.io/zone": zone,
            },
        },
    }, nil
}
```

---

## Volume ID Format

```
Format:  {pool-name}/{share-vpc-id}/{pv-name}
Example: general-purpose/r006-a1b2c3d4-5678-90ab/pvc-abc123
```

Parser:
```go
func parseVolumeID(volumeID string) (poolName, shareID, pvName string, err error) {
    parts := strings.SplitN(volumeID, "/", 3)
    if len(parts) != 3 {
        return "", "", "", fmt.Errorf("volume ID must have 3 parts separated by '/', got: %s", volumeID)
    }
    return parts[0], parts[1], parts[2], nil
}
```

---

## gRPC Server Setup

```go
// pkg/driver/driver.go

type Driver struct {
    csi.UnimplementedIdentityServer
    csi.UnimplementedControllerServer
    csi.UnimplementedNodeServer

    name         string
    version      string
    nodeID       string
    endpoint     string
    mode         string
    poolManager  pool.PoolManager
    k8sClient    k8s.Client
    mounter      mount.Interface
    mountCache   *util.MountCache
    nodeZone     string
    nodeZoneOnce sync.Once
}

func (d *Driver) Run() error {
    // Parse endpoint (unix socket)
    listener, err := net.Listen("unix", d.endpoint)
    if err != nil {
        return fmt.Errorf("failed to listen on %s: %w", d.endpoint, err)
    }

    // Create gRPC server with logging interceptor
    server := grpc.NewServer(
        grpc.UnaryInterceptor(logInterceptor),
    )

    // Register services based on mode
    csi.RegisterIdentityServer(server, d)

    if d.isController() {
        csi.RegisterControllerServer(server, d)
    }

    if d.isNode() {
        csi.RegisterNodeServer(server, d)
    }

    return server.Serve(listener)
}
```

The controller and node run from the same binary but with different flags:
- `--mode=controller` — starts Identity + Controller services
- `--mode=node` — starts Identity + Node services
- `--endpoint=/csi/csi.sock` — Unix socket path
- `--node-id=<hostname>` — node identifier (from downward API)
- `--vpc-id=<vpc-id>` — VPC ID for mount target creation (controller mode)
- `--subnet-id=<subnet-id>` — Subnet ID for mount target creation (controller mode)
- `--kubelet-dir=/var/lib/kubelet` — Kubelet root directory; set to `/var/data/kubelet` on ROKS (controller mode, used for staging path construction)

---

## Sidecar Container Configuration

### Controller Pod

```yaml
containers:
  - name: csi-controller
    image: ibm-vpc-file-pool-csi:latest
    args:
      - "--mode=controller"
      - "--endpoint=unix:///csi/csi.sock"
      - "--v=4"
    volumeMounts:
      - name: socket-dir
        mountPath: /csi

  - name: csi-provisioner
    image: registry.k8s.io/sig-storage/csi-provisioner:v5.1.0
    args:
      - "--csi-address=/csi/csi.sock"
      - "--feature-gates=Topology=true"
      - "--leader-election"
      - "--leader-election-namespace=kube-system"
      - "--timeout=300s"           # Long timeout for share creation
      - "--retry-interval-start=5s"
    volumeMounts:
      - name: socket-dir
        mountPath: /csi

  - name: csi-resizer
    image: registry.k8s.io/sig-storage/csi-resizer:v1.12.0
    args:
      - "--csi-address=/csi/csi.sock"
      - "--leader-election"
      - "--leader-election-namespace=kube-system"
      - "--timeout=300s"           # Long timeout for VPC API calls (30-90s)
      - "--http-endpoint=:9810"
    livenessProbe:
      httpGet:
        path: /healthz/leader-election
        port: 9810
      periodSeconds: 30
    volumeMounts:
      - name: socket-dir
        mountPath: /csi

  - name: csi-snapshotter
    image: registry.k8s.io/sig-storage/csi-snapshotter:v8.2.0
    args:
      - "--csi-address=/csi/csi.sock"
      - "--leader-election"
      - "--leader-election-namespace=kube-system"
      - "--timeout=300s"           # Long timeout for VPC API calls (30-90s)
      - "--enable-volume-group-snapshots=true"
      - "--http-endpoint=:9811"
    livenessProbe:
      httpGet:
        path: /healthz/leader-election
        port: 9811
      periodSeconds: 30
    volumeMounts:
      - name: socket-dir
        mountPath: /csi

  - name: liveness-probe
    image: registry.k8s.io/sig-storage/livenessprobe:v2.14.0
    args:
      - "--csi-address=/csi/csi.sock"
    volumeMounts:
      - name: socket-dir
        mountPath: /csi
```

### Node DaemonSet

```yaml
hostNetwork: true               # Required for NFS mount persistence across container restarts
hostPID: true                   # Required for nsenter mount wrapper (NFS mounts via host namespace)
containers:
  - name: csi-node
    image: ibm-vpc-file-pool-csi:latest
    args:
      - "--mode=node"
      - "--endpoint=unix:///csi/csi.sock"
      - "--node-id=$(NODE_NAME)"
    env:
      - name: NODE_NAME
        valueFrom:
          fieldRef:
            fieldPath: spec.nodeName
    securityContext:
      privileged: true    # Required for mount operations
    startupProbe:
      exec:
        command: [test, -S, /csi/csi.sock]
      initialDelaySeconds: 5
      periodSeconds: 10
      failureThreshold: 18
    readinessProbe:
      exec:
        command: [test, -S, /csi/csi.sock]
      periodSeconds: 10
    livenessProbe:
      exec:
        command: [test, -S, /csi/csi.sock]
      periodSeconds: 30
    volumeMounts:
      - name: socket-dir
        mountPath: /csi
      - name: kubelet-dir
        mountPath: /var/lib/kubelet   # Use node.kubeletDir value
        mountPropagation: Bidirectional

  - name: node-driver-registrar
    image: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.12.0
    args:
      - "--csi-address=/csi/csi.sock"
      - "--kubelet-registration-path=/var/lib/kubelet/plugins/vpc-file-pool.csi.ibm.io/csi.sock"
      - "--http-endpoint=:9809"
    livenessProbe:
      httpGet:
        path: /healthz
        port: 9809
      periodSeconds: 30
    volumeMounts:
      - name: socket-dir
        mountPath: /csi
      - name: registration-dir
        mountPath: /registration
```
