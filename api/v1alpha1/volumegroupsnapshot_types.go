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

// +kubebuilder:object:root=true

// VolumeGroupSnapshotList contains a list of VolumeGroupSnapshot resources.
type VolumeGroupSnapshotList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []VolumeGroupSnapshot `json:"items"`
}

func init() {
	SchemeBuilder.Register(&VolumeGroupSnapshot{}, &VolumeGroupSnapshotList{})
}
