package pool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
)

// Sentinel errors for pool operations.
var (
	ErrPoolNotFound              = errors.New("pool not found")
	ErrPoolExhausted             = errors.New("pool has no available capacity")
	ErrPoolExpanding             = errors.New("pool is expanding, retry shortly")
	ErrShareCreationPending      = errors.New("share creation is in progress")
	ErrNoUsableShare             = errors.New("pool has shares without a usable mount target")
	ErrSubVolumeNotFound         = errors.New("subvolume not found")
	ErrInsufficientShareCapacity = errors.New("share does not have enough remaining capacity")
	ErrSnapshotNotFound          = errors.New("snapshot not found")
	ErrSnapshotAlreadyExists     = errors.New("snapshot already exists")
	ErrSourceNotFound            = errors.New("source volume not found")
	ErrGroupSnapshotNotFound     = errors.New("group snapshot not found")
	ErrGroupSnapshotFailed       = errors.New("group snapshot failed")
	ErrEmptySourceList           = errors.New("source volume list is empty")
	ErrOperationInProgress       = errors.New("operation in progress, retry shortly")
	ErrRestoreTooSmall           = errors.New("the new volume is smaller than the snapshot")

	errShareDataNotSet = errors.New("share data operations are not configured")
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

// restoreSnapshotLabel names the snapshot a SubVolume is being restored from.
const restoreSnapshotLabel = "storage.ibmcloud.io/restore-snapshot"

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
	// The SubVolume CR is created with cloneStatus=Pending, and the clone
	// worker copies the data in a Job, because the controller pod has no NFS
	// access. The node agent will not mount the volume until the copy is done.
	CloneVolume(ctx context.Context, sourceVolumeID string, req AllocationRequest) (*AllocationResult, error)

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
	// RestorePending makes the volume held back from pods from the start,
	// because a BackupRestore is about to fill it.
	RestorePending bool
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
	mu        sync.Mutex
	k8sClient k8s.Client
	vpcClient ibmcloud.VPCFileClient
	nfsOps    NFSOperations
	shareData ShareData
	vm        *vmSnapshots
	instant   *instantSnapshots
	vmNow     func() time.Time
	// volumes serializes the changes to one volume; see volumeLocks.
	volumes volumeLocks
	// vmMu serializes the steps of the VM snapshot loop with the delete of a
	// snapshot, without holding mu while commands run in a VM pod. Lock order:
	// a volume lock, then mu, then vmMu. The loop never takes mu while it
	// holds vmMu.
	vmMu                 sync.Mutex
	stagingBasePath      string
	defaultResourceGroup string
	network              *ShareNetwork
}

// Verify Manager implements PoolManager at compile time.
var _ PoolManager = (*Manager)(nil)

// SetDefaultResourceGroup sets the fallback resource group used when the pool spec doesn't specify one.
// SetShareData sets how the manager removes and copies folders on shares.
func (m *Manager) SetShareData(sd ShareData) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.shareData = sd
}

func (m *Manager) SetDefaultResourceGroup(rg string) {
	m.defaultResourceGroup = rg
}

// SetShareNetwork sets the cluster network that auto-expanded shares join.
func (m *Manager) SetShareNetwork(network *ShareNetwork) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.network = network
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
			// Persist pool status even on error — tryAutoExpand may have appended
			// a new share (state=creating) that the reconciler needs to see in order
			// to create a mount target and promote the share to stable.
			if updateErr := m.k8sClient.UpdateFileSharePoolStatus(ctx, pool); updateErr != nil {
				klog.ErrorS(updateErr, "Failed to update pool status after auto-expand", "pool", req.PoolName)
			}
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
	labels := map[string]string{
		"storage.ibmcloud.io/pool":     req.PoolName,
		"storage.ibmcloud.io/share-id": share.ShareID,
	}
	if req.RestorePending {
		labels[RestorePendingLabel] = "true"
	}
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name:   req.PVName,
			Labels: labels,
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

	// A volume is not deleted while one of its snapshots changes it.
	defer m.volumes.lock(subVolumeName)()
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

	// 3. Remove the volume folder. The SubVolume CR and its capacity stay
	// until the folder is gone.
	if m.shareData == nil {
		return errShareDataNotSet
	}
	if copyKey := dataCopyKey(sv); copyKey != "" {
		stopped, stopErr := m.shareData.Stop(ctx, copyKey)
		if stopErr != nil {
			return fmt.Errorf("stop copy into volume: %w", stopErr)
		}
		if !stopped {
			return ErrOperationInProgress
		}
	}
	// The folder of a VM disk with instant snapshots holds the snapshots'
	// files. It stays until the last snapshot is deleted.
	keep, err := m.hasInstantSnapshots(ctx, sv.Name)
	if err != nil {
		return err
	}
	if keep {
		klog.V(2).InfoS("Keeping the folder of a deleted volume for its instant snapshots", "subVolume", sv.Name, "subPath", sv.Spec.SubPath)
	} else {
		done, rmErr := m.shareData.RemoveDir(ctx, "rm-"+sv.Name, subVolumeShareMount(sv), sv.Spec.SubPath)
		if rmErr != nil {
			klog.ErrorS(rmErr, "Failed to remove subdirectory, retaining SubVolume CR", "subPath", sv.Spec.SubPath)
			return fmt.Errorf("remove subdirectory: %w", rmErr)
		}
		if !done {
			return ErrOperationInProgress
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

	_, sizeGB, _, maxShares, _, err := pool.Spec.TierConfig(tier)
	if err != nil {
		return nil, err
	}

	// Grow only when the ready shares are full. A share that is still being
	// created, or that has no mount target, will add capacity without a new
	// share; creating more would only add cost.
	for _, s := range pool.Status.Shares {
		if s.Tier == tier && s.State == "creating" {
			return nil, ErrPoolExpanding
		}
	}
	if ids := sharesWithoutMountTarget(pool.Status.Shares, tier); len(ids) > 0 {
		return nil, fmt.Errorf("%w: %s", ErrNoUsableShare, strings.Join(ids, ", "))
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

	input, err := newShareInput(ctx, m.vpcClient, m.network, pool, tier, m.defaultResourceGroup)
	if err != nil {
		return nil, err
	}

	shareInfo, err := m.vpcClient.CreateFileShare(ctx, input)
	if err != nil {
		return nil, fmt.Errorf("create VPC share: %w", err)
	}

	newShare := newPoolShareStatus(shareInfo, tier)
	pool.Status.Shares = append(pool.Status.Shares, newShare)
	pool.Status.ShareCount = int32(len(pool.Status.Shares)) //nolint:gosec // safe: MaxShares capped at 100
	pool.Status.TotalCapacityGB += shareInfo.SizeGB

	// If the share is not ready, don't return it for immediate allocation.
	// Return ErrPoolExpanding so the CSI provisioner retries. The reconciler's
	// health check fills in the mount target once the VPC reports it.
	if !shareReady(newShare) {
		klog.V(2).InfoS("New share is not ready yet", "shareID", shareInfo.ID, "shareName", shareInfo.Name)
		return nil, ErrPoolExpanding
	}

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

// chargeShare adds sizeGB (or, when negative, gives it back) to a share's
// AllocatedGB for a snapshot. A snapshot uses space but is not a PVC.
func (m *Manager) chargeShare(pool *v1alpha1.FileSharePool, shareID string, sizeGB int64) {
	for i := range pool.Status.Shares {
		if pool.Status.Shares[i].ShareID == shareID {
			pool.Status.Shares[i].AllocatedGB += sizeGB
			break
		}
	}
	pool.Status.TotalAllocatedGB += sizeGB
}

// bookShare charges sizeGB to a share for a snapshot, on a fresh copy of the
// pool and under the Manager's lock, so it never overwrites another call's
// bookkeeping. It refuses a charge that the share has no room for.
func (m *Manager) bookShare(ctx context.Context, poolName, shareID string, sizeGB int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	pool, err := m.k8sClient.GetFileSharePool(ctx, poolName)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrPoolNotFound, poolName)
	}
	if err = checkShareRoom(pool, shareID, sizeGB); err != nil {
		return err
	}
	m.chargeShare(pool, shareID, sizeGB)
	if err = m.k8sClient.UpdateFileSharePoolStatus(ctx, pool); err != nil {
		return fmt.Errorf("record the snapshot's space on pool %s: %w", poolName, err)
	}
	return nil
}

// unbookShare gives a snapshot's charge back, like bookShare. A failure is
// logged: the pool's next recount corrects the share.
func (m *Manager) unbookShare(ctx context.Context, poolName, shareID string, sizeGB int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	pool, err := m.k8sClient.GetFileSharePool(ctx, poolName)
	if err == nil {
		m.chargeShare(pool, shareID, -sizeGB)
		err = m.k8sClient.UpdateFileSharePoolStatus(ctx, pool)
	}
	if err != nil {
		klog.ErrorS(err, "Failed to give a snapshot's space back; the next recount corrects it", "pool", poolName, "shareID", shareID)
	}
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

	// Parse sourceVolumeID to get pvName, and hold that volume's lock: a
	// snapshot may wait long on a VM pod, so it must not hold the Manager's
	// lock, but no other change to the same disk may run beside it.
	_, _, pvName, err := parseManagerVolumeID(sourceVolumeID)
	if err != nil {
		return nil, fmt.Errorf("invalid source volume ID: %w", err)
	}
	defer m.volumes.lock(pvName)()

	// 1. Idempotency check
	existing, err := m.k8sClient.GetSnapshot(ctx, snapshotName)
	if err == nil && existing != nil {
		poolName = existing.Spec.PoolName
		if existing.Status.Phase == "Failed" {
			return nil, fmt.Errorf("%w: snapshot %s: %s", ErrSnapshotFailed, snapshotName, existing.Status.Message)
		}
		if !existing.Status.ReadyToUse && existing.Annotations[snapshotMethodAnnotation] == snapshotMethodInstant {
			src, svErr := m.k8sClient.GetSubVolume(ctx, existing.Spec.SourceSubVolume)
			if svErr != nil {
				return nil, fmt.Errorf("%w: %s", ErrSourceNotFound, existing.Spec.SourceSubVolume)
			}
			if takeErr := m.takeInstant(ctx, existing, src); takeErr != nil {
				return nil, takeErr
			}
			return snapshotResult(existing), nil
		}
		if !existing.Status.ReadyToUse && existing.Annotations[snapshotMethodAnnotation] != snapshotMethodVM {
			pool, poolErr := m.k8sClient.GetFileSharePool(ctx, poolName)
			if poolErr != nil {
				return nil, fmt.Errorf("%w: %s", ErrPoolNotFound, poolName)
			}
			if copyErr := m.copySnapshotData(ctx, existing, pool); copyErr != nil {
				return nil, copyErr
			}
		}
		return snapshotResult(existing), nil
	}

	speed, err := snapshotSpeed(params)
	if err != nil {
		return nil, err
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

	// A disk that a running VM writes to is copied with QEMU. Check before
	// anything is created, so a failed check leaves nothing behind.
	method := snapshotMethodFile
	methodReason := ""
	if m.instant != nil {
		plan, reason, planErr := m.planInstant(ctx, sv)
		if planErr != nil {
			return nil, planErr
		}
		if plan != nil {
			method = snapshotMethodInstant
		}
		methodReason = reason
	}
	if method != snapshotMethodInstant && len(sv.Status.DiskChain) > 0 {
		return nil, fmt.Errorf("%w: %s/%s has instant snapshots, and only an instant snapshot can be taken of it (%s); turn the KubeVirt Sidecar feature gate on, or delete its snapshots first",
			ErrSnapshotUnsupported, sv.Spec.PVCNamespace, sv.Spec.PVCName, orDefault(methodReason, "no VM uses it"))
	}
	if method == snapshotMethodFile && m.vm != nil {
		pod, _, vmErr := findVMPodForClaim(ctx, m.vm.kube, sv.Spec.PVCNamespace, sv.Spec.PVCName)
		if vmErr != nil {
			return nil, fmt.Errorf("check for a running VM: %w", vmErr)
		}
		if pod != nil {
			if pod.Labels[kubevirtPodLabel] != "virt-launcher" {
				return nil, fmt.Errorf("%w: %s/%s is a hot-plugged disk of a running VM; stop the VM or detach the disk, because a file copy would not be safe and QEMU cannot copy a hot-plugged disk yet",
					ErrSnapshotUnsupported, sv.Spec.PVCNamespace, sv.Spec.PVCName)
			}
			method = snapshotMethodVM
		}
	}

	// 6. Create the Snapshot CR, not ready until its copy has finished
	if m.shareData == nil {
		return nil, errShareDataNotSet
	}
	now := metav1.Now()
	annotations := map[string]string{snapshotMethodAnnotation: method}
	if speed != "" {
		annotations[snapshotSpeedAnnotation] = speed
	}
	snap := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{
			Name:        snapshotName,
			Annotations: annotations,
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
			Phase:        "Creating",
			ReadyToUse:   false,
			SizeBytes:    sv.Spec.RequestedGB * (1 << 30),
			CreationTime: &now,
			MethodReason: methodReason,
		},
	}

	desiredStatus := snap.Status

	// 7. Charge the snapshot's space to its source's share. A share with no
	// room refuses it, so the share is never over-committed.
	if err := m.bookShare(ctx, poolName, sv.Spec.ShareID, snapshotChargeGB(snap)); err != nil {
		return nil, err
	}

	if err := m.k8sClient.CreateSnapshot(ctx, snap); err != nil {
		m.unbookShare(ctx, poolName, sv.Spec.ShareID, snapshotChargeGB(snap))
		return nil, fmt.Errorf("create Snapshot CR: %w", err)
	}

	snap.Status = desiredStatus
	if err := m.k8sClient.UpdateSnapshotStatus(ctx, snap); err != nil {
		klog.ErrorS(err, "Failed to update Snapshot status", "snapshot", snapshotName)
	}

	// 8. Copy the volume folder into the snapshot folder. A VM disk is
	// copied by AdvanceVMSnapshots instead, and an instant snapshot copies
	// nothing.
	if method == snapshotMethodInstant {
		if err := m.takeInstant(ctx, snap, sv); err != nil {
			return nil, err
		}
	}
	if method == snapshotMethodFile {
		if err := m.copySnapshotData(ctx, snap, pool); err != nil {
			return nil, err
		}
	}

	klog.V(2).InfoS("Created snapshot",
		"snapshot", snapshotName,
		"source", pvName,
		"pool", poolName,
		"shareID", sv.Spec.ShareID,
		"readyToUse", snap.Status.ReadyToUse,
	)

	return snapshotResult(snap), nil
}

// copySnapshotData starts or checks the copy of a snapshot's source folder.
// When the copy has finished it marks the Snapshot CR ready. When the copy
// Job has failed (it removes its partial folder), it deletes the CR and gives
// back its capacity, so the next CreateSnapshot call starts again. Other
// errors leave everything as it is.
func (m *Manager) copySnapshotData(ctx context.Context, snap *v1alpha1.Snapshot, pool *v1alpha1.FileSharePool) error {
	if m.shareData == nil {
		return errShareDataNotSet
	}
	// A VM that started since the copy began may write to the disk while it
	// is read. Do not go on with the file copy then.
	if handled, vmErr := m.switchToVMPath(ctx, snap); vmErr != nil {
		return vmErr
	} else if handled {
		return nil
	}
	share, err := poolShareMount(pool, snap.Spec.ShareID)
	if err != nil {
		return err
	}
	done, err := m.shareData.CopyDir(ctx, "snap-"+snap.Name, share, snap.Spec.SourceSubPath, share, snap.Spec.SnapshotPath, nil)
	if err != nil && !errors.Is(err, ErrShareJobFailed) {
		return fmt.Errorf("copy snapshot: %w", err)
	}
	if err != nil {
		klog.ErrorS(err, "Snapshot copy failed, discarding snapshot", "snapshot", snap.Name)
		if delErr := m.k8sClient.DeleteSnapshot(ctx, snap.Name); delErr != nil {
			return fmt.Errorf("copy snapshot: %w (and deleting the Snapshot CR failed: %v)", err, delErr)
		}
		m.unbookShare(ctx, pool.Name, snap.Spec.ShareID, snapshotChargeGB(snap))
		return fmt.Errorf("copy snapshot: %w", err)
	}
	if !done {
		return nil
	}
	// A VM that started during the copy may have written to the disk while it
	// was read. Then the folder is not a snapshot of one moment.
	if handled, vmErr := m.switchToVMPath(ctx, snap); vmErr != nil {
		return vmErr
	} else if handled {
		return nil
	}
	snap.Status.Phase = "Ready"
	snap.Status.ReadyToUse = true
	if err := m.k8sClient.UpdateSnapshotStatus(ctx, snap); err != nil {
		return fmt.Errorf("mark snapshot ready: %w", err)
	}
	return nil
}

func snapshotResult(snap *v1alpha1.Snapshot) *SnapshotResult {
	var creationTime time.Time
	if snap.Status.CreationTime != nil {
		creationTime = snap.Status.CreationTime.Time
	}
	return &SnapshotResult{
		SnapshotName:  snap.Name,
		PoolName:      snap.Spec.PoolName,
		ShareID:       snap.Spec.ShareID,
		SnapshotPath:  snap.Spec.SnapshotPath,
		SourceSubPath: snap.Spec.SourceSubPath,
		SizeBytes:     snap.Spec.SizeGB * (1 << 30),
		CreationTime:  creationTime,
		ReadyToUse:    snap.Status.ReadyToUse,
	}
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

	// 1. Fetch Snapshot CR, then hold its volume's lock and read it again.
	snap, err := m.k8sClient.GetSnapshot(ctx, snapshotName)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapshotName)
	}
	defer m.volumes.lock(snap.Spec.SourceSubVolume)()
	if snap, err = m.k8sClient.GetSnapshot(ctx, snapshotName); err != nil {
		return fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapshotName)
	}
	poolName = snap.Spec.PoolName

	// A pool deleted by an older driver did not wait for its snapshots, and
	// took their data with it. Only the record is left to remove.
	if _, err = m.k8sClient.GetFileSharePool(ctx, poolName); apierrors.IsNotFound(err) {
		if err = m.k8sClient.DeleteSnapshot(ctx, snapshotName); err != nil {
			return fmt.Errorf("delete Snapshot CR: %w", err)
		}
		klog.InfoS("Removed the record of a snapshot whose pool is gone", "snapshot", snapshotName, "pool", poolName)
		return nil
	}

	if snap.Annotations[snapshotMethodAnnotation] == snapshotMethodInstant {
		return m.deleteInstantSnapshot(ctx, snap)
	}

	// Mark the snapshot first, so the copy loop leaves it alone, then cancel a
	// QEMU copy in progress. A copy that cannot be cancelled keeps its record
	// and the delete is tried again.
	if err = m.cancelVMSnapshotForDelete(ctx, snap); err != nil {
		return err
	}

	// 2. Remove the snapshot folder. The CR and its capacity stay until
	// the folder is gone.
	if m.shareData == nil {
		return errShareDataNotSet
	}
	pool, err := m.k8sClient.GetFileSharePool(ctx, poolName)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrPoolNotFound, poolName)
	}
	share, err := poolShareMount(pool, snap.Spec.ShareID)
	if err != nil {
		return err
	}
	for _, key := range []string{"snap-" + snapshotName, "snapvm-" + snapshotName} {
		stopped, stopErr := m.shareData.Stop(ctx, key)
		if stopErr != nil {
			return fmt.Errorf("stop snapshot copy: %w", stopErr)
		}
		if !stopped {
			return ErrOperationInProgress
		}
	}
	done, err := m.shareData.RemoveDir(ctx, "rmsnap-"+snapshotName, share, snap.Spec.SnapshotPath)
	if err != nil {
		return fmt.Errorf("remove snapshot dir: %w", err)
	}
	if !done {
		return ErrOperationInProgress
	}

	// 3. Delete Snapshot CR
	if err = m.k8sClient.DeleteSnapshot(ctx, snapshotName); err != nil {
		return fmt.Errorf("delete Snapshot CR: %w", err)
	}

	// 4. Give the snapshot's space back
	m.unbookShare(ctx, poolName, snap.Spec.ShareID, snapshotChargeGB(snap))

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

	// Hold the new volume's lock for the whole call. The Manager's lock is
	// only held while a share is picked and charged; the copy runs without it.
	defer m.volumes.lock(req.PVName)()

	// 1. Idempotency check
	existing, err := m.k8sClient.GetSubVolume(ctx, req.PVName)
	if err == nil && existing != nil {
		klog.V(2).InfoS("Idempotent restore: SubVolume already exists", "pvName", req.PVName)
		if existing.Status.Phase != "Bound" && existing.Labels[restoreSnapshotLabel] != "" {
			snap, snapErr := m.k8sClient.GetSnapshot(ctx, existing.Labels[restoreSnapshotLabel])
			if snapErr != nil {
				return nil, fmt.Errorf("%w: %s", ErrSnapshotNotFound, existing.Labels[restoreSnapshotLabel])
			}
			if copyErr := m.copyRestoreLocked(ctx, existing, snap); copyErr != nil {
				return nil, copyErr
			}
		}
		return subVolumeAllocationResult(existing), nil
	}

	// 2. Fetch Snapshot CR
	snap, err := m.k8sClient.GetSnapshot(ctx, snapshotName)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrSnapshotNotFound, snapshotName)
	}
	if !snap.Status.ReadyToUse {
		return nil, fmt.Errorf("snapshot %q is not ready to use (phase: %s)", snapshotName, snap.Status.Phase)
	}
	if req.RequestedGB < snap.Spec.SizeGB {
		return nil, fmt.Errorf("%w: snapshot %q holds a %d GB volume; the new volume asks for %d GB",
			ErrRestoreTooSmall, snapshotName, snap.Spec.SizeGB, req.RequestedGB)
	}

	sv, err := m.reserveRestore(ctx, snapshotName, req)
	if err != nil {
		return nil, err
	}

	// 6. Copy the snapshot folder into the new volume folder
	if err := m.copyRestoreLocked(ctx, sv, snap); err != nil {
		return nil, err
	}

	klog.V(2).InfoS("Restored snapshot",
		"snapshot", snapshotName,
		"pvName", req.PVName,
		"pool", req.PoolName,
		"shareID", sv.Spec.ShareID,
	)

	return subVolumeAllocationResult(sv), nil
}

// copyRestoreLocked runs a restore's copy step while it holds the lock of the
// snapshot's volume, so no merge or snapshot of that disk runs beside it.
func (m *Manager) copyRestoreLocked(ctx context.Context, sv *v1alpha1.SubVolume, snap *v1alpha1.Snapshot) error {
	defer m.volumes.lock(snap.Spec.SourceSubVolume)()
	return m.copyRestoreData(ctx, sv, snap)
}

// reserveRestore picks a share for a restored volume, records its SubVolume
// in phase Creating and charges the share, under the Manager's lock.
func (m *Manager) reserveRestore(ctx context.Context, snapshotName string, req AllocationRequest) (*v1alpha1.SubVolume, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

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

	if m.shareData == nil {
		return nil, errShareDataNotSet
	}

	// 4. Create the SubVolume CR in phase Creating. It holds the share
	// reservation while the snapshot is copied in.
	subPath := fmt.Sprintf("/pvcs/%s", req.PVName)
	uid, gid, perms := m.resolvePermissions(req, pool)
	now := metav1.Now()
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: req.PVName,
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":     req.PoolName,
				"storage.ibmcloud.io/share-id": share.ShareID,
				restoreSnapshotLabel:           snapshotName,
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
			Phase:     "Creating",
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

	// 5. Update pool status
	m.updateShareAllocation(pool, share.ShareID, req.RequestedGB)
	if err := m.k8sClient.UpdateFileSharePoolStatus(ctx, pool); err != nil {
		klog.ErrorS(err, "Failed to update pool status after restore", "pool", req.PoolName)
	}

	return sv, nil
}

// copyRestoreData starts or checks the copy of a snapshot into a restored
// volume. It returns ErrOperationInProgress until the copy has finished, then
// moves the SubVolume to Bound. A failed copy keeps the SubVolume, so the
// next call starts a new copy.
func (m *Manager) copyRestoreData(ctx context.Context, sv *v1alpha1.SubVolume, snap *v1alpha1.Snapshot) error {
	if m.shareData == nil {
		return errShareDataNotSet
	}
	snapPool, err := m.k8sClient.GetFileSharePool(ctx, snap.Spec.PoolName)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrPoolNotFound, snap.Spec.PoolName)
	}
	src, err := poolShareMount(snapPool, snap.Spec.ShareID)
	if err != nil {
		return err
	}
	var done bool
	if snap.Annotations[snapshotMethodAnnotation] == snapshotMethodInstant {
		done, err = m.restoreInstant(ctx, sv, snap, src)
	} else {
		owner := &Ownership{UID: sv.Spec.UID, GID: sv.Spec.GID, Permissions: sv.Spec.Permissions}
		done, err = m.shareData.CopyDir(ctx, "restore-"+sv.Name, src, snap.Spec.SnapshotPath, subVolumeShareMount(sv), sv.Spec.SubPath, owner)
	}
	if err != nil {
		return fmt.Errorf("restore snapshot: %w", err)
	}
	if !done {
		return ErrOperationInProgress
	}
	sv.Status.Phase = "Bound"
	if err := m.k8sClient.UpdateSubVolumeStatus(ctx, sv); err != nil {
		return fmt.Errorf("mark restored volume bound: %w", err)
	}
	return nil
}

func subVolumeAllocationResult(sv *v1alpha1.SubVolume) *AllocationResult {
	sharePath := sv.Spec.ShareExportPath
	if sharePath == "" {
		sharePath = "/"
	}
	return &AllocationResult{
		ShareID:       sv.Spec.ShareID,
		MountTargetIP: sv.Spec.ShareMountTargetIP,
		SubPath:       sv.Spec.SubPath,
		SharePath:     sharePath,
		UID:           sv.Spec.UID,
		GID:           sv.Spec.GID,
		Permissions:   sv.Spec.Permissions,
	}
}

// dataCopyKey names the share data Job that copies data into a restored or
// cloned SubVolume, or returns "" for a plain volume.
func dataCopyKey(sv *v1alpha1.SubVolume) string {
	switch {
	case sv.Labels[restoreSnapshotLabel] != "":
		return "restore-" + sv.Name
	case sv.Spec.SourceVolume != "":
		return cloneCopyKey(sv.Name)
	}
	return ""
}

// subVolumeShareMount returns the NFS export that holds a SubVolume.
func subVolumeShareMount(sv *v1alpha1.SubVolume) ShareMount {
	return ShareMount{MountTargetIP: sv.Spec.ShareMountTargetIP, ExportPath: sv.Spec.ShareExportPath}
}

// poolShareMount returns the NFS export of one share of a pool.
func poolShareMount(pool *v1alpha1.FileSharePool, shareID string) (ShareMount, error) {
	for _, sh := range pool.Status.Shares {
		if sh.ShareID == shareID {
			return ShareMount{MountTargetIP: sh.MountTargetIP, ExportPath: sh.ExportPath}, nil
		}
	}
	return ShareMount{}, fmt.Errorf("share %s not found in pool %s", shareID, pool.Name)
}

func (m *Manager) CloneVolume(ctx context.Context, sourceVolumeID string, req AllocationRequest) (_ *AllocationResult, retErr error) {
	defer func() {
		status := "success"
		if retErr != nil {
			status = "error"
		}
		metrics.ClonesTotal.WithLabelValues(req.PoolName, status).Inc()
	}()

	// A clone of a disk must not overlap a snapshot or merge of that disk.
	if _, _, src, parseErr := parseManagerVolumeID(sourceVolumeID); parseErr == nil {
		defer m.volumes.lock(src)()
	}
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
	if err == nil && len(sourceSV.Status.DiskChain) > 0 {
		return nil, fmt.Errorf("%w: %s/%s has instant snapshots, so a copy of its folder is not a clone; restore one of its snapshots instead",
			ErrSnapshotUnsupported, sourceSV.Spec.PVCNamespace, sourceSV.Spec.PVCName)
	}
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

	// 6. Create the SubVolume CR as Pending. The clone worker copies the data
	// in a Job, because the controller pod has no NFS access.
	subPath := fmt.Sprintf("/pvcs/%s", req.PVName)
	uid, gid, perms := m.resolvePermissions(req, pool)
	now := metav1.Now()

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
		if existing.Status.Phase == "InProgress" {
			return m.refreshGroupSnapshot(ctx, existing, req)
		}
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
		readySnapshots     []SnapshotResult
		pendingCount       int32
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

		// Created; ready once its copy has finished
		snapResult.SourceVolumeID = volID
		completedSnapshots = append(completedSnapshots, *snapResult)
		if snapResult.ReadyToUse {
			readySnapshots = append(readySnapshots, *snapResult)
			vgs.Status.Members[i].Phase = "Ready"
			readyCount++
		} else {
			pendingCount++
		}
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
	switch {
	case pendingCount > 0:
		finalPhase = "InProgress"
		readyToUse = false
	case failed:
		finalPhase = "PartialFailure"
		readyToUse = false
	}

	vgs.Status.Phase = finalPhase
	vgs.Status.ReadyCount = readyCount
	vgs.Status.FailedCount = failedCount
	if finalPhase != "InProgress" {
		vgs.Status.CompletedAt = &completedAt
	}
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
		Members:               readySnapshots,
		CreationTime:          now.Time,
		ReadyToUse:            readyToUse,
		InconsistencyWindowMs: inconsistencyMs,
	}, nil
}

// refreshGroupSnapshot checks the members of a group whose copies were still
// running, and completes the group once every member is ready or failed.
func (m *Manager) refreshGroupSnapshot(ctx context.Context, vgs *v1alpha1.VolumeGroupSnapshot, req GroupSnapshotRequest) (*GroupSnapshotResult, error) {
	volumeIDs := make(map[string]string, len(req.SourceVolumeIDs))
	for _, volID := range req.SourceVolumeIDs {
		if _, _, pvName, err := parseManagerVolumeID(volID); err == nil {
			volumeIDs[pvName] = volID
		}
	}

	var ready []SnapshotResult
	var readyCount, failedCount, pendingCount int32
	for i := range vgs.Status.Members {
		member := &vgs.Status.Members[i]
		if member.Phase == "Pending" || member.Phase == "Creating" {
			member.Phase = "Creating"
			snapResult, err := m.CreateSnapshot(ctx, member.SnapshotName, volumeIDs[member.SubVolumeName], req.Parameters)
			switch {
			case err != nil:
				member.Phase = "Failed"
				member.Error = err.Error()
			case snapResult.ReadyToUse:
				member.Phase = "Ready"
			}
		}
		switch member.Phase {
		case "Ready":
			readyCount++
			ready = append(ready, SnapshotResult{
				SnapshotName:   member.SnapshotName,
				PoolName:       vgs.Spec.PoolName,
				ReadyToUse:     true,
				SourceVolumeID: volumeIDs[member.SubVolumeName],
			})
		case "Failed":
			failedCount++
		default:
			pendingCount++
		}
	}

	vgs.Status.ReadyCount = readyCount
	vgs.Status.FailedCount = failedCount
	switch {
	case pendingCount > 0:
		vgs.Status.Phase = "InProgress"
	case failedCount > 0:
		vgs.Status.Phase = "PartialFailure"
	default:
		vgs.Status.Phase = "Complete"
	}
	if vgs.Status.Phase != "InProgress" {
		completedAt := metav1.Now()
		vgs.Status.CompletedAt = &completedAt
	}
	if err := m.k8sClient.UpdateVolumeGroupSnapshotStatus(ctx, vgs); err != nil {
		klog.ErrorS(err, "Failed to update VolumeGroupSnapshot status", "group", vgs.Name)
	}

	result := &GroupSnapshotResult{
		GroupName:             vgs.Name,
		PoolName:              vgs.Spec.PoolName,
		Members:               ready,
		ReadyToUse:            vgs.Status.Phase == "Complete",
		InconsistencyWindowMs: vgs.Status.InconsistencyWindowMs,
	}
	if vgs.Status.CreationTime != nil {
		result.CreationTime = vgs.Status.CreationTime.Time
	}
	return result, nil
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
