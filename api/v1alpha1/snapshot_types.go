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

// +kubebuilder:object:root=true

// SnapshotList contains a list of Snapshot resources.
type SnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Snapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Snapshot{}, &SnapshotList{})
}
