package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:scope=Cluster,shortName=pb
// +kubebuilder:printcolumn:name="Size GB",type=integer,JSONPath=`.spec.shareSizeGB`
// +kubebuilder:printcolumn:name="IOPS",type=integer,JSONPath=`.spec.iops`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="VMs",type=integer,JSONPath=`.status.vms`
// +kubebuilder:printcolumn:name="Knee",type=integer,JSONPath=`.status.kneeVMs`
// +kubebuilder:printcolumn:name="Recommended",type=integer,JSONPath=`.status.recommendedVMs`

// PoolBench is one performance test run: a new pool with one share, and VMs
// added in steps until users would notice the share is slow. The PoolBench
// service makes and deletes everything the run needs. See
// docs/specs/2026-10-25-poolbench.md.
type PoolBench struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   PoolBenchSpec   `json:"spec,omitempty"`
	Status PoolBenchStatus `json:"status,omitempty"`
}

// PoolBenchSpec holds every setting of a run. Once the run has started, only
// Control and Events may change.
type PoolBenchSpec struct {
	// Zone of the test pool. Unset means the zone with the most worker nodes.
	// +optional
	Zone string `json:"zone,omitempty"`

	// Profile of the test share. Only dp2 is supported.
	// +kubebuilder:default=dp2
	// +optional
	Profile string `json:"profile,omitempty"`

	// ShareSizeGB is the size of the one share of the test pool, in GB.
	// +kubebuilder:validation:Required
	ShareSizeGB int64 `json:"shareSizeGB"`

	// IOPS is the share's IOPS limit. IBM's dp2 table decides which values a
	// share of this size may have.
	// +kubebuilder:validation:Required
	IOPS int64 `json:"iops"`

	// StepVMs is the number of VMs added at each step.
	// +kubebuilder:default=15
	// +optional
	StepVMs int32 `json:"stepVMs,omitempty"`

	// MaxVMs is the largest number of VMs. The run stops there, or at the knee.
	// +kubebuilder:default=90
	// +optional
	MaxVMs int32 `json:"maxVMs,omitempty"`

	// WindowsEvery makes every Nth VM a Windows VM. 0 means Linux only.
	// +kubebuilder:default=3
	// +optional
	WindowsEvery *int32 `json:"windowsEvery,omitempty"`

	// Linux is the size and image of the Linux VMs.
	// +optional
	Linux PoolBenchVM `json:"linux,omitempty"`

	// Windows is the size and image of the Windows VMs.
	// +optional
	Windows PoolBenchVM `json:"windows,omitempty"`

	// Load is the disk load each VM makes.
	// +optional
	Load PoolBenchLoad `json:"load,omitempty"`

	// SettleMinutes is the wait after the new VMs of a step are ready, before
	// the measuring starts. Making and filling new VMs slows the others.
	// +kubebuilder:default=15
	// +optional
	SettleMinutes *int32 `json:"settleMinutes,omitempty"`

	// MeasureMinutes is how long each step is measured.
	// +kubebuilder:default=45
	// +optional
	MeasureMinutes int32 `json:"measureMinutes,omitempty"`

	// Knee says when users would notice the share is slow.
	// +optional
	Knee PoolBenchKnee `json:"knee,omitempty"`

	// Events are extra tests run at a given VM count: a boot storm, a patch
	// window, a live migration, a snapshot or a backup. The console adds one
	// here to run an event now.
	// +optional
	Events []PoolBenchEvent `json:"events,omitempty"`

	// KeepAfterEnd keeps the VMs, pool and namespace after the run ends, for a
	// look. They cost money until the PoolBench is deleted or Control.Stop is set.
	// +optional
	KeepAfterEnd bool `json:"keepAfterEnd,omitempty"`

	// Control changes a running run: pause, hold, measure longer, skip, stop.
	// +optional
	Control PoolBenchControl `json:"control,omitempty"`
}

// PoolBenchVM is the size and image of one kind of test VM.
type PoolBenchVM struct {
	// CPU is the number of vCPUs.
	// +optional
	CPU int32 `json:"cpu,omitempty"`

	// MemoryGiB is the memory of the VM.
	// +optional
	MemoryGiB int32 `json:"memoryGiB,omitempty"`

	// Image is the name of the DataSource the root disk is copied from.
	// +optional
	Image string `json:"image,omitempty"`

	// ImageNamespace is the namespace of the DataSource.
	// +optional
	ImageNamespace string `json:"imageNamespace,omitempty"`

	// Preference is the VirtualMachineClusterPreference of the VM.
	// +optional
	Preference string `json:"preference,omitempty"`

	// RootDiskGiB is the size of the root disk. It must be at least the size
	// of the image.
	// +optional
	RootDiskGiB int32 `json:"rootDiskGiB,omitempty"`
}

// PoolBenchLoad is the disk load of one VM: a steady rate, then a short
// burst, over and over.
type PoolBenchLoad struct {
	// SteadyIOPS is the rate between bursts.
	// +kubebuilder:default=30
	// +optional
	SteadyIOPS int32 `json:"steadyIOPS,omitempty"`

	// BurstIOPS is the rate of a burst.
	// +kubebuilder:default=300
	// +optional
	BurstIOPS int32 `json:"burstIOPS,omitempty"`

	// BurstSeconds is the length of a burst.
	// +kubebuilder:default=45
	// +optional
	BurstSeconds int32 `json:"burstSeconds,omitempty"`

	// PeriodSeconds is the time from the start of one burst to the next.
	// +kubebuilder:default=600
	// +optional
	PeriodSeconds int32 `json:"periodSeconds,omitempty"`

	// ReadPercent is the share of reads.
	// +kubebuilder:default=70
	// +optional
	ReadPercent *int32 `json:"readPercent,omitempty"`

	// BlockMix is the block sizes and the share of the I/O each one takes, in
	// percent, for example "4k:50 16k:30 64k:20". The shares add up to 100.
	// +kubebuilder:default="4k:50 16k:30 64k:20"
	// +optional
	BlockMix string `json:"blockMix,omitempty"`

	// AreaGiB is the size of the test area on each VM's load disk.
	// +kubebuilder:default=3
	// +optional
	AreaGiB int32 `json:"areaGiB,omitempty"`
}

// PoolBenchKnee is the rule for "users would notice".
type PoolBenchKnee struct {
	// LatencyLimitMs is the p95 latency over which a VM counts as slow.
	// +kubebuilder:default=20
	// +optional
	LatencyLimitMs int32 `json:"latencyLimitMs,omitempty"`

	// SlowVMPercent is the share of slow VMs at which a step fails.
	// +kubebuilder:default=25
	// +optional
	SlowVMPercent int32 `json:"slowVMPercent,omitempty"`

	// StallSeconds is the I/O time that counts as a stall. Any stall fails the step.
	// +kubebuilder:default=5
	// +optional
	StallSeconds int32 `json:"stallSeconds,omitempty"`

	// StopAtKnee ends the run at the first failed step. Unset means true.
	// +optional
	StopAtKnee *bool `json:"stopAtKnee,omitempty"`
}

// PoolBenchEvent is an extra test at a given VM count.
type PoolBenchEvent struct {
	// Type is the event.
	// +kubebuilder:validation:Enum=BootStorm;PatchWindow;Migration;Snapshot;Backup
	Type string `json:"type"`

	// AtVMs is the step, by VM count, at which the event runs. It must be a
	// step of the run.
	AtVMs int32 `json:"atVMs"`
}

// The event types.
const (
	PoolBenchEventBootStorm   = "BootStorm"
	PoolBenchEventPatchWindow = "PatchWindow"
	PoolBenchEventMigration   = "Migration"
	PoolBenchEventSnapshot    = "Snapshot"
	PoolBenchEventBackup      = "Backup"
)

// PoolBenchControl changes a running run. Each field is a wish; the status
// shows when the service has acted on it.
type PoolBenchControl struct {
	// Paused stops the VMs' load and the clock of the step until it is false.
	// +optional
	Paused bool `json:"paused,omitempty"`

	// Hold keeps measuring the current step until it is false.
	// +optional
	Hold bool `json:"hold,omitempty"`

	// Stop ends the run and cleans up, also when KeepAfterEnd is set.
	// +optional
	Stop bool `json:"stop,omitempty"`

	// SkipStep ends the measuring of this step (by number, from 1) now. Its
	// result is marked as cut short.
	// +optional
	SkipStep int32 `json:"skipStep,omitempty"`

	// ExtendStep and ExtendMinutes add minutes to the measuring of one step.
	// +optional
	ExtendStep int32 `json:"extendStep,omitempty"`
	// +optional
	ExtendMinutes int32 `json:"extendMinutes,omitempty"`
}

// PoolBenchStatus is the live state and the results of a run.
type PoolBenchStatus struct {
	// Phase is the state of the run.
	// +kubebuilder:validation:Enum=Pending;Checking;Running;Paused;CleaningUp;Complete;Stopped;Failed
	// +optional
	Phase string `json:"phase,omitempty"`

	// Stage is what the run does now: Preparing (the namespace and pool),
	// Creating (new VMs), Settling, Measuring or Event.
	// +optional
	Stage string `json:"stage,omitempty"`

	// Step is the current step, from 1. TotalSteps is the number of steps if
	// the run reaches MaxVMs.
	// +optional
	Step int32 `json:"step,omitempty"`
	// +optional
	TotalSteps int32 `json:"totalSteps,omitempty"`

	// VMs is the number of test VMs that exist, WindowsVMs how many of them
	// run Windows, and ReadyVMs how many send results.
	// +optional
	VMs int32 `json:"vms,omitempty"`
	// +optional
	WindowsVMs int32 `json:"windowsVMs,omitempty"`
	// +optional
	ReadyVMs int32 `json:"readyVMs,omitempty"`

	// Namespace and PoolName name the run's own namespace and pool.
	// +optional
	Namespace string `json:"namespace,omitempty"`
	// +optional
	PoolName string `json:"poolName,omitempty"`

	// StartTime and EndTime are the start and end of the run.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// +optional
	EndTime *metav1.Time `json:"endTime,omitempty"`

	// StageStarted is the start of the current stage. StageEnds is its
	// planned end, when it has one.
	// +optional
	StageStarted *metav1.Time `json:"stageStarted,omitempty"`
	// +optional
	StageEnds *metav1.Time `json:"stageEnds,omitempty"`

	// PausedAt is when the run was paused; unset while it is not paused.
	// +optional
	PausedAt *metav1.Time `json:"pausedAt,omitempty"`

	// ShareAtStart and ShareAtEnd are the share's NFS counters at the start
	// and the end of the current measuring, so a restart of the service does
	// not lose them.
	// +optional
	ShareAtStart *PoolBenchCounters `json:"shareAtStart,omitempty"`
	// +optional
	ShareAtEnd *PoolBenchCounters `json:"shareAtEnd,omitempty"`

	// Message says in plain words what the run does, or why it failed.
	// +optional
	Message string `json:"message,omitempty"`

	// Steps holds one result for each step measured.
	// +optional
	Steps []PoolBenchStepResult `json:"steps,omitempty"`

	// KneeVMs is the VM count of the first failed step; 0 means no knee was
	// reached. RecommendedVMs is 80% of the knee, rounded down to a step, or
	// the largest passed step when there is no knee.
	// +optional
	KneeVMs int32 `json:"kneeVMs,omitempty"`
	// +optional
	RecommendedVMs int32 `json:"recommendedVMs,omitempty"`

	// Events holds the result of each event that has run.
	// +optional
	Events []PoolBenchEventResult `json:"events,omitempty"`

	// Log holds the newest entries of the run's log: stage changes, control
	// actions, warnings and errors.
	// +optional
	Log []PoolBenchLogEntry `json:"log,omitempty"`

	// CleanedUp is true once the run's VMs, pool and namespace are gone and the
	// VPC no longer lists the share.
	// +optional
	CleanedUp bool `json:"cleanedUp,omitempty"`
}

// The phases of a run.
const (
	PoolBenchPending    = "Pending"
	PoolBenchChecking   = "Checking"
	PoolBenchRunning    = "Running"
	PoolBenchPaused     = "Paused"
	PoolBenchCleaningUp = "CleaningUp"
	PoolBenchComplete   = "Complete"
	PoolBenchStopped    = "Stopped"
	PoolBenchFailed     = "Failed"
)

// PoolBenchStepResult is the result of one step, as in HCIBench: what the
// share did and what the VMs felt.
type PoolBenchStepResult struct {
	// VMs and WindowsVMs are the VM counts of the step.
	VMs int32 `json:"vms"`
	// +optional
	WindowsVMs int32 `json:"windowsVMs,omitempty"`

	// ShareIOPS is the NFS operations a second the share served, GuestIOPS
	// the I/Os a second the VMs made, and IBMOps the operations IBM counts
	// against the share's limit (one for every 16 KiB of an I/O).
	// +optional
	ShareIOPS int64 `json:"shareIOPS,omitempty"`
	// +optional
	GuestIOPS int64 `json:"guestIOPS,omitempty"`
	// +optional
	IBMOps int64 `json:"ibmOps,omitempty"`

	// ShareLatencyMicros is the share's average time per request.
	// +optional
	ShareLatencyMicros int64 `json:"shareLatencyMicros,omitempty"`

	// P50Micros, P95Micros and P99Micros are the VMs' latency percentiles,
	// the median over all VMs' steady phases.
	// +optional
	P50Micros int64 `json:"p50Micros,omitempty"`
	// +optional
	P95Micros int64 `json:"p95Micros,omitempty"`
	// +optional
	P99Micros int64 `json:"p99Micros,omitempty"`

	// ThroughputKBps is the VMs' read plus write throughput.
	// +optional
	ThroughputKBps int64 `json:"throughputKBps,omitempty"`

	// SlowPercent is the share of VMs over the latency limit, and Stalls the
	// number of I/Os over the stall time.
	// +optional
	SlowPercent int32 `json:"slowPercent,omitempty"`
	// +optional
	Stalls int32 `json:"stalls,omitempty"`

	// BurstSlowPercent is the share of VMs whose burst p95 was over 50 ms.
	// +optional
	BurstSlowPercent int32 `json:"burstSlowPercent,omitempty"`

	// AtLimit is true when the share ran at 95% of its IOPS limit or more.
	// +optional
	AtLimit bool `json:"atLimit,omitempty"`

	// ReadySeconds is the time from making the step's new VMs until all of
	// them send results.
	// +optional
	ReadySeconds int64 `json:"readySeconds,omitempty"`

	// Pass is true when the step meets the knee rule.
	Pass bool `json:"pass"`

	// CutShort is true when the step was skipped before its measuring ended.
	// +optional
	CutShort bool `json:"cutShort,omitempty"`

	// Started and Ended bound the measuring.
	// +optional
	Started *metav1.Time `json:"started,omitempty"`
	// +optional
	Ended *metav1.Time `json:"ended,omitempty"`
}

// PoolBenchCounters is a reading of the share's NFS counters, added up over
// every node: the reads and writes done, and the microseconds they took.
type PoolBenchCounters struct {
	Ops    int64       `json:"ops"`
	Micros int64       `json:"micros"`
	At     metav1.Time `json:"at"`
}

// The stages of a running run.
const (
	PoolBenchStagePreparing = "Preparing"
	PoolBenchStageCreating  = "Creating"
	PoolBenchStageSettling  = "Settling"
	PoolBenchStageMeasuring = "Measuring"
	PoolBenchStageEvent     = "Event"
)

// PoolBenchEventResult is the result of one event.
type PoolBenchEventResult struct {
	Type  string `json:"type"`
	AtVMs int32  `json:"atVMs"`
	// +optional
	Started *metav1.Time `json:"started,omitempty"`
	// +optional
	Ended *metav1.Time `json:"ended,omitempty"`
	// Seconds is the event's main measure: for a boot storm, the time until
	// the last VM sends results again.
	// +optional
	Seconds int64 `json:"seconds,omitempty"`
	// +optional
	Message string `json:"message,omitempty"`
}

// PoolBenchLogEntry is one line of a run's log.
type PoolBenchLogEntry struct {
	Time metav1.Time `json:"time"`
	// Kind is Info, Warning, Error or Control.
	// +kubebuilder:validation:Enum=Info;Warning;Error;Control
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// +kubebuilder:object:root=true

// PoolBenchList contains a list of PoolBench.
type PoolBenchList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []PoolBench `json:"items"`
}

func init() {
	SchemeBuilder.Register(&PoolBench{}, &PoolBenchList{})
}
