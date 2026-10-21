package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/hooks"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// DefaultReplicationInterval is how often the replication controller checks for work.
	DefaultReplicationInterval = 30 * time.Second

	// DefaultReplicationImage is the fallback container image for replication Jobs.
	// In practice, main.go overrides this with the driver image (which includes rsync).
	// This default is only used if SetReplicationImage() is not called.
	DefaultReplicationImage = "quay.io/centos/centos:stream9"

	// replTempLabel marks temporary resources created by the replication controller.
	replTempLabel = "storage.ibmcloud.io/repl-temp"

	// replPolicyLabel identifies which ReplicationPolicy a resource belongs to.
	replPolicyLabel = "storage.ibmcloud.io/repl-policy"

	// replSubVolumeLabel identifies which SubVolume a replication resource belongs to.
	replSubVolumeLabel = "storage.ibmcloud.io/repl-subvolume"

	// replRoleLabel identifies the role of a PV/PVC (source vs dest).
	replRoleLabel = "storage.ibmcloud.io/repl-role"

	// replJobNamespace is the namespace where replication Jobs are created.
	replJobNamespace = "kube-system"

	// pausedAnnotation pauses a policy while it is set to "true". The controller
	// sets it after maxRetries; a person removes it to resume.
	pausedAnnotation = "storage.ibmcloud.io/paused"

	// skippedRunningVMPrefix starts the status error of a volume that the
	// controller did not copy because a running VM uses it.
	skippedRunningVMPrefix = "skipped: in use by running VM"

	// kubevirtPodLabel marks KubeVirt pods. Its value is "virt-launcher" for a
	// VM pod and "hotplug-disk" for a pod that holds a hot-plugged disk.
	kubevirtPodLabel = "kubevirt.io"
)

// ReplicationController manages cross-region replication of SubVolume data.
// It runs as a background goroutine (like CloneWorker) that periodically
// checks ReplicationPolicy CRs and creates Kubernetes Jobs to sync
// SubVolume data to the destination NFS server.
type ReplicationController struct {
	k8sClient    k8s.Client
	directClient client.Client
	nfsOps       NFSOperations
	orchestrator *hooks.Orchestrator

	// vmCopier copies the disks of running VMs. If nil, their volumes are
	// skipped.
	vmCopier VMCopier

	// replicationImage is the container image used for direct-NFS replication Jobs (rsync).
	replicationImage string

	// driverImage is the container image used for receiver-mode sync-client Jobs.
	// Must contain the driver binary with --mode=sync-client support.
	driverImage string

	// interval is the poll interval for checking replication policies.
	interval time.Duration

	// lastSyncTimes tracks the last time each policy was synced, keyed by policy name.
	lastSyncTimes map[string]time.Time

	// lastSubVolumeSyncTimes tracks the last time each SubVolume was synced,
	// keyed by "policyName/svName". Used to prevent scheduling starvation
	// by sorting SubVolumes oldest-first when parallelism limits apply.
	lastSubVolumeSyncTimes map[string]time.Time

	// results holds, per policy, the results of volumes whose Job finished
	// in the current cycle. See processPolicy.
	results map[string]map[string]cycleResult

	mu sync.Mutex

	// nowFunc is used for time operations (injectable for tests).
	nowFunc func() time.Time
}

// NewReplicationController creates a new replication controller.
func NewReplicationController(k8sClient k8s.Client, directClient client.Client, nfsOps NFSOperations) *ReplicationController {
	return &ReplicationController{
		k8sClient:              k8sClient,
		directClient:           directClient,
		nfsOps:                 nfsOps,
		replicationImage:       DefaultReplicationImage,
		interval:               DefaultReplicationInterval,
		lastSyncTimes:          make(map[string]time.Time),
		lastSubVolumeSyncTimes: make(map[string]time.Time),
		results:                make(map[string]map[string]cycleResult),
		nowFunc:                time.Now,
	}
}

// SetOrchestrator sets the hook orchestrator for pre/post sync hooks.
func (rc *ReplicationController) SetOrchestrator(orch *hooks.Orchestrator) {
	rc.orchestrator = orch
}

// SetReplicationImage overrides the default container image for direct-NFS replication Jobs.
func (rc *ReplicationController) SetReplicationImage(image string) {
	if image != "" {
		rc.replicationImage = image
	}
}

// SetDriverImage sets the driver image used for receiver-mode sync-client Jobs.
func (rc *ReplicationController) SetDriverImage(image string) {
	if image != "" {
		rc.driverImage = image
	}
}

// SetInterval overrides the default poll interval. Useful for tests.
func (rc *ReplicationController) SetInterval(d time.Duration) {
	rc.interval = d
}

// SetNowFunc overrides the time source. Useful for tests.
func (rc *ReplicationController) SetNowFunc(f func() time.Time) {
	rc.nowFunc = f
}

// Run starts the replication controller loop. It blocks until the context is cancelled.
func (rc *ReplicationController) Run(ctx context.Context) {
	klog.V(2).InfoS("Replication controller started", "interval", rc.interval)
	ticker := time.NewTicker(rc.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			klog.V(2).InfoS("Replication controller stopped")
			return
		case <-ticker.C:
			rc.processOnce(ctx)
		}
	}
}

// processOnce lists all ReplicationPolicy CRs and processes any that are due.
func (rc *ReplicationController) processOnce(ctx context.Context) {
	policies, err := rc.k8sClient.ListReplicationPolicies(ctx)
	if err != nil {
		klog.ErrorS(err, "Replication controller failed to list policies")
		return
	}

	for i := range policies {
		policy := &policies[i]

		// Handle pause/resume via annotation.
		annotationPaused := policy.Annotations[pausedAnnotation] == "true"

		if annotationPaused {
			// Pause requested — update status if needed and skip.
			if policy.Status.Phase != "Paused" {
				policy.Status.Phase = "Paused"
				if err := rc.k8sClient.UpdateReplicationPolicyStatus(ctx, policy); err != nil {
					klog.ErrorS(err, "Failed to update policy phase to Paused", "policy", policy.Name)
				} else {
					klog.V(2).InfoS("Paused replication policy", "policy", policy.Name)
				}
			}
			continue
		}

		// Resume: annotation removed but status still Paused. Start counting
		// failures afresh, or one new failure would pause the policy again.
		if policy.Status.Phase == "Paused" {
			policy.Status.Phase = "Active"
			policy.Status.ConsecutiveFailures = 0
			if err := rc.k8sClient.UpdateReplicationPolicyStatus(ctx, policy); err != nil {
				klog.ErrorS(err, "Failed to resume policy", "policy", policy.Name)
			} else {
				klog.V(2).InfoS("Resumed replication policy", "policy", policy.Name)
			}
		}

		// Skip failed policies.
		if policy.Status.Phase == "Failed" {
			continue
		}

		// Parse the schedule interval.
		scheduleInterval, err := time.ParseDuration(policy.Spec.Schedule)
		if err != nil {
			klog.ErrorS(err, "Invalid schedule duration in policy",
				"policy", policy.Name, "schedule", policy.Spec.Schedule)
			continue
		}

		// Check if it's time to sync. Copies of running VM disks move forward
		// on every tick.
		if !rc.isDue(policy.Name, scheduleInterval) {
			rc.advanceVMCopies(ctx, policy)
			continue
		}

		// Process this policy (poll-based: create or check Jobs).
		rc.processPolicy(ctx, policy)
	}
}

// cycleResult is the result of one volume's Job in the current cycle.
type cycleResult struct {
	status  v1alpha1.SubVolumeReplicationStatus
	syncErr string // "" if the copy succeeded
}

// cycleResults returns the results recorded so far in the policy's current
// cycle. Only the controller goroutine uses it.
func (rc *ReplicationController) cycleResults(policyName string) map[string]cycleResult {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	r := rc.results[policyName]
	if r == nil {
		r = make(map[string]cycleResult)
		rc.results[policyName] = r
	}
	return r
}

func (rc *ReplicationController) clearCycleResults(policyName string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	delete(rc.results, policyName)
}

// isDue returns true if the policy is due for a replication cycle.
func (rc *ReplicationController) isDue(policyName string, interval time.Duration) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()

	lastSync, ok := rc.lastSyncTimes[policyName]
	if !ok {
		return true
	}
	return rc.nowFunc().Sub(lastSync) >= interval
}

// recordSyncTime records the current time as the last sync time for the policy.
func (rc *ReplicationController) recordSyncTime(policyName string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.lastSyncTimes[policyName] = rc.nowFunc()
}

// recordSubVolumeSyncTime records the current time as the last sync time for a SubVolume.
func (rc *ReplicationController) recordSubVolumeSyncTime(policyName, svName string) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	rc.lastSubVolumeSyncTimes[policyName+"/"+svName] = rc.nowFunc()
}

// sortByLastSyncTime sorts SubVolumes by their last sync time (oldest first).
// SubVolumes that have never been synced sort first (zero time).
func (rc *ReplicationController) sortByLastSyncTime(policyName string, svs []v1alpha1.SubVolume) {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	sort.SliceStable(svs, func(i, j int) bool {
		keyI := policyName + "/" + svs[i].Name
		keyJ := policyName + "/" + svs[j].Name
		tI := rc.lastSubVolumeSyncTimes[keyI]
		tJ := rc.lastSubVolumeSyncTimes[keyJ]
		return tI.Before(tJ)
	})
}

// processPolicy runs a poll-based replication cycle for the given policy.
// On each tick, for each SubVolume:
//   - If no Job exists → create PV/PVC/Job
//   - If Job running → skip (check next tick)
//   - If Job succeeded → record success, cleanup temp resources
//   - If Job failed → record failure, cleanup, increment failure count
func (rc *ReplicationController) processPolicy(ctx context.Context, policy *v1alpha1.ReplicationPolicy) {
	start := rc.nowFunc()
	policyName := policy.Name
	sourcePool := policy.Spec.SourcePoolName

	klog.V(2).InfoS("Starting replication cycle",
		"policy", policyName,
		"sourcePool", sourcePool,
	)

	// Always record that we attempted a sync (to prevent tight retry loops).
	rc.recordSyncTime(policyName)

	// Set phase to Active if not already.
	if policy.Status.Phase == "" {
		policy.Status.Phase = "Active"
	}

	// 1. List SubVolumes in the source pool.
	subVolumes, err := rc.k8sClient.ListSubVolumes(ctx, sourcePool)
	if err != nil {
		rc.handleFailure(ctx, policy, fmt.Sprintf("list SubVolumes: %v", err), start)
		return
	}

	// 2. Filter by selector if specified.
	matched := filterSubVolumes(subVolumes, policy.Spec.SubVolumeSelector)

	// Sort matched SubVolumes by last sync time (oldest first) to prevent
	// scheduling starvation when maxParallelSyncs < total SubVolumes.
	rc.sortByLastSyncTime(policyName, matched)

	// Update SubVolume count metric.
	metrics.ReplicationSubVolumeCount.WithLabelValues(policyName, sourcePool).Set(float64(len(matched)))
	total := len(matched)

	// 2a. Run pre-sync hooks.
	if rc.orchestrator != nil && len(policy.Spec.PreSyncHooks) > 0 {
		_, hookErr := rc.orchestrator.RunPreHooks(ctx, policy.Spec.PreSyncHooks)
		if hookErr != nil {
			rc.handleFailure(ctx, policy, fmt.Sprintf("pre-sync hooks: %v", hookErr), start)
			return
		}
	}

	// 3. Process each matched SubVolume via Jobs (with parallelism limit).
	maxParallel := policy.Spec.MaxParallelSyncs
	if maxParallel <= 0 {
		maxParallel = 1
	}

	var svStatuses []v1alpha1.SubVolumeReplicationStatus
	var syncErrors []string
	activeJobs := int32(0)
	skipped := 0

	// Disks of running VMs take the VM copy path, if it is on.
	matched, vmRes := rc.processVMVolumes(ctx, policy, matched, true)
	svStatuses = append(svStatuses, vmRes.statuses...)

	// A volume whose Job finished in this cycle keeps its result, and gets no
	// new Job, until every volume of the cycle has finished. Otherwise a
	// cycle with all Jobs finished at once might never come, and failures
	// would never be counted.
	// A status keeps the volume's last good sync, so the age of the last good
	// copy stays visible while a new copy runs or after one fails.
	prevStatus := map[string]v1alpha1.SubVolumeReplicationStatus{}
	for _, st := range policy.Status.SubVolumeStatuses {
		prevStatus[st.SubVolumeName] = st
	}
	carry := func(name, lastError string) v1alpha1.SubVolumeReplicationStatus {
		st := prevStatus[name]
		st.SubVolumeName = name
		st.LastError = lastError
		st.VMCopy = nil
		return st
	}

	round := rc.cycleResults(policyName)
	finish := func(st v1alpha1.SubVolumeReplicationStatus, syncErr string) {
		round[st.SubVolumeName] = cycleResult{status: st, syncErr: syncErr}
		svStatuses = append(svStatuses, st)
		if syncErr != "" {
			syncErrors = append(syncErrors, syncErr)
		}
	}

	for _, sv := range matched {
		svCopy := sv

		if r, ok := round[svCopy.Name]; ok {
			svStatuses = append(svStatuses, r.status)
			if r.syncErr != "" {
				syncErrors = append(syncErrors, r.syncErr)
			}
			continue
		}

		// Check if a Job already exists for this SubVolume.
		done, succeeded, jobRes, checkErr := rc.checkReplicationJob(ctx, policyName, svCopy.Name)
		bytesWritten := jobRes.BytesWritten
		if checkErr != nil {
			klog.ErrorS(checkErr, "Failed to check replication Job status",
				"policy", policyName, "subVolume", svCopy.Name)
			syncErrors = append(syncErrors, fmt.Sprintf("%s: check job: %v", svCopy.Name, checkErr))
			svStatuses = append(svStatuses, carry(svCopy.Name, fmt.Sprintf("check job: %v", checkErr)))
			continue
		}

		if done {
			if succeeded {
				// A VM that started during the copy wrote to the disk while it
				// was read, so the copy may not boot.
				if vmPod, vmErr := rc.runningVMPod(ctx, &svCopy); vmErr != nil || vmPod != "" {
					msg := fmt.Sprintf("VM pod %s/%s started during the copy; the copy may not boot", svCopy.Spec.PVCNamespace, vmPod)
					if vmErr != nil {
						msg = fmt.Sprintf("check for a running VM after the copy: %v", vmErr)
					}
					finish(carry(svCopy.Name, msg), fmt.Sprintf("%s: %s", svCopy.Name, msg))
					rc.cleanupReplicationResources(ctx, policy, svCopy.Name)
					continue
				}
				// Job succeeded — record success and cleanup.
				now := metav1.NewTime(rc.nowFunc())
				svStatus := v1alpha1.SubVolumeReplicationStatus{
					SubVolumeName: svCopy.Name,
					LastSyncTime:  &now,
				}
				if bytesWritten > 0 {
					svStatus.BytesSynced = &bytesWritten
				}
				if jobRes.RestorePoints > 0 {
					svStatus.RestorePoints = jobRes.RestorePoints
					if jobRes.OldestRestorePoint != nil {
						t := metav1.NewTime(*jobRes.OldestRestorePoint)
						svStatus.OldestRestorePoint = &t
					}
				}
				finish(svStatus, "")
				rc.recordSubVolumeSyncTime(policyName, svCopy.Name)
				rc.cleanupReplicationResources(ctx, policy, svCopy.Name)
			} else {
				// Job failed.
				finish(carry(svCopy.Name, "replication Job failed after retries"), fmt.Sprintf("%s: replication Job failed", svCopy.Name))
				rc.cleanupReplicationResources(ctx, policy, svCopy.Name)
			}
			continue
		}

		// Check if Job exists but is still running.
		jobName := replJobName(policyName, svCopy.Name)
		existing := &batchv1.Job{}
		if err := rc.directClient.Get(ctx, types.NamespacedName{
			Namespace: replJobNamespace,
			Name:      jobName,
		}, existing); err == nil {
			// Job still running, skip.
			activeJobs++
			klog.V(6).InfoS("Replication Job still running",
				"policy", policyName, "subVolume", svCopy.Name, "job", jobName)
			svStatuses = append(svStatuses, carry(svCopy.Name, "sync in progress"))
			continue
		}

		// No Job exists yet — create one if under parallel limit.
		if activeJobs >= maxParallel {
			klog.V(4).InfoS("Parallel sync limit reached, deferring SubVolume",
				"policy", policyName, "subVolume", svCopy.Name,
				"activeJobs", activeJobs, "maxParallel", maxParallel)
			svStatuses = append(svStatuses, carry(svCopy.Name, "deferred: parallel limit"))
			continue
		}

		// A file copy of a running VM's disk mixes data from different
		// moments and may not boot, so the controller never makes one.
		vmPod, vmErr := rc.runningVMPod(ctx, &svCopy)
		if vmErr != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("%s: check for a running VM: %v", svCopy.Name, vmErr))
			svStatuses = append(svStatuses, carry(svCopy.Name, fmt.Sprintf("check for a running VM: %v", vmErr)))
			continue
		}
		if vmPod != "" {
			klog.InfoS("Skipping replication of a volume in use by a running VM",
				"policy", policyName, "subVolume", svCopy.Name,
				"pod", klog.KRef(svCopy.Spec.PVCNamespace, vmPod))
			skipped++
			svStatuses = append(svStatuses, carry(svCopy.Name, fmt.Sprintf("%s %s/%s; a file copy of a running VM disk may not boot",
				skippedRunningVMPrefix, svCopy.Spec.PVCNamespace, vmPod)))
			continue
		}

		// Create PV/PVC/Job for this SubVolume.
		if createErr := rc.createReplicationResources(ctx, policy, &svCopy); createErr != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("%s: create resources: %v", svCopy.Name, createErr))
			svStatuses = append(svStatuses, carry(svCopy.Name, fmt.Sprintf("create resources: %v", createErr)))
			rc.cleanupReplicationResources(ctx, policy, svCopy.Name)
			continue
		}

		activeJobs++
		svStatuses = append(svStatuses, carry(svCopy.Name, "sync in progress"))
	}

	// 4. Update policy status.
	duration := rc.nowFunc().Sub(start)

	// Check if all Jobs completed (no active or deferred).
	allComplete := !vmRes.inProgress
	for _, svs := range svStatuses {
		if svs.LastError == "sync in progress" || svs.LastError == "deferred: parallel limit" {
			allComplete = false
			break
		}
	}

	if !allComplete {
		// Some Jobs still running or deferred — don't count as a fully completed cycle.
		// However, update LastSyncTime incrementally if any SubVolumes succeeded,
		// so the policy status reflects progress rather than staying permanently empty.
		anySucceeded := false
		for _, svs := range svStatuses {
			if svs.LastSyncTime != nil && svs.LastError == "" {
				anySucceeded = true
				break
			}
		}
		if anySucceeded {
			now := metav1.NewTime(rc.nowFunc())
			policy.Status.LastSyncTime = &now
			policy.Status.LastSyncDuration = duration.String()
			policy.Status.ConsecutiveFailures = 0
			policy.Status.LastError = ""
		}
		policy.Status.SubVolumeStatuses = svStatuses
		if err := rc.k8sClient.UpdateReplicationPolicyStatus(ctx, policy); err != nil {
			klog.ErrorS(err, "Failed to update policy status (cycle in progress)", "policy", policyName)
		}
		klog.V(4).InfoS("Replication cycle still in progress",
			"policy", policyName, "activeJobs", activeJobs)
		return
	}

	// All SubVolumes processed — evaluate results. The next cycle starts
	// afresh.
	rc.clearCycleResults(policyName)
	if len(syncErrors) > 0 {
		combinedErr := fmt.Sprintf("%d/%d SubVolumes failed to sync", len(syncErrors), total)
		// Set the per-volume statuses first, so the one status write in
		// handleFailure carries them with the failure count.
		policy.Status.SubVolumeStatuses = svStatuses
		rc.handleFailure(ctx, policy, combinedErr, start)
		return
	}

	// 3a. Run post-sync hooks.
	if rc.orchestrator != nil && len(policy.Spec.PostSyncHooks) > 0 {
		_, hookErr := rc.orchestrator.RunPostHooks(ctx, policy.Spec.PostSyncHooks)
		if hookErr != nil {
			klog.ErrorS(hookErr, "Post-sync hooks failed", "policy", policyName)
		}
	}

	// All succeeded, apart from skipped VM volumes. Those do not count as a
	// failed cycle, so they never pause the policy for the other volumes.
	now := metav1.NewTime(rc.nowFunc())
	policy.Status.Phase = "Active"
	policy.Status.LastSyncTime = &now
	policy.Status.LastSyncDuration = duration.String()
	policy.Status.SubVolumeStatuses = svStatuses
	policy.Status.ConsecutiveFailures = 0
	policy.Status.LastError = vmProblemsError(skipped, vmRes.failed)

	if err := rc.k8sClient.UpdateReplicationPolicyStatus(ctx, policy); err != nil {
		klog.ErrorS(err, "Failed to update policy status after successful sync", "policy", policyName)
	}

	// Record metrics.
	metrics.ReplicationSyncsTotal.WithLabelValues(policyName, sourcePool, "success").Inc()
	metrics.ReplicationSyncDuration.WithLabelValues(policyName, sourcePool).Observe(duration.Seconds())
	metrics.ReplicationLagSeconds.WithLabelValues(policyName, sourcePool).Set(0)
	metrics.ReplicationConsecutiveFailures.WithLabelValues(policyName, sourcePool).Set(0)

	klog.V(2).InfoS("Replication cycle completed",
		"policy", policyName,
		"sourcePool", sourcePool,
		"subVolumes", total,
		"duration", duration.String(),
	)
}

// runningVMPod returns the name of a KubeVirt pod that is running or starting
// and mounts the SubVolume's PVC, or "" if there is none.
func (rc *ReplicationController) runningVMPod(ctx context.Context, sv *v1alpha1.SubVolume) (string, error) {
	pod, _, err := rc.vmPodForClaim(ctx, sv.Spec.PVCNamespace, sv.Spec.PVCName)
	if err != nil || pod == nil {
		return "", err
	}
	return pod.Name, nil
}

// vmPodForClaim returns a KubeVirt pod that is running or starting and mounts
// the claim, and the pod's volume name for it, or nil if there is none. Both a
// VM pod and a hot-plug disk pod count, because both mean a running VM writes
// to the disk; a VM pod is preferred.
func (rc *ReplicationController) vmPodForClaim(ctx context.Context, namespace, claim string) (*corev1.Pod, string, error) {
	return findVMPodForClaim(ctx, rc.directClient, namespace, claim)
}

// vmProblemsError is the policy error for a cycle that skipped VM volumes or
// whose VM disk copies failed. Neither counts as a failed cycle, so neither
// pauses the policy for its other volumes.
func vmProblemsError(skipped, failed int) string {
	var parts []string
	switch skipped {
	case 0:
	case 1:
		parts = append(parts, "1 SubVolume skipped: in use by a running VM")
	default:
		parts = append(parts, fmt.Sprintf("%d SubVolumes skipped: in use by a running VM", skipped))
	}
	if failed > 0 {
		parts = append(parts, fmt.Sprintf("%d VM disk copies failed; see subVolumeStatuses", failed))
	}
	return strings.Join(parts, "; ")
}

// isReceiverMode returns true if the policy uses driver-to-driver replication.
func isReceiverMode(policy *v1alpha1.ReplicationPolicy) bool {
	return policy.Spec.DestinationEndpoint != ""
}

// createReplicationResources creates the PV, PVC, and Job for a single SubVolume sync.
func (rc *ReplicationController) createReplicationResources(ctx context.Context, policy *v1alpha1.ReplicationPolicy, sv *v1alpha1.SubVolume) error {
	if isReceiverMode(policy) {
		return rc.createReceiverModeResources(ctx, policy, sv)
	}
	return rc.createDirectNFSResources(ctx, policy, sv)
}

// createDirectNFSResources creates resources for direct NFS mode (existing behavior).
func (rc *ReplicationController) createDirectNFSResources(ctx context.Context, policy *v1alpha1.ReplicationPolicy, sv *v1alpha1.SubVolume) error {
	policyName := policy.Name

	// 1. Create source PV (mounts the source share).
	if err := rc.ensureReplicationSourcePV(ctx, sv, policyName); err != nil {
		return fmt.Errorf("source PV: %w", err)
	}

	// 2. Create source PVC bound to source PV.
	srcPVName := replSourcePVName(policyName, sv.Name)
	srcPVCName := replSourcePVCName(policyName, sv.Name)
	if err := rc.ensureReplicationPVC(ctx, srcPVName, srcPVCName, policyName, sv.Name, "source"); err != nil {
		return fmt.Errorf("source PVC: %w", err)
	}

	// 3. Create destination PV (mounts the destination share).
	if err := rc.ensureReplicationDestPV(ctx, policy, sv.Name); err != nil {
		return fmt.Errorf("dest PV: %w", err)
	}

	// 4. Create destination PVC bound to dest PV.
	dstPVName := replDestPVName(policyName, sv.Name)
	dstPVCName := replDestPVCName(policyName, sv.Name)
	if err := rc.ensureReplicationPVC(ctx, dstPVName, dstPVCName, policyName, sv.Name, "dest"); err != nil {
		return fmt.Errorf("dest PVC: %w", err)
	}

	// 5. Create the replication Job.
	if err := rc.ensureReplicationJob(ctx, policy, sv); err != nil {
		return fmt.Errorf("job: %w", err)
	}

	klog.V(2).InfoS("Created replication Job",
		"policy", policyName,
		"subVolume", sv.Name,
		"job", replJobName(policyName, sv.Name),
	)
	return nil
}

// createReceiverModeResources creates resources for driver-to-driver replication.
// Only the source PV/PVC is created (no dest). The Job runs the driver binary
// in sync-client mode and uploads data via HTTPS to the destination receiver.
func (rc *ReplicationController) createReceiverModeResources(ctx context.Context, policy *v1alpha1.ReplicationPolicy, sv *v1alpha1.SubVolume) error {
	policyName := policy.Name

	// 1. Create source PV (mounts the source share).
	if err := rc.ensureReplicationSourcePV(ctx, sv, policyName); err != nil {
		return fmt.Errorf("source PV: %w", err)
	}

	// 2. Create source PVC bound to source PV.
	srcPVName := replSourcePVName(policyName, sv.Name)
	srcPVCName := replSourcePVCName(policyName, sv.Name)
	if err := rc.ensureReplicationPVC(ctx, srcPVName, srcPVCName, policyName, sv.Name, "source"); err != nil {
		return fmt.Errorf("source PVC: %w", err)
	}

	// 3. Create the sync-client Job (no dest PV/PVC needed).
	if err := rc.ensureReceiverModeJob(ctx, policy, sv); err != nil {
		return fmt.Errorf("job: %w", err)
	}

	klog.V(2).InfoS("Created receiver-mode replication Job",
		"policy", policyName,
		"subVolume", sv.Name,
		"job", replJobName(policyName, sv.Name),
		"endpoint", policy.Spec.DestinationEndpoint,
	)
	return nil
}

// ensureReceiverModeJob creates a Job that runs the driver binary in sync-client mode.
func (rc *ReplicationController) ensureReceiverModeJob(ctx context.Context, policy *v1alpha1.ReplicationPolicy, sv *v1alpha1.SubVolume) error {
	policyName := policy.Name
	jobName := replJobName(policyName, sv.Name)

	existing := &batchv1.Job{}
	if err := rc.directClient.Get(ctx, types.NamespacedName{
		Namespace: replJobNamespace,
		Name:      jobName,
	}, existing); err == nil {
		return nil // already exists
	}

	// Build metadata JSON.
	meta := &SubVolumeMetadata{
		SubVolumeName:     sv.Name,
		Spec:              sv.Spec,
		Labels:            sv.Labels,
		LastSyncTime:      rc.nowFunc(),
		ReplicationPolicy: policyName,
	}
	metaJSON, _ := json.MarshalIndent(meta, "", "  ")

	subPath := sv.Spec.SubPath
	destBasePath := policy.Spec.DestinationBasePath

	backoffLimit := int32(3)
	ttl := int32(600)

	srcPVCName := replSourcePVCName(policyName, sv.Name)

	containerArgs := []string{
		"--mode=sync-client",
		"--receiver-endpoint=" + policy.Spec.DestinationEndpoint,
		"--auth-token-file=/etc/replication/token",
		"--source-path=/source",
		"--dest-base-path=" + destBasePath,
		"--sub-path=" + subPath,
		"--subvolume-name=" + sv.Name,
		"--metadata-json=" + string(metaJSON),
	}

	volumeMounts := []corev1.VolumeMount{
		{Name: "source", MountPath: "/source", ReadOnly: true},
		{Name: "auth-token", MountPath: "/etc/replication", ReadOnly: true},
	}

	volumes := []corev1.Volume{
		{
			Name: "source",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: srcPVCName,
					ReadOnly:  true,
				},
			},
		},
		{
			Name: "auth-token",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: policy.Spec.DestinationAuthSecretRef,
				},
			},
		},
	}

	// Mount CA certificate for TLS verification if configured.
	if policy.Spec.DestinationCACertSecretRef != "" {
		containerArgs = append(containerArgs, "--ca-cert-file=/etc/replication-ca/ca.crt")
		volumeMounts = append(volumeMounts, corev1.VolumeMount{
			Name:      "ca-cert",
			MountPath: "/etc/replication-ca",
			ReadOnly:  true,
		})
		volumes = append(volumes, corev1.Volume{
			Name: "ca-cert",
			VolumeSource: corev1.VolumeSource{
				Secret: &corev1.SecretVolumeSource{
					SecretName: policy.Spec.DestinationCACertSecretRef,
				},
			},
		})
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: replJobNamespace,
			Labels: map[string]string{
				replTempLabel:      "true",
				replPolicyLabel:    policyName,
				replSubVolumeLabel: sv.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						replTempLabel:      "true",
						replPolicyLabel:    policyName,
						replSubVolumeLabel: sv.Name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: int64Ptr(0),
					},
					Containers: []corev1.Container{
						{
							Name:  "sync-client",
							Image: rc.driverImage,
							Args:  containerArgs,
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
							VolumeMounts: volumeMounts,
						},
					},
					Volumes: volumes,
				},
			},
		},
	}

	if err := rc.directClient.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating receiver-mode replication Job %s/%s: %w", replJobNamespace, jobName, err)
	}

	klog.V(2).InfoS("Created receiver-mode replication Job",
		"namespace", replJobNamespace,
		"job", jobName,
		"policy", policyName,
		"subVolume", sv.Name,
		"image", rc.driverImage,
		"endpoint", policy.Spec.DestinationEndpoint,
	)
	return nil
}

// ensureReplicationSourcePV creates a temporary NFS PV for the source share.
func (rc *ReplicationController) ensureReplicationSourcePV(ctx context.Context, sv *v1alpha1.SubVolume, policyName string) error {
	pvName := replSourcePVName(policyName, sv.Name)

	existing := &corev1.PersistentVolume{}
	if err := rc.directClient.Get(ctx, types.NamespacedName{Name: pvName}, existing); err == nil {
		return nil // already exists
	}

	exportPath := sv.Spec.ShareExportPath
	if exportPath == "" {
		exportPath = "/"
	}

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				replTempLabel:      "true",
				replPolicyLabel:    policyName,
				replSubVolumeLabel: sv.Name,
				replRoleLabel:      "source",
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("100Gi"),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			MountOptions: []string{
				"nfsvers=4.1",
				"soft",
				"timeo=600",
				"retrans=3",
				"sec=sys",
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{
					Server: sv.Spec.ShareMountTargetIP,
					Path:   exportPath,
				},
			},
			StorageClassName: "",
			ClaimRef: &corev1.ObjectReference{
				Name:      replSourcePVCName(policyName, sv.Name),
				Namespace: replJobNamespace,
			},
		},
	}

	if err := rc.directClient.Create(ctx, pv); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating source PV %s: %w", pvName, err)
	}

	klog.V(4).InfoS("Created temp replication source PV",
		"pv", pvName,
		"server", sv.Spec.ShareMountTargetIP,
		"exportPath", exportPath,
	)
	return nil
}

// ensureReplicationDestPV creates a temporary NFS PV for the destination share.
func (rc *ReplicationController) ensureReplicationDestPV(ctx context.Context, policy *v1alpha1.ReplicationPolicy, svName string) error {
	pvName := replDestPVName(policy.Name, svName)

	existing := &corev1.PersistentVolume{}
	if err := rc.directClient.Get(ctx, types.NamespacedName{Name: pvName}, existing); err == nil {
		return nil // already exists
	}

	exportPath := policy.Spec.DestinationExportPath
	if exportPath == "" {
		exportPath = "/"
	}

	pv := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				replTempLabel:      "true",
				replPolicyLabel:    policy.Name,
				replSubVolumeLabel: svName,
				replRoleLabel:      "dest",
			},
		},
		Spec: corev1.PersistentVolumeSpec{
			Capacity: corev1.ResourceList{
				corev1.ResourceStorage: resource.MustParse("100Gi"),
			},
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			PersistentVolumeReclaimPolicy: corev1.PersistentVolumeReclaimDelete,
			MountOptions: []string{
				"nfsvers=4.1",
				"soft",
				"timeo=600",
				"retrans=3",
				"sec=sys",
			},
			PersistentVolumeSource: corev1.PersistentVolumeSource{
				NFS: &corev1.NFSVolumeSource{
					Server: policy.Spec.DestinationNFSServer,
					Path:   exportPath,
				},
			},
			StorageClassName: "",
			ClaimRef: &corev1.ObjectReference{
				Name:      replDestPVCName(policy.Name, svName),
				Namespace: replJobNamespace,
			},
		},
	}

	if err := rc.directClient.Create(ctx, pv); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating dest PV %s: %w", pvName, err)
	}

	klog.V(4).InfoS("Created temp replication dest PV",
		"pv", pvName,
		"server", policy.Spec.DestinationNFSServer,
		"exportPath", exportPath,
	)
	return nil
}

// ensureReplicationPVC creates a temporary PVC pre-bound to a temp NFS PV.
func (rc *ReplicationController) ensureReplicationPVC(ctx context.Context, pvName, pvcName, policyName, svName, role string) error {
	existing := &corev1.PersistentVolumeClaim{}
	if err := rc.directClient.Get(ctx, types.NamespacedName{
		Namespace: replJobNamespace,
		Name:      pvcName,
	}, existing); err == nil {
		return nil // already exists
	}

	emptyStorageClass := ""
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcName,
			Namespace: replJobNamespace,
			Labels: map[string]string{
				replTempLabel:      "true",
				replPolicyLabel:    policyName,
				replSubVolumeLabel: svName,
				replRoleLabel:      role,
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			StorageClassName: &emptyStorageClass,
			VolumeName:       pvName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("100Gi"),
				},
			},
		},
	}

	if err := rc.directClient.Create(ctx, pvc); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating PVC %s/%s: %w", replJobNamespace, pvcName, err)
	}

	klog.V(4).InfoS("Created temp replication PVC",
		"namespace", replJobNamespace,
		"pvc", pvcName,
		"role", role,
		"boundTo", pvName,
	)
	return nil
}

// metadataScript writes the metadata file of a volume at the destination. With
// atomic set it writes a new file and renames it, so a moment that links the
// old file keeps its contents.
func metadataScript(destBasePath, subPath, metaJSON string, atomic bool) string {
	file := fmt.Sprintf("/dest%s%s/.subvolume-metadata.json", destBasePath, subPath)
	if !atomic {
		return fmt.Sprintf("echo \"Writing metadata sidecar...\"\ncat > \"%s\" <<'METADATA'\n%s\nMETADATA\n", file, metaJSON)
	}
	return fmt.Sprintf("echo \"Writing metadata sidecar...\"\ncat > \"%[1]s.tmp\" <<'METADATA'\n%[2]s\nMETADATA\nmv \"%[1]s.tmp\" \"%[1]s\"\n", file, metaJSON)
}

// momentsScript keeps the synced volume as a moment: a folder of hard links in
// .moments next to the destination. It removes moments older than the keep
// time, and reports how many are left and the oldest, in the Job's termination
// message. It returns "" when moments are off.
func momentsScript(destBasePath, subPath string, keepHours *int32) string {
	if keepHours == nil || *keepHours <= 0 {
		return ""
	}
	return fmt.Sprintf(`echo "Keeping a moment..."
moments="/dest%[1]s/.moments%[2]s"
mkdir -p "$moments"
stamp=$(date -u +%%Y%%m%%dT%%H%%M%%SZ)
[ -e "$moments/$stamp" ] || cp -a -l "/dest%[1]s%[2]s" "$moments/$stamp"
cutoff=$(date -u -d "-%[3]d hours" +%%Y%%m%%dT%%H%%M%%SZ)
for d in "$moments"/*; do
  [ -d "$d" ] || continue
  if [[ "$(basename "$d")" < "$cutoff" ]]; then rm -r -f -- "$d"; fi
done
count=$(find "$moments" -mindepth 1 -maxdepth 1 -type d | wc -l)
oldest=$(ls -1 "$moments" | head -n 1)
oldest_iso=$(date -u -d "${oldest:0:4}-${oldest:4:2}-${oldest:6:2} ${oldest:9:2}:${oldest:11:2}:${oldest:13:2}" +%%Y-%%m-%%dT%%H:%%M:%%SZ)
printf '{"bytesWritten":0,"restorePoints":%%s,"oldestRestorePoint":"%%s"}' "$count" "$oldest_iso" > /dev/termination-log
`, destBasePath, subPath, *keepHours)
}

// ensureReplicationJob creates a Job that runs rsync from source to destination.
func (rc *ReplicationController) ensureReplicationJob(ctx context.Context, policy *v1alpha1.ReplicationPolicy, sv *v1alpha1.SubVolume) error {
	policyName := policy.Name
	jobName := replJobName(policyName, sv.Name)

	existing := &batchv1.Job{}
	if err := rc.directClient.Get(ctx, types.NamespacedName{
		Namespace: replJobNamespace,
		Name:      jobName,
	}, existing); err == nil {
		return nil // already exists
	}

	// Build rsync options. Moments are hard links, so the copy must write a
	// changed file as a new file: rsync does, cp does not.
	keepMoments := policy.Spec.MomentKeepHours != nil && *policy.Spec.MomentKeepHours > 0
	useIncremental := policy.Spec.IncrementalSync == nil || *policy.Spec.IncrementalSync || keepMoments

	bwlimit := ""
	if policy.Spec.BandwidthLimitMbps > 0 {
		// Convert Mbps to KBps: 1 Mbps = 125 KBps.
		kbps := policy.Spec.BandwidthLimitMbps * 125
		bwlimit = fmt.Sprintf("--bwlimit=%d", kbps)
	}

	extraArgs := ""
	for _, arg := range policy.Spec.RsyncOptions {
		extraArgs += " " + arg
	}

	// Build metadata JSON for the sidecar file.
	meta := &SubVolumeMetadata{
		SubVolumeName:     sv.Name,
		Spec:              sv.Spec,
		Labels:            sv.Labels,
		LastSyncTime:      rc.nowFunc(),
		ReplicationPolicy: policyName,
	}
	metaJSON, _ := json.MarshalIndent(meta, "", "  ")

	subPath := sv.Spec.SubPath
	destBasePath := policy.Spec.DestinationBasePath

	var script string
	if useIncremental {
		script = fmt.Sprintf(`set -euo pipefail
echo "Replication: syncing %s -> %s%s"
mkdir -p "/dest%s%s"
rsync -a --delete --exclude=/.repl-staging-* %s %s %s "/source%s/" "/dest%s%s/"
%s%s
echo "Replication complete."
`, subPath, destBasePath, subPath,
			destBasePath, subPath,
			bwlimit, extraArgs, "", // empty string placeholder for cleaner formatting
			subPath, destBasePath, subPath,
			metadataScript(destBasePath, subPath, string(metaJSON), keepMoments),
			momentsScript(destBasePath, subPath, policy.Spec.MomentKeepHours))
	} else {
		script = fmt.Sprintf(`set -euo pipefail
echo "Replication: copying %s -> %s%s"
mkdir -p "/dest%s%s"
cp -a "/source%s/." "/dest%s%s/"
find "/dest%s%s" -maxdepth 1 -name '.repl-staging-*' -delete
%s
echo "Replication complete."
`, subPath, destBasePath, subPath,
			destBasePath, subPath,
			subPath, destBasePath, subPath,
			destBasePath, subPath,
			metadataScript(destBasePath, subPath, string(metaJSON), false))
	}

	backoffLimit := int32(3)
	ttl := int32(600)

	srcPVCName := replSourcePVCName(policyName, sv.Name)
	dstPVCName := replDestPVCName(policyName, sv.Name)

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName,
			Namespace: replJobNamespace,
			Labels: map[string]string{
				replTempLabel:      "true",
				replPolicyLabel:    policyName,
				replSubVolumeLabel: sv.Name,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						replTempLabel:      "true",
						replPolicyLabel:    policyName,
						replSubVolumeLabel: sv.Name,
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser: int64Ptr(0),
					},
					Containers: []corev1.Container{
						{
							Name:    "repl-sync",
							Image:   rc.replicationImage,
							Command: []string{"/bin/sh", "-c", script},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("100m"),
									corev1.ResourceMemory: resource.MustParse("128Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    resource.MustParse("1"),
									corev1.ResourceMemory: resource.MustParse("512Mi"),
								},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "source", MountPath: "/source", ReadOnly: true},
								{Name: "dest", MountPath: "/dest"},
							},
						},
					},
					Volumes: []corev1.Volume{
						{
							Name: "source",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: srcPVCName,
									ReadOnly:  true,
								},
							},
						},
						{
							Name: "dest",
							VolumeSource: corev1.VolumeSource{
								PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
									ClaimName: dstPVCName,
								},
							},
						},
					},
				},
			},
		},
	}

	if err := rc.directClient.Create(ctx, job); err != nil {
		if errors.IsAlreadyExists(err) {
			return nil
		}
		return fmt.Errorf("creating replication Job %s/%s: %w", replJobNamespace, jobName, err)
	}

	klog.V(2).InfoS("Created replication Job",
		"namespace", replJobNamespace,
		"job", jobName,
		"policy", policyName,
		"subVolume", sv.Name,
		"image", rc.replicationImage,
	)
	return nil
}

// checkReplicationJob checks the status of a replication Job.
// Returns (done, succeeded, result, error). If done=false, the Job is still running or doesn't exist.
// bytesWritten is parsed from the pod's termination message (only for receiver-mode Jobs).
func (rc *ReplicationController) checkReplicationJob(ctx context.Context, policyName, svName string) (bool, bool, replJobResult, error) {
	jobName := replJobName(policyName, svName)

	job := &batchv1.Job{}
	err := rc.directClient.Get(ctx, types.NamespacedName{
		Namespace: replJobNamespace,
		Name:      jobName,
	}, job)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, false, replJobResult{}, nil // no Job yet
		}
		return false, false, replJobResult{}, fmt.Errorf("getting replication Job %s/%s: %w", replJobNamespace, jobName, err)
	}

	if job.Status.Succeeded > 0 {
		return true, true, rc.readJobResult(ctx, job), nil
	}

	// Check if all retries exhausted.
	backoffLimit := int32(3)
	if job.Spec.BackoffLimit != nil {
		backoffLimit = *job.Spec.BackoffLimit
	}
	if job.Status.Failed >= backoffLimit+1 {
		return true, false, replJobResult{}, nil
	}

	return false, false, replJobResult{}, nil // still running
}

// replJobResult is what a finished replication Job reports in its pod's
// termination message (sync-client and apply-vm-disk modes).
type replJobResult struct {
	BytesWritten       int64      `json:"bytesWritten"`
	RestorePoints      int32      `json:"restorePoints"`
	OldestRestorePoint *time.Time `json:"oldestRestorePoint,omitempty"`
	// WrongBase is true if a send-vm-disk Job found the far-site disk at
	// another moment than the copy starts from, and DiskMoment says which.
	WrongBase  bool   `json:"wrongBase,omitempty"`
	DiskMoment string `json:"diskMoment,omitempty"`
}

// readJobResult reads the result from the completed pod's termination
// message, which the Job writes as JSON to /dev/termination-log.
func (rc *ReplicationController) readJobResult(ctx context.Context, job *batchv1.Job) replJobResult {
	// List pods for this Job.
	podList := &corev1.PodList{}
	labels := job.Spec.Template.Labels
	matchLabels := client.MatchingLabels(labels)
	if err := rc.directClient.List(ctx, podList, matchLabels, client.InNamespace(job.Namespace)); err != nil {
		klog.V(4).InfoS("Could not list pods for replication Job", "job", job.Name, "err", err)
		return replJobResult{}
	}

	for _, pod := range podList.Items {
		for _, cs := range pod.Status.ContainerStatuses {
			if cs.State.Terminated != nil && cs.State.Terminated.Message != "" {
				var msg replJobResult
				if err := json.Unmarshal([]byte(cs.State.Terminated.Message), &msg); err == nil && (msg.BytesWritten > 0 || msg.RestorePoints > 0 || msg.WrongBase) {
					return msg
				}
			}
		}
	}
	return replJobResult{}
}

// cleanupReplicationResources deletes the temporary PV, PVC, and Job for a replication sync.
func (rc *ReplicationController) cleanupReplicationResources(ctx context.Context, policy *v1alpha1.ReplicationPolicy, svName string) {
	policyName := policy.Name
	receiverMode := isReceiverMode(policy)
	jobName := replJobName(policyName, svName)
	srcPVCName := replSourcePVCName(policyName, svName)
	srcPVName := replSourcePVName(policyName, svName)

	// Delete Job first (stops pods).
	propagation := metav1.DeletePropagationBackground
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: replJobNamespace},
	}
	if err := rc.directClient.Delete(ctx, job, &client.DeleteOptions{
		PropagationPolicy: &propagation,
	}); err != nil && !errors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to delete temp replication Job", "job", jobName)
	}

	// Delete source PVC.
	srcPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: srcPVCName, Namespace: replJobNamespace},
	}
	if err := rc.directClient.Delete(ctx, srcPVC); err != nil && !errors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to delete temp replication source PVC", "pvc", srcPVCName)
	}

	// Delete source PV.
	srcPV := &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: srcPVName},
	}
	if err := rc.directClient.Delete(ctx, srcPV); err != nil && !errors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to delete temp replication source PV", "pv", srcPVName)
	}

	// In receiver mode, there are no dest PV/PVC resources to clean up.
	if !receiverMode {
		dstPVCName := replDestPVCName(policyName, svName)
		dstPVName := replDestPVName(policyName, svName)

		// Delete dest PVC.
		dstPVC := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{Name: dstPVCName, Namespace: replJobNamespace},
		}
		if err := rc.directClient.Delete(ctx, dstPVC); err != nil && !errors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete temp replication dest PVC", "pvc", dstPVCName)
		}

		// Delete dest PV.
		dstPV := &corev1.PersistentVolume{
			ObjectMeta: metav1.ObjectMeta{Name: dstPVName},
		}
		if err := rc.directClient.Delete(ctx, dstPV); err != nil && !errors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete temp replication dest PV", "pv", dstPVName)
		}
	}

	klog.V(4).InfoS("Cleaned up temp replication resources",
		"policy", policyName, "subVolume", svName, "receiverMode", receiverMode)
}

// handleFailure updates the policy status for a failed replication cycle.
func (rc *ReplicationController) handleFailure(ctx context.Context, policy *v1alpha1.ReplicationPolicy, errMsg string, start time.Time) {
	policyName := policy.Name
	sourcePool := policy.Spec.SourcePoolName
	duration := rc.nowFunc().Sub(start)

	policy.Status.ConsecutiveFailures++
	policy.Status.LastError = errMsg

	// Check if max retries exceeded.
	maxRetries := policy.Spec.MaxRetries
	if maxRetries <= 0 {
		maxRetries = 3 // default
	}
	autoPause := policy.Status.ConsecutiveFailures >= maxRetries
	if autoPause {
		policy.Status.Phase = "Paused"
		klog.ErrorS(nil, "Replication policy paused after max retries exceeded",
			"policy", policyName,
			"consecutiveFailures", policy.Status.ConsecutiveFailures,
			"maxRetries", maxRetries,
		)
	}

	if err := rc.k8sClient.UpdateReplicationPolicyStatus(ctx, policy); err != nil {
		klog.ErrorS(err, "Failed to update policy status after failure", "policy", policyName)
	}

	// Set the pause annotation too, so the pause holds until a person removes
	// it. Without it the next cycle would read the policy as resumed. This runs
	// after the status update, because Update returns the stored status.
	if autoPause {
		if policy.Annotations == nil {
			policy.Annotations = map[string]string{}
		}
		policy.Annotations[pausedAnnotation] = "true"
		if err := rc.k8sClient.UpdateReplicationPolicy(ctx, policy); err != nil {
			klog.ErrorS(err, "Failed to set pause annotation after max retries", "policy", policyName)
		}
	}

	// Record metrics.
	metrics.ReplicationSyncsTotal.WithLabelValues(policyName, sourcePool, "failure").Inc()
	metrics.ReplicationSyncDuration.WithLabelValues(policyName, sourcePool).Observe(duration.Seconds())
	metrics.ReplicationConsecutiveFailures.WithLabelValues(policyName, sourcePool).Set(float64(policy.Status.ConsecutiveFailures))

	// Update lag metric if we have a last sync time.
	if policy.Status.LastSyncTime != nil {
		lag := rc.nowFunc().Sub(policy.Status.LastSyncTime.Time).Seconds()
		metrics.ReplicationLagSeconds.WithLabelValues(policyName, sourcePool).Set(lag)
	}

	klog.ErrorS(nil, "Replication cycle failed",
		"policy", policyName,
		"sourcePool", sourcePool,
		"error", errMsg,
		"consecutiveFailures", policy.Status.ConsecutiveFailures,
		"duration", duration.String(),
	)
}

// filterSubVolumes filters SubVolumes by label selector.
// If selector is nil, all SubVolumes are returned.
func filterSubVolumes(subVolumes []v1alpha1.SubVolume, selector *metav1.LabelSelector) []v1alpha1.SubVolume {
	if selector == nil {
		return subVolumes
	}

	// For simplicity, only support MatchLabels (not MatchExpressions).
	if len(selector.MatchLabels) == 0 {
		return subVolumes
	}

	var matched []v1alpha1.SubVolume
	for _, sv := range subVolumes {
		if matchesLabels(sv.Labels, selector.MatchLabels) {
			matched = append(matched, sv)
		}
	}
	return matched
}

// matchesLabels returns true if the resource labels contain all the required labels.
func matchesLabels(resourceLabels, required map[string]string) bool {
	for k, v := range required {
		if resourceLabels[k] != v {
			return false
		}
	}
	return true
}

// Naming helpers for temp replication resources.
// Resource names are limited to 63 chars. Policy names and SV names (pvc-<uuid>)
// can be long, so we truncate to fit.

func replJobName(policyName, svName string) string {
	name := fmt.Sprintf("repl-%s-%s", truncate(policyName, 15), truncate(svName, 36))
	return truncate(name, 63)
}

func replSourcePVName(policyName, svName string) string {
	name := fmt.Sprintf("repl-src-%s-%s", truncate(policyName, 12), truncate(svName, 36))
	return truncate(name, 63)
}

func replSourcePVCName(policyName, svName string) string {
	name := fmt.Sprintf("repl-src-%s-%s", truncate(policyName, 12), truncate(svName, 36))
	return truncate(name, 63)
}

func replDestPVName(policyName, svName string) string {
	name := fmt.Sprintf("repl-dst-%s-%s", truncate(policyName, 12), truncate(svName, 36))
	return truncate(name, 63)
}

func replDestPVCName(policyName, svName string) string {
	name := fmt.Sprintf("repl-dst-%s-%s", truncate(policyName, 12), truncate(svName, 36))
	return truncate(name, 63)
}

func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen]
}
