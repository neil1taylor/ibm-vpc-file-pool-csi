# User Guide — IBM VPC File Pool CSI Driver

## How It Works

The IBM VPC File Pool CSI Driver changes the relationship between PVCs and VPC file shares. Instead of one PVC creating one share, many PVCs share a common pool of large file shares. Each PVC gets its own subdirectory on a share.

```
Traditional (stock IBM CSI):          Pool-based (this driver):

PVC-1 → Share-1 (10 GB)              PVC-1 ──┐
PVC-2 → Share-2 (10 GB)              PVC-2 ──┤
PVC-3 → Share-3 (10 GB)              PVC-3 ──┼──→ Share-1 (2 TB)
PVC-4 → Share-4 (10 GB)              PVC-4 ──┤    /pvcs/pvc-1/
PVC-5 → Share-5 (10 GB)              PVC-5 ──┘    /pvcs/pvc-2/
                                                    /pvcs/pvc-3/
5 shares, 50 GB billed               PVC-6 ──┐    /pvcs/pvc-4/
                                      PVC-7 ──┼──→ Share-2 (2 TB)    /pvcs/pvc-5/
                                      ...     ┘
                                      1-2 shares, capacity shared
```

This means PVCs provision in under a second (a mkdir vs. a 30-90 second API call), you use a fraction of your 300-share quota, and small PVCs don't each waste a 10 GB minimum allocation.

---

## Quick Start

Get a PVC running in 4 steps:

```bash
# 1. Create a pool (if one doesn't exist yet)
kubectl apply -f examples/basic/pool.yaml
kubectl get filesharepools -w          # Wait for Phase: Ready (~60 seconds)

# 2. Create a StorageClass
kubectl apply -f examples/basic/storageclass.yaml

# 3. Create a PVC
kubectl apply -f examples/basic/pvc.yaml
kubectl get pvc my-app-data            # Should be Bound within seconds

# 4. Use the PVC in a pod
kubectl apply -f examples/basic/pod.yaml
kubectl logs my-app-pod                # Should print "Hello from pool CSI"
```

See `examples/` for more patterns: multi-zone, StatefulSet, shared RWX, tiered performance, custom permissions, and data retention.

---

## Concepts

### FileSharePool

A `FileSharePool` is a cluster-scoped custom resource that defines a pool of VPC file shares. It specifies the zone, storage profile, share size, and capacity management rules. The driver's controller watches these resources and ensures enough VPC file shares exist to serve PVC requests.

You can have multiple pools for different use cases — for example a high-IOPS pool for databases and a standard pool for application configs.

### SubVolume

A `SubVolume` is a cluster-scoped custom resource that tracks a single PVC's allocation. It records which share the PVC's subdirectory lives on, the mount target IP, the subdirectory path, and the requested capacity. SubVolumes are created automatically when you create a PVC and deleted when the PVC is deleted.

### StorageClass

A standard Kubernetes `StorageClass` that references a pool by name. You create PVCs against this StorageClass just like any other CSI driver.

---

## Managing Pools

### Creating a Pool

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: general-purpose
spec:
  zone: us-south-1
  profile: dp2
  shareSizeGB: 2000
  maxShares: 10
  initialShares: 1
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultPermissions: "0755"
  defaultUID: 1000    # Best-effort on VPC NFS (sec=null ignores chown)
  defaultGID: 1000    # Best-effort on VPC NFS (sec=null ignores chown)
  # Authentication is handled globally via secret-common-lib (no secretRef needed)
  mountOptions:
    - nfsvers=4.1
    - soft
    - timeo=600
    - retrans=3
```

```bash
kubectl apply -f pool.yaml
```

Wait for the pool phase to become `Ready`:

```bash
kubectl get filesharepools
```

```
NAME              ZONE         PROFILE  SHARES  CAPACITY  ALLOCATED  PVCS  PHASE
general-purpose   us-south-1   dp2      1       2000      0          0     Ready
```

### Pool Sizing Guidance

| Workload Type | Suggested Profile | Share Size | IOPS | Strategy |
|---------------|-------------------|------------|------|----------|
| General workloads (configs, logs, small apps) | dp2 | 1000-2000 GB | default | spread |
| Databases, analytics | dp2 | 2000-4000 GB | custom (high) | spread |
| CI/CD ephemeral volumes | dp2 | 500-1000 GB | default | binpack |
| Shared team storage | dp2 | 4000+ GB | default | spread |

Rules of thumb:
- **shareSizeGB** should be large enough to hold 50-200 PVCs without needing a new share. If most PVCs are 1-5 GB, a 1 TB share gives you 200-1000 PVCs per share.
- **maxShares** controls your maximum VPC file share quota usage per pool. With 300 shares per account, budget across your pools and the stock IBM CSI driver.
- **expandThresholdPercent** at 80% means a new share is created when 80% of the pool's total capacity is allocated. This gives a buffer so PVCs don't fail while the new share is being created (30-90 seconds).
- **allocationStrategy**: `spread` distributes PVCs evenly across shares (better fault isolation); `binpack` fills shares before starting new ones (fewer shares used, but higher blast radius if one fails).

### Viewing Pool Status

```bash
# Summary
kubectl get filesharepools

# Detailed status with per-share breakdown
kubectl get filesharespool general-purpose -o yaml
```

The status section shows each share's ID, mount target IP, total/allocated capacity, PVC count, and health state.

### Multi-Zone Pools

VPC file shares are zonal, but the driver supports **cross-zone accessor bindings** to make shares accessible from multiple zones with zone-local NFS IPs.

#### Option A: Cross-Zone Pool (Recommended for Multi-Zone Clusters)

A single pool with `accessorZones` creates mount targets in additional zones:

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: general-purpose
spec:
  zone: us-south-1                    # Home zone — shares created here
  profile: dp2
  shareSizeGB: 2000
  maxShares: 10
  initialShares: 1
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultPermissions: "0755"
  accessorZones:                       # Nodes in these zones get zone-local IPs
    - zone: us-south-2
      subnetID: "0717-xxxx-yyyy"
    - zone: us-south-3
      subnetID: "0727-aaaa-bbbb"
```

Nodes in `us-south-2` mount via the accessor mount target IP in their zone, avoiding cross-zone NFS traffic. The PV volumeAttributes include `server.us-south-1`, `server.us-south-2`, etc.

#### Option B: One Pool Per Zone

Alternatively, create separate pools per zone:

```yaml
# pool-south-1.yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: general-us-south-1
spec:
  zone: us-south-1
  # ...rest of spec
---
# pool-south-2.yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: general-us-south-2
spec:
  zone: us-south-2
  # ...rest of spec
```

Then create a StorageClass per zone, or a single StorageClass with `volumeBindingMode: WaitForFirstConsumer` and topology awareness (the driver will pick the pool matching the pod's zone).

### Expanding a Pool Manually

If `autoExpand` is `false` or you want to proactively add capacity:

```bash
# Option 1: Increase maxShares and let the reconciler add shares as needed
kubectl patch filesharespool general-purpose --type merge -p '{"spec":{"maxShares":15}}'

# Option 2: Increase individual share size (expands the underlying VPC file share)
# This is handled by the reconciler when the pool detects shares approaching capacity
# You can trigger it by lowering expandThresholdPercent temporarily
```

### Draining a Share

If you need to take a share out of rotation (e.g., for maintenance or migration), the pool manager supports a draining state where no new PVCs are allocated to that share but existing PVCs continue to work. This is managed automatically when the VPC API reports a share as degraded.

Manual draining is not yet exposed as a user-facing API — it's tracked as a future feature.

---

## StorageClasses

### Default StorageClass

The installation creates a default StorageClass:

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ibm-vpc-file-pool
provisioner: vpc-file-pool.csi.ibm.io
parameters:
  pool: general-purpose
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowVolumeExpansion: true
mountOptions:
  - nfsvers=4.1
  - soft
  - timeo=600
  - retrans=3
```

### Custom StorageClasses

Create StorageClasses for different use cases:

```yaml
# High-security: specific UID/GID, restricted permissions
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ibm-vpc-file-pool-secure
provisioner: vpc-file-pool.csi.ibm.io
parameters:
  pool: general-purpose
  uid: "65534"
  gid: "65534"
  permissions: "0700"
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowVolumeExpansion: true
mountOptions:
  - nfsvers=4.1
  - soft
  - timeo=600
  - retrans=3
---
# Retain data after PVC deletion
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ibm-vpc-file-pool-retain
provisioner: vpc-file-pool.csi.ibm.io
parameters:
  pool: general-purpose
  reclaimAction: retain
reclaimPolicy: Retain
volumeBindingMode: Immediate
allowVolumeExpansion: true
mountOptions:
  - nfsvers=4.1
  - soft
  - timeo=600
  - retrans=3
---
# Archive data on deletion (moved to .archived/ directory on the share)
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ibm-vpc-file-pool-archive
provisioner: vpc-file-pool.csi.ibm.io
parameters:
  pool: general-purpose
  reclaimAction: archive
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowVolumeExpansion: true
mountOptions:
  - nfsvers=4.1
  - soft
  - timeo=600
  - retrans=3
```

### StorageClass Parameters Reference

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `pool` | Yes | — | Name of the `FileSharePool` CR to allocate from |
| `uid` | No | Pool's `defaultUID` | Unix UID owner for the subdirectory (best-effort: VPC NFS `sec=null` ignores chown) |
| `gid` | No | Pool's `defaultGID` | Unix GID owner for the subdirectory (best-effort: VPC NFS `sec=null` ignores chown) |
| `permissions` | No | Pool's `defaultPermissions` | Unix permissions (e.g., `"0755"`) |
| `reclaimAction` | No | `delete` | What happens to the subdirectory on PVC deletion: `delete` (remove it), `retain` (leave it), `archive` (move to `.archived/`) |

---

## Working with PVCs

### Creating a PVC

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-app-data
  namespace: default
spec:
  accessModes:
    - ReadWriteMany        # NFS supports RWX natively
  storageClassName: ibm-vpc-file-pool
  resources:
    requests:
      storage: 5Gi
```

```bash
kubectl apply -f pvc.yaml

# PVC should bind within seconds
kubectl get pvc my-app-data
```

### Access Modes

Since the underlying storage is NFS, all access modes work:

| Mode | Supported | Description |
|------|-----------|-------------|
| `ReadWriteOnce` (RWO) | Yes | Single node read-write |
| `ReadOnlyMany` (ROX) | Yes | Multiple nodes read-only |
| `ReadWriteMany` (RWX) | Yes | Multiple nodes read-write |
| `ReadWriteOncePod` (RWOP) | Yes | Single pod read-write (K8s 1.29+) |

**RWX is the natural fit** for this driver since NFS is inherently multi-reader/writer. Use it for shared storage across replicas, batch jobs, and cross-pod data.

### Using a PVC in a Pod

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: my-app
spec:
  replicas: 3
  selector:
    matchLabels:
      app: my-app
  template:
    metadata:
      labels:
        app: my-app
    spec:
      containers:
        - name: app
          image: my-app:latest
          volumeMounts:
            - name: data
              mountPath: /data
          securityContext:
            runAsUser: 1000        # Should match StorageClass uid
            runAsGroup: 1000       # Should match StorageClass gid
      volumes:
        - name: data
          persistentVolumeClaim:
            claimName: my-app-data
```

### Expanding a PVC

```bash
# Edit the PVC to request more storage
kubectl patch pvc my-app-data -p '{"spec":{"resources":{"requests":{"storage":"20Gi"}}}}'

# Check the new size
kubectl get pvc my-app-data
```

Expansion is nearly instant — the driver updates the allocation tracking in the SubVolume CR. No VPC API call is needed unless the share itself needs to grow.

**Note:** You can only expand, not shrink. This is a Kubernetes-wide constraint on PVCs.

### Deleting a PVC

```bash
kubectl delete pvc my-app-data
```

What happens depends on the StorageClass's reclaim policy:

- **Delete** (default): The subdirectory and all its contents are removed from the share. The SubVolume CR is deleted. Capacity is freed in the pool.
- **Retain**: The subdirectory stays on the share. The SubVolume CR is marked as `Retained`. Data must be manually cleaned up later.
- **Archive**: The subdirectory is moved to `.archived/<pvc-name>-<timestamp>/` on the share. Useful for accidental deletion recovery.

### Viewing SubVolumes

```bash
# All subvolumes
kubectl get subvolumes

# Detailed view
kubectl get subvolumes -o wide

# Filter by pool
kubectl get subvolumes -l storage.ibmcloud.io/pool=general-purpose

# Filter by share
kubectl get subvolumes -l storage.ibmcloud.io/share-id=r006-xxxx

# Detailed status for a specific subvolume
kubectl get subvolume pvc-abc123 -o yaml
```

---

## Snapshots

### Creating a Snapshot

Create a `VolumeSnapshot` referencing an existing PVC to capture a point-in-time copy of the data:

```yaml
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: my-app-snapshot
  namespace: default
spec:
  volumeSnapshotClassName: ibm-vpc-file-pool-snapclass
  source:
    persistentVolumeClaimName: my-app-data
```

```bash
kubectl apply -f snapshot.yaml

# Check snapshot status
kubectl get volumesnapshot my-app-snapshot
```

The snapshot creates a directory copy under `/pvcs/.snapshots/` on the same share. Snapshot creation time is proportional to data size.

### Restoring from a Snapshot

Create a new PVC with `dataSource` referencing the snapshot:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-app-restored
  namespace: default
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ibm-vpc-file-pool
  resources:
    requests:
      storage: 5Gi
  dataSource:
    name: my-app-snapshot
    kind: VolumeSnapshot
    apiGroup: snapshot.storage.k8s.io
```

### Deleting a Snapshot

```bash
kubectl delete volumesnapshot my-app-snapshot
```

This removes the snapshot directory and frees capacity on the share.

---

## Volume Cloning

### Cloning a PVC

Create a new PVC with `dataSource` referencing an existing PVC:

```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: my-app-clone
  namespace: default
spec:
  accessModes:
    - ReadWriteMany
  storageClassName: ibm-vpc-file-pool
  resources:
    requests:
      storage: 5Gi          # Must be >= source PVC size
  dataSource:
    kind: PersistentVolumeClaim
    name: my-app-data        # Source PVC in the same namespace
```

```bash
kubectl apply -f clone-pvc.yaml

# Check clone status
kubectl get subvolumes -l storage.ibmcloud.io/clone-source
```

**Small volumes** (default: <= 10 GB) clone synchronously — the PVC binds immediately with data ready.

**Large volumes** clone asynchronously — the PVC binds immediately but pods wait in `ContainerCreating` until the background copy completes. Monitor progress via the SubVolume CR's `cloneStatus` field.

### Clone Consistency

- **Best practice:** Scale down the application or pause writes before cloning for full consistency.
- If the source is being actively written to, each file is individually consistent (NFS close-to-open), but cross-file relationships are not guaranteed.
- **Do not clone running VM disk images** — the clone will be corrupt.

---

## Volume Group Snapshots

Group snapshots coordinate the snapshot of multiple related PVCs in a single operation.

### Creating a Group Snapshot

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: VolumeGroupSnapshot
metadata:
  name: myapp-group-snap
spec:
  poolName: general-purpose
  sourcePVCs:
    - pvcName: myapp-data
      pvcNamespace: default
    - pvcName: myapp-wal
      pvcNamespace: default
    - pvcName: myapp-config
      pvcNamespace: default
  failurePolicy: Abort          # Abort or Continue
```

```bash
kubectl apply -f group-snapshot.yaml

# Check group snapshot status
kubectl get volumegroupsnapshots myapp-group-snap
```

### Group Snapshot Consistency

Without quiesce hooks (current implementation), group snapshots provide **best-effort coordinated** consistency — SubVolumes are copied sequentially with an inconsistency window equal to the total copy duration.

For workloads requiring cross-PVC consistency, scale down the application before taking the group snapshot.

### Deleting a Group Snapshot

```bash
kubectl delete volumegroupsnapshot myapp-group-snap
```

This deletes all member snapshots and frees the associated capacity.

---

## Coexistence with the Stock IBM CSI Driver

This driver runs alongside the stock `ibm-vpc-file-csi-driver` without conflict. They use different provisioner names:

| Driver | Provisioner Name |
|--------|-----------------|
| Stock IBM File CSI | `vpc.file.csi.ibm.io` |
| Pool File CSI (this driver) | `vpc-file-pool.csi.ibm.io` |

Use the stock driver for workloads that need a dedicated share with isolated IOPS (e.g., a high-traffic database). Use the pool driver for everything else — application configs, shared data, CI/CD volumes, log storage, and any workload where fast provisioning and efficient quota usage matter more than per-PVC IOPS isolation.

Both StorageClasses can exist at the same time. Pods pick which driver to use by referencing the appropriate StorageClass.

---

## Monitoring

### Prometheus Metrics

The controller exposes metrics on `:8080/metrics`. Scrape this endpoint with Prometheus or use the ServiceMonitor if your cluster runs the Prometheus Operator.

**Key metrics:**

| Metric | Type | Description |
|--------|------|-------------|
| `vpc_file_pool_capacity_gb` | Gauge | Total capacity across all shares in a pool |
| `vpc_file_pool_allocated_gb` | Gauge | Total allocated (requested) capacity in a pool |
| `vpc_file_pool_pvc_count` | Gauge | Number of active PVCs in a pool |
| `vpc_file_pool_share_count` | Gauge | Number of VPC file shares in a pool |
| `vpc_file_pool_allocations_total` | Counter | Cumulative PVC allocations (by pool, success/error) |
| `vpc_file_pool_allocation_duration_seconds` | Histogram | Time to allocate a PVC (should be <1s in steady state) |
| `vpc_file_pool_api_calls_total` | Counter | VPC API calls (by operation, success/error) |
| `vpc_file_pool_api_call_duration_seconds` | Histogram | VPC API call duration |

### Recommended Alerts

```yaml
# Pool near capacity
- alert: FileSharePoolNearCapacity
  expr: vpc_file_pool_allocated_gb / vpc_file_pool_capacity_gb > 0.9
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: "File share pool {{ $labels.pool }} is over 90% allocated"

# Pool at max shares
- alert: FileSharePoolAtMaxShares
  expr: vpc_file_pool_share_count >= on(pool) group_left max by(pool) (vpc_file_pool_max_shares)
  for: 1m
  labels:
    severity: critical
  annotations:
    summary: "File share pool {{ $labels.pool }} has reached max shares and cannot auto-expand"

# PVC allocation failures
- alert: FileSharePoolAllocationFailures
  expr: rate(vpc_file_pool_allocations_total{status="error"}[5m]) > 0
  for: 2m
  labels:
    severity: warning
  annotations:
    summary: "PVC allocations failing in pool {{ $labels.pool }}"

# VPC API errors
- alert: VPCAPIErrors
  expr: rate(vpc_file_pool_api_calls_total{status="error"}[5m]) > 0.1
  for: 5m
  labels:
    severity: warning
  annotations:
    summary: "VPC API errors detected for {{ $labels.operation }}"
```

### Checking Pool Health

```bash
# Quick health check
kubectl get filesharepools

# If a pool shows "Degraded", check which share is unhealthy
kubectl get filesharespool <pool-name> -o jsonpath='{range .status.shares[*]}{.shareID}{"\t"}{.state}{"\n"}{end}'

# Check controller logs for reconciliation errors
kubectl logs -n kube-system -l app=vpc-file-pool-csi-controller -c csi-controller --tail=50
```

---

## Troubleshooting

Here are the three most common issues. For the full troubleshooting guide (pool issues, VPC API errors, mount debugging, metrics, logging), see [TROUBLESHOOTING.md](TROUBLESHOOTING.md).

### PVC Pending: "pool not found"

The `pool` parameter in the StorageClass doesn't match any `FileSharePool` CR name. Verify with:
```bash
kubectl get filesharepools
kubectl get storageclass <sc-name> -o jsonpath='{.parameters.pool}'
```

### NFS mount failed

TCP port 2049 is blocked between worker nodes and NFS mount targets. Check security group rules. See [TROUBLESHOOTING.md — Mount Issues](TROUBLESHOOTING.md#nfs-mount-failed) for detailed steps.

### Permission denied

UID/GID mismatch between the pod's `securityContext` and the StorageClass's `uid`/`gid` parameters. See [TROUBLESHOOTING.md — Permission denied](TROUBLESHOOTING.md#permission-denied-on-mount) for details.

---

## Frequently Asked Questions

**Q: Can I use this with OpenShift?**
Yes. The driver is fully compatible with ROKS (Red Hat OpenShift on IBM Cloud). On OpenShift, the node agent requires the `privileged` SCC, which is standard for all CSI node agents. The Helm chart includes the necessary SCC configuration.

**Q: What happens if a VPC file share goes down?**
All PVCs on that share will experience I/O errors until the share recovers. IBM VPC File Storage has 99.999% availability with HA-paired nodes, so this is extremely rare. The pool manager will detect the degraded state and stop allocating new PVCs to that share. Existing PVCs recover automatically when the share comes back.

**Q: Can I migrate existing PVCs from the stock IBM CSI driver to this driver?**
Not in-place — Kubernetes doesn't support changing a PV's CSI driver. You would need to create a new PVC with the pool StorageClass, copy the data (e.g., with `rsync` in a pod that mounts both PVCs), then switch your application to the new PVC. A migration tool is planned for a future release.

**Q: Is there per-PVC quota enforcement?**
Currently, quotas are advisory (soft enforcement). The driver tracks allocated vs. requested capacity in SubVolume CRs, and the metrics/alerts system will warn you when a PVC exceeds its allocation. Hard per-subdirectory quotas depend on NFS server-side project quota support, which IBM VPC File Storage does not currently expose. If hard quotas are critical for your use case, use the stock IBM CSI driver for those specific workloads.

**Q: How many PVCs can I have per share?**
There's no hard limit in the driver. The practical limits are the share's total capacity and the NFS server's ability to handle concurrent connections. In testing, hundreds of PVCs per share work well. If you're running thousands, monitor NFS connection counts and consider spreading across more shares (use the `spread` allocation strategy).

**Q: Can I use this across multiple availability zones?**
Yes. Add `accessorZones` to your `FileSharePool` spec — the driver creates mount targets in each accessor zone so nodes get zone-local NFS IPs. This avoids cross-zone NFS traffic. See [Multi-Zone Pools](#multi-zone-pools) for details. Alternatively, create one pool per zone with separate StorageClasses.

**Q: Does this work with IBM Cloud Satellite?**
This driver targets IBM Cloud VPC infrastructure specifically (it uses the VPC API to create file shares). For Satellite locations with their own storage backends, the community `csi-driver-nfs` may be more appropriate since it works with any existing NFS server.

---

## Compatibility Matrix

| Component | Minimum Version | Tested With |
|-----------|----------------|-------------|
| Kubernetes | 1.28 | 1.28, 1.29, 1.30, 1.31 |
| OpenShift (ROKS) | 4.14 | 4.14, 4.15, 4.16, 4.17 |
| Infrastructure | VPC Gen2 | VPC Gen2 only (Classic not supported) |
| Helm | v3.10 | v3.10+ |
| CSI sidecar: csi-provisioner | v4.0 | v4.0, v5.0 |
| CSI sidecar: csi-resizer | v1.10 | v1.10, v1.11 |
| CSI sidecar: liveness-probe | v2.12 | v2.12, v2.13 |
| NFS | v4.1 | NFSv4.1 |

---

## See Also

- [TROUBLESHOOTING.md](TROUBLESHOOTING.md) — Comprehensive troubleshooting guide
- [PERFORMANCE-TUNING.md](PERFORMANCE-TUNING.md) — NFS mount optimization, IOPS planning, benchmarking
- [CONTRIBUTING.md](CONTRIBUTING.md) — Developer guide for contributing to the project
- [examples/](examples/) — Ready-to-use YAML examples for common deployment patterns
