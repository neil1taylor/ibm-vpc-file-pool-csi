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

	// VMCopy is the state of the QEMU copy while a snapshot of a running VM's
	// disk is made. It is empty for other snapshots and once the copy ends.
	// +optional
	VMCopy *VMCopyState `json:"vmCopy,omitempty"`

	// VMCopyAttempts counts the copies that were cancelled or failed for a
	// snapshot of a running VM. After 3 the snapshot fails.
	// +optional
	VMCopyAttempts int32 `json:"vmCopyAttempts,omitempty"`

	// VMCopyStarted is when the current copy try started. A try that runs
	// much longer than the disk needs is cancelled.
	// +optional
	VMCopyStarted *metav1.Time `json:"vmCopyStarted,omitempty"`

	// VMCopyRetryAfter is the earliest time of the next copy try.
	// +optional
	VMCopyRetryAfter *metav1.Time `json:"vmCopyRetryAfter,omitempty"`

	// CopyMoment is the moment a snapshot of a running VM's disk shows.
	// +optional
	CopyMoment *metav1.Time `json:"copyMoment,omitempty"`

	// Consistency of a snapshot of a running VM's disk.
	// FileSystemConsistent means the guest file systems were frozen for the
	// copy. CrashConsistent means they were not, as after a power loss.
	// +kubebuilder:validation:Enum=FileSystemConsistent;CrashConsistent
	// +optional
	Consistency string `json:"consistency,omitempty"`

	// ConsistencyReason says why a snapshot is only crash consistent.
	// +optional
	ConsistencyReason string `json:"consistencyReason,omitempty"`

	// Message says what a snapshot waits for or why it failed.
	// +optional
	Message string `json:"message,omitempty"`
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
