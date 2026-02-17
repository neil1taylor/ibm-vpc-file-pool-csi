package pool

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
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
	k8sClient            k8s.Client
	vpcClient            ibmcloud.VPCFileClient
	vpcID                string
	subnetID             string
	defaultResourceGroup string
}

// NewFileSharePoolReconciler creates a new reconciler with the given dependencies.
func NewFileSharePoolReconciler(k8sClient k8s.Client, vpcClient ibmcloud.VPCFileClient) *FileSharePoolReconciler {
	return &FileSharePoolReconciler{
		k8sClient: k8sClient,
		vpcClient: vpcClient,
	}
}

// SetVPCConfig sets the VPC ID, subnet ID, and default resource group used when creating shares.
func (r *FileSharePoolReconciler) SetVPCConfig(vpcID, subnetID, defaultResourceGroup string) {
	r.vpcID = vpcID
	r.subnetID = subnetID
	r.defaultResourceGroup = defaultResourceGroup
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

	// 2b. Ensure StorageClasses exist for this pool
	r.ensureStorageClasses(ctx, pool)

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

		// Persist status immediately after provisioning to prevent share name
		// conflicts on retry. VPC shares are expensive resources; losing them
		// from status due to a later conflict error means the next reconcile
		// would try to create the same share name again and get HTTP 400.
		pool.Status.Phase = "Expanding"
		pool.Status.LastReconcileTime = timeNow()
		if err := r.k8sClient.UpdateFileSharePoolStatus(ctx, pool); err != nil {
			klog.ErrorS(err, "Failed to persist post-provisioning status", "pool", pool.Name)
			return reconcile.Result{}, err
		}
		// Re-fetch to get fresh resource version after status write.
		pool, err = r.k8sClient.GetFileSharePool(ctx, req.Name)
		if err != nil {
			return reconcile.Result{}, err
		}
	}

	// 4. Health check
	r.healthCheck(ctx, pool)

	// 4b. Ensure accessor mount targets
	r.ensureAccessorMountTargets(ctx, pool)

	// 4c. Process drain requests
	r.processDrainRequests(ctx, pool)

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

// initialProvisioning creates VPC shares up to spec.initialShares (or per-tier initialShares).
func (r *FileSharePoolReconciler) initialProvisioning(ctx context.Context, pool *v1alpha1.FileSharePool) error {
	if len(pool.Spec.Tiers) > 0 {
		// Tier-aware provisioning: iterate each tier
		for _, tier := range pool.Spec.Tiers {
			existingCount := int32(0)
			for _, s := range pool.Status.Shares {
				if s.Tier == tier.Name && (s.State == "creating" || s.State == "stable") {
					existingCount++
				}
			}
			needed := tier.InitialShares - existingCount
			for i := int32(0); i < needed; i++ {
				if err := r.createPoolShare(ctx, pool, tier.Name); err != nil {
					return fmt.Errorf("create initial share for tier %q %d/%d: %w", tier.Name, i+1, needed, err)
				}
			}
		}
		return nil
	}

	// No tiers: use top-level fields
	existingCount := int32(0)
	for _, s := range pool.Status.Shares {
		if s.State == "creating" || s.State == "stable" {
			existingCount++
		}
	}

	needed := pool.Spec.InitialShares - existingCount
	for i := int32(0); i < needed; i++ {
		if err := r.createPoolShare(ctx, pool, ""); err != nil {
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
				klog.V(2).InfoS("Share transitioned to stable",
					"shareID", share.ShareID, "pool", pool.Name)
			}
			// Backfill mount target IP if missing (regardless of previous state)
			if share.MountTargetIP == "" && len(info.MountTargets) > 0 {
				share.MountTargetIP = info.MountTargets[0].IPAddress
				share.MountTargetID = info.MountTargets[0].ID
				klog.V(2).InfoS("Backfilled mount target IP",
					"shareID", share.ShareID, "ip", share.MountTargetIP)
			}
			// Backfill MountTargets from all discovered mount targets
			if len(info.MountTargets) > 0 && len(share.MountTargets) < len(info.MountTargets) {
				existing := make(map[string]bool)
				for _, mt := range share.MountTargets {
					existing[mt.MountTargetID] = true
				}
				for _, mt := range info.MountTargets {
					if mt.IPAddress != "" && !existing[mt.ID] {
						share.MountTargets = append(share.MountTargets, v1alpha1.ZoneMountTarget{
							Zone:          share.Zone, // will be corrected by ensureAccessorMountTargets
							MountTargetID: mt.ID,
							MountTargetIP: mt.IPAddress,
						})
					}
				}
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

// ensureAccessorMountTargets populates MountTargets status for accessor zones.
// VPC access mode uses a single mount target per VPC with a FQDN that the VPC
// DNS resolves to zone-optimal IPs automatically. We record this FQDN as the
// IP for each accessible zone so that the CSI driver can emit server.<zone> keys.
func (r *FileSharePoolReconciler) ensureAccessorMountTargets(_ context.Context, pool *v1alpha1.FileSharePool) {
	if len(pool.Spec.AccessorZones) == 0 {
		return
	}

	for i := range pool.Status.Shares {
		share := &pool.Status.Shares[i]
		if share.State != "stable" || share.MountTargetIP == "" {
			continue
		}

		// Seed MountTargets with home zone if not present
		if len(share.MountTargets) == 0 {
			share.MountTargets = []v1alpha1.ZoneMountTarget{
				{
					Zone:          share.Zone,
					MountTargetID: share.MountTargetID,
					MountTargetIP: share.MountTargetIP,
				},
			}
		}

		// Add accessor zones using the same VPC-mode FQDN/IP (DNS handles routing)
		for _, az := range pool.Spec.AccessorZones {
			exists := false
			for _, mt := range share.MountTargets {
				if mt.Zone == az.Zone {
					exists = true
					break
				}
			}
			if exists {
				continue
			}

			share.MountTargets = append(share.MountTargets, v1alpha1.ZoneMountTarget{
				Zone:          az.Zone,
				MountTargetID: share.MountTargetID,
				MountTargetIP: share.MountTargetIP,
			})

			klog.V(2).InfoS("Added accessor zone to mount targets (same VPC FQDN)",
				"pool", pool.Name, "shareID", share.ShareID,
				"zone", az.Zone, "server", share.MountTargetIP)
		}
	}
}

// ensureStorageClasses creates StorageClass(es) for the pool if they don't already exist.
// Errors are logged but non-fatal — pool reconciliation continues regardless.
func (r *FileSharePoolReconciler) ensureStorageClasses(ctx context.Context, pool *v1alpha1.FileSharePool) {
	if shouldSkipStorageClass(pool) {
		return
	}

	// API server strips TypeMeta on reads; restore it for OwnerReference.
	pool.APIVersion = "storage.ibmcloud.io/v1alpha1"
	pool.Kind = "FileSharePool"

	for _, desired := range storageClassesForPool(pool) {
		_, err := r.k8sClient.GetStorageClass(ctx, desired.Name)
		if err == nil {
			// Already exists — do not overwrite (user may have customized).
			continue
		}
		if err := r.k8sClient.CreateStorageClass(ctx, desired); err != nil {
			klog.ErrorS(err, "Failed to create StorageClass (non-fatal)",
				"pool", pool.Name, "storageClass", desired.Name)
			continue
		}
		klog.V(2).InfoS("Created StorageClass for pool",
			"pool", pool.Name, "storageClass", desired.Name)
	}
}

// processDrainRequests handles share drain requests from spec.drainShares.
// It marks requested shares as "draining", tracks drain progress, and detects completion.
func (r *FileSharePoolReconciler) processDrainRequests(ctx context.Context, pool *v1alpha1.FileSharePool) {
	if len(pool.Spec.DrainShares) == 0 {
		// No drain requests: clear any stale drain status
		pool.Status.DrainStatus = nil
		return
	}

	// Build a set of requested drain share IDs
	drainSet := make(map[string]bool, len(pool.Spec.DrainShares))
	for _, id := range pool.Spec.DrainShares {
		drainSet[id] = true
	}

	// Mark requested shares as "draining" if they are currently stable
	for i := range pool.Status.Shares {
		share := &pool.Status.Shares[i]
		if drainSet[share.ShareID] && share.State == "stable" {
			share.State = "draining"
			klog.V(2).InfoS("Share marked as draining (user-requested)",
				"pool", pool.Name, "shareID", share.ShareID)
		}
	}

	// Build/update drain status for each requested share
	existingDrainStatus := make(map[string]*v1alpha1.ShareDrainStatus, len(pool.Status.DrainStatus))
	for i := range pool.Status.DrainStatus {
		existingDrainStatus[pool.Status.DrainStatus[i].ShareID] = &pool.Status.DrainStatus[i]
	}

	var updatedDrainStatus []v1alpha1.ShareDrainStatus
	for _, shareID := range pool.Spec.DrainShares {
		// Count SubVolumes on this share
		subVolumes, err := r.k8sClient.ListSubVolumesByShare(ctx, shareID)
		if err != nil {
			klog.ErrorS(err, "Failed to list SubVolumes for draining share", "shareID", shareID)
			continue
		}

		remaining := int32(len(subVolumes)) //nolint:gosec // len bounded by MaxShares * PVCs-per-share, well within int32
		drained := remaining == 0

		ds := v1alpha1.ShareDrainStatus{
			ShareID:             shareID,
			RemainingSubVolumes: remaining,
			Drained:             drained,
		}

		// Preserve DrainStartedAt from existing status
		if existing, ok := existingDrainStatus[shareID]; ok && existing.DrainStartedAt != nil {
			ds.DrainStartedAt = existing.DrainStartedAt
		} else {
			now := metav1.Now()
			ds.DrainStartedAt = &now
		}

		updatedDrainStatus = append(updatedDrainStatus, ds)

		if drained {
			klog.V(2).InfoS("Share fully drained",
				"pool", pool.Name, "shareID", shareID)
		} else {
			klog.V(4).InfoS("Share drain in progress",
				"pool", pool.Name, "shareID", shareID, "remaining", remaining)
		}
	}

	pool.Status.DrainStatus = updatedDrainStatus

	// Set a condition summarizing drain progress
	totalDraining := len(pool.Spec.DrainShares)
	drainedCount := 0
	totalRemaining := int32(0)
	for _, ds := range updatedDrainStatus {
		if ds.Drained {
			drainedCount++
		}
		totalRemaining += ds.RemainingSubVolumes
	}

	if drainedCount == totalDraining {
		r.setCondition(pool, "DrainComplete", metav1.ConditionTrue, "AllSharesDrained",
			fmt.Sprintf("All %d shares fully drained", totalDraining))
	} else {
		r.setCondition(pool, "DrainComplete", metav1.ConditionFalse, "DrainInProgress",
			fmt.Sprintf("%d/%d shares drained, %d SubVolumes remaining",
				drainedCount, totalDraining, totalRemaining))
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
	pool.Status.ShareCount = int32(len(pool.Status.Shares)) //nolint:gosec // safe: MaxShares capped at 100

	// Recalculate total capacity
	var totalCapacity int64
	for _, s := range pool.Status.Shares {
		totalCapacity += s.TotalGB
	}
	pool.Status.TotalCapacityGB = totalCapacity

	// Emit Prometheus gauges
	metrics.PoolCapacityGB.WithLabelValues(pool.Name).Set(float64(pool.Status.TotalCapacityGB))
	metrics.PoolAllocatedGB.WithLabelValues(pool.Name).Set(float64(pool.Status.TotalAllocatedGB))
	metrics.PoolShareCount.WithLabelValues(pool.Name).Set(float64(pool.Status.ShareCount))
	metrics.PoolPVCCount.WithLabelValues(pool.Name).Set(float64(pool.Status.TotalPVCCount))
}

// proactiveExpansion creates a new share if utilization exceeds the threshold.
func (r *FileSharePoolReconciler) proactiveExpansion(ctx context.Context, pool *v1alpha1.FileSharePool) {
	if !pool.Spec.AutoExpand {
		return
	}

	// Skip if any share is still creating
	for _, s := range pool.Status.Shares {
		if s.State == "creating" {
			return
		}
	}

	if len(pool.Spec.Tiers) > 0 {
		// Tier-aware expansion: check utilization per tier
		for _, tier := range pool.Spec.Tiers {
			var tierCapacity, tierAllocated int64
			tierShareCount := int32(0)
			for _, s := range pool.Status.Shares {
				if s.Tier == tier.Name {
					tierCapacity += s.TotalGB
					tierAllocated += s.AllocatedGB
					tierShareCount++
				}
			}
			if tierShareCount >= tier.MaxShares {
				continue
			}
			if tierCapacity == 0 {
				continue
			}
			utilization := int32(tierAllocated * 100 / tierCapacity) //nolint:gosec // safe: percentage 0-100
			if utilization <= pool.Spec.ExpandThresholdPercent {
				continue
			}
			klog.V(2).InfoS("Proactive expansion triggered for tier",
				"pool", pool.Name, "tier", tier.Name,
				"utilization", utilization, "threshold", pool.Spec.ExpandThresholdPercent,
			)
			if err := r.createPoolShare(ctx, pool, tier.Name); err != nil {
				klog.ErrorS(err, "Proactive expansion failed for tier", "pool", pool.Name, "tier", tier.Name)
			}
		}
		return
	}

	// No tiers: pool-wide check
	if int32(len(pool.Status.Shares)) >= pool.Spec.MaxShares { //nolint:gosec // safe: MaxShares capped at 100
		return
	}

	if pool.Status.TotalCapacityGB == 0 {
		return
	}

	utilization := int32(pool.Status.TotalAllocatedGB * 100 / pool.Status.TotalCapacityGB) //nolint:gosec // safe: percentage 0-100
	if utilization <= pool.Spec.ExpandThresholdPercent {
		return
	}

	klog.V(2).InfoS("Proactive expansion triggered",
		"pool", pool.Name,
		"utilization", utilization,
		"threshold", pool.Spec.ExpandThresholdPercent,
	)

	if err := r.createPoolShare(ctx, pool, ""); err != nil {
		klog.ErrorS(err, "Proactive expansion failed", "pool", pool.Name)
	}
}

// createPoolShare creates a new VPC share and appends it to pool status.
// tier is the name of the ShareTier; empty for pools without tiers.
func (r *FileSharePoolReconciler) createPoolShare(ctx context.Context, pool *v1alpha1.FileSharePool, tier string) error {
	profile, sizeGB, iops, _, _, err := pool.Spec.TierConfig(tier)
	if err != nil {
		return err
	}

	resourceGroup := pool.Spec.ResourceGroup
	if resourceGroup == "" {
		resourceGroup = r.defaultResourceGroup
	}

	// Build share name: include tier when present
	shareName := fmt.Sprintf("%s-share-%d", pool.Name, len(pool.Status.Shares)+1)
	if tier != "" {
		tierShareCount := int32(0)
		for _, s := range pool.Status.Shares {
			if s.Tier == tier {
				tierShareCount++
			}
		}
		shareName = fmt.Sprintf("%s-%s-share-%d", pool.Name, tier, tierShareCount+1)
	}

	input := ibmcloud.CreateShareInput{
		Name:             shareName,
		Zone:             pool.Spec.Zone,
		Profile:          profile,
		SizeGB:           sizeGB,
		IOPS:             iops,
		ResourceGroupID:  resourceGroup,
		Tags:             pool.Spec.Tags,
		EncryptInTransit: pool.Spec.EncryptionInTransit,
		VPCId:            r.vpcID,
		SubnetID:         r.subnetID,
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

	// Build initial MountTargets slice for the home zone.
	var mountTargets []v1alpha1.ZoneMountTarget
	if mountTargetIP != "" {
		mountTargets = []v1alpha1.ZoneMountTarget{
			{
				Zone:          shareInfo.Zone,
				MountTargetID: mountTargetID,
				MountTargetIP: mountTargetIP,
			},
		}
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
		Tier:          tier,
		Zone:          shareInfo.Zone,
		MountTargets:  mountTargets,
		CreatedAt:     &now,
	}

	pool.Status.Shares = append(pool.Status.Shares, newShare)
	pool.Status.ShareCount = int32(len(pool.Status.Shares)) //nolint:gosec // safe: MaxShares capped at 100
	pool.Status.TotalCapacityGB += shareInfo.SizeGB

	klog.V(2).InfoS("Created pool share",
		"pool", pool.Name,
		"tier", tier,
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
	if int32(len(pool.Status.Shares)) >= pool.Spec.MaxShares { //nolint:gosec // safe: MaxShares capped at 100
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
