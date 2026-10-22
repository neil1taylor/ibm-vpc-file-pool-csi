# IBM Cloud ROKS File CSI Research & Design Proposal

## 1. The Current IBM VPC File CSI Driver

### How It Works

The `ibm-vpc-file-csi-driver` is the official CSI plugin for IBM Cloud File Storage for VPC. It runs two components:

- **vpc-file-csi-controller** — a Deployment that handles provisioning (CreateVolume/DeleteVolume) by calling the IBM Cloud VPC API to create or destroy file shares.
- **vpc-file-csi-node** — a DaemonSet on every worker that handles NodeStageVolume/NodePublishVolume by NFS-mounting the share into the pod.

When a PVC is created referencing an IBM file storage StorageClass, the controller calls the VPC API to create **an entirely new file share** (a new NFS export with its own IP/mount target). The node agent then mounts that share into the pod via NFSv4.1 over TCP 2049.

### What It Supports

| Feature | Status |
|---------|--------|
| Dynamic provisioning | Yes — creates a new VPC file share per PVC |
| Static provisioning | Yes — bind to pre-existing share |
| Volume expansion | Yes — grows the underlying VPC share |
| ReadWriteMany (RWX) | Yes — NFS is inherently multi-reader/writer |
| Snapshots | No |
| Cloning | No |

### Key Quotas & Limits (IBM Cloud VPC)

| Resource | Limit |
|----------|-------|
| File shares per account (all VPCs) | **300** (can request increase) |
| Mount targets per share per zone | 256 |
| Accessor bindings per share | 100 |
| Minimum share size | 10 GB |
| Maximum share size | 32,000 GB |
| Snapshots per zonal share | 750 |
| Snapshots per regional share | 30 |

### Strengths

- **Fully managed** — IBM handles the storage backend; 99.999% availability, 11-nines durability.
- **NFS-native** — no iSCSI or block-level complexity; any pod can mount via standard NFS.
- **RWX by default** — unlike block CSI, file shares naturally support ReadWriteMany.
- **IOPS profiles** — tiered and custom IOPS, adjustable after creation.
- **Encryption** — at-rest encryption standard; optional encryption-in-transit.
- **Integrated with ROKS** — available as a managed add-on, no manual deployment.

### Weaknesses & Pain Points

1. **1:1 PVC-to-share mapping** — every PVC creates a brand-new VPC file share. This is the core architectural problem.
2. **300 share quota per account** — in a busy cluster with many microservices, you hit this fast. Each 1 GiB PVC for a config volume burns a share.
3. **Provisioning latency** — creating a VPC file share via the API takes 30-90+ seconds. Pods block waiting for storage.
4. **Cost overhead** — each share has a minimum 10 GB allocation and its own billing line item. Hundreds of small PVCs = hundreds of minimum-sized shares.
5. **No capacity sharing** — if you provision 100 GB but use 2 GB, the other 98 GB is wasted (not available to other PVCs).
6. **No snapshots or cloning** — limits CI/CD and data protection workflows.
7. **API rate limits** — bulk PVC creation hammers the VPC API; risk of throttling.
8. **Zonal** — shares are zonal; cross-zone access requires accessor bindings (limited to 100).

---

## 2. Community/Open-Source NFS CSI Drivers

### kubernetes-csi/csi-driver-nfs

The official Kubernetes CSI community NFS driver. It assumes you **already have an NFS server** and dynamically provisions PVCs as **subdirectories** on that server.

**How it works:**
- You provide an NFS server address and export path in the StorageClass.
- When a PVC is created, the controller creates a subdirectory under that export (e.g., `/export/pvc-abc123`).
- The node mounts the subdirectory into the pod.
- On PVC deletion (with `reclaimPolicy: Delete`), the subdirectory is removed.

**StorageClass example:**
```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: nfs-csi
provisioner: nfs.csi.k8s.io
parameters:
  server: nfs-server.example.com
  share: /export
  subDir: ${pvc.metadata.namespace}/${pvc.metadata.name}
reclaimPolicy: Delete
volumeBindingMode: Immediate
```

**Strengths:** Near-instant provisioning (mkdir vs. API call), no per-PVC cost, unlimited PVCs from one share, simple architecture.

**Weaknesses:** No quota enforcement (any PVC can consume the whole share), no IOPS isolation, you manage the NFS server yourself, no integration with cloud storage APIs.

### kubernetes-sigs/nfs-subdir-external-provisioner

The older, non-CSI predecessor. Same concept — subdirectories on an existing NFS export — but implemented as an external provisioner rather than a CSI driver. Still widely used. Same strengths and weaknesses as csi-driver-nfs, plus it's not CSI-native (no volume expansion, no topology awareness).

### AWS EFS CSI Driver (for comparison)

AWS took a hybrid approach: EFS is a managed elastic NFS filesystem, and the CSI driver provisions **access points** (not new filesystems) per PVC. Each access point is essentially a scoped subdirectory with its own UID/GID enforcement.

- One EFS filesystem, many access points (up to 1,000 per filesystem).
- No capacity enforcement (EFS is elastic).
- Each access point = a subdirectory with POSIX identity isolation.

This is the closest existing model to what we want to build for IBM Cloud.

---

## 3. The VMware Analogy: VMDKs in an NFS Datastore

VMware's model is instructive. In VMware:

- An **NFS datastore** is a single NFS export mounted by all ESXi hosts.
- **VMDKs** (virtual disks) are just files within that datastore.
- You can have hundreds of VMDKs in a single NFS datastore.
- Thin provisioning means VMDKs only consume actual used space.
- The datastore is the unit of capacity planning; VMDKs share the pool.
- vSphere manages the mapping of VM → VMDK → datastore path.

**The analogy for Kubernetes:**

| VMware Concept | Proposed K8s Concept |
|----------------|---------------------|
| NFS Datastore | IBM VPC File Share ("Pool Share") |
| VMDK file | Subdirectory per PVC |
| vSphere | Our CSI controller |
| Thin provisioning | On-demand subdirectory creation |
| Datastore capacity | Share capacity (expandable) |
| Multiple VMDKs per datastore | Multiple PVCs per share |

---

## 4. Proposed Design: Pool-Based File CSI Driver

### Core Concept

Instead of creating one VPC file share per PVC, we create a **pool of large VPC file shares** and carve out **subdirectories** within them for individual PVCs. The CSI driver manages the mapping.

```
┌─────────────────────────────────────────────┐
│              VPC File Share "Pool-1"         │
│              (e.g., 2 TB, dp2 profile)       │
│                                              │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐    │
│  │ /pvc-aaa │ │ /pvc-bbb │ │ /pvc-ccc │    │
│  │  (5 Gi)  │ │ (10 Gi)  │ │  (1 Gi)  │    │
│  └──────────┘ └──────────┘ └──────────┘    │
│                                              │
│  ┌──────────┐ ┌──────────┐                  │
│  │ /pvc-ddd │ │ /pvc-eee │    ... more      │
│  │ (20 Gi)  │ │  (2 Gi)  │                  │
│  └──────────┘ └──────────┘                  │
└─────────────────────────────────────────────┘

┌─────────────────────────────────────────────┐
│              VPC File Share "Pool-2"         │
│              (e.g., 2 TB, dp2 profile)       │
│  ┌──────────┐ ┌──────────┐                  │
│  │ /pvc-fff │ │ /pvc-ggg │    ... more      │
│  └──────────┘ └──────────┘                  │
└─────────────────────────────────────────────┘
```

### Architecture

```
                    ┌─────────────────────┐
                    │   K8s API Server    │
                    └─────────┬───────────┘
                              │
              PVC created     │    PV bound
                              ▼
                    ┌─────────────────────┐
                    │   CSI Controller    │
                    │                     │
                    │  1. Check pool for  │
                    │     available space │
                    │  2. Pick a share    │
                    │  3. Create subdir   │
                    │  4. Record mapping  │
                    │     in ConfigMap/CR │
                    │  5. Return PV spec  │
                    └─────────┬───────────┘
                              │
                              ▼
                    ┌─────────────────────┐
                    │   Pool Manager      │
                    │                     │
                    │  - Track allocated  │
                    │    vs. free space   │
                    │  - Auto-create new  │
                    │    shares when pool │
                    │    is low           │
                    │  - Expand shares    │
                    │    when needed      │
                    └─────────┬───────────┘
                              │
              NFS mount       │
                              ▼
                    ┌─────────────────────┐
                    │   CSI Node Agent    │
                    │                     │
                    │  Mount share at     │
                    │  subdir path into   │
                    │  pod                │
                    └─────────────────────┘
```

### Key Components

#### 4.1 FileSharePool CRD

A new Custom Resource that defines a pool of shares:

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: general-purpose
spec:
  zone: us-south-1
  profile: dp2               # VPC file storage profile
  shareSize: 2000            # GB per share
  iops: 1000                 # IOPS per share
  maxShares: 10              # Max shares in this pool
  autoExpand: true           # Create new shares when pool is 80% allocated
  expandThreshold: 80        # Percentage
  encryptionInTransit: false
  tags:
    - env:production
    - pool:general
status:
  shares:
    - id: r006-xxxx-1
      totalGB: 2000
      allocatedGB: 1450
      pvcCount: 87
      mountTarget: 10.240.1.5
    - id: r006-xxxx-2
      totalGB: 2000
      allocatedGB: 320
      pvcCount: 15
      mountTarget: 10.240.1.6
  totalCapacityGB: 4000
  totalAllocatedGB: 1770
  totalPVCCount: 102
```

#### 4.2 StorageClass

```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ibm-vpc-file-pool
provisioner: vpc-file-pool.csi.ibm.io
parameters:
  pool: general-purpose       # Reference to FileSharePool
  uid: "1000"                 # Optional: UID for the subdirectory
  gid: "1000"                 # Optional: GID
  permissions: "0755"         # Optional: directory permissions
  quotaEnforcement: "soft"    # soft (advisory) | hard (project quotas)
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowVolumeExpansion: true
```

#### 4.3 PVC Allocation Record (SubVolume CR)

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: SubVolume
metadata:
  name: pvc-abc123
spec:
  pool: general-purpose
  shareID: r006-xxxx-1
  shareMountTarget: 10.240.1.5
  subPath: /pvcs/pvc-abc123
  requestedGB: 5
  uid: 1000
  gid: 1000
status:
  actualUsageGB: 2.3
  created: "2026-02-15T10:30:00Z"
```

### Provisioning Flow (CreateVolume)

1. PVC created → CSI controller receives CreateVolume request.
2. Controller looks up the `FileSharePool` referenced by the StorageClass.
3. **Share selection algorithm:**
   - Find shares in the correct zone with enough remaining allocated capacity.
   - Prefer the share with the most free space (spread) or least free space (pack) — configurable.
   - If no share has room, and `autoExpand` is true, create a new VPC file share via the API.
4. Create the subdirectory on the selected share (NFS mkdir).
5. Optionally set a **project quota** on the subdirectory (XFS/NFS project quotas via `xfs_quota` or NFS server-side enforcement if available).
6. Create a `SubVolume` CR recording the mapping.
7. Return the PV with `volumeAttributes` containing the share's mount target IP and subpath.

### Node Mount Flow (NodePublishVolume)

1. Node agent receives the mount request with volume attributes.
2. Mount the VPC file share at the share-level NFS export (if not already mounted — cache mounts).
3. Bind-mount the specific subdirectory into the pod's volume path.
4. This means each node only has **one NFS mount per share**, not one per PVC — significant performance improvement.

### Quota Enforcement Options

This is the trickiest part, as NFS doesn't natively enforce per-subdirectory quotas. Options:

| Method | Pros | Cons |
|--------|------|------|
| **No enforcement (advisory)** | Simplest; works like EFS | PVCs can overrun; requires monitoring |
| **NFS project quotas** | Kernel-level enforcement | Requires the NFS server to support it; IBM VPC file storage likely doesn't expose this |
| **Periodic du monitoring** | No server changes needed | Soft enforcement only; lag between check and action |
| **Linux quota on loopback** | Per-subdir hard quotas | Complexity; performance overhead |
| **Controller-side tracking** | Track allocations in CRs | Advisory only; pods see full share capacity |

**Recommended approach:** Start with advisory (soft) enforcement — track allocations in SubVolume CRs, report usage via metrics, and alert when a PVC exceeds its allocation. This matches how EFS and the community NFS drivers work. Add hard enforcement later if IBM VPC file storage adds project quota support.

### Handling Volume Expansion

When a PVC requests more capacity:
1. Controller updates the `SubVolume` CR with the new requested size.
2. Controller checks if the share has enough unallocated capacity.
3. If yes, update the allocation — no NFS-level operation needed.
4. If no, either expand the underlying VPC share (API call) or migrate the subdir to a share with more room.

### Handling Volume Deletion

1. Controller receives DeleteVolume.
2. **Reclaim policy = Delete:** Remove the subdirectory contents (`rm -rf`) and the directory itself. Update the share's allocated capacity. Delete the SubVolume CR.
3. **Reclaim policy = Retain:** Mark the SubVolume CR as `retained`. Leave data intact.
4. **Optional archive:** Move to a `.archived/` directory on the share (like nfs-subdir-external-provisioner's `archiveOnDelete`).

---

## 5. Comparison: Current vs. Proposed

| Aspect | Current IBM CSI | Proposed Pool CSI |
|--------|----------------|-------------------|
| **Shares per 100 PVCs** | 100 shares | 1-2 shares |
| **Provisioning time** | 30-90s (API call) | <1s (mkdir) |
| **Quota usage** | 100 of 300 quota | 1-2 of 300 quota |
| **Cost (100 × 10GB PVCs)** | 100 × 10GB shares billed | 1 × 1TB share billed |
| **Minimum waste** | 100 × 10GB minimum | 1 × share size (shared) |
| **IOPS isolation** | Per-share IOPS | Shared IOPS (noisy neighbor) |
| **Failure blast radius** | 1 PVC per share | Many PVCs per share |
| **RWX support** | Yes | Yes |
| **Snapshots** | No (per share) | No (but could snapshot whole pool) |
| **Complexity** | Low (1:1 mapping) | Medium (pool management) |

---

## 6. Risks & Mitigations

### Risk: Noisy Neighbor (IOPS)

Multiple PVCs sharing IOPS on one share. A runaway pod could starve others.

**Mitigation:** Use IOPS-tiered StorageClasses (high-IOPS pool for databases, low-IOPS pool for configs). Monitor per-PVC I/O with cgroup metrics. Implement I/O priority or rate limiting at the pod level via cgroup v2 I/O controllers.

### Risk: Blast Radius

If a VPC file share fails, all PVCs on it are affected.

**Mitigation:** IBM VPC file storage has 99.999% availability with HA-paired nodes. Spread PVCs across multiple shares in the pool. Don't pack too many critical PVCs on one share. Allow "dedicated" pools for mission-critical workloads that want 1:1 behavior.

### Risk: Capacity Overcommit

Without hard quotas, PVCs can overrun their allocation.

**Mitigation:** Monitor actual usage via periodic `du` or `statfs`. Expose metrics to Prometheus. Alert at 80% and 95% of share capacity. Auto-expand shares before they fill. The controller should not allocate more than the share's total capacity (bookkeeping prevents overcommit of *allocations*, even if actual usage isn't enforced).

### Risk: Data Isolation / Security

Pods could potentially traverse up the directory tree if mount is misconfigured.

**Mitigation:** Use bind mounts (not NFS submounts) so the pod's mount root is the subdirectory. Set directory ownership (UID/GID) per PVC. Use PodSecurityAdmission / SecurityContextConstraints to prevent privilege escalation. NFS root squash on the share.

---

## 7. Implementation Roadmap

### Phase 1: MVP (Core Provisioning)
- FileSharePool CRD and controller
- SubVolume CRD for tracking
- CreateVolume: pick share → mkdir → bind-mount
- DeleteVolume: rm subdir → update tracking
- Basic StorageClass with pool reference
- Manual share pre-creation (operator creates shares, adds to pool)

### Phase 2: Auto-Management
- Automatic share creation when pool capacity is low
- Automatic share expansion
- Share rebalancing (spread PVCs across shares)
- Prometheus metrics (pool utilization, per-PVC usage, provisioning latency)

### Phase 3: Advanced Features
- Soft quota enforcement with alerting
- Per-PVC usage reporting via CSI `NodeGetVolumeStats`
- Share tiering (different IOPS profiles in one pool)
- Cross-zone accessor binding support
- Snapshot support (whole-share snapshots with per-PVC restore)
- Migration tool: convert existing 1:1 PVCs to pool-based PVCs

### Phase 4: OpenShift Virtualization Ready (ODF Replacement)

**Why replace ODF?** ODF (OpenShift Data Foundation / Ceph) is the default storage for KubeVirt on ROKS, but it's expensive (3+ dedicated nodes), operationally complex (Ceph tuning, PG balancing), and slow to deploy. NFS file shares give RWX access mode for free — required for live migration — without a storage cluster. The pool CSI driver already handles provisioning and capacity management; Phase 4 fills the remaining gaps (snapshots, clones, DR) to make it a viable ODF replacement for VM workloads.

#### 4a: Snapshots + Clones (P0 — blocks golden image workflows)
- CSI `CreateSnapshot` / `DeleteSnapshot` / `ListSnapshots` implementation
- Two snapshot strategies configurable per pool:
  - `share` — VPC API whole-share snapshot; fast CoW, restores entire share state
  - `copy` — directory-level `cp -a` within the share; per-SubVolume granularity, no snapshot quota cost
- New CRD: `SnapshotRecord` (cluster-scoped) — tracks SubVolume → VPC snapshot mapping, stores snapshot strategy and source metadata
- Volume cloning via `CreateVolume` with `VolumeContentSource` (both snapshot source and volume source)
- New VPC client methods: `CreateShareSnapshot`, `GetShareSnapshot`, `DeleteShareSnapshot`, `ListShareSnapshots`
- `csi-snapshotter` sidecar added to controller deployment
- Capability additions: `CREATE_DELETE_SNAPSHOT`, `LIST_SNAPSHOTS`, `CLONE_VOLUME`

#### 4b: CDI Integration (P1 — required for VM import)
- Mostly validation — CDI already works with filesystem-mode PVCs
- Publish a `StorageProfile` CR for the pool StorageClass (cloneStrategy, access modes, volume mode)
- Smart cloning: CDI auto-detects CSI snapshot/clone capabilities from 4a and prefers CSI clone over host-assisted copy
- Validate `qemu-img` file locking over NFSv4.1 (required for CDI's QCOW2 → raw conversion)
- Test matrix: import from HTTP, registry, upload, and clone-from-existing-VM workflows

#### 4c: Volume Group Snapshots (P2 — multi-disk VM consistency)
- CSI `CreateVolumeGroupSnapshot` / `DeleteVolumeGroupSnapshot` (CSI spec v1.11+, K8s 1.32+ beta)
- Same-share case: naturally crash-consistent — one VPC snapshot covers all subdirectories on the share
- Cross-share case: best-effort — separate VPC snapshots per share, no cross-share consistency guarantee
- Group affinity in allocator: new `groupKey` parameter co-locates multi-disk VM PVCs on the same share to maximize consistency
- `csi-group-snapshotter` sidecar added to controller deployment

#### 4d: Disaster Recovery / Cross-Region Replication (P2 — production DR SLAs)
- Leverage VPC file share async replication (source share → replica share in another zone/region)
- New VPC client methods: `CreateReplicaShare`, `FailoverShare`, `GetShareReplicaStatus`
- New fields on FileSharePool CR: `replication.enabled`, `replication.targetZone`, `replication.cronSpec`, `replication.failoverPolicy`
- Planned failover: quiesce workloads → final sync → promote replica → update SubVolume CRs
- Unplanned failover: promote replica immediately → accept RPO data loss → update SubVolume CRs
- DR reconstruction tool: scan replica share directories → recreate SubVolume CRs in target cluster
- Constraints: ~15-min RPO same-region, ~1-hour RPO cross-region (VPC replication limits)

#### Phase 4 Risks / Limitations
- **NFS performance tax:** VM disk I/O over NFS adds latency vs. block storage; acceptable for general workloads, not for I/O-intensive databases
- **Noisy neighbor IOPS:** shared shares mean one VM can starve others — mitigate with smaller max shares per pool and dedicated VM-only pools
- **Soft quotas only:** no hard per-subdirectory NFS quotas; a runaway VM disk can fill the share
- **Clone duration:** ~7 min for a 40 GB disk at ~100 MB/s NFS throughput (directory-copy strategy)
- **Snapshot quota:** 750 snapshots per zonal share, 30 per regional share (VPC limits)
- **Replication lag:** 15 min to 1 hour RPO depending on zone/region — not suitable for RPO < 15 min SLAs

---

## 8. Key Design Decisions to Make

1. **Go vs. other language?** — Go is standard for CSI drivers; both IBM's driver and the community drivers are in Go.

2. **CRD vs. ConfigMap for state?** — CRDs are strongly preferred: typed, watchable, support status subresource, and can be managed by `kubectl`.

3. **Packing strategy?** — "Bin-pack" (fill shares before starting new ones) saves shares but increases blast radius. "Spread" (distribute evenly) is safer but uses more shares. Make it configurable.

4. **NFS mount caching on nodes?** — Mount the share once per node and bind-mount subdirs into pods. This reduces NFS connection count and is how most NFS CSI drivers work.

5. **How to handle zone affinity?** — Each pool is zone-scoped. Topology-aware provisioning (CSI topology keys) ensures pods get storage in their zone.

6. **Naming convention for subdirs?** — Use PV name as the subdirectory name (e.g., `/pvcs/pvc-<uuid>`). This is unique and traceable.

---

## Sources

- [IBM/ibm-vpc-file-csi-driver (GitHub)](https://github.com/IBM/ibm-vpc-file-csi-driver)
- [IBM Cloud File Storage for VPC - About](https://cloud.ibm.com/docs/vpc?topic=vpc-file-storage-vpc-about)
- [IBM Cloud VPC Quotas](https://cloud.ibm.com/docs/vpc?topic=vpc-quotas) / [GitHub source](https://github.com/ibm-cloud-docs/vpc/blob/master/quotas.md)
- [IBM Cloud File Storage for VPC - FAQ](https://cloud.ibm.com/docs/vpc?topic=vpc-file-storage-vpc-faqs)
- [IBM Cloud File Storage for VPC - Profiles](https://cloud.ibm.com/docs/vpc?topic=vpc-file-storage-profiles)
- [Enabling the VPC File CSI add-on](https://cloud.ibm.com/docs/openshift?topic=openshift-storage-file-vpc-install)
- [kubernetes-csi/csi-driver-nfs (GitHub)](https://github.com/kubernetes-csi/csi-driver-nfs)
- [csi-driver-nfs parameters](https://github.com/kubernetes-csi/csi-driver-nfs/blob/master/docs/driver-parameters.md)
- [kubernetes-sigs/nfs-subdir-external-provisioner (GitHub)](https://github.com/kubernetes-sigs/nfs-subdir-external-provisioner)
- [AWS EFS CSI Driver (GitHub)](https://github.com/kubernetes-sigs/aws-efs-csi-driver)
- [AWS EFS CSI Dynamic Provisioning Blog](https://aws.amazon.com/blogs/containers/introducing-efs-csi-dynamic-provisioning/)
