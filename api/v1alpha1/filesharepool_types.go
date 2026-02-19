package v1alpha1

import (
	"fmt"

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
}

// AccessorZone defines a zone where pool shares should have additional mount targets.
type AccessorZone struct {
	// Zone is the VPC availability zone (e.g., "us-south-2").
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^[a-z]{2}-[a-z]+-\d+$`
	Zone string `json:"zone"`

	// SubnetID is the VPC subnet in the accessor zone to create mount targets in.
	// +kubebuilder:validation:Required
	SubnetID string `json:"subnetID"`
}

// ZoneMountTarget records a mount target created in a specific zone.
type ZoneMountTarget struct {
	// Zone is the VPC availability zone of this mount target.
	Zone string `json:"zone"`

	// MountTargetID is the VPC mount target resource ID.
	MountTargetID string `json:"mountTargetID"`

	// MountTargetIP is the NFS server IP or FQDN in this zone.
	MountTargetIP string `json:"mountTargetIP"`

	// ExportPath is the NFS export path for this zone's mount target.
	// +optional
	ExportPath string `json:"exportPath,omitempty"`
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

// TierConfig returns the share configuration for the given tier name.
// If no tiers are defined, returns the top-level spec fields regardless of tierName.
// If tiers are defined and tierName is empty, returns an error.
// Returns an error if the named tier doesn't exist.
func (s *FileSharePoolSpec) TierConfig(tierName string) (profile string, sizeGB int64, iops *int64, maxShares int32, initialShares int32, err error) {
	if len(s.Tiers) == 0 {
		return s.Profile, s.ShareSizeGB, s.IOPS, s.MaxShares, s.InitialShares, nil
	}
	if tierName == "" {
		return "", 0, nil, 0, 0, fmt.Errorf("tier is required when pool has tiers configured")
	}
	for i := range s.Tiers {
		if s.Tiers[i].Name == tierName {
			t := &s.Tiers[i]
			return t.Profile, t.ShareSizeGB, t.IOPS, t.MaxShares, t.InitialShares, nil
		}
	}
	return "", 0, nil, 0, 0, fmt.Errorf("tier %q not found in pool", tierName)
}

// AllAccessibleZones returns all zones that this pool can serve: home zone + accessor zones.
func (s *FileSharePoolSpec) AllAccessibleZones() []string {
	zones := []string{s.Zone}
	for _, az := range s.AccessorZones {
		zones = append(zones, az.Zone)
	}
	return zones
}

// IsAccessibleZone returns true if the given zone is the home zone or an accessor zone.
func (s *FileSharePoolSpec) IsAccessibleZone(zone string) bool {
	if zone == s.Zone {
		return true
	}
	for _, az := range s.AccessorZones {
		if az.Zone == zone {
			return true
		}
	}
	return false
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

	// DrainStatus tracks the progress of share draining operations.
	// +optional
	DrainStatus []ShareDrainStatus `json:"drainStatus,omitempty"`

	// Conditions follows the standard Kubernetes conditions pattern.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastReconcileTime is when the pool was last reconciled.
	// +optional
	LastReconcileTime *metav1.Time `json:"lastReconcileTime,omitempty"`
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

	// MountTargetIP is the NFS server IP or FQDN for this share.
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

// MountTargetIPForZone returns the mount target IP for the given zone.
// Falls back to the primary MountTargetIP if no zone-specific target exists.
func (s *PoolShareStatus) MountTargetIPForZone(zone string) string {
	for _, mt := range s.MountTargets {
		if mt.Zone == zone {
			return mt.MountTargetIP
		}
	}
	return s.MountTargetIP
}

// AllAccessibleZones returns all zones that have mount targets for this share.
func (s *PoolShareStatus) AllAccessibleZones() []string {
	var zones []string
	for _, mt := range s.MountTargets {
		zones = append(zones, mt.Zone)
	}
	return zones
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
