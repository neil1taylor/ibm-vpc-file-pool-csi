package pool

import (
	"context"
	"encoding/json"
	"fmt"
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

	// DefaultReplicationImage is the container image used for replication Jobs.
	// The driver image includes rsync; fall back to CentOS Stream 9 which has rsync.
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

	// replicationImage is the container image used for replication Jobs.
	replicationImage string

	// interval is the poll interval for checking replication policies.
	interval time.Duration

	// lastSyncTimes tracks the last time each policy was synced, keyed by policy name.
	lastSyncTimes map[string]time.Time
	mu            sync.Mutex

	// nowFunc is used for time operations (injectable for tests).
	nowFunc func() time.Time
}

// NewReplicationController creates a new replication controller.
func NewReplicationController(k8sClient k8s.Client, directClient client.Client, nfsOps NFSOperations) *ReplicationController {
	return &ReplicationController{
		k8sClient:        k8sClient,
		directClient:     directClient,
		nfsOps:           nfsOps,
		replicationImage: DefaultReplicationImage,
		interval:         DefaultReplicationInterval,
		lastSyncTimes:    make(map[string]time.Time),
		nowFunc:          time.Now,
	}
}

// SetOrchestrator sets the hook orchestrator for pre/post sync hooks.
func (rc *ReplicationController) SetOrchestrator(orch *hooks.Orchestrator) {
	rc.orchestrator = orch
}

// SetReplicationImage overrides the default container image for replication Jobs.
func (rc *ReplicationController) SetReplicationImage(image string) {
	if image != "" {
		rc.replicationImage = image
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

		// Skip paused or failed policies.
		if policy.Status.Phase == "Paused" || policy.Status.Phase == "Failed" {
			continue
		}

		// Parse the schedule interval.
		scheduleInterval, err := time.ParseDuration(policy.Spec.Schedule)
		if err != nil {
			klog.ErrorS(err, "Invalid schedule duration in policy",
				"policy", policy.Name, "schedule", policy.Spec.Schedule)
			continue
		}

		// Check if it's time to sync.
		if !rc.isDue(policy.Name, scheduleInterval) {
			continue
		}

		// Process this policy (poll-based: create or check Jobs).
		rc.processPolicy(ctx, policy)
	}
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

	// Update SubVolume count metric.
	metrics.ReplicationSubVolumeCount.WithLabelValues(policyName, sourcePool).Set(float64(len(matched)))

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

	for _, sv := range matched {
		svCopy := sv

		// Check if a Job already exists for this SubVolume.
		done, succeeded, checkErr := rc.checkReplicationJob(ctx, policyName, svCopy.Name)
		if checkErr != nil {
			klog.ErrorS(checkErr, "Failed to check replication Job status",
				"policy", policyName, "subVolume", svCopy.Name)
			syncErrors = append(syncErrors, fmt.Sprintf("%s: check job: %v", svCopy.Name, checkErr))
			svStatuses = append(svStatuses, v1alpha1.SubVolumeReplicationStatus{
				SubVolumeName: svCopy.Name,
				LastError:     fmt.Sprintf("check job: %v", checkErr),
			})
			continue
		}

		if done {
			if succeeded {
				// Job succeeded — record success and cleanup.
				now := metav1.NewTime(rc.nowFunc())
				svStatuses = append(svStatuses, v1alpha1.SubVolumeReplicationStatus{
					SubVolumeName: svCopy.Name,
					LastSyncTime:  &now,
				})
				rc.cleanupReplicationResources(ctx, policy, svCopy.Name)
			} else {
				// Job failed.
				syncErrors = append(syncErrors, fmt.Sprintf("%s: replication Job failed", svCopy.Name))
				svStatuses = append(svStatuses, v1alpha1.SubVolumeReplicationStatus{
					SubVolumeName: svCopy.Name,
					LastError:     "replication Job failed after retries",
				})
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
			svStatuses = append(svStatuses, v1alpha1.SubVolumeReplicationStatus{
				SubVolumeName: svCopy.Name,
				LastError:     "sync in progress",
			})
			continue
		}

		// No Job exists yet — create one if under parallel limit.
		if activeJobs >= maxParallel {
			klog.V(4).InfoS("Parallel sync limit reached, deferring SubVolume",
				"policy", policyName, "subVolume", svCopy.Name,
				"activeJobs", activeJobs, "maxParallel", maxParallel)
			svStatuses = append(svStatuses, v1alpha1.SubVolumeReplicationStatus{
				SubVolumeName: svCopy.Name,
				LastError:     "deferred: parallel limit",
			})
			continue
		}

		// Create PV/PVC/Job for this SubVolume.
		if createErr := rc.createReplicationResources(ctx, policy, &svCopy); createErr != nil {
			syncErrors = append(syncErrors, fmt.Sprintf("%s: create resources: %v", svCopy.Name, createErr))
			svStatuses = append(svStatuses, v1alpha1.SubVolumeReplicationStatus{
				SubVolumeName: svCopy.Name,
				LastError:     fmt.Sprintf("create resources: %v", createErr),
			})
			rc.cleanupReplicationResources(ctx, policy, svCopy.Name)
			continue
		}

		activeJobs++
		svStatuses = append(svStatuses, v1alpha1.SubVolumeReplicationStatus{
			SubVolumeName: svCopy.Name,
			LastError:     "sync in progress",
		})
	}

	// 4. Update policy status.
	duration := rc.nowFunc().Sub(start)

	// Check if all Jobs completed (no active or deferred).
	allComplete := true
	for _, svs := range svStatuses {
		if svs.LastError == "sync in progress" || svs.LastError == "deferred: parallel limit" {
			allComplete = false
			break
		}
	}

	if !allComplete {
		// Some Jobs still running or deferred — don't count as a completed cycle.
		// Update SubVolume statuses for visibility but don't update sync time or metrics.
		policy.Status.SubVolumeStatuses = svStatuses
		if err := rc.k8sClient.UpdateReplicationPolicyStatus(ctx, policy); err != nil {
			klog.ErrorS(err, "Failed to update policy status (cycle in progress)", "policy", policyName)
		}
		klog.V(4).InfoS("Replication cycle still in progress",
			"policy", policyName, "activeJobs", activeJobs)
		return
	}

	// All SubVolumes processed — evaluate results.
	// Filter out non-error statuses from syncErrors count.
	realErrors := filterRealErrors(syncErrors)

	if len(realErrors) > 0 {
		combinedErr := fmt.Sprintf("%d/%d SubVolumes failed to sync", len(realErrors), len(matched))
		rc.handleFailure(ctx, policy, combinedErr, start)
		policy.Status.SubVolumeStatuses = svStatuses
		if err := rc.k8sClient.UpdateReplicationPolicyStatus(ctx, policy); err != nil {
			klog.ErrorS(err, "Failed to update policy status after partial failure", "policy", policyName)
		}
		return
	}

	// 3a. Run post-sync hooks.
	if rc.orchestrator != nil && len(policy.Spec.PostSyncHooks) > 0 {
		_, hookErr := rc.orchestrator.RunPostHooks(ctx, policy.Spec.PostSyncHooks)
		if hookErr != nil {
			klog.ErrorS(hookErr, "Post-sync hooks failed", "policy", policyName)
		}
	}

	// All succeeded.
	now := metav1.NewTime(rc.nowFunc())
	policy.Status.Phase = "Active"
	policy.Status.LastSyncTime = &now
	policy.Status.LastSyncDuration = duration.String()
	policy.Status.SubVolumeStatuses = svStatuses
	policy.Status.ConsecutiveFailures = 0
	policy.Status.LastError = ""

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
		"subVolumes", len(matched),
		"duration", duration.String(),
	)
}

// filterRealErrors returns only errors that are actual failures (not in-progress markers).
func filterRealErrors(errors []string) []string {
	var real []string
	for _, e := range errors {
		real = append(real, e)
	}
	return real
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
		return fmt.Errorf("Job: %w", err)
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
		return fmt.Errorf("Job: %w", err)
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
							Image: rc.replicationImage,
							Args: []string{
								"--mode=sync-client",
								"--receiver-endpoint=" + policy.Spec.DestinationEndpoint,
								"--auth-token-file=/etc/replication/token",
								"--source-path=/source",
								"--dest-base-path=" + destBasePath,
								"--sub-path=" + subPath,
								"--subvolume-name=" + sv.Name,
								"--metadata-json=" + string(metaJSON),
							},
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
								{Name: "auth-token", MountPath: "/etc/replication", ReadOnly: true},
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
							Name: "auth-token",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: policy.Spec.DestinationAuthSecretRef,
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
		return fmt.Errorf("creating receiver-mode replication Job %s/%s: %w", replJobNamespace, jobName, err)
	}

	klog.V(2).InfoS("Created receiver-mode replication Job",
		"namespace", replJobNamespace,
		"job", jobName,
		"policy", policyName,
		"subVolume", sv.Name,
		"image", rc.replicationImage,
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

	// Build rsync options.
	useIncremental := policy.Spec.IncrementalSync == nil || *policy.Spec.IncrementalSync

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
rsync -a --delete %s %s %s "/source%s/" "/dest%s%s/"
echo "Writing metadata sidecar..."
cat > "/dest%s%s/.subvolume-metadata.json" <<'METADATA'
%s
METADATA
echo "Replication complete."
`, subPath, destBasePath, subPath,
			destBasePath, subPath,
			bwlimit, extraArgs, "", // empty string placeholder for cleaner formatting
			subPath, destBasePath, subPath,
			destBasePath, subPath,
			string(metaJSON))
	} else {
		script = fmt.Sprintf(`set -euo pipefail
echo "Replication: copying %s -> %s%s"
mkdir -p "/dest%s%s"
cp -a "/source%s/." "/dest%s%s/"
echo "Writing metadata sidecar..."
cat > "/dest%s%s/.subvolume-metadata.json" <<'METADATA'
%s
METADATA
echo "Replication complete."
`, subPath, destBasePath, subPath,
			destBasePath, subPath,
			subPath, destBasePath, subPath,
			destBasePath, subPath,
			string(metaJSON))
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
// Returns (done, succeeded, error). If done=false, the Job is still running or doesn't exist.
func (rc *ReplicationController) checkReplicationJob(ctx context.Context, policyName, svName string) (bool, bool, error) {
	jobName := replJobName(policyName, svName)

	job := &batchv1.Job{}
	err := rc.directClient.Get(ctx, types.NamespacedName{
		Namespace: replJobNamespace,
		Name:      jobName,
	}, job)
	if err != nil {
		if errors.IsNotFound(err) {
			return false, false, nil // no Job yet
		}
		return false, false, fmt.Errorf("getting replication Job %s/%s: %w", replJobNamespace, jobName, err)
	}

	if job.Status.Succeeded > 0 {
		return true, true, nil
	}

	// Check if all retries exhausted.
	backoffLimit := int32(3)
	if job.Spec.BackoffLimit != nil {
		backoffLimit = *job.Spec.BackoffLimit
	}
	if job.Status.Failed >= backoffLimit+1 {
		return true, false, nil
	}

	return false, false, nil // still running
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
	if policy.Status.ConsecutiveFailures >= maxRetries {
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
