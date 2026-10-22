package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=br
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=`.spec.policy`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceName`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// BackupRestore asks the driver to make a new PVC from a backup. The driver
// makes the PVC, copies the backup into it with a Job, and holds back any pod
// that mounts it until the copy has ended. It never writes over a volume that
// exists. See docs/specs/2026-10-21-backup-to-snapshot-pool.md.
type BackupRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupRestoreSpec   `json:"spec,omitempty"`
	Status BackupRestoreStatus `json:"status,omitempty"`
}

// BackupRestoreSpec says what to restore, and where to.
type BackupRestoreSpec struct {
	// Policy is the BackupPolicy that holds the backup. With fromCOS it need not
	// exist: it only names the policy's area in the bucket.
	// +kubebuilder:validation:Required
	Policy string `json:"policy"`

	// SourceNamespace and SourceName name the claim that was backed up. The
	// claim may be gone: the policy remembers its backups until the keep time.
	// They are required unless fromCOS is set (COS does not keep claim names).
	// +optional
	SourceNamespace string `json:"sourceNamespace,omitempty"`
	// +optional
	SourceName string `json:"sourceName,omitempty"`

	// Moment is the time to restore. The newest backup at or before it is
	// used. Unset means the latest backup.
	// +optional
	Moment *metav1.Time `json:"moment,omitempty"`

	// TargetNamespace and TargetName name the new PVC. It must not exist.
	// +kubebuilder:validation:Required
	TargetNamespace string `json:"targetNamespace"`
	// +kubebuilder:validation:Required
	TargetName string `json:"targetName"`

	// StorageClassName is the class of the new PVC. Unset means the class of
	// the backed-up claim.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// FromCOS reads the backup from a COS bucket, not from a snapshot pool. The
	// storage class is then required, because COS does not keep it.
	// +optional
	FromCOS *RestoreFromCOS `json:"fromCOS,omitempty"`
}

// BackupRestoreStatus reports how far the restore is.
type BackupRestoreStatus struct {
	// Phase is Pending, Provisioning (the PVC is being made), Restoring (the
	// copy runs), Done or Failed.
	// +kubebuilder:validation:Enum=Pending;Provisioning;Restoring;Done;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message says what the restore waits for, or why it failed.
	// +optional
	Message string `json:"message,omitempty"`

	// RestoredMoment is the moment of the backup that was written. Unset if
	// it is not known (the first copy of a volume).
	// +optional
	RestoredMoment *metav1.Time `json:"restoredMoment,omitempty"`

	// Latest is true if the newest backup was written.
	// +optional
	Latest bool `json:"latest,omitempty"`

	// Fetched is true once a restore from COS has copied the backup out of the
	// bucket into a scratch folder on the new volume's share.
	// +optional
	Fetched bool `json:"fetched,omitempty"`

	// Copied is true once the scratch copy has been written into the new volume.
	// +optional
	Copied bool `json:"copied,omitempty"`
}

// +kubebuilder:object:root=true

// BackupRestoreList contains a list of BackupRestore.
type BackupRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupRestore{}, &BackupRestoreList{})
}
