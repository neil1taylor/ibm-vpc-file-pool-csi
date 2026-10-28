package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=bb
// +kubebuilder:printcolumn:name="Policy",type=string,JSONPath=`.spec.policy`
// +kubebuilder:printcolumn:name="Claim",type=string,JSONPath=`.spec.pvcName`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`

// BackupBrowse is one open session on one backup of one volume. Users never
// make or see it: the backup gateway makes it after it checks the user's
// rights. The backup controller starts a read-only helper pod for it, and
// removes the pod and the session when it is idle, at its hard limit, or closed.
// See docs/specs/2026-10-22-backup-file-restore.md.
type BackupBrowse struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   BackupBrowseSpec   `json:"spec,omitempty"`
	Status BackupBrowseStatus `json:"status,omitempty"`
}

// BackupBrowseSpec says which backup to open.
type BackupBrowseSpec struct {
	// Policy is the BackupPolicy that holds the backup.
	// +kubebuilder:validation:Required
	Policy string `json:"policy"`

	// PVCNamespace and PVCName name the claim that was backed up. The claim may
	// be gone: the policy remembers its backups until the keep time.
	// +kubebuilder:validation:Required
	PVCNamespace string `json:"pvcNamespace"`
	// +kubebuilder:validation:Required
	PVCName string `json:"pvcName"`

	// Moment is the time to open. The newest backup at or before it is read.
	// Unset means the latest copy.
	// +optional
	Moment *metav1.Time `json:"moment,omitempty"`

	// RequestedBy is the user who asked, for the audit trail. The gateway sets
	// it from the user's token; it is not used to decide anything.
	// +optional
	RequestedBy string `json:"requestedBy,omitempty"`

	// Source says where the backup is read from: local (the snapshot pool, the
	// default) or cos (the Kopia repository in the COS bucket that the policy's
	// export block names). A COS session needs the policy to have an export
	// block, unless fromCOS is set.
	// +kubebuilder:validation:Enum=local;cos
	// +optional
	Source string `json:"source,omitempty"`

	// FromCOS names a bucket directly. It is for a cluster administrator, for a
	// backup that no policy of this cluster describes; policy is then only the
	// name of the policy's area in the bucket, and pvcNamespace and pvcName
	// hold "-" and the volume. Source must be cos.
	// +optional
	FromCOS *BrowseFromCOS `json:"fromCOS,omitempty"`

	// Close ends the session. The gateway sets it when a user is done.
	// +optional
	Close bool `json:"close,omitempty"`
}

// BackupBrowseStatus reports the session.
type BackupBrowseStatus struct {
	// Phase is Pending (waiting for a free session slot), Starting (the helper
	// pod is starting), Ready (the helper answers), Closed or Failed.
	// +kubebuilder:validation:Enum=Pending;Starting;Ready;Closed;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message says what the session waits for, or why it failed or closed.
	// +optional
	Message string `json:"message,omitempty"`

	// PodIP and Port are where the helper answers, over TLS.
	// +optional
	PodIP string `json:"podIP,omitempty"`
	// +optional
	Port int32 `json:"port,omitempty"`

	// SecretName is the Secret, in the browse namespace, that holds the session
	// token and the session certificate and key. It is owned by this resource.
	// A session that reads COS has a copy in the driver namespace, which its
	// helper mounts; the gateway reads only the one in the browse namespace.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// StartedTime is when the helper pod was started. A session that reads COS
	// fails if its helper is not ready a few minutes after this.
	// +optional
	StartedTime *metav1.Time `json:"startedTime,omitempty"`

	// EndReason is why a session closed: idle, max-life, closed or stopped.
	// +optional
	EndReason string `json:"endReason,omitempty"`

	// EndedTime is when the session closed or failed.
	// +optional
	EndedTime *metav1.Time `json:"endedTime,omitempty"`
}

// +kubebuilder:object:root=true

// BackupBrowseList contains a list of BackupBrowse.
type BackupBrowseList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []BackupBrowse `json:"items"`
}

func init() {
	SchemeBuilder.Register(&BackupBrowse{}, &BackupBrowseList{})
}
