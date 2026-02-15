package pool

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const (
	// FinalizerName protects FileSharePool resources from premature deletion.
	FinalizerName = "storage.ibmcloud.io/pool-protection"

	// ReconcileInterval is the default requeue interval for periodic reconciliation.
	ReconcileInterval = 60 * time.Second
)

// FileSharePoolReconciler reconciles FileSharePool CRs, handling initial
// provisioning, health checks, proactive expansion, metrics reconciliation,
// and safe deletion.
type FileSharePoolReconciler struct {
	k8sClient k8s.Client
	vpcClient ibmcloud.VPCFileClient
}

// NewFileSharePoolReconciler creates a new reconciler with the given dependencies.
func NewFileSharePoolReconciler(k8sClient k8s.Client, vpcClient ibmcloud.VPCFileClient) *FileSharePoolReconciler {
	return &FileSharePoolReconciler{
		k8sClient: k8sClient,
		vpcClient: vpcClient,
	}
}

// Reconcile performs a single reconciliation pass for a FileSharePool.
func (r *FileSharePoolReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	// 1. Fetch pool
	pool, err := r.k8sClient.GetFileSharePool(ctx, req.Name)
	if err != nil {
		klog.V(4).InfoS("FileSharePool not found, likely deleted", "name", req.Name)
		return reconcile.Result{}, nil
	}

	// 2. Finalizer handling
	if pool.DeletionTimestamp != nil {
		return r.handleDeletion(ctx, pool)
	}
	if !hasFinalizer(pool) {
		addFinalizer(pool)
		if err := r.k8sClient.UpdateFileSharePool(ctx, pool); err != nil {
			klog.ErrorS(err, "Failed to add finalizer", "pool", pool.Name)
			return reconcile.Result{}, err
		}
		// Re-fetch after metadata update to avoid stale resource version
		pool, err = r.k8sClient.GetFileSharePool(ctx, req.Name)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	// 3. Initial provisioning
	if pool.Status.Phase == "" || pool.Status.Phase == "Initializing" {
		if err := r.initialProvisioning(ctx, pool); err != nil {
			klog.ErrorS(err, "Initial provisioning failed, will retry", "pool", pool.Name)
			pool.Status.Phase = "Initializing"
			r.setCondition(pool, "SharesReady", metav1.ConditionFalse, "ProvisioningFailed", err.Error())
			pool.Status.LastReconcileTime = timeNow()
			_ = r.k8sClient.UpdateFileSharePoolStatus(ctx, pool)
			return reconcile.Result{RequeueAfter: 15 * time.Second}, nil
		}
	}

	// 4. Health check
	r.healthCheck(ctx, pool)

	// 5. Reconcile metrics from SubVolume CRs
	r.reconcileMetrics(ctx, pool)

	// 6. Proactive expansion
	r.proactiveExpansion(ctx, pool)

	// 7. Determine phase
	pool.Status.Phase = determinePhase(pool)

	// 8. Set conditions
	r.updateConditions(pool)

	// 9. Set LastReconcileTime
	pool.Status.LastReconcileTime = timeNow()

	// 10. Persist
	if err := r.k8sClient.UpdateFileSharePoolStatus(ctx, pool); err != nil {
		klog.ErrorS(err, "Failed to update pool status", "pool", pool.Name)
		return reconcile.Result{}, err
	}

	return reconcile.Result{RequeueAfter: ReconcileInterval}, nil
}

// handleDeletion processes a pool that has a DeletionTimestamp set.
func (r *FileSharePoolReconciler) handleDeletion(ctx context.Context, pool *v1alpha1.FileSharePool) (ctrl.Result, error) {
	// Check for active SubVolumes
	subVolumes, err := r.k8sClient.ListSubVolumes(ctx, pool.Name)
	if err != nil {
		klog.ErrorS(err, "Failed to list SubVolumes during deletion", "pool", pool.Name)
		return reconcile.Result{}, err
	}

	if len(subVolumes) > 0 {
		klog.V(2).InfoS("Deletion blocked: active SubVolumes exist",
			"pool", pool.Name, "subVolumeCount", len(subVolumes))
		r.setCondition(pool, "DeletionBlocked", metav1.ConditionTrue,
			"ActiveSubVolumes", fmt.Sprintf("%d SubVolumes still exist", len(subVolumes)))
		pool.Status.LastReconcileTime = timeNow()
		_ = r.k8sClient.UpdateFileSharePoolStatus(ctx, pool)
		return reconcile.Result{RequeueAfter: 30 * time.Second}, nil
	}

	// Delete all VPC shares
	for _, share := range pool.Status.Shares {
		if err := r.vpcClient.DeleteFileShare(ctx, share.ShareID); err != nil {
			klog.ErrorS(err, "Failed to delete VPC share during pool deletion",
				"pool", pool.Name, "shareID", share.ShareID)
			return reconcile.Result{}, err
		}
		klog.V(2).InfoS("Deleted VPC share", "pool", pool.Name, "shareID", share.ShareID)
	}

	// Remove finalizer
	removeFinalizer(pool)
	if err := r.k8sClient.UpdateFileSharePool(ctx, pool); err != nil {
		klog.ErrorS(err, "Failed to remove finalizer", "pool", pool.Name)
		return reconcile.Result{}, err
	}

	klog.V(2).InfoS("Pool deletion complete", "pool", pool.Name)
	return reconcile.Result{}, nil
}

// initialProvisioning creates VPC shares up to spec.initialShares.
func (r *FileSharePoolReconciler) initialProvisioning(ctx context.Context, pool *v1alpha1.FileSharePool) error {
	// Count existing creating+stable shares
	existingCount := int32(0)
	for _, s := range pool.Status.Shares {
		if s.State == "creating" || s.State == "stable" {
			existingCount++
		}
	}

	needed := pool.Spec.InitialShares - existingCount
	for i := int32(0); i < needed; i++ {
		if err := r.createPoolShare(ctx, pool); err != nil {
			return fmt.Errorf("create initial share %d/%d: %w", i+1, needed, err)
		}
	}

	return nil
}

// healthCheck queries VPC for each share's current state and updates pool status accordingly.
func (r *FileSharePoolReconciler) healthCheck(ctx context.Context, pool *v1alpha1.FileSharePool) {
	for i := range pool.Status.Shares {
		share := &pool.Status.Shares[i]

		info, err := r.vpcClient.GetFileShare(ctx, share.ShareID)
		if err != nil {
			klog.ErrorS(err, "Health check failed for share", "shareID", share.ShareID)
			continue
		}

		switch info.LifecycleState {
		case "stable":
			if share.State == "creating" {
				share.State = "stable"
				// Backfill mount target IP if missing
				if share.MountTargetIP == "" && len(info.MountTargets) > 0 {
					share.MountTargetIP = info.MountTargets[0].IPAddress
					share.MountTargetID = info.MountTargets[0].ID
				}
				klog.V(2).InfoS("Share transitioned to stable",
					"shareID", share.ShareID, "pool", pool.Name)
			}
		case "failed", "degraded":
			if share.State != "draining" {
				share.State = "draining"
				klog.V(2).InfoS("Share marked as draining due to VPC state",
					"shareID", share.ShareID, "vpcState", info.LifecycleState)
			}
		case "pending":
			if share.State != "creating" {
				share.State = "creating"
			}
		}
	}
}

// reconcileMetrics recalculates per-share and pool totals from SubVolume CRs.
func (r *FileSharePoolReconciler) reconcileMetrics(ctx context.Context, pool *v1alpha1.FileSharePool) {
	subVolumes, err := r.k8sClient.ListSubVolumes(ctx, pool.Name)
	if err != nil {
		klog.ErrorS(err, "Failed to list SubVolumes for metrics reconciliation", "pool", pool.Name)
		return
	}

	// Build per-share tallies
	shareAllocated := make(map[string]int64)
	sharePVCCount := make(map[string]int32)
	for _, sv := range subVolumes {
		shareAllocated[sv.Spec.ShareID] += sv.Spec.RequestedGB
		sharePVCCount[sv.Spec.ShareID]++
	}

	// Update per-share values
	var totalAllocated int64
	var totalPVCs int32
	for i := range pool.Status.Shares {
		share := &pool.Status.Shares[i]
		share.AllocatedGB = shareAllocated[share.ShareID]
		share.PVCCount = sharePVCCount[share.ShareID]
		totalAllocated += share.AllocatedGB
		totalPVCs += share.PVCCount
	}

	// Update pool totals
	pool.Status.TotalAllocatedGB = totalAllocated
	pool.Status.TotalPVCCount = totalPVCs
	pool.Status.ShareCount = int32(len(pool.Status.Shares))

	// Recalculate total capacity
	var totalCapacity int64
	for _, s := range pool.Status.Shares {
		totalCapacity += s.TotalGB
	}
	pool.Status.TotalCapacityGB = totalCapacity
}

// proactiveExpansion creates a new share if utilization exceeds the threshold.
func (r *FileSharePoolReconciler) proactiveExpansion(ctx context.Context, pool *v1alpha1.FileSharePool) {
	if !pool.Spec.AutoExpand {
		return
	}

	if int32(len(pool.Status.Shares)) >= pool.Spec.MaxShares {
		return
	}

	// Skip if any share is still creating
	for _, s := range pool.Status.Shares {
		if s.State == "creating" {
			return
		}
	}

	if pool.Status.TotalCapacityGB == 0 {
		return
	}

	utilization := int32(pool.Status.TotalAllocatedGB * 100 / pool.Status.TotalCapacityGB)
	if utilization <= pool.Spec.ExpandThresholdPercent {
		return
	}

	klog.V(2).InfoS("Proactive expansion triggered",
		"pool", pool.Name,
		"utilization", utilization,
		"threshold", pool.Spec.ExpandThresholdPercent,
	)

	if err := r.createPoolShare(ctx, pool); err != nil {
		klog.ErrorS(err, "Proactive expansion failed", "pool", pool.Name)
	}
}

// createPoolShare creates a new VPC share and appends it to pool status.
func (r *FileSharePoolReconciler) createPoolShare(ctx context.Context, pool *v1alpha1.FileSharePool) error {
	input := ibmcloud.CreateShareInput{
		Name:             fmt.Sprintf("%s-share-%d", pool.Name, len(pool.Status.Shares)+1),
		Zone:             pool.Spec.Zone,
		Profile:          pool.Spec.Profile,
		SizeGB:           pool.Spec.ShareSizeGB,
		IOPS:             pool.Spec.IOPS,
		ResourceGroupID:  pool.Spec.ResourceGroup,
		Tags:             pool.Spec.Tags,
		EncryptInTransit: pool.Spec.EncryptionInTransit,
	}

	shareInfo, err := r.vpcClient.CreateFileShare(ctx, input)
	if err != nil {
		return fmt.Errorf("create VPC share: %w", err)
	}

	mountTargetIP := ""
	mountTargetID := ""
	if len(shareInfo.MountTargets) > 0 {
		mountTargetIP = shareInfo.MountTargets[0].IPAddress
		mountTargetID = shareInfo.MountTargets[0].ID
	}

	state := "creating"
	if shareInfo.LifecycleState == "stable" {
		state = "stable"
	}

	now := metav1.Now()
	newShare := v1alpha1.PoolShareStatus{
		ShareID:       shareInfo.ID,
		ShareName:     shareInfo.Name,
		MountTargetIP: mountTargetIP,
		MountTargetID: mountTargetID,
		TotalGB:       shareInfo.SizeGB,
		AllocatedGB:   0,
		PVCCount:      0,
		State:         state,
		Zone:          shareInfo.Zone,
		CreatedAt:     &now,
	}

	pool.Status.Shares = append(pool.Status.Shares, newShare)
	pool.Status.ShareCount = int32(len(pool.Status.Shares))
	pool.Status.TotalCapacityGB += shareInfo.SizeGB

	klog.V(2).InfoS("Created pool share",
		"pool", pool.Name,
		"shareID", shareInfo.ID,
		"state", state,
	)

	return nil
}

// determinePhase computes the pool phase based on share states.
func determinePhase(pool *v1alpha1.FileSharePool) string {
	if len(pool.Status.Shares) == 0 {
		return "Initializing"
	}

	hasStable := false
	hasDraining := false
	hasCreating := false

	for _, s := range pool.Status.Shares {
		switch s.State {
		case "stable":
			hasStable = true
		case "draining", "degraded":
			hasDraining = true
		case "creating":
			hasCreating = true
		}
	}

	if hasDraining {
		return "Degraded"
	}
	if hasCreating {
		return "Expanding"
	}
	if !hasStable {
		return "Initializing"
	}

	// Check if all shares are full at max capacity
	if int32(len(pool.Status.Shares)) >= pool.Spec.MaxShares {
		allFull := true
		for _, s := range pool.Status.Shares {
			if s.State == "stable" && s.AllocatedGB < s.TotalGB {
				allFull = false
				break
			}
		}
		if allFull {
			return "Full"
		}
	}

	return "Ready"
}

// setCondition upserts a condition in pool.Status.Conditions.
func (r *FileSharePoolReconciler) setCondition(pool *v1alpha1.FileSharePool, condType string, status metav1.ConditionStatus, reason, message string) {
	now := metav1.Now()
	newCondition := metav1.Condition{
		Type:               condType,
		Status:             status,
		ObservedGeneration: pool.Generation,
		LastTransitionTime: now,
		Reason:             reason,
		Message:            message,
	}

	for i, c := range pool.Status.Conditions {
		if c.Type == condType {
			if c.Status != status {
				pool.Status.Conditions[i] = newCondition
			} else {
				// Keep existing transition time, update reason/message
				pool.Status.Conditions[i].Reason = reason
				pool.Status.Conditions[i].Message = message
				pool.Status.Conditions[i].ObservedGeneration = pool.Generation
			}
			return
		}
	}

	pool.Status.Conditions = append(pool.Status.Conditions, newCondition)
}

// updateConditions sets SharesReady and CapacityAvailable conditions.
func (r *FileSharePoolReconciler) updateConditions(pool *v1alpha1.FileSharePool) {
	// SharesReady
	stableCount := 0
	for _, s := range pool.Status.Shares {
		if s.State == "stable" {
			stableCount++
		}
	}
	if stableCount > 0 {
		r.setCondition(pool, "SharesReady", metav1.ConditionTrue, "SharesAvailable",
			fmt.Sprintf("%d shares ready", stableCount))
	} else {
		r.setCondition(pool, "SharesReady", metav1.ConditionFalse, "NoSharesReady",
			"No shares in stable state")
	}

	// CapacityAvailable
	if pool.Status.TotalCapacityGB > 0 && pool.Status.TotalAllocatedGB < pool.Status.TotalCapacityGB {
		r.setCondition(pool, "CapacityAvailable", metav1.ConditionTrue, "CapacityExists",
			fmt.Sprintf("%d GB available", pool.Status.TotalCapacityGB-pool.Status.TotalAllocatedGB))
	} else {
		r.setCondition(pool, "CapacityAvailable", metav1.ConditionFalse, "NoCapacity",
			"No available capacity")
	}
}

// SetupWithManager registers the reconciler with a controller-runtime Manager.
func (r *FileSharePoolReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.FileSharePool{}).
		Named("filesharepool").
		Complete(r)
}

// Finalizer helpers

func hasFinalizer(pool *v1alpha1.FileSharePool) bool {
	for _, f := range pool.Finalizers {
		if f == FinalizerName {
			return true
		}
	}
	return false
}

func addFinalizer(pool *v1alpha1.FileSharePool) {
	pool.Finalizers = append(pool.Finalizers, FinalizerName)
}

func removeFinalizer(pool *v1alpha1.FileSharePool) {
	var filtered []string
	for _, f := range pool.Finalizers {
		if f != FinalizerName {
			filtered = append(filtered, f)
		}
	}
	pool.Finalizers = filtered
}

// timeNow returns a pointer to metav1.Now(). Extracted for testability.
var timeNow = func() *metav1.Time {
	t := metav1.Now()
	return &t
}
