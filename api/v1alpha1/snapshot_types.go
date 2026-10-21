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

	// ChainFile is the file of the source disk's chain that holds an instant
	// snapshot. It changes when a newer snapshot of the disk is merged into it.
	// +optional
	ChainFile string `json:"chainFile,omitempty"`

	// MergeChild is the newer file that is merged into ChainFile while the
	// snapshot is deleted.
	// +optional
	MergeChild string `json:"mergeChild,omitempty"`

	// MethodReason says why a snapshot of a VM disk is a copy and not an
	// instant snapshot.
	// +optional
	MethodReason string `json:"methodReason,omitempty"`

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

	// CopyID names the QEMU copy that made this snapshot. Snapshots of the
	// disks of one VM that show the same moment have the same CopyID.
	// +optional
	CopyID string `json:"copyID,omitempty"`

	// VMCopySet is the set of disks that the QEMU copy of this snapshot
	// covered. Other Snapshots of the same VM join the set for a while. When
	// the time to join has passed, the controller deletes the staging files
	// of the disks that no Snapshot claimed, and clears the set.
	// +optional
	VMCopySet *VMCopySet `json:"vmCopySet,omitempty"`

	// Message says what a snapshot waits for or why it failed.
	// +optional
	Message string `json:"message,omitempty"`
}

// VMCopySet describes a QEMU copy of all the pool disks of one VM.
type VMCopySet struct {
	// ID is the copy id.
	ID string `json:"id"`

	// PodNamespace, PodName and PodUID identify the VM pod that made the copy.
	PodNamespace string `json:"podNamespace"`
	PodName      string `json:"podName"`
	PodUID       string `json:"podUID"`

	// VMIName is the VirtualMachineInstance name.
	VMIName string `json:"vmiName"`

	// Moment is the moment the copy shows.
	Moment metav1.Time `json:"moment"`

	// FileSystemConsistent is true if the guest was frozen for the copy.
	FileSystemConsistent bool `json:"fileSystemConsistent"`

	// FreezeError is the freeze error, if the freeze failed.
	// +optional
	FreezeError string `json:"freezeError,omitempty"`

	// Bitmap is the change list the copy started on each disk.
	Bitmap string `json:"bitmap"`

	// JoinUntil is the time after which no more Snapshots join the set.
	JoinUntil metav1.Time `json:"joinUntil"`

	// Disks are the disks of the set, in the order QEMU numbered them.
	Disks []VMCopySetDisk `json:"disks"`
}

// VMCopySetDisk is one disk of a VMCopySet.
type VMCopySetDisk struct {
	// Volume is the pod volume name of the disk.
	Volume string `json:"volume"`

	// StagingFile is the staging file name in the volume's root directory.
	StagingFile string `json:"stagingFile"`

	// SubVolume is the name of the SubVolume of the disk.
	SubVolume string `json:"subVolume"`
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
