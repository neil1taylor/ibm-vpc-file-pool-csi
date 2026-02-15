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

// FileSharePool defines a pool of VPC file shares that back PVC subdirectories.
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

// +kubebuilder:object:root=true

// FileSharePoolList contains a list of FileSharePool resources.
type FileSharePoolList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []FileSharePool `json:"items"`
}

func init() {
	SchemeBuilder.Register(&FileSharePool{}, &FileSharePoolList{})
}
