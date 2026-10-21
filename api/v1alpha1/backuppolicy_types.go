package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=bp
// +kubebuilder:printcolumn:name="Pool",type=string,JSONPath=`.spec.snapshotPool`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Keep",type=string,JSONPath=`.spec.keepFor`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Volumes",type=integer,JSONPath=`.status.volumeCount`

// BackupPolicy backs up the PVCs it selects, and the VMs whose disks it
// selects, into a pool that holds only backups. The controller makes a
// ReplicationPolicy for each source pool and keeps it in step. See
// docs/specs/2026-10-21-backup-to-snapshot-pool.md.
type BackupPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupPolicySpec   `json:"spec,omitempty"`
	Status BackupPolicyStatus `json:"status,omitempty"`
}

// BackupPolicySpec defines what is backed up, where, how often and for how long.
type BackupPolicySpec struct {
	// SnapshotPool is the pool that holds the backups. It must hold no workload
	// claims, so a lost or full data share does not take the backups with it.
	// +kubebuilder:validation:Required
	SnapshotPool string `json:"snapshotPool"`

	// Schedule is a Go duration such as "6h": how often a backup is made.
	// +kubebuilder:validation:Required
	Schedule string `json:"schedule"`

	// Selector selects PVCs by label. A VM is selected through its disks: if
	// any disk matches, all disks of the VM are backed up. It must select
	// something: an empty selector is refused.
	// +kubebuilder:validation:Required
	Selector *metav1.LabelSelector `json:"selector"`

	// Namespaces limits the PVCs to these namespaces. Empty means all.
	// +optional
	Namespaces []string `json:"namespaces,omitempty"`

	// KeepFor is how long backups are kept: a Go duration, or a number of days
	// such as "30d". At least one hour.
	// +kubebuilder:default="7d"
	// +optional
	KeepFor string `json:"keepFor,omitempty"`

	// VMCopySpeedLimitMBps limits how fast QEMU copies each disk of a running
	// VM, in megabytes per second. 0 means no limit.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=20
	// +optional
	VMCopySpeedLimitMBps *int32 `json:"vmCopySpeedLimitMBps,omitempty"`

	// Suspend stops new backups. Existing backups stay.
	// +optional
	Suspend bool `json:"suspend,omitempty"`
}

// BackupPolicyStatus reports what the policy does.
type BackupPolicyStatus struct {
	// Phase is Active, Suspended or Error.
	// +kubebuilder:validation:Enum=Active;Suspended;Error
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message says why the policy is in Error, or why it is Suspended.
	// +optional
	Message string `json:"message,omitempty"`

	// VolumeCount is the number of volumes the policy backs up.
	// +optional
	VolumeCount int32 `json:"volumeCount,omitempty"`

	// ReplicationPolicies names the ReplicationPolicy objects the controller
	// made for this policy, one for each source pool.
	// +optional
	ReplicationPolicies []string `json:"replicationPolicies,omitempty"`

	// Volumes lists each backed-up volume.
	// +optional
	Volumes []BackupVolumeStatus `json:"volumes,omitempty"`
}

// BackupVolumeStatus is the state of one backed-up volume.
type BackupVolumeStatus struct {
	// PVCNamespace and PVCName name the claim.
	PVCNamespace string `json:"pvcNamespace"`
	PVCName      string `json:"pvcName"`

	// SubVolumeName is the SubVolume of the claim.
	SubVolumeName string `json:"subVolumeName"`

	// VMName is the VirtualMachine the disk belongs to, if any.
	// +optional
	VMName string `json:"vmName,omitempty"`

	// SubPath is the folder of the volume on its share (/pvcs/pvc-...). The
	// backup is in the same place below the policy's folder. A restore needs
	// it after the volume is gone.
	// +optional
	SubPath string `json:"subPath,omitempty"`

	// ShareID is the share of the snapshot pool that holds the backups of this
	// volume. It is chosen when the volume is first backed up and never changes,
	// because the backups stay where they are. A restore reads from it. An entry
	// with no ShareID was made when the snapshot pool had one share; it belongs
	// to the first share of the pool.
	// +optional
	ShareID string `json:"shareID,omitempty"`

	// RequestedGB and StorageClassName are what the volume had, so a restore can
	// make a volume like it after the volume is gone.
	// +optional
	RequestedGB int64 `json:"requestedGB,omitempty"`
	// +optional
	StorageClassName string `json:"storageClassName,omitempty"`

	// Retained is true when the policy no longer backs up the volume (it was
	// deleted, or no longer matches the selector). Its existing backups stay
	// until the keep time has passed since the last backup.
	// +optional
	Retained bool `json:"retained,omitempty"`

	// LastBackupTime is the moment of the newest backup.
	// +optional
	LastBackupTime *metav1.Time `json:"lastBackupTime,omitempty"`

	// OldestBackupTime is the moment of the oldest restore point that is kept.
	// +optional
	OldestBackupTime *metav1.Time `json:"oldestBackupTime,omitempty"`

	// RestorePoints counts the older moments that can be restored.
	// +optional
	RestorePoints int32 `json:"restorePoints,omitempty"`

	// BytesWritten is what the newest backup wrote.
	// +optional
	BytesWritten int64 `json:"bytesWritten,omitempty"`

	// Consistency is FileSystemConsistent or CrashConsistent for a VM disk
	// and FileCopy for any other volume.
	// +optional
	Consistency string `json:"consistency,omitempty"`

	// LastError is the last error of the volume's backup.
	// +optional
	LastError string `json:"lastError,omitempty"`
}

// +kubebuilder:object:root=true

// BackupPolicyList contains a list of BackupPolicy.
type BackupPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupPolicy{}, &BackupPolicyList{})
}
