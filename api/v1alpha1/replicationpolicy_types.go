package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=rp
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.sourcePoolName`
// +kubebuilder:printcolumn:name="Dest",type=string,JSONPath=`.spec.destinationNFSServer`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Last Sync",type=date,JSONPath=`.status.lastSyncTime`

// ReplicationPolicy defines a replication relationship between a source pool
// and a destination NFS server for cross-region disaster recovery.
type ReplicationPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ReplicationPolicySpec   `json:"spec,omitempty"`
	Status ReplicationPolicyStatus `json:"status,omitempty"`
}

// ReplicationPolicySpec defines the desired state of a replication policy.
type ReplicationPolicySpec struct {
	// SourcePoolName is the name of the FileSharePool CR on this cluster to replicate from.
	// +kubebuilder:validation:Required
	SourcePoolName string `json:"sourcePoolName"`

	// DestinationNFSServer is the NFS mount target IP of the destination pool,
	// reachable over Transit Gateway or VPN.
	// +kubebuilder:validation:Required
	DestinationNFSServer string `json:"destinationNFSServer"`

	// DestinationBasePath is the base path on the destination NFS server
	// where replicated SubVolume directories are written (e.g., "/pvcs").
	// +kubebuilder:validation:Required
	DestinationBasePath string `json:"destinationBasePath"`

	// Schedule is a Go duration string controlling replication frequency.
	// Examples: "15m" (every 15 minutes), "1h" (hourly), "6h" (every 6 hours).
	// Parsed as time.Duration.
	// +kubebuilder:validation:Required
	Schedule string `json:"schedule"`

	// SubVolumeSelector selects which SubVolumes to replicate.
	// If nil, ALL SubVolumes in the source pool are replicated.
	// +optional
	SubVolumeSelector *metav1.LabelSelector `json:"subVolumeSelector,omitempty"`

	// MaxRetries is the number of consecutive failures before the policy is paused.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=3
	MaxRetries int32 `json:"maxRetries"`

	// IncrementalSync enables rsync-based incremental replication instead of full copy.
	// When true (default), only changed files are transferred.
	// +kubebuilder:default=true
	// +optional
	IncrementalSync *bool `json:"incrementalSync,omitempty"`

	// BandwidthLimitMbps limits rsync bandwidth in megabits per second.
	// Only applies when IncrementalSync is true. 0 means no limit.
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=0
	// +optional
	BandwidthLimitMbps int32 `json:"bandwidthLimitMbps,omitempty"`

	// MaxParallelSyncs controls how many SubVolumes are synced concurrently.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	// +optional
	MaxParallelSyncs int32 `json:"maxParallelSyncs,omitempty"`

	// RsyncOptions are extra rsync flags appended after the base flags (-a --delete).
	// Dangerous flags (--daemon, --server, --rsh, --rsync-path) are rejected by the webhook.
	// +optional
	RsyncOptions []string `json:"rsyncOptions,omitempty"`

	// PreSyncHooks are hooks executed before each replication sync cycle.
	// +optional
	PreSyncHooks []Hook `json:"preSyncHooks,omitempty"`

	// PostSyncHooks are hooks executed after each replication sync cycle.
	// +optional
	PostSyncHooks []Hook `json:"postSyncHooks,omitempty"`
}

// ReplicationPolicyStatus describes the observed state of a replication policy.
type ReplicationPolicyStatus struct {
	// Phase is the overall replication state.
	// +kubebuilder:validation:Enum=Active;Paused;Failed
	Phase string `json:"phase,omitempty"`

	// LastSyncTime is when the last successful replication cycle completed.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// LastSyncDuration is how long the last successful sync took, as a Go duration string.
	// +optional
	LastSyncDuration string `json:"lastSyncDuration,omitempty"`

	// SubVolumeStatuses tracks per-SubVolume replication state.
	// +optional
	SubVolumeStatuses []SubVolumeReplicationStatus `json:"subVolumeStatuses,omitempty"`

	// ConsecutiveFailures counts sequential failed replication cycles.
	// Resets to 0 on success.
	ConsecutiveFailures int32 `json:"consecutiveFailures"`

	// LastError is the error message from the most recent failure.
	// +optional
	LastError string `json:"lastError,omitempty"`
}

// SubVolumeReplicationStatus tracks the replication state of a single SubVolume.
type SubVolumeReplicationStatus struct {
	// SubVolumeName is the SubVolume CR name.
	SubVolumeName string `json:"subVolumeName"`

	// LastSyncTime for this specific SubVolume.
	// +optional
	LastSyncTime *metav1.Time `json:"lastSyncTime,omitempty"`

	// BytesSynced in the last sync for this SubVolume.
	// +optional
	BytesSynced *int64 `json:"bytesSynced,omitempty"`

	// LastError recorded for this SubVolume's last sync attempt.
	// +optional
	LastError string `json:"lastError,omitempty"`
}

// +kubebuilder:object:root=true

// ReplicationPolicyList contains a list of ReplicationPolicy resources.
type ReplicationPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ReplicationPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ReplicationPolicy{}, &ReplicationPolicyList{})
}
