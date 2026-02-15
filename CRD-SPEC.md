# CRD Specification Reference

## API Group and Version

```
Group:   storage.ibmcloud.io
Version: v1alpha1
```

Register in `api/v1alpha1/groupversion_info.go`:
```go
package v1alpha1

import (
    "k8s.io/apimachinery/pkg/runtime/schema"
    "sigs.k8s.io/controller-runtime/pkg/scheme"
)

var (
    GroupVersion = schema.GroupVersion{Group: "storage.ibmcloud.io", Version: "v1alpha1"}
    SchemeBuilder = &scheme.Builder{GroupVersion: GroupVersion}
    AddToScheme = SchemeBuilder.AddToScheme
)
```

---

## FileSharePool

Cluster-scoped resource. Defines a pool of VPC file shares that back PVC subdirectories.

### Go Type Definition

```go
// api/v1alpha1/filesharespool_types.go
package v1alpha1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=fsp
// +kubebuilder:printcolumn:name="Zone",type=string,JSONPath=`.spec.zone`
// +kubebuilder:printcolumn:name="Profile",type=string,JSONPath=`.spec.profile`
// +kubebuilder:printcolumn:name="Shares",type=integer,JSONPath=`.status.shareCount`
// +kubebuilder:printcolumn:name="Capacity",type=string,JSONPath=`.status.totalCapacityGB`
// +kubebuilder:printcolumn:name="Allocated",type=string,JSONPath=`.status.totalAllocatedGB`
// +kubebuilder:printcolumn:name="PVCs",type=integer,JSONPath=`.status.totalPVCCount`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
type FileSharePool struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   FileSharePoolSpec   `json:"spec,omitempty"`
    Status FileSharePoolStatus `json:"status,omitempty"`
}

type FileSharePoolSpec struct {
    // Zone is the VPC availability zone for shares in this pool.
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Pattern=`^[a-z]{2}-[a-z]+-\d+$`
    Zone string `json:"zone"`

    // Profile is the VPC file storage profile (e.g., "dp2", "custom").
    // +kubebuilder:validation:Required
    Profile string `json:"profile"`

    // ShareSizeGB is the size in GB for each share created in the pool.
    // +kubebuilder:validation:Minimum=10
    // +kubebuilder:validation:Maximum=32000
    // +kubebuilder:default=1000
    ShareSizeGB int64 `json:"shareSizeGB"`

    // IOPS is the IOPS allocation per share. Only used with custom profiles.
    // +kubebuilder:validation:Minimum=100
    // +optional
    IOPS *int64 `json:"iops,omitempty"`

    // MaxShares is the maximum number of VPC file shares the pool may create.
    // Prevents runaway share creation. Remember the account-wide 300 share quota.
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=100
    // +kubebuilder:default=10
    MaxShares int32 `json:"maxShares"`

    // InitialShares is the number of shares to pre-create when the pool is first reconciled.
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:default=1
    InitialShares int32 `json:"initialShares"`

    // AutoExpand controls whether the pool manager automatically creates new shares
    // when existing shares are filling up.
    // +kubebuilder:default=true
    AutoExpand bool `json:"autoExpand"`

    // ExpandThresholdPercent is the pool-wide allocation percentage that triggers
    // creation of a new share. Range 50-95.
    // +kubebuilder:validation:Minimum=50
    // +kubebuilder:validation:Maximum=95
    // +kubebuilder:default=80
    ExpandThresholdPercent int32 `json:"expandThresholdPercent"`

    // AllocationStrategy controls how PVCs are distributed across shares.
    // "spread" distributes evenly (lower blast radius).
    // "binpack" fills shares before using the next (fewer shares used).
    // +kubebuilder:validation:Enum=spread;binpack
    // +kubebuilder:default=spread
    AllocationStrategy string `json:"allocationStrategy"`

    // EncryptionInTransit enables NFS encryption in transit (NFSv4.1 + Kerberos).
    // Reduces throughput by ~20-30%.
    // +kubebuilder:default=false
    EncryptionInTransit bool `json:"encryptionInTransit"`

    // DefaultPermissions is the Unix permissions for new PVC subdirectories.
    // +kubebuilder:default="0755"
    DefaultPermissions string `json:"defaultPermissions"`

    // DefaultUID is the default owner UID for new subdirectories.
    // +optional
    DefaultUID *int64 `json:"defaultUID,omitempty"`

    // DefaultGID is the default owner GID for new subdirectories.
    // +optional
    DefaultGID *int64 `json:"defaultGID,omitempty"`

    // ResourceGroup is the IBM Cloud resource group ID for created shares.
    // +optional
    ResourceGroup string `json:"resourceGroup,omitempty"`

    // Tags are IBM Cloud tags applied to created shares.
    // +optional
    Tags []string `json:"tags,omitempty"`

    // MountOptions are additional NFS mount options applied by the node agent.
    // +optional
    MountOptions []string `json:"mountOptions,omitempty"`
}

// NOTE: No secretRef field. Authentication is handled globally by secret-common-lib,
// which reads from the cluster's existing ibm-cloud-credentials or storage-secret-store
// secret. See API-KEY-SETUP.md for details.

// --- Status ---

type FileSharePoolStatus struct {
    // Phase is the overall pool state.
    // +kubebuilder:validation:Enum=Initializing;Ready;Expanding;Degraded;Full
    Phase string `json:"phase,omitempty"`

    // Shares lists all VPC file shares managed by this pool.
    Shares []PoolShareStatus `json:"shares,omitempty"`

    // ShareCount is the current number of shares.
    ShareCount int32 `json:"shareCount"`

    // TotalCapacityGB is the sum of all share sizes.
    TotalCapacityGB int64 `json:"totalCapacityGB"`

    // TotalAllocatedGB is the sum of all SubVolume requested sizes.
    TotalAllocatedGB int64 `json:"totalAllocatedGB"`

    // TotalPVCCount is the total number of active SubVolumes across all shares.
    TotalPVCCount int32 `json:"totalPVCCount"`

    // Conditions follows the standard Kubernetes conditions pattern.
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    // LastReconcileTime is when the pool was last reconciled.
    // +optional
    LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`
}

type PoolShareStatus struct {
    // ShareID is the VPC file share ID (e.g., "r006-xxxx").
    ShareID string `json:"shareID"`

    // ShareName is the VPC file share name.
    ShareName string `json:"shareName"`

    // MountTargetIP is the NFS server IP for this share.
    MountTargetIP string `json:"mountTargetIP"`

    // MountTargetID is the VPC mount target resource ID.
    MountTargetID string `json:"mountTargetID"`

    // TotalGB is the provisioned size of this share.
    TotalGB int64 `json:"totalGB"`

    // AllocatedGB is the sum of SubVolume allocations on this share.
    AllocatedGB int64 `json:"allocatedGB"`

    // PVCCount is the number of SubVolumes on this share.
    PVCCount int32 `json:"pvcCount"`

    // State is the share's health state.
    // +kubebuilder:validation:Enum=creating;stable;draining;degraded;deleting
    State string `json:"state"`

    // Zone is the availability zone of this share.
    Zone string `json:"zone"`

    // CreatedAt is when the share was created.
    CreatedAt *metav1.Time `json:"createdAt,omitempty"`
}
```

### Example FileSharePool CR

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: general-purpose
spec:
  zone: us-south-1
  profile: dp2
  shareSizeGB: 2000
  iops: 1000
  maxShares: 10
  initialShares: 2
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  encryptionInTransit: false
  defaultPermissions: "0755"
  defaultUID: 1000
  defaultGID: 1000
  resourceGroup: "abc123-def456"
  tags:
    - "env:production"
    - "team:platform"
  # No secretRef — authentication is handled globally via secret-common-lib
  # (reads from ibm-cloud-credentials or storage-secret-store in kube-system)
  mountOptions:
    - nfsvers=4.1
    - soft
    - timeo=600
    - retrans=3
```

### Reconciler Behavior

The FileSharePool reconciler (in `pkg/k8s/reconciler.go`) watches FileSharePool CRs and:

1. On create: set `Phase: Initializing`, create `initialShares` VPC file shares, move to `Phase: Ready`.
2. On reconcile: check allocation vs. threshold, create new shares if needed, update Status.
3. Sets a finalizer (`storage.ibmcloud.io/pool-protection`) to prevent deletion while SubVolumes exist.
4. On delete (finalizer): verify no SubVolumes reference this pool, then delete all VPC file shares, remove finalizer.

---

## SubVolume

Cluster-scoped resource. Tracks a single PVC's allocation within a pool share.

### Go Type Definition

```go
// api/v1alpha1/subvolume_types.go
package v1alpha1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=sv
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolName`
// +kubebuilder:printcolumn:name="Share",type=string,JSONPath=`.spec.shareID`,priority=1
// +kubebuilder:printcolumn:name="Size",type=string,JSONPath=`.spec.requestedGB`
// +kubebuilder:printcolumn:name="PVC",type=string,JSONPath=`.spec.pvcName`
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.pvcNamespace`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
type SubVolume struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   SubVolumeSpec   `json:"spec,omitempty"`
    Status SubVolumeStatus `json:"status,omitempty"`
}

type SubVolumeSpec struct {
    // PoolName references the FileSharePool this SubVolume belongs to.
    // +kubebuilder:validation:Required
    PoolName string `json:"poolName"`

    // ShareID is the VPC file share ID hosting this SubVolume.
    // +kubebuilder:validation:Required
    ShareID string `json:"shareID"`

    // ShareMountTargetIP is the NFS mount target IP.
    // Denormalized here for fast lookups during NodePublishVolume.
    // +kubebuilder:validation:Required
    ShareMountTargetIP string `json:"shareMountTargetIP"`

    // SubPath is the subdirectory path within the share (e.g., "/pvcs/pvc-abc123").
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Pattern=`^/pvcs/pvc-[a-f0-9-]+$`
    SubPath string `json:"subPath"`

    // RequestedGB is the capacity requested by the PVC.
    // This is an allocation tracker, not a hard quota.
    // +kubebuilder:validation:Minimum=1
    RequestedGB int64 `json:"requestedGB"`

    // PVName is the name of the PersistentVolume bound to this SubVolume.
    // +kubebuilder:validation:Required
    PVName string `json:"pvName"`

    // PVCName is the name of the PersistentVolumeClaim.
    // +kubebuilder:validation:Required
    PVCName string `json:"pvcName"`

    // PVCNamespace is the namespace of the PersistentVolumeClaim.
    // +kubebuilder:validation:Required
    PVCNamespace string `json:"pvcNamespace"`

    // UID is the Unix UID for the subdirectory owner.
    // +optional
    UID *int64 `json:"uid,omitempty"`

    // GID is the Unix GID for the subdirectory owner.
    // +optional
    GID *int64 `json:"gid,omitempty"`

    // Permissions is the Unix permissions string (e.g., "0755").
    // +optional
    Permissions string `json:"permissions,omitempty"`

    // ReclaimPolicy determines what happens to the subdirectory when the PVC is deleted.
    // +kubebuilder:validation:Enum=Delete;Retain;Archive
    // +kubebuilder:default=Delete
    ReclaimPolicy string `json:"reclaimPolicy"`
}

type SubVolumeStatus struct {
    // Phase is the SubVolume lifecycle state.
    // +kubebuilder:validation:Enum=Creating;Bound;Expanding;Deleting;Retained;Archived;Failed
    Phase string `json:"phase,omitempty"`

    // ActualUsageBytes is the last measured disk usage of the subdirectory.
    // Updated periodically by the controller (not real-time).
    // +optional
    ActualUsageBytes *int64 `json:"actualUsageBytes,omitempty"`

    // LastUsageCheck is when ActualUsageBytes was last updated.
    // +optional
    LastUsageCheck *metav1.Time `json:"lastUsageCheck,omitempty"`

    // Conditions for detailed state tracking.
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    // CreatedAt is when the subdirectory was created on the share.
    // +optional
    CreatedAt *metav1.Time `json:"createdAt,omitempty"`
}
```

### Example SubVolume CR

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: SubVolume
metadata:
  name: pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab
  labels:
    storage.ibmcloud.io/pool: general-purpose
    storage.ibmcloud.io/share-id: r006-xxxx-1
  ownerReferences:
    - apiVersion: storage.ibmcloud.io/v1alpha1
      kind: FileSharePool
      name: general-purpose
      uid: <pool-uid>
spec:
  poolName: general-purpose
  shareID: r006-xxxx-1
  shareMountTargetIP: "10.240.1.5"
  subPath: /pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab
  requestedGB: 5
  pvName: pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab
  pvcName: my-app-data
  pvcNamespace: default
  uid: 1000
  gid: 1000
  permissions: "0755"
  reclaimPolicy: Delete
status:
  phase: Bound
  actualUsageBytes: 2415919104
  lastUsageCheck: "2026-02-15T12:00:00Z"
  createdAt: "2026-02-15T10:30:00Z"
```

### Labels Convention

All SubVolumes MUST have these labels (used for efficient list queries):

```yaml
labels:
  storage.ibmcloud.io/pool: <pool-name>
  storage.ibmcloud.io/share-id: <share-id>
```

The pool manager uses label selectors to find all SubVolumes for a given pool or share without listing every SubVolume in the cluster.

### Owner References

Each SubVolume should have an ownerReference to its FileSharePool. This means:
- `kubectl get subvolumes` shows the pool relationship.
- If a pool is deleted (after finalizer checks), orphan SubVolumes are garbage collected.

### Naming Convention

SubVolume CRs are named after the PV name they back, which is typically the PVC UID:
```
name: pvc-<uuid>
```
This guarantees uniqueness and makes it easy to correlate PVs ↔ SubVolumes.

---

## StorageClass Parameters

The CSI driver reads these from the StorageClass `parameters` map:

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `pool` | Yes | — | Name of the FileSharePool CR |
| `uid` | No | Pool default | Unix UID for the subdirectory |
| `gid` | No | Pool default | Unix GID for the subdirectory |
| `permissions` | No | Pool default | Unix permissions (e.g., "0755") |
| `reclaimAction` | No | `delete` | `delete`, `retain`, or `archive` |

Example:
```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: ibm-vpc-file-pool
provisioner: vpc-file-pool.csi.ibm.io
parameters:
  pool: general-purpose
  uid: "1000"
  gid: "1000"
  permissions: "0755"
reclaimPolicy: Delete
volumeBindingMode: Immediate
allowVolumeExpansion: true
mountOptions:
  - nfsvers=4.1
  - soft
  - timeo=600
  - retrans=3
```

---

## CRD Generation

After modifying types, regenerate:

```bash
# Install controller-gen if not present
go install sigs.k8s.io/controller-tools/cmd/controller-gen@latest

# Generate CRD YAML manifests
controller-gen crd paths="./api/..." output:crd:dir=config/crd

# Generate deep copy methods
controller-gen object paths="./api/..."
```

The generated CRD YAMLs go into `config/crd/` and are applied to the cluster with `kubectl apply -f config/crd/`.
