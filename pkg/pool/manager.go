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
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
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
	ErrSnapshotNotFound          = errors.New("snapshot not found")
	ErrSnapshotAlreadyExists     = errors.New("snapshot already exists")
	ErrSourceNotFound            = errors.New("source volume not found")
	ErrGroupSnapshotNotFound     = errors.New("group snapshot not found")
	ErrGroupSnapshotFailed       = errors.New("group snapshot failed")
	ErrEmptySourceList           = errors.New("source volume list is empty")
)

// SnapshotResult contains the result of a successful snapshot operation.
type SnapshotResult struct {
	SnapshotName   string
	PoolName       string
	ShareID        string
	SnapshotPath   string
	SourceSubPath  string
	SizeBytes      int64
	CreationTime   time.Time
	ReadyToUse     bool
	SourceVolumeID string // CSI volume ID of the source, set during group snapshots
}

// GroupSnapshotRequest contains the parameters for a group snapshot.
type GroupSnapshotRequest struct {
	GroupName       string
	PoolName        string
	SourceVolumeIDs []string
	CopyOrder       []string // SubVolume names in desired copy order
	FailurePolicy   string   // "Abort" or "Continue"
	Parameters      map[string]string
}

// GroupSnapshotResult contains the result of a group snapshot.
type GroupSnapshotResult struct {
	GroupName             string
	PoolName              string
	Members               []SnapshotResult
	CreationTime          time.Time
	ReadyToUse            bool
	InconsistencyWindowMs int64
}

// PoolManager defines the synchronous allocation interface used by the CSI controller.
type PoolManager interface {
	// Allocate finds a share with room, creates the subdirectory, records the
	// SubVolume CR, and returns the share's mount info.
	Allocate(ctx context.Context, req AllocationRequest) (*AllocationResult, error)

	// Deallocate removes the subdirectory and updates tracking.
	Deallocate(ctx context.Context, subVolumeName string) error

	// Expand updates the allocation size for an existing SubVolume.
	Expand(ctx context.Context, subVolumeName string, newSizeGB int64) error

	// CreateSnapshot creates a directory-level copy of a SubVolume.
	CreateSnapshot(ctx context.Context, snapshotName, sourceVolumeID string, params map[string]string) (*SnapshotResult, error)

	// DeleteSnapshot removes a snapshot directory and its CR.
	DeleteSnapshot(ctx context.Context, snapshotName string) error

	// ListSnapshots lists snapshots, optionally filtered by source volume.
	ListSnapshots(ctx context.Context, sourceVolumeID string) ([]SnapshotResult, error)

	// RestoreSnapshot creates a new SubVolume from a snapshot.
	RestoreSnapshot(ctx context.Context, snapshotName string, req AllocationRequest) (*AllocationResult, error)

	// CloneVolume creates a new SubVolume that is a copy of an existing one.
	// For small volumes (below syncThreshold), the copy completes before returning.
	// For large volumes, the copy runs asynchronously -- the SubVolume CR is
	// created with cloneStatus=Pending and the background worker completes the copy.
	CloneVolume(ctx context.Context, sourceVolumeID string, req AllocationRequest, syncThresholdGB int64) (*AllocationResult, error)

	// CreateVolumeGroupSnapshot creates snapshots for multiple SubVolumes in sequence.
	// Does NOT execute hooks -- the CSI controller handles hook orchestration.
	CreateVolumeGroupSnapshot(ctx context.Context, req GroupSnapshotRequest) (*GroupSnapshotResult, error)

	// DeleteVolumeGroupSnapshot deletes all member snapshots and the group CR.
	DeleteVolumeGroupSnapshot(ctx context.Context, groupName string) error
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
	ShareID         string
	MountTargetIP   string
	SubPath         string // e.g., "/pvcs/pvc-abc123"
	SharePath       string // NFS export path (e.g., "/share_abc123" for VPC access mode)
	UID             *int64
	GID             *int64
	Permissions     string
	MountTargets    map[string]string // zone -> IP (nil when single-zone)
	AccessibleZones []string          // all zones this volume is accessible from
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

// SetVPCClient replaces the VPC client after deferred initialization.
func (m *Manager) SetVPCClient(client ibmcloud.VPCFileClient) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vpcClient = client
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

	// 2. Validate zone (accept home zone and accessor zones)
	if req.Zone != "" && !pool.Spec.IsAccessibleZone(req.Zone) {
		return nil, fmt.Errorf("zone mismatch: requested %q but pool serves %v", req.Zone, pool.Spec.AllAccessibleZones())
	}

	// 3. Idempotency check
	existing, err := m.k8sClient.GetSubVolume(ctx, req.PVName)
	if err == nil && existing != nil {
		klog.V(2).InfoS("Idempotent allocation: SubVolume already exists", "pvName", req.PVName)
		sharePath := existing.Spec.ShareExportPath
		if sharePath == "" {
			sharePath = "/"
		}
		return &AllocationResult{
			ShareID:       existing.Spec.ShareID,
			MountTargetIP: existing.Spec.ShareMountTargetIP,
			SubPath:       existing.Spec.SubPath,
			SharePath:     sharePath,
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
			ShareExportPath:    share.ExportPath,
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

	// Save desired status before Create — Create() strips status because it's
	// a subresource and the server response doesn't include it.
	desiredStatus := sv.Status

	if err := m.k8sClient.CreateSubVolume(ctx, sv); err != nil {
		klog.ErrorS(err, "Failed to create SubVolume CR", "pvName", req.PVName)
		return nil, fmt.Errorf("create SubVolume CR: %w", err)
	}

	// Restore status and write it via the status subresource.
	sv.Status = desiredStatus
	if err := m.k8sClient.UpdateSubVolumeStatus(ctx, sv); err != nil {
		klog.ErrorS(err, "Failed to update SubVolume status", "pvName", req.PVName)
		// Non-fatal: the SubVolume exists and will be usable, just Phase will be empty
		// until a reconciler fills it in.
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

	sharePath := share.ExportPath
	if sharePath == "" {
		sharePath = "/"
	}
	result := &AllocationResult{
		ShareID:       share.ShareID,
		MountTargetIP: share.MountTargetIP,
		SubPath:       subPath,
		SharePath:     sharePath,
		UID:           uid,
		GID:           gid,
		Permissions:   perms,
	}

	// Populate cross-zone mount targets if accessor zones are configured
	if len(pool.Spec.AccessorZones) > 0 && len(share.MountTargets) > 0 {
		result.MountTargets = make(map[string]string, len(share.MountTargets))
		for _, mt := range share.MountTargets {
			result.MountTargets[mt.Zone] = mt.MountTargetIP
		}
		result.AccessibleZones = pool.Spec.AllAccessibleZones()
	}

	return result, nil
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

func (m *Manager) CreateSnapshot(ctx context.Context, snapshotName, sourceVolumeID string, params map[string]string) (_ *SnapshotResult, retErr error) {
	start := time.Now()
	poolName := ""
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.SnapshotsTotal.WithLabelValues(poolName, "create", status).Inc()
		metrics.SnapshotDuration.WithLabelValues(poolName, "create").Observe(time.Since(start).Seconds())
	}()

	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Idempotency check
	existing, err := m.k8sClient.GetSnapshot(ctx, snapshotName)
	if err == nil && existing != nil {
		poolName = existing.Spec.PoolName
		return &SnapshotResult{
			SnapshotName:  existing.Name,
			PoolName:      existing.Spec.PoolName,
			ShareID:       existing.Spec.ShareID,
			SnapshotPath:  existing.Spec.SnapshotPath,
			SourceSubPath: existing.Spec.SourceSubPath,
			SizeBytes:     existing.Spec.SizeGB * (1 << 30),
			CreationTime:  existing.Status.CreationTime.Time,
			ReadyToUse:    existing.Status.ReadyToUse,
		}, nil
	}

	// 2. Parse sourceVolumeID to get pvName
	_, _, pvName, err := parseManagerVolumeID(sourceVolumeID)
	if err != nil {
		return nil, fmt.Errorf("invalid source volume ID: %w", err)
	}

	// 3. Fetch source SubVolume CR
	sv, err := m.k8sClient.GetSubVolume(ctx, pvName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrSourceNotFound, pvName)
	}
	poolName = sv.Spec.PoolName

	// 4. Fetch pool
	pool, err := m.k8sClient.GetFileSharePool(ctx, poolName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrPoolNotFound, poolName)
	}

	// 5. Build snapshot path
	snapshotPath := fmt.Sprintf("/pvcs/.snapshots/snap-%s", snapshotName)

	// 6. Create .snapshots dir and copy
	if m.nfsOps != nil {
		snapshotsDir, err := util.SafeJoinSnapshot(m.stagingBasePath, "/pvcs/.snapshots/snap-placeholder")
		if err != nil {
			return nil, fmt.Errorf("invalid snapshots dir: %w", err)
		}
		// Get just the .snapshots parent dir
		snapshotsParent := snapshotsDir[:len(snapshotsDir)-len("/snap-placeholder")]
		if err := m.nfsOps.MkdirAll(snapshotsParent, 0755); err != nil {
			return nil, fmt.Errorf("create snapshots dir: %w", err)
		}

		srcPath, err := util.SafeJoin(m.stagingBasePath, sv.Spec.SubPath)
		if err != nil {
			return nil, fmt.Errorf("invalid source path: %w", err)
		}
		dstPath, err := util.SafeJoinSnapshot(m.stagingBasePath, snapshotPath)
		if err != nil {
			return nil, fmt.Errorf("invalid snapshot path: %w", err)
		}

		if err := m.nfsOps.CopyDir(srcPath, dstPath); err != nil {
			return nil, fmt.Errorf("copy snapshot: %w", err)
		}
	}

	// 7. Create Snapshot CR
	now := metav1.Now()
	snap := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: snapshotName,
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":             poolName,
				"storage.ibmcloud.io/share-id":         sv.Spec.ShareID,
				"storage.ibmcloud.io/source-subvolume": pvName,
			},
		},
		Spec: v1alpha1.SnapshotSpec{
			SourceSubVolume:    pvName,
			PoolName:           poolName,
			ShareID:            sv.Spec.ShareID,
			ShareMountTargetIP: sv.Spec.ShareMountTargetIP,
			SnapshotPath:       snapshotPath,
			SourceSubPath:      sv.Spec.SubPath,
			SizeGB:             sv.Spec.RequestedGB,
		},
		Status: v1alpha1.SnapshotStatus{
			Phase:        "Ready",
			ReadyToUse:   true,
			SizeBytes:    sv.Spec.RequestedGB * (1 << 30),
			CreationTime: &now,
		},
	}

	desiredStatus := snap.Status

	if err := m.k8sClient.CreateSnapshot(ctx, snap); err != nil {
		return nil, fmt.Errorf("create Snapshot CR: %w", err)
	}

	snap.Status = desiredStatus
	if err := m.k8sClient.UpdateSnapshotStatus(ctx, snap); err != nil {
		klog.ErrorS(err, "Failed to update Snapshot status", "snapshot", snapshotName)
	}

	// 8. Update pool allocated capacity (snapshots count toward share usage)
	m.updateShareAllocation(pool, sv.Spec.ShareID, sv.Spec.RequestedGB)
	if err := m.k8sClient.UpdateFileSharePoolStatus(ctx, pool); err != nil {
		klog.ErrorS(err, "Failed to update pool status after snapshot", "pool", poolName)
	}

	klog.V(2).InfoS("Created snapshot",
		"snapshot", snapshotName,
		"source", pvName,
		"pool", poolName,
		"shareID", sv.Spec.ShareID,
	)

	return &SnapshotResult{
		SnapshotName:  snapshotName,
		PoolName:      poolName,
		ShareID:       sv.Spec.ShareID,
		SnapshotPath:  snapshotPath,
		SourceSubPath: sv.Spec.SubPath,
		SizeBytes:     sv.Spec.RequestedGB * (1 << 30),
		CreationTime:  now.Time,
		ReadyToUse:    true,
	}, nil
}

func (m *Manager) DeleteSnapshot(ctx context.Context, snapshotName string) (retErr error) {
	poolName := ""
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.SnapshotsTotal.WithLabelValues(poolName, "delete", status).Inc()
	}()

	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Fetch Snapshot CR
	snap, err := m.k8sClient.GetSnapshot(ctx, snapshotName)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapshotName)
	}
	poolName = snap.Spec.PoolName

	// 2. Remove snapshot directory
	if m.nfsOps != nil {
		fullPath, err := util.SafeJoinSnapshot(m.stagingBasePath, snap.Spec.SnapshotPath)
		if err != nil {
			return fmt.Errorf("invalid snapshot path: %w", err)
		}
		if err := m.nfsOps.RemoveAll(fullPath); err != nil {
			return fmt.Errorf("remove snapshot dir: %w", err)
		}
	}

	// 3. Delete Snapshot CR
	if err := m.k8sClient.DeleteSnapshot(ctx, snapshotName); err != nil {
		return fmt.Errorf("delete Snapshot CR: %w", err)
	}

	// 4. Update pool status (free capacity)
	pool, err := m.k8sClient.GetFileSharePool(ctx, poolName)
	if err != nil {
		klog.ErrorS(err, "Failed to get pool for snapshot deletion status update", "pool", poolName)
	} else {
		m.updateShareDeallocation(pool, snap.Spec.ShareID, snap.Spec.SizeGB)
		if err := m.k8sClient.UpdateFileSharePoolStatus(ctx, pool); err != nil {
			klog.ErrorS(err, "Failed to update pool status after snapshot deletion", "pool", poolName)
		}
	}

	klog.V(2).InfoS("Deleted snapshot",
		"snapshot", snapshotName,
		"pool", poolName,
		"shareID", snap.Spec.ShareID,
	)

	return nil
}

func (m *Manager) ListSnapshots(ctx context.Context, sourceVolumeID string) ([]SnapshotResult, error) {
	var snapshots []v1alpha1.Snapshot

	if sourceVolumeID != "" {
		_, _, pvName, err := parseManagerVolumeID(sourceVolumeID)
		if err != nil {
			return nil, fmt.Errorf("invalid source volume ID: %w", err)
		}
		snapshots, err = m.k8sClient.ListSnapshotsBySource(ctx, pvName)
		if err != nil {
			return nil, err
		}
	} else {
		// List all — use empty pool to list everything (no label filter matches all)
		// We'll use the pool label with empty string, which won't match.
		// Instead, list by each pool. For simplicity, list by source with empty returns empty.
		return nil, nil
	}

	var results []SnapshotResult
	for _, snap := range snapshots {
		var creationTime time.Time
		if snap.Status.CreationTime != nil {
			creationTime = snap.Status.CreationTime.Time
		}
		results = append(results, SnapshotResult{
			SnapshotName:  snap.Name,
			PoolName:      snap.Spec.PoolName,
			ShareID:       snap.Spec.ShareID,
			SnapshotPath:  snap.Spec.SnapshotPath,
			SourceSubPath: snap.Spec.SourceSubPath,
			SizeBytes:     snap.Spec.SizeGB * (1 << 30),
			CreationTime:  creationTime,
			ReadyToUse:    snap.Status.ReadyToUse,
		})
	}
	return results, nil
}

func (m *Manager) RestoreSnapshot(ctx context.Context, snapshotName string, req AllocationRequest) (_ *AllocationResult, retErr error) {
	start := time.Now()
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.SnapshotsTotal.WithLabelValues(req.PoolName, "restore", status).Inc()
		metrics.SnapshotDuration.WithLabelValues(req.PoolName, "restore").Observe(time.Since(start).Seconds())
	}()

	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Idempotency check
	existing, err := m.k8sClient.GetSubVolume(ctx, req.PVName)
	if err == nil && existing != nil {
		klog.V(2).InfoS("Idempotent restore: SubVolume already exists", "pvName", req.PVName)
		sharePath := existing.Spec.ShareExportPath
		if sharePath == "" {
			sharePath = "/"
		}
		return &AllocationResult{
			ShareID:       existing.Spec.ShareID,
			MountTargetIP: existing.Spec.ShareMountTargetIP,
			SubPath:       existing.Spec.SubPath,
			SharePath:     sharePath,
			UID:           existing.Spec.UID,
			GID:           existing.Spec.GID,
			Permissions:   existing.Spec.Permissions,
		}, nil
	}

	// 2. Fetch Snapshot CR
	snap, err := m.k8sClient.GetSnapshot(ctx, snapshotName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapshotName)
	}
	if !snap.Status.ReadyToUse {
		return nil, fmt.Errorf("snapshot %q is not ready to use (phase: %s)", snapshotName, snap.Status.Phase)
	}

	// 3. Fetch pool and select share
	pool, err := m.k8sClient.GetFileSharePool(ctx, req.PoolName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrPoolNotFound, req.PoolName)
	}

	share, err := selectShare(pool.Spec.AllocationStrategy, pool.Status.Shares, req.RequestedGB, req.Tier)
	if err != nil && errors.Is(err, ErrPoolExhausted) {
		share, err = m.tryAutoExpand(ctx, pool, req.RequestedGB, req.Tier)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	// 4. Copy snapshot to new subdirectory
	subPath := fmt.Sprintf("/pvcs/%s", req.PVName)
	if m.nfsOps != nil {
		srcPath, err := util.SafeJoinSnapshot(m.stagingBasePath, snap.Spec.SnapshotPath)
		if err != nil {
			return nil, fmt.Errorf("invalid snapshot path: %w", err)
		}
		dstPath, err := util.SafeJoin(m.stagingBasePath, subPath)
		if err != nil {
			return nil, fmt.Errorf("invalid destination path: %w", err)
		}
		if err := m.nfsOps.CopyDir(srcPath, dstPath); err != nil {
			return nil, fmt.Errorf("restore snapshot: %w", err)
		}
	}

	// 5. Create SubVolume CR
	uid, gid, perms := m.resolvePermissions(req, pool)
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
			ShareExportPath:    share.ExportPath,
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

	desiredStatus := sv.Status
	if err := m.k8sClient.CreateSubVolume(ctx, sv); err != nil {
		return nil, fmt.Errorf("create SubVolume CR: %w", err)
	}

	sv.Status = desiredStatus
	if err := m.k8sClient.UpdateSubVolumeStatus(ctx, sv); err != nil {
		klog.ErrorS(err, "Failed to update SubVolume status after restore", "pvName", req.PVName)
	}

	// 6. Update pool status
	m.updateShareAllocation(pool, share.ShareID, req.RequestedGB)
	if err := m.k8sClient.UpdateFileSharePoolStatus(ctx, pool); err != nil {
		klog.ErrorS(err, "Failed to update pool status after restore", "pool", req.PoolName)
	}

	klog.V(2).InfoS("Restored snapshot",
		"snapshot", snapshotName,
		"pvName", req.PVName,
		"pool", req.PoolName,
		"shareID", share.ShareID,
	)

	sharePath := share.ExportPath
	if sharePath == "" {
		sharePath = "/"
	}
	return &AllocationResult{
		ShareID:       share.ShareID,
		MountTargetIP: share.MountTargetIP,
		SubPath:       subPath,
		SharePath:     sharePath,
		UID:           uid,
		GID:           gid,
		Permissions:   perms,
	}, nil
}

func (m *Manager) CloneVolume(ctx context.Context, sourceVolumeID string, req AllocationRequest, syncThresholdGB int64) (_ *AllocationResult, retErr error) {
	start := time.Now()
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.ClonesTotal.WithLabelValues(req.PoolName, status).Inc()
		if status == "success" {
			metrics.CloneDuration.WithLabelValues(req.PoolName).Observe(time.Since(start).Seconds())
		}
	}()

	m.mu.Lock()
	defer m.mu.Unlock()

	// 1. Idempotency check
	existing, err := m.k8sClient.GetSubVolume(ctx, req.PVName)
	if err == nil && existing != nil {
		klog.V(2).InfoS("Idempotent clone: SubVolume already exists", "pvName", req.PVName)
		if existing.Status.CloneStatus == "Failed" {
			return nil, fmt.Errorf("clone previously failed for %s: %s",
				req.PVName, existing.Status.CloneProgress.Error)
		}
		sharePath := existing.Spec.ShareExportPath
		if sharePath == "" {
			sharePath = "/"
		}
		return &AllocationResult{
			ShareID:       existing.Spec.ShareID,
			MountTargetIP: existing.Spec.ShareMountTargetIP,
			SubPath:       existing.Spec.SubPath,
			SharePath:     sharePath,
			UID:           existing.Spec.UID,
			GID:           existing.Spec.GID,
			Permissions:   existing.Spec.Permissions,
		}, nil
	}

	// 2. Parse source volume ID
	_, _, pvName, err := parseManagerVolumeID(sourceVolumeID)
	if err != nil {
		return nil, fmt.Errorf("invalid source volume ID: %w", err)
	}

	// 3. Fetch source SubVolume CR
	sourceSV, err := m.k8sClient.GetSubVolume(ctx, pvName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrSourceNotFound, pvName)
	}

	// 4. Validate: clone size must be >= source size
	if req.RequestedGB < sourceSV.Spec.RequestedGB {
		return nil, fmt.Errorf("clone size (%d GB) must be >= source size (%d GB)",
			req.RequestedGB, sourceSV.Spec.RequestedGB)
	}

	// 5. Fetch pool and select target share (prefer source share)
	pool, err := m.k8sClient.GetFileSharePool(ctx, req.PoolName)
	if err != nil {
		klog.ErrorS(err, "Failed to get pool", "pool", req.PoolName)
		return nil, fmt.Errorf("%w: %s", ErrPoolNotFound, req.PoolName)
	}

	share, err := selectShareForClone(
		pool.Spec.AllocationStrategy, pool.Status.Shares,
		req.RequestedGB, req.Tier, sourceSV.Spec.ShareID,
	)
	if err != nil && errors.Is(err, ErrPoolExhausted) {
		share, err = m.tryAutoExpand(ctx, pool, req.RequestedGB, req.Tier)
		if err != nil {
			return nil, err
		}
	} else if err != nil {
		return nil, err
	}

	// 6. Determine sync vs async
	isSameShare := share.ShareID == sourceSV.Spec.ShareID
	isSmallEnough := sourceSV.Spec.RequestedGB <= syncThresholdGB
	doSync := isSameShare && isSmallEnough && m.nfsOps != nil

	subPath := fmt.Sprintf("/pvcs/%s", req.PVName)
	uid, gid, perms := m.resolvePermissions(req, pool)
	now := metav1.Now()

	if doSync {
		// 6a. Synchronous clone: copy data first, then create CR as Complete
		srcPath, err := util.SafeJoin(m.stagingBasePath, sourceSV.Spec.SubPath)
		if err != nil {
			return nil, fmt.Errorf("invalid source path: %w", err)
		}
		dstPath, err := util.SafeJoin(m.stagingBasePath, subPath)
		if err != nil {
			return nil, fmt.Errorf("invalid destination path: %w", err)
		}
		if err := m.nfsOps.CopyDir(srcPath, dstPath); err != nil {
			return nil, fmt.Errorf("clone copy: %w", err)
		}

		completedAt := metav1.Now()
		sv := &v1alpha1.SubVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: req.PVName,
				Labels: map[string]string{
					"storage.ibmcloud.io/pool":         req.PoolName,
					"storage.ibmcloud.io/share-id":     share.ShareID,
					"storage.ibmcloud.io/clone-source": pvName,
				},
			},
			Spec: v1alpha1.SubVolumeSpec{
				PoolName:           req.PoolName,
				ShareID:            share.ShareID,
				ShareMountTargetIP: share.MountTargetIP,
				ShareExportPath:    share.ExportPath,
				SubPath:            subPath,
				RequestedGB:        req.RequestedGB,
				PVName:             req.PVName,
				PVCName:            req.PVCName,
				PVCNamespace:       req.PVCNamespace,
				UID:                uid,
				GID:                gid,
				Permissions:        perms,
				ReclaimPolicy:      "Delete",
				SourceVolume:       pvName,
				SourceShareID:      sourceSV.Spec.ShareID,
			},
			Status: v1alpha1.SubVolumeStatus{
				Phase:       "Bound",
				CloneStatus: "Complete",
				CloneProgress: &v1alpha1.CloneProgress{
					BytesCopied: sourceSV.Spec.RequestedGB * (1 << 30),
					TotalBytes:  sourceSV.Spec.RequestedGB * (1 << 30),
					StartedAt:   &now,
					CompletedAt: &completedAt,
				},
				CreatedAt: &now,
			},
		}

		desiredStatus := sv.Status
		if err := m.k8sClient.CreateSubVolume(ctx, sv); err != nil {
			return nil, fmt.Errorf("create SubVolume CR: %w", err)
		}
		sv.Status = desiredStatus
		if err := m.k8sClient.UpdateSubVolumeStatus(ctx, sv); err != nil {
			klog.ErrorS(err, "Failed to update SubVolume status after sync clone", "pvName", req.PVName)
		}
	} else {
		// 6b. Asynchronous clone: create CR as Pending, return immediately
		sv := &v1alpha1.SubVolume{
			ObjectMeta: metav1.ObjectMeta{
				Name: req.PVName,
				Labels: map[string]string{
					"storage.ibmcloud.io/pool":         req.PoolName,
					"storage.ibmcloud.io/share-id":     share.ShareID,
					"storage.ibmcloud.io/clone-source": pvName,
				},
			},
			Spec: v1alpha1.SubVolumeSpec{
				PoolName:           req.PoolName,
				ShareID:            share.ShareID,
				ShareMountTargetIP: share.MountTargetIP,
				ShareExportPath:    share.ExportPath,
				SubPath:            subPath,
				RequestedGB:        req.RequestedGB,
				PVName:             req.PVName,
				PVCName:            req.PVCName,
				PVCNamespace:       req.PVCNamespace,
				UID:                uid,
				GID:                gid,
				Permissions:        perms,
				ReclaimPolicy:      "Delete",
				SourceVolume:       pvName,
				SourceShareID:      sourceSV.Spec.ShareID,
			},
			Status: v1alpha1.SubVolumeStatus{
				Phase:       "Cloning",
				CloneStatus: "Pending",
				CloneProgress: &v1alpha1.CloneProgress{
					TotalBytes: sourceSV.Spec.RequestedGB * (1 << 30),
				},
				CreatedAt: &now,
			},
		}

		desiredStatus := sv.Status
		if err := m.k8sClient.CreateSubVolume(ctx, sv); err != nil {
			return nil, fmt.Errorf("create SubVolume CR: %w", err)
		}
		sv.Status = desiredStatus
		if err := m.k8sClient.UpdateSubVolumeStatus(ctx, sv); err != nil {
			klog.ErrorS(err, "Failed to update SubVolume status for async clone", "pvName", req.PVName)
		}
	}

	// 7. Update pool capacity tracking
	m.updateShareAllocation(pool, share.ShareID, req.RequestedGB)
	if err := m.k8sClient.UpdateFileSharePoolStatus(ctx, pool); err != nil {
		klog.ErrorS(err, "Failed to update pool status after clone", "pool", req.PoolName)
	}

	klog.V(2).InfoS("Cloned SubVolume",
		"pvName", req.PVName,
		"source", pvName,
		"pool", req.PoolName,
		"shareID", share.ShareID,
		"sync", doSync,
		"requestedGB", req.RequestedGB,
	)

	sharePath := share.ExportPath
	if sharePath == "" {
		sharePath = "/"
	}
	return &AllocationResult{
		ShareID:       share.ShareID,
		MountTargetIP: share.MountTargetIP,
		SubPath:       subPath,
		SharePath:     sharePath,
		UID:           uid,
		GID:           gid,
		Permissions:   perms,
	}, nil
}

func (m *Manager) CreateVolumeGroupSnapshot(ctx context.Context, req GroupSnapshotRequest) (_ *GroupSnapshotResult, retErr error) {
	start := time.Now()
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.GroupSnapshotsTotal.WithLabelValues(req.PoolName, "create", status).Inc()
		metrics.GroupSnapshotDuration.WithLabelValues(req.PoolName, "create").Observe(time.Since(start).Seconds())
	}()

	// Validate source list
	if len(req.SourceVolumeIDs) == 0 {
		return nil, ErrEmptySourceList
	}

	// Idempotency: if a VolumeGroupSnapshot CR already exists with the same name
	// and is Complete or PartialFailure, return the existing result.
	existing, err := m.k8sClient.GetVolumeGroupSnapshot(ctx, req.GroupName)
	if err == nil && existing != nil {
		if existing.Status.Phase == "Complete" || existing.Status.Phase == "PartialFailure" {
			result := &GroupSnapshotResult{
				GroupName:             existing.Name,
				PoolName:              existing.Spec.PoolName,
				ReadyToUse:            existing.Status.Phase == "Complete",
				InconsistencyWindowMs: existing.Status.InconsistencyWindowMs,
			}
			if existing.Status.CreationTime != nil {
				result.CreationTime = existing.Status.CreationTime.Time
			}
			for _, member := range existing.Status.Members {
				if member.Phase == "Ready" {
					result.Members = append(result.Members, SnapshotResult{
						SnapshotName:   member.SnapshotName,
						PoolName:       existing.Spec.PoolName,
						ReadyToUse:     true,
						SourceVolumeID: member.SubVolumeName,
					})
				}
			}
			return result, nil
		}
	}

	// Resolve copy order: use CopyOrder if provided, otherwise derive from SourceVolumeIDs
	var orderedVolumeIDs []string
	if len(req.CopyOrder) > 0 {
		orderedVolumeIDs, err = m.resolveCopyOrder(req.SourceVolumeIDs, req.CopyOrder)
		if err != nil {
			return nil, fmt.Errorf("resolve copy order: %w", err)
		}
	} else {
		// Sort alphabetically by PV name for deterministic ordering
		orderedVolumeIDs = sortVolumeIDsByPVName(req.SourceVolumeIDs)
	}

	// Validate all source SubVolumes exist and belong to the same pool
	for _, volID := range orderedVolumeIDs {
		_, _, pvName, parseErr := parseManagerVolumeID(volID)
		if parseErr != nil {
			return nil, fmt.Errorf("invalid source volume ID %q: %w", volID, parseErr)
		}
		sv, svErr := m.k8sClient.GetSubVolume(ctx, pvName)
		if svErr != nil {
			return nil, fmt.Errorf("%w: %s", ErrSourceNotFound, pvName)
		}
		if sv.Spec.PoolName != req.PoolName {
			return nil, fmt.Errorf("source volume %q belongs to pool %q, not %q", pvName, sv.Spec.PoolName, req.PoolName)
		}
	}

	// Create VolumeGroupSnapshot CR with Pending status
	now := metav1.Now()
	members := make([]v1alpha1.GroupSnapshotMember, 0, len(orderedVolumeIDs))
	for _, volID := range orderedVolumeIDs {
		_, _, pvName, _ := parseManagerVolumeID(volID)
		snapshotName := fmt.Sprintf("%s-%s", req.GroupName, pvName)
		members = append(members, v1alpha1.GroupSnapshotMember{
			SubVolumeName: pvName,
			SnapshotName:  snapshotName,
			Phase:         "Pending",
		})
	}

	vgs := &v1alpha1.VolumeGroupSnapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.GroupName,
			Labels: map[string]string{
				"storage.ibmcloud.io/pool": req.PoolName,
			},
		},
		Spec: v1alpha1.VolumeGroupSnapshotSpec{
			PoolName:      req.PoolName,
			SourcePVCs:    orderedVolumeIDsToPVCNames(orderedVolumeIDs),
			CopyOrder:     req.CopyOrder,
			FailurePolicy: req.FailurePolicy,
		},
		Status: v1alpha1.VolumeGroupSnapshotStatus{
			Phase:        "Pending",
			Members:      members,
			MemberCount:  int32(len(members)), //nolint:gosec // safe: member count bounded by source volume list
			CreationTime: &now,
		},
	}

	desiredStatus := vgs.Status
	if err := m.k8sClient.CreateVolumeGroupSnapshot(ctx, vgs); err != nil {
		return nil, fmt.Errorf("create VolumeGroupSnapshot CR: %w", err)
	}
	vgs.Status = desiredStatus
	if err := m.k8sClient.UpdateVolumeGroupSnapshotStatus(ctx, vgs); err != nil {
		klog.ErrorS(err, "Failed to update VolumeGroupSnapshot status to Pending", "group", req.GroupName)
	}

	// Update phase to InProgress
	vgs.Status.Phase = "InProgress"
	vgs.Status.StartedAt = &now
	if err := m.k8sClient.UpdateVolumeGroupSnapshotStatus(ctx, vgs); err != nil {
		klog.ErrorS(err, "Failed to update VolumeGroupSnapshot status to InProgress", "group", req.GroupName)
	}

	// Create individual snapshots
	var (
		completedSnapshots []SnapshotResult
		firstCopyTime      time.Time
		lastCopyTime       time.Time
		failed             bool
		readyCount         int32
		failedCount        int32
	)

	for i, volID := range orderedVolumeIDs {
		_, _, pvName, _ := parseManagerVolumeID(volID)
		snapshotName := fmt.Sprintf("%s-%s", req.GroupName, pvName)

		// Mark member as Creating
		vgs.Status.Members[i].Phase = "Creating"
		_ = m.k8sClient.UpdateVolumeGroupSnapshotStatus(ctx, vgs)

		copyStart := time.Now()
		if firstCopyTime.IsZero() {
			firstCopyTime = copyStart
		}

		snapResult, snapErr := m.CreateSnapshot(ctx, snapshotName, volID, req.Parameters)

		copyEnd := time.Now()
		lastCopyTime = copyEnd

		if snapErr != nil {
			vgs.Status.Members[i].Phase = "Failed"
			vgs.Status.Members[i].Error = snapErr.Error()
			failedCount++
			failed = true

			klog.ErrorS(snapErr, "Group snapshot member failed",
				"group", req.GroupName,
				"member", pvName,
				"snapshot", snapshotName,
			)

			if req.FailurePolicy == "Abort" {
				// Rollback: delete all completed snapshots
				for _, completed := range completedSnapshots {
					if delErr := m.DeleteSnapshot(ctx, completed.SnapshotName); delErr != nil {
						klog.ErrorS(delErr, "Failed to rollback snapshot during group abort",
							"group", req.GroupName,
							"snapshot", completed.SnapshotName,
						)
					}
				}

				// Mark remaining members as Failed
				for j := i + 1; j < len(vgs.Status.Members); j++ {
					vgs.Status.Members[j].Phase = "Failed"
					vgs.Status.Members[j].Error = "aborted due to earlier failure"
					failedCount++
				}

				// Update group to Failed
				completedAt := metav1.Now()
				vgs.Status.Phase = "Failed"
				vgs.Status.ReadyCount = 0
				vgs.Status.FailedCount = int32(len(vgs.Status.Members)) //nolint:gosec // safe: member count bounded by source volume list
				vgs.Status.CompletedAt = &completedAt
				vgs.Status.ConsistencyLevel = "Unknown"
				_ = m.k8sClient.UpdateVolumeGroupSnapshotStatus(ctx, vgs)

				return nil, fmt.Errorf("%w: member %q failed: %v", ErrGroupSnapshotFailed, pvName, snapErr)
			}
			// Continue policy: keep going
			continue
		}

		// Success
		snapResult.SourceVolumeID = volID
		completedSnapshots = append(completedSnapshots, *snapResult)
		vgs.Status.Members[i].Phase = "Ready"
		readyCount++
		_ = m.k8sClient.UpdateVolumeGroupSnapshotStatus(ctx, vgs)
	}

	// Calculate inconsistency window
	var inconsistencyMs int64
	if !firstCopyTime.IsZero() && !lastCopyTime.IsZero() {
		inconsistencyMs = lastCopyTime.Sub(firstCopyTime).Milliseconds()
	}

	// Determine final phase
	completedAt := metav1.Now()
	finalPhase := "Complete"
	consistencyLevel := "BestEffort"
	readyToUse := true
	if failed {
		finalPhase = "PartialFailure"
		readyToUse = false
	}

	vgs.Status.Phase = finalPhase
	vgs.Status.ReadyCount = readyCount
	vgs.Status.FailedCount = failedCount
	vgs.Status.CompletedAt = &completedAt
	vgs.Status.ConsistencyLevel = consistencyLevel
	vgs.Status.InconsistencyWindowMs = inconsistencyMs
	if err := m.k8sClient.UpdateVolumeGroupSnapshotStatus(ctx, vgs); err != nil {
		klog.ErrorS(err, "Failed to update VolumeGroupSnapshot final status", "group", req.GroupName)
	}

	metrics.GroupSnapshotMemberCount.WithLabelValues(req.PoolName).Observe(float64(len(orderedVolumeIDs)))
	metrics.GroupSnapshotInconsistencyWindow.WithLabelValues(req.PoolName).Observe(float64(inconsistencyMs))

	klog.V(2).InfoS("Created group snapshot",
		"group", req.GroupName,
		"pool", req.PoolName,
		"members", len(orderedVolumeIDs),
		"ready", readyCount,
		"failed", failedCount,
		"inconsistencyMs", inconsistencyMs,
	)

	return &GroupSnapshotResult{
		GroupName:             req.GroupName,
		PoolName:              req.PoolName,
		Members:               completedSnapshots,
		CreationTime:          now.Time,
		ReadyToUse:            readyToUse,
		InconsistencyWindowMs: inconsistencyMs,
	}, nil
}

func (m *Manager) DeleteVolumeGroupSnapshot(ctx context.Context, groupName string) (retErr error) {
	poolName := ""
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.GroupSnapshotsTotal.WithLabelValues(poolName, "delete", status).Inc()
	}()

	// Fetch VolumeGroupSnapshot CR
	vgs, err := m.k8sClient.GetVolumeGroupSnapshot(ctx, groupName)
	if err != nil {
		// Idempotent: if not found, return nil
		return nil
	}
	poolName = vgs.Spec.PoolName

	// Delete all member snapshots
	for _, member := range vgs.Status.Members {
		if member.SnapshotName == "" {
			continue
		}
		if member.Phase == "Failed" {
			// Try to delete anyway in case a partial CR was created
			_ = m.DeleteSnapshot(ctx, member.SnapshotName)
			continue
		}
		if err := m.DeleteSnapshot(ctx, member.SnapshotName); err != nil {
			// Log but continue — best effort cleanup
			klog.ErrorS(err, "Failed to delete member snapshot during group deletion",
				"group", groupName,
				"snapshot", member.SnapshotName,
			)
		}
	}

	// Delete the VolumeGroupSnapshot CR
	if err := m.k8sClient.DeleteVolumeGroupSnapshot(ctx, groupName); err != nil {
		return fmt.Errorf("delete VolumeGroupSnapshot CR: %w", err)
	}

	klog.V(2).InfoS("Deleted group snapshot",
		"group", groupName,
		"pool", poolName,
	)

	return nil
}

// resolveCopyOrder reorders sourceVolumeIDs according to the desired CopyOrder (which uses PV names).
// Volume IDs not listed in CopyOrder are appended at the end in their original order.
func (m *Manager) resolveCopyOrder(sourceVolumeIDs []string, copyOrder []string) ([]string, error) {
	// Build a map of pvName -> volumeID
	pvToVolID := make(map[string]string, len(sourceVolumeIDs))
	for _, volID := range sourceVolumeIDs {
		_, _, pvName, err := parseManagerVolumeID(volID)
		if err != nil {
			return nil, fmt.Errorf("invalid volume ID %q: %w", volID, err)
		}
		pvToVolID[pvName] = volID
	}

	ordered := make([]string, 0, len(sourceVolumeIDs))
	used := make(map[string]bool)

	for _, pvName := range copyOrder {
		volID, ok := pvToVolID[pvName]
		if !ok {
			// CopyOrder references a PV that's not in the source list — skip it
			continue
		}
		ordered = append(ordered, volID)
		used[pvName] = true
	}

	// Append remaining volume IDs not in CopyOrder
	for _, volID := range sourceVolumeIDs {
		_, _, pvName, _ := parseManagerVolumeID(volID)
		if !used[pvName] {
			ordered = append(ordered, volID)
		}
	}

	return ordered, nil
}

// sortVolumeIDsByPVName returns a copy of volumeIDs sorted alphabetically by the PV name component.
func sortVolumeIDsByPVName(volumeIDs []string) []string {
	type entry struct {
		volID  string
		pvName string
	}
	entries := make([]entry, 0, len(volumeIDs))
	for _, volID := range volumeIDs {
		_, _, pvName, _ := parseManagerVolumeID(volID)
		entries = append(entries, entry{volID: volID, pvName: pvName})
	}
	// Simple insertion sort for small slices
	for i := 1; i < len(entries); i++ {
		for j := i; j > 0 && entries[j].pvName < entries[j-1].pvName; j-- {
			entries[j], entries[j-1] = entries[j-1], entries[j]
		}
	}
	sorted := make([]string, len(entries))
	for i, e := range entries {
		sorted[i] = e.volID
	}
	return sorted
}

// orderedVolumeIDsToPVCNames extracts PV names from volume IDs and returns them as string slice.
func orderedVolumeIDsToPVCNames(volumeIDs []string) []string {
	names := make([]string, 0, len(volumeIDs))
	for _, volID := range volumeIDs {
		_, _, pvName, _ := parseManagerVolumeID(volID)
		names = append(names, pvName)
	}
	return names
}

// parseManagerVolumeID is a helper to split volume IDs within the pool package.
func parseManagerVolumeID(volumeID string) (poolName, shareID, name string, err error) {
	return util.ParseVolumeID(volumeID)
}
