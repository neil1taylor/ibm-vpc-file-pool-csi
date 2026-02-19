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

// SubVolume tracks a single PVC's allocation within a pool share.
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

// +kubebuilder:object:root=true

// SubVolumeList contains a list of SubVolume resources.
type SubVolumeList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []SubVolume `json:"items"`
}

func init() {
	SchemeBuilder.Register(&SubVolume{}, &SubVolumeList{})
}
