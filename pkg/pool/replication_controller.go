package pool

import (
	"context"
	"fmt"
	"sync"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/hooks"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

const (
	// DefaultReplicationInterval is how often the replication controller checks for work.
	DefaultReplicationInterval = 30 * time.Second
)

// ReplicationController manages cross-region replication of SubVolume data.
// It runs as a background goroutine (like CloneWorker) that periodically
// checks ReplicationPolicy CRs and syncs SubVolume data to the destination.
type ReplicationController struct {
	k8sClient    k8s.Client
	nfsOps       NFSOperations
	orchestrator *hooks.Orchestrator

	// interval is the poll interval for checking replication policies.
	interval time.Duration

	// lastSyncTimes tracks the last time each policy was synced, keyed by policy name.
	lastSyncTimes map[string]time.Time
	mu            sync.Mutex

	// nowFunc is used for time operations (injectable for tests).
	nowFunc func() time.Time
}

// NewReplicationController creates a new replication controller.
func NewReplicationController(k8sClient k8s.Client, nfsOps NFSOperations) *ReplicationController {
	return &ReplicationController{
		k8sClient:     k8sClient,
		nfsOps:        nfsOps,
		interval:      DefaultReplicationInterval,
		lastSyncTimes: make(map[string]time.Time),
		nowFunc:       time.Now,
	}
}

// SetOrchestrator sets the hook orchestrator for pre/post sync hooks.
func (rc *ReplicationController) SetOrchestrator(orch *hooks.Orchestrator) {
	rc.orchestrator = orch
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

		// Process this policy synchronously (one at a time for simplicity).
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

// processPolicy runs a single replication cycle for the given policy.
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

	// 3. Sync each matched SubVolume.
	var svStatuses []v1alpha1.SubVolumeReplicationStatus
	var syncErrors []string

	for _, sv := range matched {
		svStatus := rc.syncSubVolume(ctx, policy, &sv)
		svStatuses = append(svStatuses, svStatus)
		if svStatus.LastError != "" {
			syncErrors = append(syncErrors, fmt.Sprintf("%s: %s", sv.Name, svStatus.LastError))
		}
	}

	// 4. Update policy status.
	duration := rc.nowFunc().Sub(start)

	if len(syncErrors) > 0 {
		// Some SubVolumes failed.
		combinedErr := fmt.Sprintf("%d/%d SubVolumes failed to sync", len(syncErrors), len(matched))
		rc.handleFailure(ctx, policy, combinedErr, start)
		// Still update SubVolume statuses on partial failure.
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

// syncSubVolume syncs a single SubVolume to the destination.
// It copies from the SubVolume's subPath to the destination base path.
func (rc *ReplicationController) syncSubVolume(ctx context.Context, policy *v1alpha1.ReplicationPolicy, sv *v1alpha1.SubVolume) v1alpha1.SubVolumeReplicationStatus {
	_ = ctx // reserved for future use (e.g., context-based cancellation of long copies)

	now := metav1.NewTime(rc.nowFunc())
	status := v1alpha1.SubVolumeReplicationStatus{
		SubVolumeName: sv.Name,
	}

	// Source path: the SubVolume's NFS path (the share mount target + subPath).
	// For the replication controller, we construct:
	//   source: <shareMountTargetIP>:<subPath>  (in practice, the NFS mount is already done)
	// For simplicity, we use the destination NFS server + base path for the destination,
	// and the source share mount target IP for the source.
	// The actual paths on the filesystem are:
	//   source: /pvcs/<pvc-name> (on source NFS mount)
	//   dest:   /pvcs/<pvc-name> (on destination NFS mount)
	//
	// Since CopyDir works with local paths, we construct the paths using the
	// mount target IPs as path prefixes. In a real deployment, the NFS shares
	// are mounted at well-known paths; here we use a simple convention.

	srcPath := fmt.Sprintf("/repl/src/%s%s", sv.Spec.ShareMountTargetIP, sv.Spec.SubPath)
	dstPath := fmt.Sprintf("/repl/dst/%s%s", policy.Spec.DestinationNFSServer, sv.Spec.SubPath)

	// Ensure destination directory parent exists.
	dstParent := fmt.Sprintf("/repl/dst/%s/pvcs", policy.Spec.DestinationNFSServer)
	if err := rc.nfsOps.MkdirAll(dstParent, 0755); err != nil {
		status.LastError = fmt.Sprintf("mkdir destination: %v", err)
		return status
	}

	// Copy data from source to destination.
	if err := rc.nfsOps.CopyDir(srcPath, dstPath); err != nil {
		status.LastError = fmt.Sprintf("copy: %v", err)
		return status
	}

	status.LastSyncTime = &now
	return status
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
