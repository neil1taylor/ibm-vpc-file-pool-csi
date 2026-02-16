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

func (d *Driver) GetPluginInfo(ctx context.Context, req *csi.GetPluginInfoRequest) (*csi.GetPluginInfoResponse, error) {
    return &csi.GetPluginInfoResponse{
        Name:          DriverName,
        VendorVersion: d.version,
    }, nil
}

func (d *Driver) GetPluginCapabilities(ctx context.Context, req *csi.GetPluginCapabilitiesRequest) (*csi.GetPluginCapabilitiesResponse, error) {
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

func (d *Driver) Probe(ctx context.Context, req *csi.ProbeRequest) (*csi.ProbeResponse, error) {
    return &csi.ProbeResponse{}, nil
}
```

---

## Controller Service

### Capabilities

```go
func (d *Driver) ControllerGetCapabilities(ctx context.Context, req *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
    return &csi.ControllerGetCapabilitiesResponse{
        Capabilities: []*csi.ControllerServiceCapability{
            newCap(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
            newCap(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME),
            // We do NOT support:
            // - PUBLISH_UNPUBLISH_VOLUME (no attach needed for NFS)
            // - CREATE_DELETE_SNAPSHOT (future work)
            // - LIST_VOLUMES (optional, implement if useful for debugging)
        },
    }, nil
}
```

### CreateVolume

This is the most critical method. It MUST be idempotent.

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
    requiredGB := (requiredBytes + (1<<30 - 1)) / (1 << 30) // Round up to GB
    if requiredGB < 1 {
        requiredGB = 1
    }

    // 3. Check for idempotency — does a SubVolume CR already exist for this volume name?
    existing, err := d.k8sClient.GetSubVolume(ctx, req.GetName())
    if err == nil && existing != nil {
        // Already exists — return the same response (idempotent)
        return &csi.CreateVolumeResponse{
            Volume: subVolumeToCSIVolume(existing),
        }, nil
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

    // 5. Delegate to Pool Manager
    allocReq := pool.AllocationRequest{
        PVName:       req.GetName(),
        PVCName:      params["csi.storage.k8s.io/pvc/name"],
        PVCNamespace: params["csi.storage.k8s.io/pvc/namespace"],
        PoolName:     poolName,
        RequestedGB:  requiredGB,
        Zone:         zone,
        UID:          parseOptionalInt64(params["uid"]),
        GID:          parseOptionalInt64(params["gid"]),
        Permissions:  params["permissions"],
    }

    result, err := d.poolManager.Allocate(ctx, allocReq)
    if err != nil {
        // Map pool errors to CSI gRPC codes
        switch {
        case errors.Is(err, pool.ErrPoolNotFound):
            return nil, status.Errorf(codes.NotFound, "pool %q not found", poolName)
        case errors.Is(err, pool.ErrPoolExhausted):
            return nil, status.Errorf(codes.ResourceExhausted, "pool %q has no available capacity", poolName)
        case errors.Is(err, pool.ErrShareCreationPending):
            // A new share is being created — tell the provisioner to retry
            return nil, status.Errorf(codes.Unavailable, "pool %q is expanding, retry shortly", poolName)
        default:
            return nil, status.Errorf(codes.Internal, "allocation failed: %v", err)
        }
    }

    // 6. Build volume ID
    volumeID := fmt.Sprintf("%s/%s/%s", poolName, result.ShareID, req.GetName())

    // 7. Return response
    return &csi.CreateVolumeResponse{
        Volume: &csi.Volume{
            VolumeId:      volumeID,
            CapacityBytes: requiredGB * (1 << 30),
            VolumeContext: buildVolumeContext(result, poolName),
            AccessibleTopology: []*csi.Topology{
                {
                    Segments: map[string]string{
                        "topology.kubernetes.io/zone": zone,
                    },
                },
            },
        },
    }, nil
}
```

**VolumeContext helper:**

```go
func buildVolumeContext(result *pool.AllocationResult, poolName string) map[string]string {
    vc := map[string]string{
        "server":  result.MountTargetIP,
        "share":   result.SharePath,
        "subDir":  result.SubPath,
        "pool":    poolName,
        "shareID": result.ShareID,
    }
    // Optional fields — passed through to NodePublishVolume for subdirectory creation
    if result.Permissions != "" {
        vc["permissions"] = result.Permissions
    }
    if result.UID != nil {
        vc["uid"] = strconv.FormatInt(*result.UID, 10)
    }
    if result.GID != nil {
        vc["gid"] = strconv.FormatInt(*result.GID, 10)
    }
    return vc
}
```

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

### Unimplemented Methods

These must exist but return `Unimplemented`:

```go
// Not needed — NFS doesn't require controller publish (no attach)
func (d *Driver) ControllerPublishVolume(ctx, req) { return Unimplemented }
func (d *Driver) ControllerUnpublishVolume(ctx, req) { return Unimplemented }

// Not implemented yet (future work)
func (d *Driver) CreateSnapshot(ctx, req) { return Unimplemented }
func (d *Driver) DeleteSnapshot(ctx, req) { return Unimplemented }
func (d *Driver) ListSnapshots(ctx, req) { return Unimplemented }

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
    // 1. Extract volume context
    server := req.GetVolumeContext()["server"]
    sharePath := req.GetVolumeContext()["share"]
    stagingPath := req.GetStagingTargetPath()

    // 2. Check if already mounted
    if d.mountCache.IsMounted(stagingPath) {
        return &csi.NodeStageVolumeResponse{}, nil
    }

    // 3. Build NFS mount source
    source := fmt.Sprintf("%s:%s", server, sharePath)

    // 4. Mount options (from StorageClass mountOptions + defaults)
    mountOptions := []string{"nfsvers=4.1", "soft", "timeo=600", "retrans=3"}
    if opts := req.GetVolumeCapability().GetMount().GetMountFlags(); len(opts) > 0 {
        mountOptions = opts
    }

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

Bind-mounts the specific subdirectory into the pod's volume path.

```go
func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
    // 1. Extract paths
    stagingPath := req.GetStagingTargetPath()
    targetPath := req.GetTargetPath()
    subDir := req.GetVolumeContext()["subDir"]

    // 2. SECURITY: Validate subDir path
    //    Must match pattern /pvcs/pvc-<uuid> and must not contain ".."
    if !isValidSubDir(subDir) {
        return nil, status.Errorf(codes.InvalidArgument, "invalid subDir path: %s", subDir)
    }

    // 3. Build source path (staging mount + subdirectory)
    sourcePath := filepath.Join(stagingPath, subDir)

    // 4. Create the subdirectory if it does not exist (deferred from controller)
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
        uid, gid := -1, -1
        if u := req.GetVolumeContext()["uid"]; u != "" {
            if parsed, err := strconv.Atoi(u); err == nil {
                uid = parsed
            }
        }
        if g := req.GetVolumeContext()["gid"]; g != "" {
            if parsed, err := strconv.Atoi(g); err == nil {
                gid = parsed
            }
        }
        if uid >= 0 || gid >= 0 {
            if err := os.Chown(sourcePath, uid, gid); err != nil {
                klog.ErrorS(err, "Failed to chown subdirectory", "subDir", subDir, "uid", uid, "gid", gid)
            }
        }
    }

    // 5. Create target directory
    if err := os.MkdirAll(targetPath, 0750); err != nil {
        return nil, status.Errorf(codes.Internal, "failed to create target dir: %v", err)
    }

    // 6. Bind mount
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

Reports per-subdirectory usage. This is how `kubectl exec -- df` shows capacity info for the PVC.

```go
func (d *Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
    volumePath := req.GetVolumePath()

    // Use statfs to get filesystem stats for the mount point
    var stat unix.Statfs_t
    if err := unix.Statfs(volumePath, &stat); err != nil {
        return nil, status.Errorf(codes.Internal, "statfs failed: %v", err)
    }

    totalBytes := int64(stat.Blocks) * int64(stat.Bsize)
    freeBytes := int64(stat.Bfree) * int64(stat.Bsize)
    usedBytes := totalBytes - freeBytes

    totalInodes := int64(stat.Files)
    freeInodes := int64(stat.Ffree)
    usedInodes := totalInodes - freeInodes

    return &csi.NodeGetVolumeStatsResponse{
        Usage: []*csi.VolumeUsage{
            {
                Available: freeBytes,
                Total:     totalBytes,
                Used:      usedBytes,
                Unit:      csi.VolumeUsage_BYTES,
            },
            {
                Available: freeInodes,
                Total:     totalInodes,
                Used:      usedInodes,
                Unit:      csi.VolumeUsage_INODES,
            },
        },
    }, nil
}
```

**Note:** `statfs` on a bind-mounted subdirectory returns stats for the whole NFS share, not just the subdirectory. This is a known limitation of NFS. The reported capacity is the share's total capacity, not the PVC's requested capacity. To report per-PVC capacity, you would need to run `du` on the subdirectory, which is expensive. For now, accept the share-level stats — this is what the community NFS driver does too.

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
    name        string
    version     string
    nodeID      string
    endpoint    string
    poolManager pool.PoolManager
    k8sClient   k8s.Client
    mounter     mount.Interface
    mountCache  *MountCache
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
    volumeMounts:
      - name: socket-dir
        mountPath: /csi
      - name: kubelet-dir
        mountPath: /var/lib/kubelet
        mountPropagation: Bidirectional
      - name: staging-dir
        mountPath: /var/lib/kubelet/plugins/vpc-file-pool.csi.ibm.io
        mountPropagation: Bidirectional

  - name: node-driver-registrar
    image: registry.k8s.io/sig-storage/csi-node-driver-registrar:v2.12.0
    args:
      - "--csi-address=/csi/csi.sock"
      - "--kubelet-registration-path=/var/lib/kubelet/plugins/vpc-file-pool.csi.ibm.io/csi.sock"
    volumeMounts:
      - name: socket-dir
        mountPath: /csi
      - name: registration-dir
        mountPath: /registration

  - name: liveness-probe
    image: registry.k8s.io/sig-storage/livenessprobe:v2.14.0
    args:
      - "--csi-address=/csi/csi.sock"
    volumeMounts:
      - name: socket-dir
        mountPath: /csi
```
