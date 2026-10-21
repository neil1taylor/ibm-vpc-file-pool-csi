package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Failover phases.
const (
	FailoverPhasePlanning  = "Planning"
	FailoverPhasePlanned   = "Planned"
	FailoverPhaseExecuting = "Executing"
	FailoverPhaseDone      = "Done"
	FailoverPhaseFailed    = "Failed"
)

// Failover actions.
const (
	FailoverActionPlan    = "Plan"
	FailoverActionExecute = "Execute"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=fo
// +kubebuilder:printcolumn:name="Action",type=string,JSONPath=`.spec.action`
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.status.source`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="RPO",type=string,JSONPath=`.status.rpo`

// Failover asks the driver on the destination (DR) cluster to read a
// replicated copy and plan, or carry out, a failover to it. A Job reads the
// copy with the same code as `kubectl failover`. An Execute makes the
// SubVolume, PV and PVC objects, as the CLI does. See
// docs/specs for the design (issue #44).
type Failover struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   FailoverSpec   `json:"spec,omitempty"`
	Status FailoverStatus `json:"status,omitempty"`
}

// FailoverSpec says what to read and whether to change anything.
type FailoverSpec struct {
	// Source is the name of the destination claim to read, in the driver's
	// namespace. For receiver mode it is the receiver's data claim. Unset
	// means the claim the Helm chart names for the receiver.
	// +optional
	Source string `json:"source,omitempty"`

	// Action is Plan (read and report; changes nothing) or Execute (make the
	// objects for the failed-over volumes). An Execute is allowed only after a
	// Plan for the same source finished in the last 15 minutes.
	// +kubebuilder:validation:Enum=Plan;Execute
	// +kubebuilder:validation:Required
	Action string `json:"action"`

	// RewindTo limits the moment of each running VM to the newest one at or
	// before it. Unset means the newest moment all disks of a VM share.
	// +optional
	RewindTo *metav1.Time `json:"rewindTo,omitempty"`

	// Namespace overrides the namespace of the PVCs that are made.
	// +optional
	Namespace string `json:"namespace,omitempty"`

	// Force goes on although a copy may still be applied to a disk. An
	// unfinished copy is rolled back first.
	// +optional
	Force bool `json:"force,omitempty"`

	// DRPool is the FileSharePool on this cluster that the new SubVolumes
	// belong to. Required for Execute.
	// +optional
	DRPool string `json:"drPool,omitempty"`

	// DRShareIP and DRExportPath are the NFS server and export of the
	// destination claim. Unset means they are read from the claim's volume,
	// which must then be an NFS volume.
	// +optional
	DRShareIP string `json:"drShareIP,omitempty"`
	// +optional
	DRExportPath string `json:"drExportPath,omitempty"`
}

// FailoverStatus reports the plan or the result.
type FailoverStatus struct {
	// Phase is Planning, Planned, Executing, Done or Failed.
	// +kubebuilder:validation:Enum=Planning;Planned;Executing;Done;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Message says why the failover failed, or what it waits for.
	// +optional
	Message string `json:"message,omitempty"`

	// Source is the claim that was read (spec.source, or the default).
	// +optional
	Source string `json:"source,omitempty"`

	// StartedAt and FinishedAt bound the run.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`
	// +optional
	FinishedAt *metav1.Time `json:"finishedAt,omitempty"`

	// RPO is the age of the oldest moment that will be shown.
	// +optional
	RPO *metav1.Duration `json:"rpo,omitempty"`

	// VMs lists each running VM found, with the moment it will show.
	// +optional
	VMs []FailoverVM `json:"vms,omitempty"`

	// Volumes lists every volume that is, or would be, failed over.
	// +optional
	Volumes []FailoverVolume `json:"volumes,omitempty"`

	// MomentsTruncated is true if the list of moments of a VM was cut to fit
	// the Job's termination message.
	// +optional
	MomentsTruncated bool `json:"momentsTruncated,omitempty"`

	// CreatedPVCs lists the PVCs an Execute made.
	// +optional
	CreatedPVCs []FailoverPVC `json:"createdPVCs,omitempty"`
}

// FailoverVM is one running VM found on the copy.
type FailoverVM struct {
	// Name is "namespace/name" of the VM.
	Name string `json:"name"`
	// Moment is the moment the failed-over disks will show.
	Moment metav1.Time `json:"moment"`
	// Moments are the moments all disks of the VM can show, newest first.
	// +optional
	Moments []metav1.Time `json:"moments,omitempty"`
	// Disks are the VM's disks.
	Disks []FailoverDisk `json:"disks"`
}

// FailoverDisk is one disk of a VM.
type FailoverDisk struct {
	// SubVolume names the volume.
	SubVolume string `json:"subVolume"`
	// Rewind is true if the disk must be rewound to the VM's moment (it
	// copies the whole disk); false if it is used in place.
	Rewind bool `json:"rewind"`
}

// FailoverVolume is one volume found on the copy.
type FailoverVolume struct {
	SubVolume    string `json:"subVolume"`
	PVCName      string `json:"pvcName"`
	PVCNamespace string `json:"pvcNamespace"`
	RequestedGB  int64  `json:"requestedGB"`
	// SubPath is the folder on the share. For a rewound disk it is the new
	// folder.
	SubPath     string `json:"subPath"`
	UID         *int64 `json:"uid,omitempty"`
	GID         *int64 `json:"gid,omitempty"`
	Permissions string `json:"permissions,omitempty"`
	// Policy is the ReplicationPolicy that wrote the copy.
	Policy string `json:"policy,omitempty"`
	// LastSync is the moment the volume shows.
	LastSync metav1.Time `json:"lastSync"`
}

// FailoverPVC is a PVC an Execute made.
type FailoverPVC struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	PV        string `json:"pv"`
	SubVolume string `json:"subVolume"`
}

// +kubebuilder:object:root=true

// FailoverList contains a list of Failover.
type FailoverList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []Failover `json:"items"`
}

func init() {
	SchemeBuilder.Register(&Failover{}, &FailoverList{})
}
