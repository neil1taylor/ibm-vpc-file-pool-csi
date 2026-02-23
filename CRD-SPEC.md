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

    // AccessorZones defines additional zones where pool shares should have mount targets.
    // This enables cross-zone access: nodes in accessor zones can mount shares via
    // zone-local NFS IPs instead of cross-zone traffic.
    // When empty, shares are only accessible from the home zone (spec.zone).
    // +optional
    AccessorZones []AccessorZone `json:"accessorZones,omitempty"`

    // Tiers defines multiple performance tiers within the pool.
    // If empty, the top-level profile/shareSizeGB/iops/maxShares/initialShares fields
    // define an implicit default tier.
    // +optional
    Tiers []ShareTier `json:"tiers,omitempty"`

    // DrainShares lists VPC share IDs that should be drained (evacuated).
    // Shares in this list will be marked as "draining" and excluded from new allocations.
    // Once all SubVolumes are removed from a draining share, it is considered fully drained.
    // +optional
    DrainShares []string `json:"drainShares,omitempty"`

    // GoldenImages configures automatic golden image synchronization.
    // When enabled, the controller discovers OS images from CDI DataImportCrons
    // and maintains ready-to-clone golden image PVCs in target namespaces.
    // Not needed when the pool StorageClass is the cluster default (CDI handles natively).
    // +optional
    GoldenImages *GoldenImageConfig `json:"goldenImages,omitempty"`
}

// GoldenImageConfig configures automatic golden image synchronization for KubeVirt.
type GoldenImageConfig struct {
    // Enabled activates the golden image syncer for this pool.
    Enabled bool `json:"enabled"`

    // TargetNamespaces lists namespaces where golden image PVCs and Templates are created.
    // +kubebuilder:validation:MinItems=1
    TargetNamespaces []string `json:"targetNamespaces"`

    // ImageFilter limits syncing to images whose names contain one of these substrings.
    // Empty means sync all discovered images.
    // +optional
    ImageFilter []string `json:"imageFilter,omitempty"`

    // RefreshInterval is how often the syncer checks for new images (e.g. "24h", "1h").
    // +kubebuilder:default="24h"
    // +optional
    RefreshInterval string `json:"refreshInterval,omitempty"`

    // ConverterImage is the container image used for qcow2-to-raw conversion jobs.
    // +kubebuilder:default="quay.io/centos/centos:stream9"
    // +optional
    ConverterImage string `json:"converterImage,omitempty"`

    // PVCSizeGB is the size of golden image PVCs in GB.
    // +kubebuilder:validation:Minimum=5
    // +kubebuilder:default=15
    // +optional
    PVCSizeGB int64 `json:"pvcSizeGB,omitempty"`
}

// AccessorZone defines a zone where pool shares should have additional mount targets.
type AccessorZone struct {
    // Zone is the VPC availability zone (e.g., "us-south-2").
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Pattern=`^[a-z]{2}-[a-z]+-\d+$`
    Zone string `json:"zone"`

    // SubnetID is the VPC subnet in the accessor zone for mount target IP allocation.
    // +kubebuilder:validation:Required
    SubnetID string `json:"subnetID"`
}

// ShareTier defines the VPC share configuration for a performance tier within the pool.
type ShareTier struct {
    // Name is the tier identifier referenced from StorageClass parameters.
    // +kubebuilder:validation:Required
    // +kubebuilder:validation:Pattern=`^[a-z0-9-]+$`
    Name string `json:"name"`

    // Profile is the VPC file storage profile (e.g., "dp2", "custom").
    // +kubebuilder:validation:Required
    Profile string `json:"profile"`

    // ShareSizeGB is the size in GB for shares in this tier.
    // +kubebuilder:validation:Minimum=10
    // +kubebuilder:validation:Maximum=32000
    ShareSizeGB int64 `json:"shareSizeGB"`

    // IOPS is the IOPS allocation per share. Only used with custom profiles.
    // +optional
    IOPS *int64 `json:"iops,omitempty"`

    // MaxShares is the maximum number of shares for this tier.
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=100
    MaxShares int32 `json:"maxShares"`

    // InitialShares is the number of shares to pre-create for this tier.
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:default=1
    InitialShares int32 `json:"initialShares"`
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

    // DrainStatus tracks the progress of share draining operations.
    // +optional
    DrainStatus []ShareDrainStatus `json:"drainStatus,omitempty"`

    // GoldenImages tracks the sync state for golden images managed by this pool.
    // +optional
    GoldenImages []GoldenImageStatus `json:"goldenImages,omitempty"`

    // Conditions follows the standard Kubernetes conditions pattern.
    // +optional
    Conditions []metav1.Condition `json:"conditions,omitempty"`

    // LastReconcileTime is when the pool was last reconciled.
    // +optional
    LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`
}

// GoldenImageStatus tracks the sync state for a single golden image.
type GoldenImageStatus struct {
    // Name is the image identifier (e.g. "centos-stream9").
    Name string `json:"name"`

    // SourceURL is the container registry URL for this image.
    // +optional
    SourceURL string `json:"sourceURL,omitempty"`

    // Namespaces tracks per-namespace sync state.
    // +optional
    Namespaces []GoldenImageNamespaceStatus `json:"namespaces,omitempty"`
}

// GoldenImageNamespaceStatus tracks the sync state for a golden image in a single namespace.
type GoldenImageNamespaceStatus struct {
    // Namespace is the target namespace.
    Namespace string `json:"namespace"`

    // Phase is the sync state: Pending, Syncing, Ready, Failed.
    // +kubebuilder:validation:Enum=Pending;Syncing;Ready;Failed
    Phase string `json:"phase"`

    // PVCName is the name of the golden image PVC.
    // +optional
    PVCName string `json:"pvcName,omitempty"`

    // TemplateName is the name of the OpenShift VM Template.
    // +optional
    TemplateName string `json:"templateName,omitempty"`

    // LastSyncTime is when this image was last synced.
    // +optional
    LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`
}

// ShareDrainStatus tracks the draining progress for a single share.
type ShareDrainStatus struct {
    // ShareID is the VPC file share ID being drained.
    ShareID string `json:"shareID"`

    // RemainingSubVolumes is the number of SubVolumes still on this share.
    RemainingSubVolumes int32 `json:"remainingSubVolumes"`

    // Drained is true when the share has zero SubVolumes remaining.
    Drained bool `json:"drained"`

    // DrainStartedAt is when the drain was initiated.
    // +optional
    DrainStartedAt *metav1.Time `json:"drainStartedAt,omitempty"`
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

    // ExportPath is the NFS export path for this share (e.g. "/share_abc123").
    // VPC access mode shares use a per-share export path under a shared FQDN.
    // +optional
    ExportPath string `json:"exportPath,omitempty"`

    // TotalGB is the provisioned size of this share.
    TotalGB int64 `json:"totalGB"`

    // AllocatedGB is the sum of SubVolume allocations on this share.
    AllocatedGB int64 `json:"allocatedGB"`

    // PVCCount is the number of SubVolumes on this share.
    PVCCount int32 `json:"pvcCount"`

    // State is the share's health state.
    // +kubebuilder:validation:Enum=creating;stable;draining;degraded;deleting
    State string `json:"state"`

    // Tier is the name of the ShareTier this share belongs to. Empty for pools without tiers.
    // +optional
    Tier string `json:"tier,omitempty"`

    // Zone is the availability zone of this share.
    Zone string `json:"zone"`

    // MountTargets records mount targets across all zones (home + accessor).
    // When empty, only the primary MountTargetIP/MountTargetID are used (backward compat).
    // +optional
    MountTargets []ZoneMountTarget `json:"mountTargets,omitempty"`

    // CreatedAt is when the share was created.
    CreatedAt *metav1.Time `json:"createdAt,omitempty"`
}

// ZoneMountTarget records a mount target created in a specific zone.
type ZoneMountTarget struct {
    // Zone is the VPC availability zone of this mount target.
    Zone string `json:"zone"`

    // MountTargetID is the VPC mount target resource ID.
    MountTargetID string `json:"mountTargetID"`

    // MountTargetIP is the NFS server IP in this zone.
    MountTargetIP string `json:"mountTargetIP"`

    // ExportPath is the NFS export path for this zone's mount target.
    // +optional
    ExportPath string `json:"exportPath,omitempty"`
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

### Example FileSharePool CR with Cross-Zone Accessors

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: cross-zone-pool
spec:
  zone: us-south-1                  # Home zone — shares are created here
  profile: dp2
  shareSizeGB: 2000
  maxShares: 10
  initialShares: 1
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultPermissions: "0755"
  accessorZones:                     # Mount targets created in these zones too
    - zone: us-south-2
      subnetID: "0717-xxxx-yyyy"     # Subnet in us-south-2
    - zone: us-south-3
      subnetID: "0727-aaaa-bbbb"     # Subnet in us-south-3
```

When a share is created, mount targets are provisioned in the home zone and all accessor zones.
The PV volumeAttributes include zone-keyed server IPs: `server.us-south-1`, `server.us-south-2`, etc.
The node agent selects the IP matching its own zone for NFS mounts.

### Example FileSharePool CR with Tiers

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: multi-tier-pool
spec:
  zone: us-south-1
  # Top-level profile/shareSizeGB/maxShares are ignored when tiers are defined.
  profile: dp2
  shareSizeGB: 1000
  maxShares: 10
  initialShares: 1
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultPermissions: "0755"
  tiers:
    - name: standard
      profile: dp2
      shareSizeGB: 2000
      maxShares: 10
      initialShares: 2
    - name: high-iops
      profile: custom
      shareSizeGB: 1000
      iops: 10000
      maxShares: 5
      initialShares: 1
```

When tiers are defined, one StorageClass per tier is auto-created (e.g., `multi-tier-pool-standard`, `multi-tier-pool-high-iops`), each with the `tier` parameter set. If creating StorageClasses manually, include a `tier` key in `parameters` to select which tier to allocate from. If no tiers are defined, the top-level spec fields are used as an implicit default tier.

### Example FileSharePool CR with Golden Images

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: FileSharePool
metadata:
  name: kubevirt-pool
spec:
  zone: eu-de-1
  profile: dp2
  shareSizeGB: 500
  iops: 10000
  maxShares: 5
  initialShares: 1
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultPermissions: "0777"
  defaultUID: 107
  defaultGID: 107
  goldenImages:
    enabled: true
    targetNamespaces:
      - kubevirt-vms
    imageFilter:
      - fedora
      - centos-stream9
    refreshInterval: "24h"
    converterImage: "quay.io/centos/centos:stream9"
    pvcSizeGB: 15
```

When `goldenImages.enabled` is `true`, the golden image syncer discovers CDI DataImportCrons
matching the filter, creates golden PVCs on the pool's StorageClass, runs converter Jobs
(download + qcow2-to-raw conversion), and creates OpenShift VM Templates. Status is tracked
in `status.goldenImages`.

### Example FileSharePool CR with DrainShares

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
  initialShares: 2
  autoExpand: true
  expandThresholdPercent: 80
  allocationStrategy: spread
  defaultPermissions: "0755"
  drainShares:
    - "r006-xxxx-1"       # This share will be evacuated
    - "r006-xxxx-2"       # This share will be evacuated
```

Shares listed in `drainShares` are marked with `state: draining` in the pool status and excluded
from new allocations. The controller tracks drain progress in `status.drainStatus`. Once all
SubVolumes have been removed from a draining share (either by deletion or migration), the share
is considered fully drained.

### Reconciler Behavior

The FileSharePool reconciler (in `pkg/pool/reconciler.go`) watches FileSharePool CRs and:

1. On create: set `Phase: Initializing`, create `initialShares` VPC file shares, move to `Phase: Ready`.
2. Ensure StorageClasses: auto-create a matching StorageClass (or one per tier for tiered pools) with OwnerReference to the pool. Skipped if the `storage.ibmcloud.io/skip-storageclass: "true"` annotation is set.
3. On reconcile: check allocation vs. threshold, create new shares if needed, update Status.
4. Sets a finalizer (`storage.ibmcloud.io/pool-protection`) to prevent deletion while SubVolumes exist.
5. On delete (finalizer): verify no SubVolumes reference this pool, then delete all VPC file shares, remove finalizer. Auto-created StorageClasses are garbage-collected via OwnerReference.

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

    // ShareMountTargetIP is the NFS mount target IP or FQDN.
    // Denormalized here for fast lookups during NodePublishVolume.
    // +kubebuilder:validation:Required
    ShareMountTargetIP string `json:"shareMountTargetIP"`

    // ShareExportPath is the NFS export path for the share.
    // VPC access mode uses per-share export paths under a shared FQDN.
    // +optional
    ShareExportPath string `json:"shareExportPath,omitempty"`

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

    // SourceVolume is the name of the source SubVolume this was cloned from.
    // Empty for non-clone SubVolumes.
    // +optional
    SourceVolume string `json:"sourceVolume,omitempty"`

    // SourceShareID is the VPC file share ID of the source SubVolume.
    // Populated when the clone source is on a different share than the target.
    // +optional
    SourceShareID string `json:"sourceShareID,omitempty"`
}

type SubVolumeStatus struct {
    // Phase is the SubVolume lifecycle state.
    // +kubebuilder:validation:Enum=Creating;Cloning;Bound;Expanding;Deleting;Retained;Archived;Failed
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

    // CloneStatus tracks the progress of a clone operation.
    // Empty for non-clone SubVolumes.
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

### Example Cloned SubVolume CR

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: SubVolume
metadata:
  name: pvc-f1e2d3c4-9876-54ba-fedc-ba0987654321
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
  subPath: /pvcs/pvc-f1e2d3c4-9876-54ba-fedc-ba0987654321
  requestedGB: 5
  pvName: pvc-f1e2d3c4-9876-54ba-fedc-ba0987654321
  pvcName: my-app-data-clone
  pvcNamespace: default
  uid: 1000
  gid: 1000
  permissions: "0755"
  reclaimPolicy: Delete
  sourceVolume: pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab      # Cloned from this SubVolume
  sourceShareID: r006-xxxx-1                                    # Source share ID (if cross-share)
status:
  phase: Cloning
  cloneStatus: InProgress
  cloneProgress:
    bytesCopied: 1207959552
    totalBytes: 2415919104
    startedAt: "2026-02-15T11:00:00Z"
  createdAt: "2026-02-15T11:00:00Z"
```

When `cloneStatus` transitions to `Complete`, the SubVolume `phase` moves from `Cloning` to `Bound`.
If the clone fails, `cloneStatus` is set to `Failed` and `cloneProgress.error` records the reason.

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

## Snapshot

Cluster-scoped resource. Tracks a point-in-time directory-level copy of a SubVolume. Created by the CSI snapshot controller when a VolumeSnapshot is requested.

### Go Type Definition

```go
// api/v1alpha1/snapshot_types.go
package v1alpha1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=snap
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolName`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceSubVolume`
// +kubebuilder:printcolumn:name="Size",type=integer,JSONPath=`.spec.sizeGB`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Ready",type=boolean,JSONPath=`.status.readyToUse`

// Snapshot tracks a point-in-time directory-level copy of a SubVolume.
type Snapshot struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   SnapshotSpec   `json:"spec,omitempty"`
    Status SnapshotStatus `json:"status,omitempty"`
}

type SnapshotSpec struct {
    // SourceSubVolume is the name of the SubVolume this snapshot was taken from.
    // +kubebuilder:validation:Required
    SourceSubVolume string `json:"sourceSubVolume"`

    // PoolName references the FileSharePool containing the source volume.
    // +kubebuilder:validation:Required
    PoolName string `json:"poolName"`

    // ShareID is the VPC file share ID where the snapshot resides.
    // +kubebuilder:validation:Required
    ShareID string `json:"shareID"`

    // ShareMountTargetIP is the NFS mount target IP for the share.
    // +kubebuilder:validation:Required
    ShareMountTargetIP string `json:"shareMountTargetIP"`

    // SnapshotPath is the directory path of the snapshot (e.g., "/pvcs/.snapshots/snap-xxx").
    // +kubebuilder:validation:Required
    SnapshotPath string `json:"snapshotPath"`

    // SourceSubPath is the original SubVolume directory path (e.g., "/pvcs/pvc-xxx").
    // +kubebuilder:validation:Required
    SourceSubPath string `json:"sourceSubPath"`

    // SizeGB is the allocated size of the snapshot in GB.
    // +kubebuilder:validation:Minimum=1
    SizeGB int64 `json:"sizeGB"`
}

type SnapshotStatus struct {
    // Phase is the snapshot lifecycle state.
    // +kubebuilder:validation:Enum=Creating;Ready;Deleting;Failed
    Phase string `json:"phase,omitempty"`

    // ReadyToUse indicates whether the snapshot is complete and available for restore.
    ReadyToUse bool `json:"readyToUse"`

    // SizeBytes is the actual size of the snapshot data.
    // +optional
    SizeBytes int64 `json:"sizeBytes,omitempty"`

    // CreationTime is when the snapshot was created.
    // +optional
    CreationTime *metav1.Time `json:"creationTime,omitempty"`
}
```

### Example Snapshot CR

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: Snapshot
metadata:
  name: snap-a1b2c3d4-5678-90ab-cdef-1234567890ab
  labels:
    storage.ibmcloud.io/pool: general-purpose
    storage.ibmcloud.io/share-id: r006-xxxx-1
    storage.ibmcloud.io/source-subvolume: pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab
  ownerReferences:
    - apiVersion: storage.ibmcloud.io/v1alpha1
      kind: FileSharePool
      name: general-purpose
      uid: <pool-uid>
spec:
  sourceSubVolume: pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab
  poolName: general-purpose
  shareID: r006-xxxx-1
  shareMountTargetIP: "10.240.1.5"
  snapshotPath: /pvcs/.snapshots/snap-a1b2c3d4-5678-90ab-cdef-1234567890ab
  sourceSubPath: /pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab
  sizeGB: 5
status:
  phase: Ready
  readyToUse: true
  sizeBytes: 2415919104
  creationTime: "2026-02-15T12:00:00Z"
```

### Snapshot Lifecycle

1. **Creating**: The controller copies the SubVolume directory to the snapshot path. During this phase, `readyToUse` is `false`.
2. **Ready**: The copy is complete. `readyToUse` is `true` and the snapshot can be used as a source for clone operations or restores.
3. **Deleting**: The snapshot directory is being removed.
4. **Failed**: The copy operation failed. Check conditions for details.

Snapshots reside on the same VPC file share as the source SubVolume and are stored under a `.snapshots` subdirectory to avoid collision with PVC directories.

---

## VolumeGroupSnapshot

Cluster-scoped resource. Coordinates a group of SubVolume snapshots to achieve application-consistent or crash-consistent point-in-time copies across multiple volumes.

### Go Type Definition

```go
// api/v1alpha1/volumegroupsnapshot_types.go
package v1alpha1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=vgs
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.poolName`
// +kubebuilder:printcolumn:name="Members",type=integer,JSONPath=`.status.memberCount`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyCount`
// +kubebuilder:printcolumn:name="Consistency",type=string,JSONPath=`.status.consistencyLevel`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// VolumeGroupSnapshot tracks a coordinated group of SubVolume snapshots.
type VolumeGroupSnapshot struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   VolumeGroupSnapshotSpec   `json:"spec,omitempty"`
    Status VolumeGroupSnapshotStatus `json:"status,omitempty"`
}

// VolumeGroupSnapshotSpec defines the desired state of a VolumeGroupSnapshot.
type VolumeGroupSnapshotSpec struct {
    // PoolName references the FileSharePool containing the source volumes.
    // All source volumes must belong to this pool.
    // +kubebuilder:validation:Required
    PoolName string `json:"poolName"`

    // SourcePVCs lists the SubVolume names to include in the group snapshot.
    // +optional
    SourcePVCs []string `json:"sourcePVCs,omitempty"`

    // CopyOrder defines the order in which SubVolumes are copied.
    // If empty, copies are executed in alphabetical order of source names.
    // +optional
    CopyOrder []string `json:"copyOrder,omitempty"`

    // FailurePolicy controls behavior when a member snapshot fails.
    // "Abort" stops immediately and rolls back completed snapshots.
    // "Continue" finishes remaining members and marks the group as PartialFailure.
    // +kubebuilder:validation:Enum=Abort;Continue
    // +kubebuilder:default=Abort
    FailurePolicy string `json:"failurePolicy"`

    // PreSnapshotHooks are hooks executed before the group snapshot operation begins.
    // +optional
    PreSnapshotHooks []Hook `json:"preSnapshotHooks,omitempty"`

    // PostSnapshotHooks are hooks executed after the group snapshot operation completes.
    // +optional
    PostSnapshotHooks []Hook `json:"postSnapshotHooks,omitempty"`
}

// VolumeGroupSnapshotStatus defines the observed state of a VolumeGroupSnapshot.
type VolumeGroupSnapshotStatus struct {
    // Phase is the group snapshot lifecycle state.
    // +kubebuilder:validation:Enum=Pending;InProgress;Complete;PartialFailure;Failed
    Phase string `json:"phase,omitempty"`

    // Members lists the individual snapshots in this group.
    Members []GroupSnapshotMember `json:"members,omitempty"`

    // MemberCount is the total number of member snapshots.
    MemberCount int32 `json:"memberCount"`

    // ReadyCount is the number of member snapshots that are ready.
    ReadyCount int32 `json:"readyCount"`

    // FailedCount is the number of member snapshots that failed.
    FailedCount int32 `json:"failedCount"`

    // ConsistencyLevel reports the actual consistency achieved.
    ConsistencyLevel string `json:"consistencyLevel,omitempty"`

    // InconsistencyWindowMs is the duration in milliseconds between
    // the first and last member snapshot copy.
    // +optional
    InconsistencyWindowMs int64 `json:"inconsistencyWindowMs,omitempty"`

    // StartedAt is when the group snapshot operation began.
    // +optional
    StartedAt *metav1.Time `json:"startedAt,omitempty"`

    // CompletedAt is when the group snapshot operation finished.
    // +optional
    CompletedAt *metav1.Time `json:"completedAt,omitempty"`

    // CreationTime is when the group snapshot CR was created.
    // +optional
    CreationTime *metav1.Time `json:"creationTime,omitempty"`

    // HookResults records the outcomes of pre/post hook executions.
    // +optional
    HookResults []HookResult `json:"hookResults,omitempty"`
}

// GroupSnapshotMember tracks the status of a single snapshot within the group.
type GroupSnapshotMember struct {
    // SubVolumeName is the name of the source SubVolume.
    SubVolumeName string `json:"subVolumeName"`

    // SnapshotName is the name of the individual Snapshot CR created.
    SnapshotName string `json:"snapshotName"`

    // Phase is the individual snapshot state.
    // +kubebuilder:validation:Enum=Pending;Creating;Ready;Failed
    Phase string `json:"phase"`

    // Error contains the error message if this member failed.
    // +optional
    Error string `json:"error,omitempty"`
}
```

### Example VolumeGroupSnapshot CR

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: VolumeGroupSnapshot
metadata:
  name: app-consistent-backup-2026-02-15
  labels:
    storage.ibmcloud.io/pool: general-purpose
spec:
  poolName: general-purpose
  sourcePVCs:
    - pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab    # database data volume
    - pvc-b2c3d4e5-6789-01bc-def0-234567890abc    # database WAL volume
  copyOrder:
    - pvc-b2c3d4e5-6789-01bc-def0-234567890abc    # WAL first for consistency
    - pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab    # then data
  failurePolicy: Abort
status:
  phase: Complete
  memberCount: 2
  readyCount: 2
  failedCount: 0
  consistencyLevel: crash-consistent
  inconsistencyWindowMs: 450
  startedAt: "2026-02-15T12:00:00Z"
  completedAt: "2026-02-15T12:00:05Z"
  creationTime: "2026-02-15T12:00:00Z"
  members:
    - subVolumeName: pvc-b2c3d4e5-6789-01bc-def0-234567890abc
      snapshotName: snap-vgs-app-consistent-backup-2026-02-15-0
      phase: Ready
    - subVolumeName: pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab
      snapshotName: snap-vgs-app-consistent-backup-2026-02-15-1
      phase: Ready
```

### Failure Policies

| Policy | Behavior on member failure |
|--------|---------------------------|
| `Abort` | Stop immediately, roll back (delete) all completed member snapshots, set phase to `Failed` |
| `Continue` | Complete remaining members, set phase to `PartialFailure`, keep successful snapshots |

### Consistency Levels

The `consistencyLevel` status field reports what was achieved:
- **crash-consistent**: All member snapshots completed within an acceptable window (`inconsistencyWindowMs`).
- The `inconsistencyWindowMs` field records the actual time delta between the first and last member copy, allowing applications to assess consistency guarantees.

### VolumeGroupSnapshot Lifecycle

1. **Pending**: The group snapshot has been created but member snapshots have not started.
2. **InProgress**: Member snapshots are being created sequentially in `copyOrder`.
3. **Complete**: All member snapshots are `Ready`.
4. **PartialFailure**: Some members succeeded and some failed (only with `failurePolicy: Continue`).
5. **Failed**: The operation failed and was rolled back (with `failurePolicy: Abort`), or all members failed.

---

## ReplicationPolicy

Cluster-scoped resource. Defines a replication relationship between a source FileSharePool on the local cluster and a destination NFS server for cross-region disaster recovery. Uses rsync-based incremental replication over Transit Gateway or VPN.

### Go Type Definition

```go
// api/v1alpha1/replicationpolicy_types.go
package v1alpha1

import (
    metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=rp
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourcePool`
// +kubebuilder:printcolumn:name="Dest",type=string,JSONPath=`.spec.destinationNFSServer`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Last Sync",type=date,JSONPath=`.status.lastSyncTime`

// ReplicationPolicy defines a replication relationship between a source pool
// and a destination NFS server for cross-region disaster recovery.
type ReplicationPolicy struct {
    metav1.TypeMeta   `json:",inline"`
    metav1.ObjectMeta `json:"metadata,omitempty"`

    Spec   ReplicationPolicySpec   `json:"spec,omitempty"`
    Status ReplicationPolicyStatus `json:"status,omitempty"`
}

// ReplicationPolicySpec defines the desired state of a replication policy.
type ReplicationPolicySpec struct {
    // SourcePoolName is the name of the FileSharePool CR on this cluster to replicate from.
    // +kubebuilder:validation:Required
    SourcePoolName string `json:"sourcePoolName"`

    // DestinationNFSServer is the NFS mount target IP of the destination pool,
    // reachable over Transit Gateway or VPN.
    // +kubebuilder:validation:Required
    DestinationNFSServer string `json:"destinationNFSServer"`

    // DestinationBasePath is the base path on the destination NFS server
    // where replicated SubVolume directories are written (e.g., "/pvcs").
    // +kubebuilder:validation:Required
    DestinationBasePath string `json:"destinationBasePath"`

    // Schedule is a Go duration string controlling replication frequency.
    // Examples: "15m" (every 15 minutes), "1h" (hourly), "6h" (every 6 hours).
    // Parsed as time.Duration.
    // +kubebuilder:validation:Required
    Schedule string `json:"schedule"`

    // SubVolumeSelector selects which SubVolumes to replicate.
    // If nil, ALL SubVolumes in the source pool are replicated.
    // +optional
    SubVolumeSelector *metav1.LabelSelector `json:"subVolumeSelector,omitempty"`

    // MaxRetries is the number of consecutive failures before the policy is paused.
    // +kubebuilder:validation:Minimum=0
    // +kubebuilder:default=3
    MaxRetries int32 `json:"maxRetries"`

    // IncrementalSync enables rsync-based incremental replication instead of full copy.
    // When true (default), only changed files are transferred.
    // +kubebuilder:default=true
    // +optional
    IncrementalSync *bool `json:"incrementalSync,omitempty"`

    // PreSyncHooks are hooks executed before each replication sync cycle.
    // +optional
    PreSyncHooks []Hook `json:"preSyncHooks,omitempty"`

    // PostSyncHooks are hooks executed after each replication sync cycle.
    // +optional
    PostSyncHooks []Hook `json:"postSyncHooks,omitempty"`
}

// ReplicationPolicyStatus describes the observed state of a replication policy.
type ReplicationPolicyStatus struct {
    // Phase is the overall replication state.
    // +kubebuilder:validation:Enum=Active;Paused;Failed
    Phase string `json:"phase,omitempty"`

    // LastSyncTime is when the last successful replication cycle completed.
    // +optional
    LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

    // LastSyncDuration is how long the last successful sync took, as a Go duration string.
    // +optional
    LastSyncDuration string `json:"lastSyncDuration,omitempty"`

    // SubVolumeStatuses tracks per-SubVolume replication state.
    // +optional
    SubVolumeStatuses []SubVolumeReplicationStatus `json:"subVolumeStatuses,omitempty"`

    // ConsecutiveFailures counts sequential failed replication cycles.
    // Resets to 0 on success.
    ConsecutiveFailures int32 `json:"consecutiveFailures"`

    // LastError is the error message from the most recent failure.
    // +optional
    LastError string `json:"lastError,omitempty"`
}

// SubVolumeReplicationStatus tracks the replication state of a single SubVolume.
type SubVolumeReplicationStatus struct {
    // SubVolumeName is the SubVolume CR name.
    SubVolumeName string `json:"subVolumeName"`

    // LastSyncTime for this specific SubVolume.
    // +optional
    LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

    // BytesSynced in the last sync for this SubVolume.
    // +optional
    BytesSynced *int64 `json:"bytesSynced,omitempty"`

    // LastError recorded for this SubVolume's last sync attempt.
    // +optional
    LastError string `json:"lastError,omitempty"`
}
```

### Example ReplicationPolicy CR

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: ReplicationPolicy
metadata:
  name: dr-us-south-to-us-east
spec:
  sourcePoolName: general-purpose
  destinationNFSServer: "10.241.5.10"                  # DR site NFS IP via Transit Gateway
  destinationBasePath: /pvcs
  schedule: "15m"                                        # Replicate every 15 minutes
  maxRetries: 3
  subVolumeSelector:                                     # Only replicate labeled SubVolumes
    matchLabels:
      app.kubernetes.io/part-of: critical-app
status:
  phase: Active
  lastSyncTime: "2026-02-15T12:15:00Z"
  lastSyncDuration: "2m30s"
  consecutiveFailures: 0
  subVolumeStatuses:
    - subVolumeName: pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab
      lastSyncTime: "2026-02-15T12:15:00Z"
      bytesSynced: 104857600
    - subVolumeName: pvc-b2c3d4e5-6789-01bc-def0-234567890abc
      lastSyncTime: "2026-02-15T12:14:55Z"
      bytesSynced: 52428800
```

### Example ReplicationPolicy CR Replicating All SubVolumes

```yaml
apiVersion: storage.ibmcloud.io/v1alpha1
kind: ReplicationPolicy
metadata:
  name: dr-full-pool-replication
spec:
  sourcePoolName: general-purpose
  destinationNFSServer: "10.241.5.10"
  destinationBasePath: /pvcs
  schedule: "1h"                                         # Replicate hourly
  maxRetries: 5
  # subVolumeSelector omitted — all SubVolumes in the pool are replicated
```

### ReplicationPolicy Lifecycle

1. **Active**: The policy is operating normally. Replication cycles run on the configured `schedule`.
2. **Paused**: Replication has been paused after `maxRetries` consecutive failures. Manual intervention is required to fix the issue and reset the policy.
3. **Failed**: A permanent failure has occurred (e.g., destination unreachable after all retries).

### Replication Behavior

- Each replication cycle iterates over all SubVolumes matching the `subVolumeSelector` (or all SubVolumes in the pool if no selector is set).
- For each SubVolume, an incremental rsync copies changed files from the source subdirectory to the destination NFS server at `destinationBasePath/<subPath>`.
- `consecutiveFailures` increments on each failed cycle and resets to 0 on success. When it reaches `maxRetries`, the policy transitions to `Paused`.
- Per-SubVolume status in `subVolumeStatuses` allows operators to identify which volumes are failing independently.

---

## StorageClass Parameters

**Auto-creation:** When a FileSharePool reaches `Ready`, the controller automatically creates a matching StorageClass named after the pool (e.g., pool `general-purpose` gets StorageClass `general-purpose`). For tiered pools, one StorageClass per tier is created (e.g., `general-purpose-standard`, `general-purpose-premium`). Auto-created StorageClasses have an OwnerReference to the pool and are garbage-collected when the pool is deleted.

To opt out, add the annotation `storage.ibmcloud.io/skip-storageclass: "true"` to the FileSharePool before creating it.

The CSI driver reads these from the StorageClass `parameters` map:

| Parameter | Required | Default | Description |
|-----------|----------|---------|-------------|
| `pool` | Yes | — | Name of the FileSharePool CR |
| `tier` | No | — | Tier name within a tiered pool (required for tiered pools) |
| `uid` | No | Pool default | Unix UID for the subdirectory |
| `gid` | No | Pool default | Unix GID for the subdirectory |
| `permissions` | No | Pool default | Unix permissions (e.g., "0755") |
| `reclaimAction` | No | `delete` | `delete`, `retain`, or `archive` |

Example auto-created StorageClass (for a pool named `general-purpose`):
```yaml
apiVersion: storage.k8s.io/v1
kind: StorageClass
metadata:
  name: general-purpose
  labels:
    storage.ibmcloud.io/managed-by: ibm-vpc-file-pool-csi
    storage.ibmcloud.io/pool: general-purpose
  ownerReferences:
    - apiVersion: storage.ibmcloud.io/v1alpha1
      kind: FileSharePool
      name: general-purpose
      uid: <pool-uid>
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

Example manual StorageClass (if opting out of auto-creation):
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
