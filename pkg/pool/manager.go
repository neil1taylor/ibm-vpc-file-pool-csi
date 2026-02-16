package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// Sentinel errors for pool operations.
var (
	ErrPoolNotFound              = errors.New("pool not found")
	ErrPoolExhausted             = errors.New("pool has no available capacity")
	ErrShareCreationPending      = errors.New("share creation is in progress")
	ErrSubVolumeNotFound         = errors.New("subvolume not found")
	ErrInsufficientShareCapacity = errors.New("share does not have enough remaining capacity")
)

// PoolManager defines the synchronous allocation interface used by the CSI controller.
type PoolManager interface {
	// Allocate finds a share with room, creates the subdirectory, records the
	// SubVolume CR, and returns the share's mount info.
	Allocate(ctx context.Context, req AllocationRequest) (*AllocationResult, error)

	// Deallocate removes the subdirectory and updates tracking.
	Deallocate(ctx context.Context, subVolumeName string) error

	// Expand updates the allocation size for an existing SubVolume.
	Expand(ctx context.Context, subVolumeName string, newSizeGB int64) error
}

// AllocationRequest contains the parameters for allocating a new SubVolume.
type AllocationRequest struct {
	PVName       string
	PVCName      string
	PVCNamespace string
	PoolName     string
	RequestedGB  int64
	Zone         string
	Tier         string // Tier name from StorageClass (empty = default/no tiers)
	UID          *int64
	GID          *int64
	Permissions  string
}

// AllocationResult contains the result of a successful allocation.
type AllocationResult struct {
	ShareID       string
	MountTargetIP string
	SubPath       string // e.g., "/pvcs/pvc-abc123"
	SharePath     string // e.g., "/" (the NFS export root)
	UID           *int64
	GID           *int64
	Permissions   string
}

// Manager implements PoolManager. It is the core brain of the CSI driver.
type Manager struct {
	mu                   sync.Mutex
	k8sClient            k8s.Client
	vpcClient            ibmcloud.VPCFileClient
	nfsOps               NFSOperations
	stagingBasePath      string
	defaultResourceGroup string
}

// Verify Manager implements PoolManager at compile time.
var _ PoolManager = (*Manager)(nil)

// SetDefaultResourceGroup sets the fallback resource group used when the pool spec doesn't specify one.
func (m *Manager) SetDefaultResourceGroup(rg string) {
	m.defaultResourceGroup = rg
}

// NewManager creates a new pool manager with the given dependencies.
func NewManager(k8sClient k8s.Client, vpcClient ibmcloud.VPCFileClient, nfsOps NFSOperations, stagingBasePath string) *Manager {
	return &Manager{
		k8sClient:       k8sClient,
		vpcClient:       vpcClient,
		nfsOps:          nfsOps,
		stagingBasePath: stagingBasePath,
	}
}

func (m *Manager) Allocate(ctx context.Context, req AllocationRequest) (_ *AllocationResult, retErr error) {
	start := time.Now()
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.AllocationsTotal.WithLabelValues(req.PoolName, status).Inc()
		metrics.AllocationDuration.WithLabelValues(req.PoolName).Observe(time.Since(start).Seconds())
	}()

	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Fetch FileSharePool CR
	pool, err := m.k8sClient.GetFileSharePool(ctx, req.PoolName)
	if err != nil {
		klog.ErrorS(err, "Failed to get pool", "pool", req.PoolName)
		return nil, fmt.Errorf("%w: %s", ErrPoolNotFound, req.PoolName)
	}

	// 2. Validate zone
	if req.Zone != "" && req.Zone != pool.Spec.Zone {
		return nil, fmt.Errorf("zone mismatch: requested %q but pool is in %q", req.Zone, pool.Spec.Zone)
	}

	// 3. Idempotency check
	existing, err := m.k8sClient.GetSubVolume(ctx, req.PVName)
	if err == nil && existing != nil {
		klog.V(2).InfoS("Idempotent allocation: SubVolume already exists", "pvName", req.PVName)
		return &AllocationResult{
			ShareID:       existing.Spec.ShareID,
			MountTargetIP: existing.Spec.ShareMountTargetIP,
			SubPath:       existing.Spec.SubPath,
			SharePath:     "/",
			UID:           existing.Spec.UID,
			GID:           existing.Spec.GID,
			Permissions:   existing.Spec.Permissions,
		}, nil
	}

	// 4. Validate tier when pool has tiers configured
	if len(pool.Spec.Tiers) > 0 && req.Tier == "" {
		return nil, fmt.Errorf("tier is required when pool has tiers configured")
	}

	// 5. Select share
	share, err := selectShare(pool.Spec.AllocationStrategy, pool.Status.Shares, req.RequestedGB, req.Tier)
	if err != nil && errors.Is(err, ErrPoolExhausted) {
		// 6. Auto-expand: create a new share if allowed
		share, err = m.tryAutoExpand(ctx, pool, req.RequestedGB, req.Tier)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	// 6. Create subdirectory (only when running on the node with NFS access)
	subPath := fmt.Sprintf("/pvcs/%s", req.PVName)
	uid, gid, perms := m.resolvePermissions(req, pool)
	if m.nfsOps != nil {
		if err := createSubDirectory(m.nfsOps, m.stagingBasePath, subPath, uid, gid, perms); err != nil {
			klog.ErrorS(err, "Failed to create subdirectory", "subPath", subPath)
			return nil, fmt.Errorf("create subdirectory: %w", err)
		}
	}

	// 7. Build and create SubVolume CR
	now := metav1.Now()
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.PVName,
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":     req.PoolName,
				"storage.ibmcloud.io/share-id": share.ShareID,
			},
		},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           req.PoolName,
			ShareID:            share.ShareID,
			ShareMountTargetIP: share.MountTargetIP,
			SubPath:            subPath,
			RequestedGB:        req.RequestedGB,
			PVName:             req.PVName,
			PVCName:            req.PVCName,
			PVCNamespace:       req.PVCNamespace,
			UID:                uid,
			GID:                gid,
			Permissions:        perms,
			ReclaimPolicy:      "Delete",
		},
		Status: v1alpha1.SubVolumeStatus{
			Phase:     "Bound",
			CreatedAt: &now,
		},
	}

	if err := m.k8sClient.CreateSubVolume(ctx, sv); err != nil {
		klog.ErrorS(err, "Failed to create SubVolume CR", "pvName", req.PVName)
		return nil, fmt.Errorf("create SubVolume CR: %w", err)
	}

	// 8. Update pool status
	m.updateShareAllocation(pool, share.ShareID, req.RequestedGB)
	if err := m.k8sClient.UpdateFileSharePoolStatus(ctx, pool); err != nil {
		klog.ErrorS(err, "Failed to update pool status", "pool", req.PoolName)
	}

	klog.V(2).InfoS("Allocated SubVolume",
		"pvName", req.PVName,
		"pool", req.PoolName,
		"shareID", share.ShareID,
		"requestedGB", req.RequestedGB,
	)

	return &AllocationResult{
		ShareID:       share.ShareID,
		MountTargetIP: share.MountTargetIP,
		SubPath:       subPath,
		SharePath:     "/",
		UID:           uid,
		GID:           gid,
		Permissions:   perms,
	}, nil
}

func (m *Manager) Deallocate(ctx context.Context, subVolumeName string) (retErr error) {
	poolName := ""
	defer func() {
		status := "deallocate_success"
		if retErr != nil {
			status = "deallocate_error"
		}
		metrics.AllocationsTotal.WithLabelValues(poolName, status).Inc()
	}()

	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Fetch SubVolume CR
	sv, err := m.k8sClient.GetSubVolume(ctx, subVolumeName)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrSubVolumeNotFound, subVolumeName)
	}
	poolName = sv.Spec.PoolName

	// 2. Fetch FileSharePool CR
	pool, err := m.k8sClient.GetFileSharePool(ctx, poolName)
	if err != nil {
		return fmt.Errorf("get pool %s: %w", poolName, err)
	}

	// 3. Remove subdirectory (only when running on the node with NFS access)
	if m.nfsOps != nil {
		if err := removeSubDirectory(m.nfsOps, m.stagingBasePath, sv.Spec.SubPath); err != nil {
			klog.ErrorS(err, "Failed to remove subdirectory, retaining SubVolume CR", "subPath", sv.Spec.SubPath)
			return fmt.Errorf("remove subdirectory: %w", err)
		}
	}

	// 4. Delete SubVolume CR
	if err := m.k8sClient.DeleteSubVolume(ctx, subVolumeName); err != nil {
		klog.ErrorS(err, "Failed to delete SubVolume CR", "name", subVolumeName)
		return fmt.Errorf("delete SubVolume CR: %w", err)
	}

	// 5. Update pool status
	m.updateShareDeallocation(pool, sv.Spec.ShareID, sv.Spec.RequestedGB)
	if err := m.k8sClient.UpdateFileSharePoolStatus(ctx, pool); err != nil {
		klog.ErrorS(err, "Failed to update pool status", "pool", sv.Spec.PoolName)
	}

	klog.V(2).InfoS("Deallocated SubVolume",
		"name", subVolumeName,
		"pool", sv.Spec.PoolName,
		"shareID", sv.Spec.ShareID,
	)

	return nil
}

func (m *Manager) Expand(ctx context.Context, subVolumeName string, newSizeGB int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Fetch SubVolume CR
	sv, err := m.k8sClient.GetSubVolume(ctx, subVolumeName)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrSubVolumeNotFound, subVolumeName)
	}

	// 2. Fetch FileSharePool CR
	pool, err := m.k8sClient.GetFileSharePool(ctx, sv.Spec.PoolName)
	if err != nil {
		return fmt.Errorf("get pool %s: %w", sv.Spec.PoolName, err)
	}

	// 3. Find the share and check capacity
	delta := newSizeGB - sv.Spec.RequestedGB
	if delta <= 0 {
		return nil // no-op if shrinking or same size
	}

	shareIdx := -1
	for i := range pool.Status.Shares {
		if pool.Status.Shares[i].ShareID == sv.Spec.ShareID {
			shareIdx = i
			break
		}
	}
	if shareIdx == -1 {
		return fmt.Errorf("share %s not found in pool status", sv.Spec.ShareID)
	}

	share := &pool.Status.Shares[shareIdx]
	freeGB := share.TotalGB - share.AllocatedGB
	if delta > freeGB {
		return fmt.Errorf("%w: need %d GB but share has %d GB free", ErrInsufficientShareCapacity, delta, freeGB)
	}

	// 4. Update SubVolume CR
	sv.Spec.RequestedGB = newSizeGB
	if err := m.k8sClient.UpdateSubVolume(ctx, sv); err != nil {
		return fmt.Errorf("update SubVolume CR: %w", err)
	}

	// 5. Update pool status
	share.AllocatedGB += delta
	pool.Status.TotalAllocatedGB += delta
	if err := m.k8sClient.UpdateFileSharePoolStatus(ctx, pool); err != nil {
		klog.ErrorS(err, "Failed to update pool status after expand", "pool", sv.Spec.PoolName)
	}

	klog.V(2).InfoS("Expanded SubVolume",
		"name", subVolumeName,
		"oldGB", newSizeGB-delta,
		"newGB", newSizeGB,
	)

	return nil
}

// tryAutoExpand attempts to create a new VPC share when the pool is exhausted.
func (m *Manager) tryAutoExpand(ctx context.Context, pool *v1alpha1.FileSharePool, requestedGB int64, tier string) (*v1alpha1.PoolShareStatus, error) {
	if !pool.Spec.AutoExpand {
		return nil, ErrPoolExhausted
	}

	profile, sizeGB, iops, maxShares, _, err := pool.Spec.TierConfig(tier)
	if err != nil {
		return nil, err
	}

	// Count shares in this tier
	tierShareCount := int32(0)
	for _, s := range pool.Status.Shares {
		if s.Tier == tier {
			tierShareCount++
		}
	}
	if tierShareCount >= maxShares {
		return nil, ErrPoolExhausted
	}

	if requestedGB > sizeGB {
		return nil, fmt.Errorf("request of %d GB exceeds share size of %d GB", requestedGB, sizeGB)
	}

	klog.V(2).InfoS("Auto-expanding pool", "pool", pool.Name, "tier", tier, "currentShares", len(pool.Status.Shares))

	resourceGroup := pool.Spec.ResourceGroup
	if resourceGroup == "" {
		resourceGroup = m.defaultResourceGroup
	}

	shareName := fmt.Sprintf("%s-share-%d", pool.Name, len(pool.Status.Shares)+1)
	if tier != "" {
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
	}

	shareInfo, err := m.vpcClient.CreateFileShare(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("create VPC share: %w", err)
	}

	mountTargetIP := ""
	mountTargetID := ""
	if len(shareInfo.MountTargets) > 0 {
		mountTargetIP = shareInfo.MountTargets[0].IPAddress
		mountTargetID = shareInfo.MountTargets[0].ID
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
		State:         "stable",
		Tier:          tier,
		Zone:          shareInfo.Zone,
		CreatedAt:     &now,
	}

	pool.Status.Shares = append(pool.Status.Shares, newShare)
	pool.Status.ShareCount = int32(len(pool.Status.Shares)) //nolint:gosec // safe: MaxShares capped at 100
	pool.Status.TotalCapacityGB += shareInfo.SizeGB

	return &newShare, nil
}

// resolvePermissions returns the effective UID, GID, and permissions for a subdirectory.
// Request-level values override pool defaults.
func (m *Manager) resolvePermissions(req AllocationRequest, pool *v1alpha1.FileSharePool) (*int64, *int64, string) {
	uid := req.UID
	if uid == nil {
		uid = pool.Spec.DefaultUID
	}
	gid := req.GID
	if gid == nil {
		gid = pool.Spec.DefaultGID
	}
	perms := req.Permissions
	if perms == "" {
		perms = pool.Spec.DefaultPermissions
	}
	return uid, gid, perms
}

// updateShareAllocation increments a share's AllocatedGB and PVCCount.
func (m *Manager) updateShareAllocation(pool *v1alpha1.FileSharePool, shareID string, sizeGB int64) {
	for i := range pool.Status.Shares {
		if pool.Status.Shares[i].ShareID == shareID {
			pool.Status.Shares[i].AllocatedGB += sizeGB
			pool.Status.Shares[i].PVCCount++
			break
		}
	}
	pool.Status.TotalAllocatedGB += sizeGB
	pool.Status.TotalPVCCount++
}

// updateShareDeallocation decrements a share's AllocatedGB and PVCCount.
func (m *Manager) updateShareDeallocation(pool *v1alpha1.FileSharePool, shareID string, sizeGB int64) {
	for i := range pool.Status.Shares {
		if pool.Status.Shares[i].ShareID == shareID {
			pool.Status.Shares[i].AllocatedGB -= sizeGB
			pool.Status.Shares[i].PVCCount--
			break
		}
	}
	pool.Status.TotalAllocatedGB -= sizeGB
	pool.Status.TotalPVCCount--
}
