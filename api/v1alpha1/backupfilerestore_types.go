package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=bfr
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=`.spec.policy`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourceName`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.spec.targetName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// BackupFileRestore asks the driver to copy chosen files and folders out of a
// backup into a claim: a new claim, or a new folder in an existing claim. It
// never writes over a file that exists and never writes into a VM disk. The
// backup gateway makes it for a user after it checks the user's rights; a
// cluster administrator may make one by hand. See
// docs/specs/2026-10-22-backup-file-restore.md.
type BackupFileRestore struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupFileRestoreSpec   `json:"spec,omitempty"`
	Status BackupFileRestoreStatus `json:"status,omitempty"`
}

// BackupFileRestoreSpec says what to restore, and where to.
type BackupFileRestoreSpec struct {
	// Policy is the BackupPolicy that holds the backup.
	// +kubebuilder:validation:Required
	Policy string `json:"policy"`

	// SourceNamespace and SourceName name the claim that was backed up. The
	// claim may be gone: the policy remembers its backups until the keep time.
	// +kubebuilder:validation:Required
	SourceNamespace string `json:"sourceNamespace"`
	// +kubebuilder:validation:Required
	SourceName string `json:"sourceName"`

	// Moment is the time to restore from. The newest backup at or before it is
	// used. Unset means the latest copy.
	// +optional
	Moment *metav1.Time `json:"moment,omitempty"`

	// Source says where the backup is read from: local (the snapshot pool, the
	// default) or cos (the Kopia repository in the COS bucket that the policy's
	// export block names, or that fromCOS names).
	// +kubebuilder:validation:Enum=local;cos
	// +optional
	Source string `json:"source,omitempty"`

	// FromCOS names a bucket directly. It is for a cluster administrator, for a
	// backup that no policy of this cluster describes; policy is then only the
	// name of the policy's area in the bucket, sourceNamespace holds "-" and
	// sourceName the volume, and storageClassName must be set for a new claim.
	// Source must be cos.
	// +optional
	FromCOS *BrowseFromCOS `json:"fromCOS,omitempty"`

	// Guest restores files from inside a VM disk: the volume must be a VM disk
	// (the policy status names its VM) in the snapshot pool. The first name of
	// each path is then a file system of the guest (sda1, rhel-root), as `kubectl
	// backup ls` shows at the top, and it is kept in the restored tree. The disk
	// is held (its next backup copy waits) while the restore runs. The gateway
	// sets it for every VM disk.
	// +optional
	Guest bool `json:"guest,omitempty"`

	// Paths are the files and folders to restore, below the root of the
	// backed-up volume, one path each in the escaped form that `kubectl backup`
	// prints (a backslash is written \\ and a byte that is not text \xNN). A
	// folder is restored with everything in it. The path / restores the whole
	// volume.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=1000
	Paths []string `json:"paths"`

	// TargetNamespace and TargetName name the claim that gets the files.
	// +kubebuilder:validation:Required
	TargetNamespace string `json:"targetNamespace"`
	// +kubebuilder:validation:Required
	TargetName string `json:"targetName"`

	// Existing restores into a new folder of the claim, which must exist. Unset
	// makes a new claim, which must not exist.
	// +optional
	Existing bool `json:"existing,omitempty"`

	// SizeGB is the size of a new claim. Unset sizes it to the chosen files plus
	// 20%, at least 1 GiB. Not used with existing.
	// +optional
	SizeGB int64 `json:"sizeGB,omitempty"`

	// StorageClassName is the class of a new claim. Unset means the class of the
	// backed-up claim.
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// TargetUID is the UID of the existing claim, taken when the user's rights
	// were checked. The restore refuses if the claim was deleted and made again
	// since. The gateway sets it. If it is unset, the controller takes the UID of
	// the claim the first time it sees it.
	// +optional
	TargetUID types.UID `json:"targetUID,omitempty"`

	// RequestedBy is the user who asked, for the audit trail. The gateway sets
	// it from the user's token; it is not used to decide anything.
	// +optional
	RequestedBy string `json:"requestedBy,omitempty"`
}

// BackupFileRestoreStatus reports how far the restore is.
type BackupFileRestoreStatus struct {
	// Phase is Pending, Measuring (a new claim is being sized), Provisioning (the
	// claim is being made), Restoring (the copy runs), Done or Failed.
	// +kubebuilder:validation:Enum=Pending;Measuring;Provisioning;Restoring;Done;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message says what the restore waits for, or why it failed.
	// +optional
	Message string `json:"message,omitempty"`

	// TargetUID is the UID of the claim this restore writes to. It is set the
	// first time the claim is seen, and the restore refuses a claim with another UID.
	// +optional
	TargetUID types.UID `json:"targetUID,omitempty"`

	// SizeGB is the size chosen for a new claim.
	// +optional
	SizeGB int64 `json:"sizeGB,omitempty"`

	// Folder is the folder made in an existing claim.
	// +optional
	Folder string `json:"folder,omitempty"`

	// RestoredMoment is the moment of the backup that was read. Unset if it is
	// not known (the first copy of a volume).
	// +optional
	RestoredMoment *metav1.Time `json:"restoredMoment,omitempty"`

	// Latest is true if the newest backup was read.
	// +optional
	Latest bool `json:"latest,omitempty"`

	// Files, Dirs and Links count what was written; Skipped counts devices,
	// pipes and sockets left out, and Unreadable counts entries the reader could
	// not read. Bytes is the size of the files.
	// +optional
	Files int `json:"files,omitempty"`
	// +optional
	Dirs int `json:"dirs,omitempty"`
	// +optional
	Links int `json:"links,omitempty"`
	// +optional
	Skipped int `json:"skipped,omitempty"`
	// +optional
	Unreadable int `json:"unreadable,omitempty"`
	// +optional
	Bytes int64 `json:"bytes,omitempty"`

	// EndedTime is when the restore reached Done or Failed.
	// +optional
	EndedTime *metav1.Time `json:"endedTime,omitempty"`
}

// +kubebuilder:object:root=true

// BackupFileRestoreList contains a list of BackupFileRestore.
type BackupFileRestoreList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupFileRestore `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupFileRestore{}, &BackupFileRestoreList{})
}
