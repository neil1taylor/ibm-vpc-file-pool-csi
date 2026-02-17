package pool

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud/fake"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Fake NFS Operations
// ---------------------------------------------------------------------------

type fakeNFSOperations struct {
	mu            sync.Mutex
	dirs          map[string]os.FileMode
	chowns        map[string][2]int // path → [uid, gid]
	copies        map[string]string // dst → src
	copyCallCount int               // tracks total CopyDir calls
	MkdirErr      error
	RemoveErr     error
	ChownErr      error
	ChmodErr      error
	CopyErr       error
	CopyErrAfterN int // if > 0, return CopyErr only after N successful copies
}

func newFakeNFSOperations() *fakeNFSOperations {
	return &fakeNFSOperations{
		dirs:   make(map[string]os.FileMode),
		chowns: make(map[string][2]int),
		copies: make(map[string]string),
	}
}

func (f *fakeNFSOperations) MkdirAll(path string, perm os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.MkdirErr != nil {
		return f.MkdirErr
	}
	f.dirs[path] = perm
	return nil
}

func (f *fakeNFSOperations) RemoveAll(path string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.RemoveErr != nil {
		return f.RemoveErr
	}
	delete(f.dirs, path)
	return nil
}

func (f *fakeNFSOperations) Stat(path string) (os.FileInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.dirs[path]; ok {
		return nil, nil
	}
	return nil, os.ErrNotExist
}

func (f *fakeNFSOperations) Chown(path string, uid, gid int) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ChownErr != nil {
		return f.ChownErr
	}
	f.chowns[path] = [2]int{uid, gid}
	return nil
}

func (f *fakeNFSOperations) Chmod(path string, mode os.FileMode) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.ChmodErr != nil {
		return f.ChmodErr
	}
	f.dirs[path] = mode
	return nil
}

func (f *fakeNFSOperations) CopyDir(src, dst string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copyCallCount++
	if f.CopyErr != nil {
		if f.CopyErrAfterN > 0 && f.copyCallCount <= f.CopyErrAfterN {
			// Allow first N copies to succeed
		} else {
			return f.CopyErr
		}
	}
	f.copies[dst] = src
	f.dirs[dst] = 0755
	return nil
}

func (f *fakeNFSOperations) SyncDir(_ context.Context, src, dst string) error {
	return f.CopyDir(src, dst)
}

func (f *fakeNFSOperations) copyCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.copies)
}

func (f *fakeNFSOperations) exists(path string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	_, ok := f.dirs[path]
	return ok
}

func (f *fakeNFSOperations) dirCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dirs)
}

// ---------------------------------------------------------------------------
// Fake K8s Client
// ---------------------------------------------------------------------------

type fakeK8sClient struct {
	mu             sync.Mutex
	pools          map[string]*v1alpha1.FileSharePool
	subVolumes     map[string]*v1alpha1.SubVolume
	snapshots      map[string]*v1alpha1.Snapshot
	groupSnapshots map[string]*v1alpha1.VolumeGroupSnapshot

	GetPoolErr          error
	UpdatePoolStatusErr error
	GetSubVolumeErr     error
	CreateSubVolumeErr  error
	UpdateSubVolumeErr  error
	DeleteSubVolumeErr  error
	GetSnapshotErr      error
	CreateSnapshotErr   error
	DeleteSnapshotErr   error
}

var _ = (interface {
	GetFileSharePool(context.Context, string) (*v1alpha1.FileSharePool, error)
})((*fakeK8sClient)(nil))

func newFakeK8sClient() *fakeK8sClient {
	return &fakeK8sClient{
		pools:          make(map[string]*v1alpha1.FileSharePool),
		subVolumes:     make(map[string]*v1alpha1.SubVolume),
		snapshots:      make(map[string]*v1alpha1.Snapshot),
		groupSnapshots: make(map[string]*v1alpha1.VolumeGroupSnapshot),
	}
}

func (f *fakeK8sClient) GetFileSharePool(_ context.Context, name string) (*v1alpha1.FileSharePool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.GetPoolErr != nil {
		return nil, f.GetPoolErr
	}
	p, ok := f.pools[name]
	if !ok {
		return nil, fmt.Errorf("pool %q not found", name)
	}
	return p.DeepCopy(), nil
}

func (f *fakeK8sClient) UpdateFileSharePoolStatus(_ context.Context, pool *v1alpha1.FileSharePool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.UpdatePoolStatusErr != nil {
		return f.UpdatePoolStatusErr
	}
	existing, ok := f.pools[pool.Name]
	if !ok {
		return fmt.Errorf("pool %q not found", pool.Name)
	}
	existing.Status = pool.Status
	return nil
}

func (f *fakeK8sClient) GetSubVolume(_ context.Context, name string) (*v1alpha1.SubVolume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.GetSubVolumeErr != nil {
		return nil, f.GetSubVolumeErr
	}
	sv, ok := f.subVolumes[name]
	if !ok {
		return nil, fmt.Errorf("subvolume %q not found", name)
	}
	return sv.DeepCopy(), nil
}

func (f *fakeK8sClient) ListSubVolumes(_ context.Context, poolName string) ([]v1alpha1.SubVolume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []v1alpha1.SubVolume
	for _, sv := range f.subVolumes {
		if sv.Spec.PoolName == poolName {
			result = append(result, *sv.DeepCopy())
		}
	}
	return result, nil
}

func (f *fakeK8sClient) ListSubVolumesByShare(_ context.Context, shareID string) ([]v1alpha1.SubVolume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []v1alpha1.SubVolume
	for _, sv := range f.subVolumes {
		if sv.Spec.ShareID == shareID {
			result = append(result, *sv.DeepCopy())
		}
	}
	return result, nil
}

func (f *fakeK8sClient) ListCloneSubVolumes(_ context.Context) ([]v1alpha1.SubVolume, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []v1alpha1.SubVolume
	for _, sv := range f.subVolumes {
		if sv.Spec.SourceVolume != "" {
			result = append(result, *sv.DeepCopy())
		}
	}
	return result, nil
}

func (f *fakeK8sClient) CreateSubVolume(_ context.Context, sv *v1alpha1.SubVolume) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateSubVolumeErr != nil {
		return f.CreateSubVolumeErr
	}
	if _, exists := f.subVolumes[sv.Name]; exists {
		return fmt.Errorf("subvolume %q already exists", sv.Name)
	}
	f.subVolumes[sv.Name] = sv.DeepCopy()
	return nil
}

func (f *fakeK8sClient) UpdateSubVolume(_ context.Context, sv *v1alpha1.SubVolume) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.UpdateSubVolumeErr != nil {
		return f.UpdateSubVolumeErr
	}
	if _, exists := f.subVolumes[sv.Name]; !exists {
		return fmt.Errorf("subvolume %q not found", sv.Name)
	}
	f.subVolumes[sv.Name] = sv.DeepCopy()
	return nil
}

func (f *fakeK8sClient) UpdateSubVolumeStatus(_ context.Context, sv *v1alpha1.SubVolume) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.subVolumes[sv.Name]
	if !ok {
		return fmt.Errorf("subvolume %q not found", sv.Name)
	}
	// DeepCopy into existing to avoid sharing pointers (e.g. CloneProgress)
	// between the caller's object and the stored object.
	sv.Status.DeepCopyInto(&existing.Status)
	return nil
}

func (f *fakeK8sClient) DeleteSubVolume(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.DeleteSubVolumeErr != nil {
		return f.DeleteSubVolumeErr
	}
	if _, exists := f.subVolumes[name]; !exists {
		return fmt.Errorf("subvolume %q not found", name)
	}
	delete(f.subVolumes, name)
	return nil
}

func (f *fakeK8sClient) UpdateFileSharePool(_ context.Context, pool *v1alpha1.FileSharePool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.pools[pool.Name]; !ok {
		return fmt.Errorf("pool %q not found", pool.Name)
	}
	f.pools[pool.Name] = pool.DeepCopy()
	return nil
}

func (f *fakeK8sClient) GetConfigMapValue(_ context.Context, _, _, _ string) (string, error) {
	return "", fmt.Errorf("not implemented in fake")
}

func (f *fakeK8sClient) GetNodeZone(_ context.Context, _ string) (string, error) {
	return "us-south-1", nil
}

// --- Snapshot operations ---

func (f *fakeK8sClient) GetSnapshot(_ context.Context, name string) (*v1alpha1.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.GetSnapshotErr != nil {
		return nil, f.GetSnapshotErr
	}
	snap, ok := f.snapshots[name]
	if !ok {
		return nil, fmt.Errorf("snapshot %q not found", name)
	}
	return snap.DeepCopy(), nil
}

func (f *fakeK8sClient) ListSnapshots(_ context.Context, poolName string) ([]v1alpha1.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []v1alpha1.Snapshot
	for _, snap := range f.snapshots {
		if snap.Spec.PoolName == poolName {
			result = append(result, *snap.DeepCopy())
		}
	}
	return result, nil
}

func (f *fakeK8sClient) ListSnapshotsByShare(_ context.Context, shareID string) ([]v1alpha1.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []v1alpha1.Snapshot
	for _, snap := range f.snapshots {
		if snap.Spec.ShareID == shareID {
			result = append(result, *snap.DeepCopy())
		}
	}
	return result, nil
}

func (f *fakeK8sClient) ListSnapshotsBySource(_ context.Context, sourceSubVolume string) ([]v1alpha1.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []v1alpha1.Snapshot
	for _, snap := range f.snapshots {
		if snap.Spec.SourceSubVolume == sourceSubVolume {
			result = append(result, *snap.DeepCopy())
		}
	}
	return result, nil
}

func (f *fakeK8sClient) CreateSnapshot(_ context.Context, snap *v1alpha1.Snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.CreateSnapshotErr != nil {
		return f.CreateSnapshotErr
	}
	if _, exists := f.snapshots[snap.Name]; exists {
		return fmt.Errorf("snapshot %q already exists", snap.Name)
	}
	f.snapshots[snap.Name] = snap.DeepCopy()
	return nil
}

func (f *fakeK8sClient) UpdateSnapshot(_ context.Context, snap *v1alpha1.Snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.snapshots[snap.Name]; !exists {
		return fmt.Errorf("snapshot %q not found", snap.Name)
	}
	f.snapshots[snap.Name] = snap.DeepCopy()
	return nil
}

func (f *fakeK8sClient) UpdateSnapshotStatus(_ context.Context, snap *v1alpha1.Snapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.snapshots[snap.Name]
	if !ok {
		return fmt.Errorf("snapshot %q not found", snap.Name)
	}
	existing.Status = snap.Status
	return nil
}

func (f *fakeK8sClient) DeleteSnapshot(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.DeleteSnapshotErr != nil {
		return f.DeleteSnapshotErr
	}
	if _, exists := f.snapshots[name]; !exists {
		return fmt.Errorf("snapshot %q not found", name)
	}
	delete(f.snapshots, name)
	return nil
}

func (f *fakeK8sClient) GetVolumeGroupSnapshot(_ context.Context, name string) (*v1alpha1.VolumeGroupSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	vgs, ok := f.groupSnapshots[name]
	if !ok {
		return nil, fmt.Errorf("volume group snapshot %q not found", name)
	}
	return vgs.DeepCopy(), nil
}

func (f *fakeK8sClient) CreateVolumeGroupSnapshot(_ context.Context, vgs *v1alpha1.VolumeGroupSnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.groupSnapshots[vgs.Name]; exists {
		return fmt.Errorf("volume group snapshot %q already exists", vgs.Name)
	}
	f.groupSnapshots[vgs.Name] = vgs.DeepCopy()
	return nil
}

func (f *fakeK8sClient) UpdateVolumeGroupSnapshotStatus(_ context.Context, vgs *v1alpha1.VolumeGroupSnapshot) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	existing, ok := f.groupSnapshots[vgs.Name]
	if !ok {
		return fmt.Errorf("volume group snapshot %q not found", vgs.Name)
	}
	existing.Status = *vgs.Status.DeepCopy()
	return nil
}

func (f *fakeK8sClient) DeleteVolumeGroupSnapshot(_ context.Context, name string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.groupSnapshots[name]; !exists {
		return fmt.Errorf("volume group snapshot %q not found", name)
	}
	delete(f.groupSnapshots, name)
	return nil
}

func (f *fakeK8sClient) ListVolumeGroupSnapshots(_ context.Context, poolName string) ([]v1alpha1.VolumeGroupSnapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []v1alpha1.VolumeGroupSnapshot
	for _, vgs := range f.groupSnapshots {
		if vgs.Spec.PoolName == poolName {
			result = append(result, *vgs.DeepCopy())
		}
	}
	return result, nil
}

func (f *fakeK8sClient) getGroupSnapshot(name string) *v1alpha1.VolumeGroupSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.groupSnapshots[name]
}

func (f *fakeK8sClient) groupSnapshotCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.groupSnapshots)
}

// --- ReplicationPolicy operations (stubs) ---

func (f *fakeK8sClient) GetReplicationPolicy(_ context.Context, _ string) (*v1alpha1.ReplicationPolicy, error) {
	return nil, fmt.Errorf("not implemented in fake")
}
func (f *fakeK8sClient) ListReplicationPolicies(_ context.Context) ([]v1alpha1.ReplicationPolicy, error) {
	return nil, nil
}
func (f *fakeK8sClient) CreateReplicationPolicy(_ context.Context, _ *v1alpha1.ReplicationPolicy) error {
	return nil
}
func (f *fakeK8sClient) UpdateReplicationPolicyStatus(_ context.Context, _ *v1alpha1.ReplicationPolicy) error {
	return nil
}
func (f *fakeK8sClient) DeleteReplicationPolicy(_ context.Context, _ string) error { return nil }

func (f *fakeK8sClient) addSnapshot(snap *v1alpha1.Snapshot) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.snapshots[snap.Name] = snap.DeepCopy()
}

func (f *fakeK8sClient) getSnapshot(name string) *v1alpha1.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.snapshots[name]
}

func (f *fakeK8sClient) snapshotCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.snapshots)
}

func (f *fakeK8sClient) addPool(pool *v1alpha1.FileSharePool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pools[pool.Name] = pool.DeepCopy()
}

func (f *fakeK8sClient) addSubVolume(sv *v1alpha1.SubVolume) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.subVolumes[sv.Name] = sv.DeepCopy()
}

func (f *fakeK8sClient) getPool(name string) *v1alpha1.FileSharePool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pools[name]
}

func (f *fakeK8sClient) getSubVolume(name string) *v1alpha1.SubVolume {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.subVolumes[name]
}

func (f *fakeK8sClient) subVolumeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subVolumes)
}

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

func newTestPool(name, strategy string, shareSizeGB int64, shares ...v1alpha1.PoolShareStatus) *v1alpha1.FileSharePool {
	var totalCapacity, totalAllocated int64
	var totalPVCs int32
	for _, s := range shares {
		totalCapacity += s.TotalGB
		totalAllocated += s.AllocatedGB
		totalPVCs += s.PVCCount
	}

	return &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.FileSharePoolSpec{
			Zone:               "us-south-1",
			Profile:            "dp2",
			ShareSizeGB:        shareSizeGB,
			MaxShares:          10,
			InitialShares:      1,
			AutoExpand:         true,
			AllocationStrategy: strategy,
			DefaultPermissions: "0755",
		},
		Status: v1alpha1.FileSharePoolStatus{
			Phase:            "Ready",
			Shares:           shares,
			ShareCount:       int32(len(shares)), //nolint:gosec // safe: test data, small count
			TotalCapacityGB:  totalCapacity,
			TotalAllocatedGB: totalAllocated,
			TotalPVCCount:    totalPVCs,
		},
	}
}

func newTestSubVolume(pvName, poolName, shareID string, requestedGB int64) *v1alpha1.SubVolume {
	return &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":     poolName,
				"storage.ibmcloud.io/share-id": shareID,
			},
		},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           poolName,
			ShareID:            shareID,
			ShareMountTargetIP: "10.240.0.1",
			SubPath:            fmt.Sprintf("/pvcs/%s", pvName),
			RequestedGB:        requestedGB,
			PVName:             pvName,
			PVCName:            "my-pvc",
			PVCNamespace:       "default",
			ReclaimPolicy:      "Delete",
		},
		Status: v1alpha1.SubVolumeStatus{
			Phase: "Bound",
		},
	}
}

func newStableShare(id, name string, totalGB, allocatedGB int64, pvcCount int32) v1alpha1.PoolShareStatus {
	return v1alpha1.PoolShareStatus{
		ShareID:       id,
		ShareName:     name,
		MountTargetIP: "10.240.0.1",
		MountTargetID: id + "-mt",
		TotalGB:       totalGB,
		AllocatedGB:   allocatedGB,
		PVCCount:      pvcCount,
		State:         "stable",
		Zone:          "us-south-1",
	}
}

func newManagerForTest(k8s *fakeK8sClient, vpc *fake.FakeVPCClient, nfs *fakeNFSOperations) *Manager {
	return NewManager(k8s, vpc, nfs, "/mnt/staging")
}

// PV name that matches the regex /pvcs/pvc-[a-f0-9-]{36}
const testPVName = "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"

// ---------------------------------------------------------------------------
// Allocate Tests
// ---------------------------------------------------------------------------

func TestAllocate_SpreadStrategy(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	// share-1 at 75% (750/1000), share-2 at 25% (250/1000)
	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 750, 5),
		newStableShare("share-2", "s2", 1000, 250, 2),
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// Spread should pick share-2 (most free space: 750 GB free)
	if result.ShareID != "share-2" {
		t.Errorf("expected share-2 (most free), got %s", result.ShareID)
	}
}

func TestAllocate_BinpackStrategy(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	// share-1 at 75% (750/1000), share-2 at 25% (250/1000)
	pool := newTestPool("test-pool", "binpack", 1000,
		newStableShare("share-1", "s1", 1000, 750, 5),
		newStableShare("share-2", "s2", 1000, 250, 2),
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// Binpack should pick share-1 (least free space that still fits: 250 GB free)
	if result.ShareID != "share-1" {
		t.Errorf("expected share-1 (least free that fits), got %s", result.ShareID)
	}
}

func TestAllocate_Idempotent(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	// Pre-existing SubVolume
	existing := newTestSubVolume(testPVName, "test-pool", "share-1", 10)
	k.addSubVolume(existing)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	if result.ShareID != "share-1" {
		t.Errorf("expected share-1 from existing SubVolume, got %s", result.ShareID)
	}
	if result.SubPath != fmt.Sprintf("/pvcs/%s", testPVName) {
		t.Errorf("unexpected subPath: %s", result.SubPath)
	}

	// No new directory should have been created
	if nfs.dirCount() != 0 {
		t.Errorf("expected no dirs created for idempotent call, got %d", nfs.dirCount())
	}
}

func TestAllocate_PoolNotFound(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "nonexistent-pool",
		RequestedGB:  10,
	})

	if !errors.Is(err, ErrPoolNotFound) {
		t.Fatalf("expected ErrPoolNotFound, got: %v", err)
	}
}

func TestAllocate_PoolExhausted_NoAutoExpand(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 1000, 10), // fully allocated
	)
	pool.Spec.AutoExpand = false
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})

	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted, got: %v", err)
	}
}

func TestAllocate_PoolExhausted_AutoExpand(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 1000, 10), // fully allocated
	)
	pool.Spec.AutoExpand = true
	pool.Spec.MaxShares = 5
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// Should have created a new share via VPC
	if vpc.CreateCalls != 1 {
		t.Errorf("expected 1 VPC create call, got %d", vpc.CreateCalls)
	}

	// Result should point to the new share
	if result.ShareID == "share-1" {
		t.Error("expected allocation on new share, got share-1")
	}

	// SubVolume CR should exist
	sv := k.getSubVolume(testPVName)
	if sv == nil {
		t.Fatal("SubVolume CR not created")
	}
	if sv.Spec.ShareID == "share-1" {
		t.Error("SubVolume should be on new share, not share-1")
	}
}

func TestAllocate_MaxSharesReached(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	// Create pool with maxShares=2 and 2 full shares
	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 1000, 10),
		newStableShare("share-2", "s2", 1000, 1000, 10),
	)
	pool.Spec.MaxShares = 2
	pool.Spec.AutoExpand = true
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})

	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted, got: %v", err)
	}

	// Should not have called VPC
	if vpc.CreateCalls != 0 {
		t.Errorf("expected 0 VPC create calls, got %d", vpc.CreateCalls)
	}
}

func TestAllocate_RequestTooLarge(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 2000,
		newStableShare("share-1", "s1", 2000, 0, 0),
	)
	// MaxShares=1 so auto-expand won't help
	pool.Spec.MaxShares = 1
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  3000,
	})

	if err == nil {
		t.Fatal("expected error for request too large")
	}
}

func TestAllocate_ZoneMismatch(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
		Zone:         "us-south-2", // pool is us-south-1
	})

	if err == nil {
		t.Fatal("expected zone mismatch error")
	}
	if !contains(err.Error(), "zone mismatch") {
		t.Errorf("expected 'zone mismatch' in error, got: %v", err)
	}
}

func TestAllocate_CreatesSubdirectory(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  5,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	expectedPath := fmt.Sprintf("/mnt/staging/pvcs/%s", testPVName)
	if !nfs.exists(expectedPath) {
		t.Errorf("expected directory %s to be created", expectedPath)
	}
}

func TestAllocate_CreatesSubVolumeCR(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  5,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	sv := k.getSubVolume(testPVName)
	if sv == nil {
		t.Fatal("SubVolume CR not created")
	}
	if sv.Spec.PoolName != "test-pool" {
		t.Errorf("expected poolName 'test-pool', got %q", sv.Spec.PoolName)
	}
	if sv.Spec.ShareID != "share-1" {
		t.Errorf("expected shareID 'share-1', got %q", sv.Spec.ShareID)
	}
	if sv.Spec.RequestedGB != 5 {
		t.Errorf("expected requestedGB 5, got %d", sv.Spec.RequestedGB)
	}
	if sv.Spec.SubPath != fmt.Sprintf("/pvcs/%s", testPVName) {
		t.Errorf("unexpected subPath: %s", sv.Spec.SubPath)
	}
	if sv.Labels["storage.ibmcloud.io/pool"] != "test-pool" {
		t.Error("missing pool label on SubVolume CR")
	}
	if sv.Labels["storage.ibmcloud.io/share-id"] != "share-1" {
		t.Error("missing share-id label on SubVolume CR")
	}
}

func TestAllocate_MkdirFails_NoSubVolumeCreated(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()
	nfs.MkdirErr = errors.New("permission denied")

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  5,
	})

	if err == nil {
		t.Fatal("expected error when mkdir fails")
	}

	// Verify no SubVolume CR was created
	if k.subVolumeCount() != 0 {
		t.Error("SubVolume CR should not be created when mkdir fails")
	}
}

func TestAllocate_SkipsNonStableShares(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	creatingShare := newStableShare("share-creating", "s1", 1000, 0, 0)
	creatingShare.State = "creating"
	stableShare := newStableShare("share-stable", "s2", 1000, 500, 3)

	pool := newTestPool("test-pool", "spread", 1000, creatingShare, stableShare)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	if result.ShareID != "share-stable" {
		t.Errorf("expected share-stable (only stable share), got %s", result.ShareID)
	}
}

func TestAllocate_UpdatesPoolStatus(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 2),
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  50,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	updatedPool := k.getPool("test-pool")
	if updatedPool.Status.TotalAllocatedGB != 150 {
		t.Errorf("expected TotalAllocatedGB=150, got %d", updatedPool.Status.TotalAllocatedGB)
	}
	if updatedPool.Status.TotalPVCCount != 3 {
		t.Errorf("expected TotalPVCCount=3, got %d", updatedPool.Status.TotalPVCCount)
	}
	if updatedPool.Status.Shares[0].AllocatedGB != 150 {
		t.Errorf("expected share AllocatedGB=150, got %d", updatedPool.Status.Shares[0].AllocatedGB)
	}
}

// ---------------------------------------------------------------------------
// Deallocate Tests
// ---------------------------------------------------------------------------

func TestDeallocate_Normal(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 2),
	)
	k.addPool(pool)

	sv := newTestSubVolume(testPVName, "test-pool", "share-1", 50)
	k.addSubVolume(sv)

	// Pre-create the directory
	dirPath := fmt.Sprintf("/mnt/staging/pvcs/%s", testPVName)
	nfs.dirs[dirPath] = 0755

	mgr := newManagerForTest(k, vpc, nfs)

	err := mgr.Deallocate(context.Background(), testPVName)
	if err != nil {
		t.Fatalf("Deallocate failed: %v", err)
	}

	// Directory should be removed
	if nfs.exists(dirPath) {
		t.Error("expected directory to be removed")
	}

	// SubVolume CR should be deleted
	if k.subVolumeCount() != 0 {
		t.Error("SubVolume CR should be deleted")
	}

	// Pool status should be updated
	updatedPool := k.getPool("test-pool")
	if updatedPool.Status.TotalAllocatedGB != 50 {
		t.Errorf("expected TotalAllocatedGB=50, got %d", updatedPool.Status.TotalAllocatedGB)
	}
	if updatedPool.Status.TotalPVCCount != 1 {
		t.Errorf("expected TotalPVCCount=1, got %d", updatedPool.Status.TotalPVCCount)
	}
}

func TestDeallocate_NotFound(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	mgr := newManagerForTest(k, vpc, nfs)

	err := mgr.Deallocate(context.Background(), "pvc-nonexistent")

	if !errors.Is(err, ErrSubVolumeNotFound) {
		t.Fatalf("expected ErrSubVolumeNotFound, got: %v", err)
	}
}

func TestDeallocate_RemoveFails_SubVolumeRetained(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()
	nfs.RemoveErr = errors.New("I/O error")

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 2),
	)
	k.addPool(pool)

	sv := newTestSubVolume(testPVName, "test-pool", "share-1", 50)
	k.addSubVolume(sv)

	mgr := newManagerForTest(k, vpc, nfs)

	err := mgr.Deallocate(context.Background(), testPVName)
	if err == nil {
		t.Fatal("expected error when remove fails")
	}

	// SubVolume CR should still exist (retained for tracking)
	if k.getSubVolume(testPVName) == nil {
		t.Error("SubVolume CR should be retained when remove fails")
	}
}

// ---------------------------------------------------------------------------
// Expand Tests
// ---------------------------------------------------------------------------

func TestExpand_WithinShareCapacity(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 2),
	)
	k.addPool(pool)

	sv := newTestSubVolume(testPVName, "test-pool", "share-1", 5)
	k.addSubVolume(sv)

	mgr := newManagerForTest(k, vpc, nfs)

	err := mgr.Expand(context.Background(), testPVName, 50)
	if err != nil {
		t.Fatalf("Expand failed: %v", err)
	}

	// SubVolume should be updated
	updated := k.getSubVolume(testPVName)
	if updated.Spec.RequestedGB != 50 {
		t.Errorf("expected RequestedGB=50, got %d", updated.Spec.RequestedGB)
	}

	// Pool status should reflect the increase (delta = 45)
	updatedPool := k.getPool("test-pool")
	if updatedPool.Status.TotalAllocatedGB != 145 {
		t.Errorf("expected TotalAllocatedGB=145, got %d", updatedPool.Status.TotalAllocatedGB)
	}
	if updatedPool.Status.Shares[0].AllocatedGB != 145 {
		t.Errorf("expected share AllocatedGB=145, got %d", updatedPool.Status.Shares[0].AllocatedGB)
	}
}

func TestExpand_ExceedsShareCapacity(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 990, 10), // only 10 GB free
	)
	k.addPool(pool)

	sv := newTestSubVolume(testPVName, "test-pool", "share-1", 5)
	k.addSubVolume(sv)

	mgr := newManagerForTest(k, vpc, nfs)

	err := mgr.Expand(context.Background(), testPVName, 100)

	if !errors.Is(err, ErrInsufficientShareCapacity) {
		t.Fatalf("expected ErrInsufficientShareCapacity, got: %v", err)
	}
}

func TestExpand_SubVolumeNotFound(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	mgr := newManagerForTest(k, vpc, nfs)

	err := mgr.Expand(context.Background(), "nonexistent", 100)

	if !errors.Is(err, ErrSubVolumeNotFound) {
		t.Fatalf("expected ErrSubVolumeNotFound, got: %v", err)
	}
}

func TestExpand_SameSize_Noop(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 2),
	)
	k.addPool(pool)

	sv := newTestSubVolume(testPVName, "test-pool", "share-1", 50)
	k.addSubVolume(sv)

	mgr := newManagerForTest(k, vpc, nfs)

	err := mgr.Expand(context.Background(), testPVName, 50)
	if err != nil {
		t.Fatalf("expected no-op for same size, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Share Selection Tests
// ---------------------------------------------------------------------------

func TestSelectShare_SpreadPicksMostFree(t *testing.T) {
	shares := []v1alpha1.PoolShareStatus{
		newStableShare("s1", "s1", 1000, 800, 5),
		newStableShare("s2", "s2", 1000, 200, 2),
		newStableShare("s3", "s3", 1000, 500, 3),
	}

	got, err := selectShare("spread", shares, 10, "")
	if err != nil {
		t.Fatalf("selectShare failed: %v", err)
	}
	if got.ShareID != "s2" {
		t.Errorf("spread should pick s2 (800 free), got %s", got.ShareID)
	}
}

func TestSelectShare_BinpackPicksLeastFree(t *testing.T) {
	shares := []v1alpha1.PoolShareStatus{
		newStableShare("s1", "s1", 1000, 800, 5), // 200 free
		newStableShare("s2", "s2", 1000, 200, 2), // 800 free
		newStableShare("s3", "s3", 1000, 500, 3), // 500 free
	}

	got, err := selectShare("binpack", shares, 10, "")
	if err != nil {
		t.Fatalf("selectShare failed: %v", err)
	}
	if got.ShareID != "s1" {
		t.Errorf("binpack should pick s1 (200 free, least that fits), got %s", got.ShareID)
	}
}

func TestSelectShare_NoShareFits(t *testing.T) {
	shares := []v1alpha1.PoolShareStatus{
		newStableShare("s1", "s1", 1000, 995, 10), // 5 free
	}

	_, err := selectShare("spread", shares, 10, "")
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted, got: %v", err)
	}
}

func TestSelectShare_SkipsNonStable(t *testing.T) {
	shares := []v1alpha1.PoolShareStatus{
		{ShareID: "s1", TotalGB: 1000, AllocatedGB: 0, State: "creating"},
		{ShareID: "s2", TotalGB: 1000, AllocatedGB: 0, State: "draining"},
		newStableShare("s3", "s3", 1000, 500, 3),
	}

	got, err := selectShare("spread", shares, 10, "")
	if err != nil {
		t.Fatalf("selectShare failed: %v", err)
	}
	if got.ShareID != "s3" {
		t.Errorf("expected s3 (only stable), got %s", got.ShareID)
	}
}

func TestSelectShare_UnknownStrategy(t *testing.T) {
	shares := []v1alpha1.PoolShareStatus{
		newStableShare("s1", "s1", 1000, 0, 0),
	}

	_, err := selectShare("unknown", shares, 10, "")
	if err == nil {
		t.Fatal("expected error for unknown strategy")
	}
}

// ---------------------------------------------------------------------------
// Subdirectory Operation Tests
// ---------------------------------------------------------------------------

func TestCreateSubDirectory_WithUIDGID(t *testing.T) {
	nfs := newFakeNFSOperations()
	uid := int64(1000)
	gid := int64(2000)

	err := createSubDirectory(nfs, "/mnt/staging", "/pvcs/"+testPVName, &uid, &gid, "0750")
	if err != nil {
		t.Fatalf("createSubDirectory failed: %v", err)
	}

	expectedPath := "/mnt/staging/pvcs/" + testPVName
	if !nfs.exists(expectedPath) {
		t.Error("expected directory to be created")
	}
	if got := nfs.chowns[expectedPath]; got != [2]int{1000, 2000} {
		t.Errorf("expected chown(1000, 2000), got %v", got)
	}
	if got := nfs.dirs[expectedPath]; got != 0750 {
		t.Errorf("expected mode 0750, got %o", got)
	}
}

func TestCreateSubDirectory_ChownFails(t *testing.T) {
	nfs := newFakeNFSOperations()
	nfs.ChownErr = errors.New("chown failed")
	uid := int64(1000)

	err := createSubDirectory(nfs, "/mnt/staging", "/pvcs/"+testPVName, &uid, nil, "")
	if err == nil {
		t.Fatal("expected error when chown fails")
	}
}

func TestCreateSubDirectory_ChmodFails(t *testing.T) {
	nfs := newFakeNFSOperations()
	nfs.ChmodErr = errors.New("chmod failed")

	err := createSubDirectory(nfs, "/mnt/staging", "/pvcs/"+testPVName, nil, nil, "0700")
	if err == nil {
		t.Fatal("expected error when chmod fails")
	}
}

func TestCreateSubDirectory_InvalidPath(t *testing.T) {
	nfs := newFakeNFSOperations()

	err := createSubDirectory(nfs, "/mnt/staging", "/../../../etc/passwd", nil, nil, "")
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestRemoveSubDirectory_InvalidPath(t *testing.T) {
	nfs := newFakeNFSOperations()

	err := removeSubDirectory(nfs, "/mnt/staging", "/../../../etc/passwd")
	if err == nil {
		t.Fatal("expected error for invalid path")
	}
}

func TestCreateSubDirectory_InvalidPermissions(t *testing.T) {
	nfs := newFakeNFSOperations()

	err := createSubDirectory(nfs, "/mnt/staging", "/pvcs/"+testPVName, nil, nil, "notoctal")
	if err == nil {
		t.Fatal("expected error for invalid permissions string")
	}
}

func TestAllocate_WithCustomUIDGID(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	k.addPool(pool)

	uid := int64(1000)
	gid := int64(2000)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  5,
		UID:          &uid,
		GID:          &gid,
		Permissions:  "0700",
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	expectedPath := "/mnt/staging/pvcs/" + testPVName
	if got := nfs.chowns[expectedPath]; got != [2]int{1000, 2000} {
		t.Errorf("expected chown(1000, 2000), got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Metrics Tests
// ---------------------------------------------------------------------------

func getCounterValue(counter *dto.Metric) float64 {
	if counter.Counter != nil {
		return *counter.Counter.Value
	}
	return 0
}

func TestAllocate_IncrementsSuccessMetric(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("metrics-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	k.addPool(pool)

	// Record counter before
	var before dto.Metric
	_ = metrics.AllocationsTotal.WithLabelValues("metrics-pool", "success").Write(&before)
	beforeVal := getCounterValue(&before)

	mgr := newManagerForTest(k, vpc, nfs)
	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "metrics-pool",
		RequestedGB:  5,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	var after dto.Metric
	_ = metrics.AllocationsTotal.WithLabelValues("metrics-pool", "success").Write(&after)
	afterVal := getCounterValue(&after)

	if afterVal <= beforeVal {
		t.Errorf("expected success counter to increment, before=%f after=%f", beforeVal, afterVal)
	}
}

func TestAllocate_IncrementsErrorMetric(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()
	// No pool added → will fail

	var before dto.Metric
	_ = metrics.AllocationsTotal.WithLabelValues("missing-pool", "error").Write(&before)
	beforeVal := getCounterValue(&before)

	mgr := newManagerForTest(k, vpc, nfs)
	_, _ = mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "missing-pool",
		RequestedGB:  5,
	})

	var after dto.Metric
	_ = metrics.AllocationsTotal.WithLabelValues("missing-pool", "error").Write(&after)
	afterVal := getCounterValue(&after)

	if afterVal <= beforeVal {
		t.Errorf("expected error counter to increment, before=%f after=%f", beforeVal, afterVal)
	}
}

func TestDeallocate_IncrementsMetric(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("metrics-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 50, 1),
	)
	k.addPool(pool)
	k.addSubVolume(newTestSubVolume(testPVName, "metrics-pool", "share-1", 50))

	var before dto.Metric
	_ = metrics.AllocationsTotal.WithLabelValues("metrics-pool", "deallocate_success").Write(&before)
	beforeVal := getCounterValue(&before)

	mgr := newManagerForTest(k, vpc, nfs)
	err := mgr.Deallocate(context.Background(), testPVName)
	if err != nil {
		t.Fatalf("Deallocate failed: %v", err)
	}

	var after dto.Metric
	_ = metrics.AllocationsTotal.WithLabelValues("metrics-pool", "deallocate_success").Write(&after)
	afterVal := getCounterValue(&after)

	if afterVal <= beforeVal {
		t.Errorf("expected deallocate_success counter to increment, before=%f after=%f", beforeVal, afterVal)
	}
}

// ---------------------------------------------------------------------------
// Share Selection — Tier Tests
// ---------------------------------------------------------------------------

func newStableShareWithTier(id, name string, totalGB, allocatedGB int64, pvcCount int32, tier string) v1alpha1.PoolShareStatus {
	s := newStableShare(id, name, totalGB, allocatedGB, pvcCount)
	s.Tier = tier
	return s
}

func TestSelectShare_FiltersByTier(t *testing.T) {
	shares := []v1alpha1.PoolShareStatus{
		newStableShareWithTier("s1", "s1", 1000, 200, 2, "standard"),
		newStableShareWithTier("s2", "s2", 500, 100, 1, "premium"),
		newStableShareWithTier("s3", "s3", 1000, 500, 3, "standard"),
	}

	got, err := selectShare("spread", shares, 10, "premium")
	if err != nil {
		t.Fatalf("selectShare failed: %v", err)
	}
	if got.ShareID != "s2" {
		t.Errorf("expected s2 (only premium share), got %s", got.ShareID)
	}
}

func TestSelectShare_EmptyTierMatchesEmptyShares(t *testing.T) {
	shares := []v1alpha1.PoolShareStatus{
		newStableShareWithTier("s1", "s1", 1000, 200, 2, "premium"),
		newStableShare("s2", "s2", 1000, 100, 1), // no tier (empty)
	}

	got, err := selectShare("spread", shares, 10, "")
	if err != nil {
		t.Fatalf("selectShare failed: %v", err)
	}
	if got.ShareID != "s2" {
		t.Errorf("expected s2 (empty tier), got %s", got.ShareID)
	}
}

func TestSelectShare_TierMismatchReturnsExhausted(t *testing.T) {
	shares := []v1alpha1.PoolShareStatus{
		newStableShareWithTier("s1", "s1", 1000, 200, 2, "standard"),
		newStableShareWithTier("s2", "s2", 1000, 100, 1, "standard"),
	}

	_, err := selectShare("spread", shares, 10, "premium")
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted for tier mismatch, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Allocate — Tier Tests
// ---------------------------------------------------------------------------

func newTestPoolWithTiers(name, strategy string, tiers []v1alpha1.ShareTier, shares ...v1alpha1.PoolShareStatus) *v1alpha1.FileSharePool {
	var totalCapacity, totalAllocated int64
	var totalPVCs int32
	for _, s := range shares {
		totalCapacity += s.TotalGB
		totalAllocated += s.AllocatedGB
		totalPVCs += s.PVCCount
	}

	return &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.FileSharePoolSpec{
			Zone:               "us-south-1",
			Profile:            "dp2",
			ShareSizeGB:        1000,
			MaxShares:          10,
			InitialShares:      1,
			AutoExpand:         true,
			AllocationStrategy: strategy,
			DefaultPermissions: "0755",
			Tiers:              tiers,
		},
		Status: v1alpha1.FileSharePoolStatus{
			Phase:            "Ready",
			Shares:           shares,
			ShareCount:       int32(len(shares)), //nolint:gosec // safe: test data
			TotalCapacityGB:  totalCapacity,
			TotalAllocatedGB: totalAllocated,
			TotalPVCCount:    totalPVCs,
		},
	}
}

func TestAllocate_WithTier(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPoolWithTiers("test-pool", "spread",
		[]v1alpha1.ShareTier{
			{Name: "standard", Profile: "dp2", ShareSizeGB: 1000, MaxShares: 5, InitialShares: 1},
			{Name: "premium", Profile: "custom", ShareSizeGB: 500, MaxShares: 3, InitialShares: 1},
		},
		newStableShareWithTier("s1", "s1", 1000, 100, 1, "standard"),
		newStableShareWithTier("s2", "s2", 500, 50, 1, "premium"),
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
		Tier:         "premium",
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	if result.ShareID != "s2" {
		t.Errorf("expected allocation on premium share s2, got %s", result.ShareID)
	}
}

func TestAllocate_TierAutoExpand(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPoolWithTiers("test-pool", "spread",
		[]v1alpha1.ShareTier{
			{Name: "standard", Profile: "dp2", ShareSizeGB: 1000, MaxShares: 5, InitialShares: 1},
			{Name: "premium", Profile: "custom", ShareSizeGB: 500, MaxShares: 3, InitialShares: 1},
		},
		newStableShareWithTier("s1", "s1", 1000, 100, 1, "standard"),
		newStableShareWithTier("s2", "s2", 500, 500, 10, "premium"), // fully allocated
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
		Tier:         "premium",
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// Should auto-expand with a new premium share
	if vpc.CreateCalls != 1 {
		t.Errorf("expected 1 VPC create call for auto-expand, got %d", vpc.CreateCalls)
	}
	// Result should be on the new share, not s2 (which is full) or s1 (wrong tier)
	if result.ShareID == "s1" || result.ShareID == "s2" {
		t.Errorf("expected allocation on new share, got %s", result.ShareID)
	}
}

func TestAllocate_TierMaxSharesReached(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPoolWithTiers("test-pool", "spread",
		[]v1alpha1.ShareTier{
			{Name: "premium", Profile: "custom", ShareSizeGB: 500, MaxShares: 1, InitialShares: 1},
		},
		newStableShareWithTier("s1", "s1", 500, 500, 10, "premium"), // full, and maxShares=1
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
		Tier:         "premium",
	})

	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted, got: %v", err)
	}
	if vpc.CreateCalls != 0 {
		t.Errorf("expected 0 VPC create calls, got %d", vpc.CreateCalls)
	}
}

func TestAllocate_TierRequiredWhenPoolHasTiers(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPoolWithTiers("test-pool", "spread",
		[]v1alpha1.ShareTier{
			{Name: "standard", Profile: "dp2", ShareSizeGB: 1000, MaxShares: 5, InitialShares: 1},
		},
		newStableShareWithTier("s1", "s1", 1000, 100, 1, "standard"),
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
		Tier:         "", // empty tier when pool has tiers
	})

	if err == nil {
		t.Fatal("expected error when tier is required but not provided")
	}
	if !contains(err.Error(), "tier is required") {
		t.Errorf("expected 'tier is required' in error, got: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Cross-Zone Allocation Tests
// ---------------------------------------------------------------------------

func TestAllocate_CrossZone_Allowed(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	pool.Spec.AccessorZones = []v1alpha1.AccessorZone{
		{Zone: "us-south-2", SubnetID: "subnet-2"},
	}
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	// Request from accessor zone us-south-2 (pool home is us-south-1)
	result, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
		Zone:         "us-south-2",
	})
	if err != nil {
		t.Fatalf("Allocate from accessor zone should succeed: %v", err)
	}
	if result.ShareID != "share-1" {
		t.Errorf("expected share-1, got %s", result.ShareID)
	}
}

func TestAllocate_CrossZone_PopulatesMountTargets(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	share := newStableShare("share-1", "s1", 1000, 100, 1)
	share.MountTargets = []v1alpha1.ZoneMountTarget{
		{Zone: "us-south-1", MountTargetID: "mt-1", MountTargetIP: "10.0.0.1"},
		{Zone: "us-south-2", MountTargetID: "mt-2", MountTargetIP: "10.0.1.1"},
	}

	pool := newTestPool("test-pool", "spread", 1000, share)
	pool.Spec.AccessorZones = []v1alpha1.AccessorZone{
		{Zone: "us-south-2", SubnetID: "subnet-2"},
	}
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	if result.MountTargets == nil {
		t.Fatal("expected MountTargets to be populated")
	}
	if result.MountTargets["us-south-1"] != "10.0.0.1" {
		t.Errorf("expected us-south-1 IP 10.0.0.1, got %q", result.MountTargets["us-south-1"])
	}
	if result.MountTargets["us-south-2"] != "10.0.1.1" {
		t.Errorf("expected us-south-2 IP 10.0.1.1, got %q", result.MountTargets["us-south-2"])
	}
	if len(result.AccessibleZones) != 2 {
		t.Errorf("expected 2 accessible zones, got %d", len(result.AccessibleZones))
	}
}

func TestAllocate_UnknownZone_Rejected(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	pool.Spec.AccessorZones = []v1alpha1.AccessorZone{
		{Zone: "us-south-2", SubnetID: "subnet-2"},
	}
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	// us-south-3 is not home zone or accessor zone
	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
		Zone:         "us-south-3",
	})

	if err == nil {
		t.Fatal("expected zone mismatch error for unknown zone")
	}
	if !contains(err.Error(), "zone mismatch") {
		t.Errorf("expected 'zone mismatch' in error, got: %v", err)
	}
}

func TestAllocate_NoAccessorZones_BackwardCompat(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	// No AccessorZones
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// MountTargets should be nil when no accessor zones
	if result.MountTargets != nil {
		t.Error("expected nil MountTargets when no accessor zones")
	}
	if result.AccessibleZones != nil {
		t.Error("expected nil AccessibleZones when no accessor zones")
	}
}

func TestAllocate_NoTiersBackwardCompat(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	// Pool without tiers — should work exactly as before
	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}
	if result.ShareID != "share-1" {
		t.Errorf("expected share-1 (backward compat), got %s", result.ShareID)
	}
}

// ---------------------------------------------------------------------------
// Share Draining Tests
// ---------------------------------------------------------------------------

func TestAllocate_SkipsDrainingShares(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	drainingShare := newStableShare("share-draining", "s1", 1000, 100, 1)
	drainingShare.State = "draining"
	stableShare := newStableShare("share-stable", "s2", 1000, 500, 3)

	pool := newTestPool("test-pool", "spread", 1000, drainingShare, stableShare)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})
	if err != nil {
		t.Fatalf("Allocate failed: %v", err)
	}

	// Should pick stable share, not the draining one
	if result.ShareID != "share-stable" {
		t.Errorf("expected share-stable (draining shares excluded), got %s", result.ShareID)
	}
}

func TestAllocate_AllSharesDraining_Exhausted(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	drainingShare1 := newStableShare("share-1", "s1", 1000, 100, 1)
	drainingShare1.State = "draining"
	drainingShare2 := newStableShare("share-2", "s2", 1000, 200, 2)
	drainingShare2.State = "draining"

	pool := newTestPool("test-pool", "spread", 1000, drainingShare1, drainingShare2)
	pool.Spec.AutoExpand = false
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.Allocate(context.Background(), AllocationRequest{
		PVName:       testPVName,
		PVCName:      "my-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})

	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted when all shares are draining, got: %v", err)
	}
}

func TestSelectShare_SkipsDraining(t *testing.T) {
	shares := []v1alpha1.PoolShareStatus{
		{ShareID: "s1", TotalGB: 1000, AllocatedGB: 0, State: "draining"},
		{ShareID: "s2", TotalGB: 1000, AllocatedGB: 0, State: "draining"},
		newStableShare("s3", "s3", 1000, 500, 3),
	}

	got, err := selectShare("spread", shares, 10, "")
	if err != nil {
		t.Fatalf("selectShare failed: %v", err)
	}
	if got.ShareID != "s3" {
		t.Errorf("expected s3 (only non-draining stable), got %s", got.ShareID)
	}
}

func TestSelectShare_AllDraining_Exhausted(t *testing.T) {
	shares := []v1alpha1.PoolShareStatus{
		{ShareID: "s1", TotalGB: 1000, AllocatedGB: 0, State: "draining"},
		{ShareID: "s2", TotalGB: 1000, AllocatedGB: 0, State: "draining"},
	}

	_, err := selectShare("spread", shares, 10, "")
	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted when all shares are draining, got: %v", err)
	}
}

func TestAllocate_ConcurrentDrainAndAllocate(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	// One draining, one stable with room, one stable full
	drainingShare := newStableShare("share-draining", "s1", 1000, 100, 1)
	drainingShare.State = "draining"
	stableShare := newStableShare("share-stable", "s2", 1000, 500, 3)
	fullShare := newStableShare("share-full", "s3", 1000, 1000, 10)

	pool := newTestPool("test-pool", "spread", 1000, drainingShare, stableShare, fullShare)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	// Run 5 concurrent allocations — all should go to share-stable
	errCh := make(chan error, 5)
	resultCh := make(chan *AllocationResult, 5)
	for i := 0; i < 5; i++ {
		go func(i int) {
			pvName := fmt.Sprintf("pvc-%08x-0000-0000-0000-000000000000", i)
			r, err := mgr.Allocate(context.Background(), AllocationRequest{
				PVName:       pvName,
				PVCName:      fmt.Sprintf("pvc-%d", i),
				PVCNamespace: "default",
				PoolName:     "test-pool",
				RequestedGB:  10,
			})
			errCh <- err
			resultCh <- r
		}(i)
	}

	for i := 0; i < 5; i++ {
		err := <-errCh
		result := <-resultCh
		if err != nil {
			t.Errorf("allocation %d failed: %v", i, err)
			continue
		}
		if result.ShareID == "share-draining" {
			t.Errorf("allocation %d went to draining share", i)
		}
		if result.ShareID == "share-full" {
			t.Errorf("allocation %d went to full share", i)
		}
	}
}

func TestDeallocate_FromDrainingShare(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	// Share is draining but still has SubVolumes
	drainingShare := newStableShare("share-draining", "s1", 1000, 50, 1)
	drainingShare.State = "draining"
	pool := newTestPool("test-pool", "spread", 1000, drainingShare)
	k.addPool(pool)

	sv := newTestSubVolume(testPVName, "test-pool", "share-draining", 50)
	k.addSubVolume(sv)

	mgr := newManagerForTest(k, vpc, nfs)

	err := mgr.Deallocate(context.Background(), testPVName)
	if err != nil {
		t.Fatalf("Deallocate from draining share should succeed: %v", err)
	}

	// SubVolume should be deleted
	if k.subVolumeCount() != 0 {
		t.Error("SubVolume CR should be deleted even from draining share")
	}

	// Pool status should be updated
	updatedPool := k.getPool("test-pool")
	if updatedPool.Status.Shares[0].AllocatedGB != 0 {
		t.Errorf("expected AllocatedGB=0 after deallocation, got %d", updatedPool.Status.Shares[0].AllocatedGB)
	}
	if updatedPool.Status.Shares[0].PVCCount != 0 {
		t.Errorf("expected PVCCount=0 after deallocation, got %d", updatedPool.Status.Shares[0].PVCCount)
	}
}

// ---------------------------------------------------------------------------
// Snapshot Tests
// ---------------------------------------------------------------------------

func TestCreateSnapshot_Success(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	sv := newTestSubVolume(testPVName, "test-pool", "share-1", 10)
	k.addSubVolume(sv)

	mgr := newManagerForTest(k, vpc, nfs)

	sourceVolumeID := fmt.Sprintf("test-pool/share-1/%s", testPVName)
	result, err := mgr.CreateSnapshot(context.Background(), "my-snapshot", sourceVolumeID, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}

	if result.SnapshotName != "my-snapshot" {
		t.Errorf("expected snapshot name 'my-snapshot', got %q", result.SnapshotName)
	}
	if result.PoolName != "test-pool" {
		t.Errorf("expected pool 'test-pool', got %q", result.PoolName)
	}
	if result.ShareID != "share-1" {
		t.Errorf("expected shareID 'share-1', got %q", result.ShareID)
	}
	if !result.ReadyToUse {
		t.Error("expected ReadyToUse=true")
	}
	if result.SizeBytes != 10*(1<<30) {
		t.Errorf("expected SizeBytes=%d, got %d", 10*(1<<30), result.SizeBytes)
	}

	// Verify Snapshot CR was created
	snap := k.getSnapshot("my-snapshot")
	if snap == nil {
		t.Fatal("Snapshot CR not created")
	}
	if snap.Spec.SourceSubVolume != testPVName {
		t.Errorf("expected source %s, got %s", testPVName, snap.Spec.SourceSubVolume)
	}

	// Verify NFS copy happened
	if nfs.copyCount() != 1 {
		t.Errorf("expected 1 copy, got %d", nfs.copyCount())
	}

	// Verify pool allocation was updated (snapshot counts toward capacity)
	updatedPool := k.getPool("test-pool")
	if updatedPool.Status.TotalAllocatedGB != 110 {
		t.Errorf("expected TotalAllocatedGB=110 (100 + 10 snapshot), got %d", updatedPool.Status.TotalAllocatedGB)
	}
}

func TestCreateSnapshot_Idempotent(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	sv := newTestSubVolume(testPVName, "test-pool", "share-1", 10)
	k.addSubVolume(sv)

	// Pre-existing snapshot
	now := metav1.Now()
	existingSnap := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "my-snapshot"},
		Spec: v1alpha1.SnapshotSpec{
			SourceSubVolume: testPVName,
			PoolName:        "test-pool",
			ShareID:         "share-1",
			SnapshotPath:    "/pvcs/.snapshots/snap-my-snapshot",
			SourceSubPath:   fmt.Sprintf("/pvcs/%s", testPVName),
			SizeGB:          10,
		},
		Status: v1alpha1.SnapshotStatus{
			Phase:        "Ready",
			ReadyToUse:   true,
			SizeBytes:    10 * (1 << 30),
			CreationTime: &now,
		},
	}
	k.addSnapshot(existingSnap)

	mgr := newManagerForTest(k, vpc, nfs)

	sourceVolumeID := fmt.Sprintf("test-pool/share-1/%s", testPVName)
	result, err := mgr.CreateSnapshot(context.Background(), "my-snapshot", sourceVolumeID, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}

	if result.SnapshotName != "my-snapshot" {
		t.Errorf("expected idempotent return, got %q", result.SnapshotName)
	}

	// No copy should have happened
	if nfs.copyCount() != 0 {
		t.Errorf("expected no copies for idempotent call, got %d", nfs.copyCount())
	}
}

func TestCreateSnapshot_SourceNotFound(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.CreateSnapshot(context.Background(), "my-snapshot", "test-pool/share-1/nonexistent", nil)

	if !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("expected ErrSourceNotFound, got: %v", err)
	}
}

func TestCreateSnapshot_CopyFails(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()
	nfs.CopyErr = errors.New("cp failed")

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	sv := newTestSubVolume(testPVName, "test-pool", "share-1", 10)
	k.addSubVolume(sv)

	mgr := newManagerForTest(k, vpc, nfs)

	sourceVolumeID := fmt.Sprintf("test-pool/share-1/%s", testPVName)
	_, err := mgr.CreateSnapshot(context.Background(), "my-snapshot", sourceVolumeID, nil)

	if err == nil {
		t.Fatal("expected error when copy fails")
	}

	// Snapshot CR should not be created
	if k.snapshotCount() != 0 {
		t.Error("Snapshot CR should not be created when copy fails")
	}
}

func TestDeleteSnapshot_Success(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 110, 2), // 100 from SVs + 10 from snapshot
	)
	k.addPool(pool)

	now := metav1.Now()
	snap := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "my-snapshot"},
		Spec: v1alpha1.SnapshotSpec{
			SourceSubVolume: testPVName,
			PoolName:        "test-pool",
			ShareID:         "share-1",
			SnapshotPath:    "/pvcs/.snapshots/snap-my-snapshot",
			SourceSubPath:   fmt.Sprintf("/pvcs/%s", testPVName),
			SizeGB:          10,
		},
		Status: v1alpha1.SnapshotStatus{
			Phase:        "Ready",
			ReadyToUse:   true,
			CreationTime: &now,
		},
	}
	k.addSnapshot(snap)

	// Pre-create snapshot dir
	nfs.dirs["/mnt/staging/pvcs/.snapshots/snap-my-snapshot"] = 0755

	mgr := newManagerForTest(k, vpc, nfs)

	err := mgr.DeleteSnapshot(context.Background(), "my-snapshot")
	if err != nil {
		t.Fatalf("DeleteSnapshot failed: %v", err)
	}

	// Snapshot CR should be deleted
	if k.snapshotCount() != 0 {
		t.Error("Snapshot CR should be deleted")
	}

	// Directory should be removed
	if nfs.exists("/mnt/staging/pvcs/.snapshots/snap-my-snapshot") {
		t.Error("expected snapshot directory to be removed")
	}

	// Pool capacity should be freed
	updatedPool := k.getPool("test-pool")
	if updatedPool.Status.TotalAllocatedGB != 100 {
		t.Errorf("expected TotalAllocatedGB=100, got %d", updatedPool.Status.TotalAllocatedGB)
	}
}

func TestDeleteSnapshot_NotFound(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	mgr := newManagerForTest(k, vpc, nfs)

	err := mgr.DeleteSnapshot(context.Background(), "nonexistent")

	if !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("expected ErrSnapshotNotFound, got: %v", err)
	}
}

func TestDeleteSnapshot_RemoveFails(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()
	nfs.RemoveErr = errors.New("I/O error")

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 110, 2),
	)
	k.addPool(pool)

	snap := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "my-snapshot"},
		Spec: v1alpha1.SnapshotSpec{
			PoolName:     "test-pool",
			ShareID:      "share-1",
			SnapshotPath: "/pvcs/.snapshots/snap-my-snapshot",
			SizeGB:       10,
		},
	}
	k.addSnapshot(snap)

	mgr := newManagerForTest(k, vpc, nfs)

	err := mgr.DeleteSnapshot(context.Background(), "my-snapshot")
	if err == nil {
		t.Fatal("expected error when remove fails")
	}

	// Snapshot CR should still exist
	if k.snapshotCount() == 0 {
		t.Error("Snapshot CR should be retained when remove fails")
	}
}

func TestRestoreSnapshot_Success(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	now := metav1.Now()
	snap := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "my-snapshot"},
		Spec: v1alpha1.SnapshotSpec{
			SourceSubVolume:    testPVName,
			PoolName:           "test-pool",
			ShareID:            "share-1",
			ShareMountTargetIP: "10.240.0.1",
			SnapshotPath:       "/pvcs/.snapshots/snap-my-snapshot",
			SourceSubPath:      fmt.Sprintf("/pvcs/%s", testPVName),
			SizeGB:             10,
		},
		Status: v1alpha1.SnapshotStatus{
			Phase:        "Ready",
			ReadyToUse:   true,
			CreationTime: &now,
		},
	}
	k.addSnapshot(snap)

	mgr := newManagerForTest(k, vpc, nfs)

	const restorePVName = "pvc-b2c3d4e5-6789-01ab-cdef-234567890abc"
	result, err := mgr.RestoreSnapshot(context.Background(), "my-snapshot", AllocationRequest{
		PVName:       restorePVName,
		PVCName:      "restore-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot failed: %v", err)
	}

	if result.ShareID != "share-1" {
		t.Errorf("expected shareID 'share-1', got %q", result.ShareID)
	}
	if result.SubPath != fmt.Sprintf("/pvcs/%s", restorePVName) {
		t.Errorf("expected subPath '/pvcs/%s', got %q", restorePVName, result.SubPath)
	}

	// Verify SubVolume CR was created
	sv := k.getSubVolume(restorePVName)
	if sv == nil {
		t.Fatal("SubVolume CR not created")
	}

	// Verify NFS copy happened
	if nfs.copyCount() != 1 {
		t.Errorf("expected 1 copy, got %d", nfs.copyCount())
	}
}

func TestRestoreSnapshot_Idempotent(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	now := metav1.Now()
	snap := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "my-snapshot"},
		Spec: v1alpha1.SnapshotSpec{
			PoolName:     "test-pool",
			ShareID:      "share-1",
			SnapshotPath: "/pvcs/.snapshots/snap-my-snapshot",
			SizeGB:       10,
		},
		Status: v1alpha1.SnapshotStatus{
			Phase:        "Ready",
			ReadyToUse:   true,
			CreationTime: &now,
		},
	}
	k.addSnapshot(snap)

	// Pre-existing SubVolume (already restored)
	const restorePVName = "pvc-b2c3d4e5-6789-01ab-cdef-234567890abc"
	existingSV := newTestSubVolume(restorePVName, "test-pool", "share-1", 10)
	k.addSubVolume(existingSV)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.RestoreSnapshot(context.Background(), "my-snapshot", AllocationRequest{
		PVName:       restorePVName,
		PVCName:      "restore-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot failed: %v", err)
	}

	if result.ShareID != "share-1" {
		t.Errorf("expected idempotent return with shareID 'share-1', got %q", result.ShareID)
	}

	// No copy should happen
	if nfs.copyCount() != 0 {
		t.Errorf("expected no copies for idempotent call, got %d", nfs.copyCount())
	}
}

func TestRestoreSnapshot_NotFound(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.RestoreSnapshot(context.Background(), "nonexistent", AllocationRequest{
		PVName:       "pvc-b2c3d4e5-6789-01ab-cdef-234567890abc",
		PVCName:      "restore-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})

	if !errors.Is(err, ErrSnapshotNotFound) {
		t.Fatalf("expected ErrSnapshotNotFound, got: %v", err)
	}
}

func TestRestoreSnapshot_PoolExhausted(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 1000, 10), // full
	)
	pool.Spec.AutoExpand = false
	pool.Spec.MaxShares = 1
	k.addPool(pool)

	now := metav1.Now()
	snap := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "my-snapshot"},
		Spec: v1alpha1.SnapshotSpec{
			PoolName:     "test-pool",
			ShareID:      "share-1",
			SnapshotPath: "/pvcs/.snapshots/snap-my-snapshot",
			SizeGB:       10,
		},
		Status: v1alpha1.SnapshotStatus{
			Phase:        "Ready",
			ReadyToUse:   true,
			CreationTime: &now,
		},
	}
	k.addSnapshot(snap)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.RestoreSnapshot(context.Background(), "my-snapshot", AllocationRequest{
		PVName:       "pvc-b2c3d4e5-6789-01ab-cdef-234567890abc",
		PVCName:      "restore-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	})

	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted, got: %v", err)
	}
}

func TestListSnapshots_BySource(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	now := metav1.Now()
	snap1 := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-1"},
		Spec: v1alpha1.SnapshotSpec{
			SourceSubVolume: testPVName,
			PoolName:        "test-pool",
			ShareID:         "share-1",
			SizeGB:          10,
		},
		Status: v1alpha1.SnapshotStatus{ReadyToUse: true, CreationTime: &now},
	}
	snap2 := &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-2"},
		Spec: v1alpha1.SnapshotSpec{
			SourceSubVolume: testPVName,
			PoolName:        "test-pool",
			ShareID:         "share-1",
			SizeGB:          10,
		},
		Status: v1alpha1.SnapshotStatus{ReadyToUse: true, CreationTime: &now},
	}
	k.addSnapshot(snap1)
	k.addSnapshot(snap2)

	mgr := newManagerForTest(k, vpc, nfs)

	sourceVolumeID := fmt.Sprintf("test-pool/share-1/%s", testPVName)
	results, err := mgr.ListSnapshots(context.Background(), sourceVolumeID)
	if err != nil {
		t.Fatalf("ListSnapshots failed: %v", err)
	}

	if len(results) != 2 {
		t.Errorf("expected 2 snapshots, got %d", len(results))
	}
}

func TestCreateSnapshot_MetricsIncremented(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("snap-metrics-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	k.addPool(pool)

	sv := newTestSubVolume(testPVName, "snap-metrics-pool", "share-1", 10)
	k.addSubVolume(sv)

	var before dto.Metric
	_ = metrics.SnapshotsTotal.WithLabelValues("snap-metrics-pool", "create", "success").Write(&before)
	beforeVal := getCounterValue(&before)

	mgr := newManagerForTest(k, vpc, nfs)

	sourceVolumeID := fmt.Sprintf("snap-metrics-pool/share-1/%s", testPVName)
	_, err := mgr.CreateSnapshot(context.Background(), "metrics-snap", sourceVolumeID, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}

	var after dto.Metric
	_ = metrics.SnapshotsTotal.WithLabelValues("snap-metrics-pool", "create", "success").Write(&after)
	afterVal := getCounterValue(&after)

	if afterVal <= beforeVal {
		t.Errorf("expected snapshot create success counter to increment, before=%f after=%f", beforeVal, afterVal)
	}
}

// ---------------------------------------------------------------------------
// CloneVolume Tests
// ---------------------------------------------------------------------------

func TestCloneVolume_SyncPath(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	// Source SubVolume: 5 GB on share-1
	source := newTestSubVolume("pvc-aaaa1111-2222-3333-4444-555566667777", "test-pool", "share-1", 5)
	k.addSubVolume(source)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.CloneVolume(context.Background(),
		"test-pool/share-1/pvc-aaaa1111-2222-3333-4444-555566667777",
		AllocationRequest{
			PVName:       testPVName,
			PVCName:      "my-clone",
			PVCNamespace: "default",
			PoolName:     "test-pool",
			RequestedGB:  5,
		},
		10, // syncThreshold = 10 GB
	)
	if err != nil {
		t.Fatalf("CloneVolume failed: %v", err)
	}

	// Should land on same share (source share has capacity)
	if result.ShareID != "share-1" {
		t.Errorf("expected share-1 (same share), got %s", result.ShareID)
	}

	// CopyDir should have been called (sync path)
	if nfs.copyCount() != 1 {
		t.Errorf("expected 1 copy, got %d", nfs.copyCount())
	}

	// SubVolume CR should exist with cloneStatus=Complete
	sv := k.getSubVolume(testPVName)
	if sv == nil {
		t.Fatal("SubVolume CR not created")
	}
	if sv.Status.CloneStatus != "Complete" {
		t.Errorf("expected cloneStatus=Complete, got %q", sv.Status.CloneStatus)
	}
	if sv.Status.Phase != "Bound" {
		t.Errorf("expected phase=Bound, got %q", sv.Status.Phase)
	}
	if sv.Spec.SourceVolume != "pvc-aaaa1111-2222-3333-4444-555566667777" {
		t.Errorf("expected sourceVolume set, got %q", sv.Spec.SourceVolume)
	}
	if sv.Spec.SourceShareID != "share-1" {
		t.Errorf("expected sourceShareID=share-1, got %q", sv.Spec.SourceShareID)
	}
}

func TestCloneVolume_AsyncPath(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	// Source SubVolume: 20 GB on share-1 (above threshold)
	source := newTestSubVolume("pvc-aaaa1111-2222-3333-4444-555566667777", "test-pool", "share-1", 20)
	k.addSubVolume(source)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.CloneVolume(context.Background(),
		"test-pool/share-1/pvc-aaaa1111-2222-3333-4444-555566667777",
		AllocationRequest{
			PVName:       testPVName,
			PVCName:      "my-clone",
			PVCNamespace: "default",
			PoolName:     "test-pool",
			RequestedGB:  20,
		},
		10, // syncThreshold = 10 GB (source is 20 GB, so async)
	)
	if err != nil {
		t.Fatalf("CloneVolume failed: %v", err)
	}

	if result.ShareID != "share-1" {
		t.Errorf("expected share-1, got %s", result.ShareID)
	}

	// CopyDir should NOT have been called (async path)
	if nfs.copyCount() != 0 {
		t.Errorf("expected 0 copies for async path, got %d", nfs.copyCount())
	}

	// SubVolume CR should exist with cloneStatus=Pending
	sv := k.getSubVolume(testPVName)
	if sv == nil {
		t.Fatal("SubVolume CR not created")
	}
	if sv.Status.CloneStatus != "Pending" {
		t.Errorf("expected cloneStatus=Pending, got %q", sv.Status.CloneStatus)
	}
	if sv.Status.Phase != "Cloning" {
		t.Errorf("expected phase=Cloning, got %q", sv.Status.Phase)
	}
}

func TestCloneVolume_SameShare(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
		newStableShare("share-2", "s2", 1000, 0, 0),
	)
	k.addPool(pool)

	source := newTestSubVolume("pvc-aaaa1111-2222-3333-4444-555566667777", "test-pool", "share-1", 5)
	k.addSubVolume(source)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.CloneVolume(context.Background(),
		"test-pool/share-1/pvc-aaaa1111-2222-3333-4444-555566667777",
		AllocationRequest{
			PVName:       testPVName,
			PVCName:      "my-clone",
			PVCNamespace: "default",
			PoolName:     "test-pool",
			RequestedGB:  5,
		},
		10,
	)
	if err != nil {
		t.Fatalf("CloneVolume failed: %v", err)
	}

	// Should prefer same share (share-1) since it has capacity
	if result.ShareID != "share-1" {
		t.Errorf("expected share-1 (same share preference), got %s", result.ShareID)
	}
}

func TestCloneVolume_CrossShare(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	// share-1 is full (source is there), share-2 has space
	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 995, 10),
		newStableShare("share-2", "s2", 1000, 0, 0),
	)
	k.addPool(pool)

	source := newTestSubVolume("pvc-aaaa1111-2222-3333-4444-555566667777", "test-pool", "share-1", 10)
	k.addSubVolume(source)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.CloneVolume(context.Background(),
		"test-pool/share-1/pvc-aaaa1111-2222-3333-4444-555566667777",
		AllocationRequest{
			PVName:       testPVName,
			PVCName:      "my-clone",
			PVCNamespace: "default",
			PoolName:     "test-pool",
			RequestedGB:  10,
		},
		10,
	)
	if err != nil {
		t.Fatalf("CloneVolume failed: %v", err)
	}

	// Should fall back to share-2 (share-1 only has 5 GB free, need 10)
	if result.ShareID != "share-2" {
		t.Errorf("expected share-2 (cross-share), got %s", result.ShareID)
	}

	// Cross-share clone is always async (different share even if small)
	sv := k.getSubVolume(testPVName)
	if sv == nil {
		t.Fatal("SubVolume CR not created")
	}
	if sv.Status.CloneStatus != "Pending" {
		t.Errorf("expected cloneStatus=Pending for cross-share, got %q", sv.Status.CloneStatus)
	}
}

func TestCloneVolume_SourceNotFound(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	k.addPool(pool)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.CloneVolume(context.Background(),
		"test-pool/share-1/pvc-nonexistent-0000-0000-000000000000",
		AllocationRequest{
			PVName:       testPVName,
			PVCName:      "my-clone",
			PVCNamespace: "default",
			PoolName:     "test-pool",
			RequestedGB:  5,
		},
		10,
	)

	if !errors.Is(err, ErrSourceNotFound) {
		t.Fatalf("expected ErrSourceNotFound, got: %v", err)
	}
}

func TestCloneVolume_InsufficientCapacity(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	// Pool is full, no auto-expand
	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 1000, 10),
	)
	pool.Spec.AutoExpand = false
	k.addPool(pool)

	source := newTestSubVolume("pvc-aaaa1111-2222-3333-4444-555566667777", "test-pool", "share-1", 5)
	k.addSubVolume(source)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.CloneVolume(context.Background(),
		"test-pool/share-1/pvc-aaaa1111-2222-3333-4444-555566667777",
		AllocationRequest{
			PVName:       testPVName,
			PVCName:      "my-clone",
			PVCNamespace: "default",
			PoolName:     "test-pool",
			RequestedGB:  5,
		},
		10,
	)

	if !errors.Is(err, ErrPoolExhausted) {
		t.Fatalf("expected ErrPoolExhausted, got: %v", err)
	}
}

func TestCloneVolume_SizeTooSmall(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	k.addPool(pool)

	source := newTestSubVolume("pvc-aaaa1111-2222-3333-4444-555566667777", "test-pool", "share-1", 50)
	k.addSubVolume(source)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.CloneVolume(context.Background(),
		"test-pool/share-1/pvc-aaaa1111-2222-3333-4444-555566667777",
		AllocationRequest{
			PVName:       testPVName,
			PVCName:      "my-clone",
			PVCNamespace: "default",
			PoolName:     "test-pool",
			RequestedGB:  10, // less than source's 50 GB
		},
		10,
	)

	if err == nil {
		t.Fatal("expected error for clone size smaller than source")
	}
	if !contains(err.Error(), "clone size") {
		t.Errorf("expected 'clone size' in error, got: %v", err)
	}
}

func TestCloneVolume_Idempotent(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	source := newTestSubVolume("pvc-aaaa1111-2222-3333-4444-555566667777", "test-pool", "share-1", 5)
	k.addSubVolume(source)

	// Pre-existing clone SubVolume (already completed)
	existingClone := newTestSubVolume(testPVName, "test-pool", "share-1", 5)
	existingClone.Spec.SourceVolume = "pvc-aaaa1111-2222-3333-4444-555566667777"
	existingClone.Status.CloneStatus = "Complete"
	k.addSubVolume(existingClone)

	mgr := newManagerForTest(k, vpc, nfs)

	result, err := mgr.CloneVolume(context.Background(),
		"test-pool/share-1/pvc-aaaa1111-2222-3333-4444-555566667777",
		AllocationRequest{
			PVName:       testPVName,
			PVCName:      "my-clone",
			PVCNamespace: "default",
			PoolName:     "test-pool",
			RequestedGB:  5,
		},
		10,
	)
	if err != nil {
		t.Fatalf("idempotent CloneVolume failed: %v", err)
	}

	if result.ShareID != "share-1" {
		t.Errorf("expected share-1, got %s", result.ShareID)
	}

	// No copy should have happened
	if nfs.copyCount() != 0 {
		t.Errorf("expected 0 copies for idempotent call, got %d", nfs.copyCount())
	}
}

func TestCloneVolume_CopyFailure(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()
	nfs.CopyErr = errors.New("NFS copy failed: disk full")

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	source := newTestSubVolume("pvc-aaaa1111-2222-3333-4444-555566667777", "test-pool", "share-1", 5)
	k.addSubVolume(source)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.CloneVolume(context.Background(),
		"test-pool/share-1/pvc-aaaa1111-2222-3333-4444-555566667777",
		AllocationRequest{
			PVName:       testPVName,
			PVCName:      "my-clone",
			PVCNamespace: "default",
			PoolName:     "test-pool",
			RequestedGB:  5,
		},
		10,
	)

	if err == nil {
		t.Fatal("expected error for copy failure")
	}
	if !contains(err.Error(), "clone copy") {
		t.Errorf("expected 'clone copy' in error, got: %v", err)
	}

	// No SubVolume should be created after copy failure
	if k.subVolumeCount() != 1 { // only the source exists
		t.Errorf("expected 1 SubVolume (source only), got %d", k.subVolumeCount())
	}
}

func TestCloneVolume_UpdatesPoolCapacity(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 1),
	)
	k.addPool(pool)

	source := newTestSubVolume("pvc-aaaa1111-2222-3333-4444-555566667777", "test-pool", "share-1", 5)
	k.addSubVolume(source)

	mgr := newManagerForTest(k, vpc, nfs)

	_, err := mgr.CloneVolume(context.Background(),
		"test-pool/share-1/pvc-aaaa1111-2222-3333-4444-555566667777",
		AllocationRequest{
			PVName:       testPVName,
			PVCName:      "my-clone",
			PVCNamespace: "default",
			PoolName:     "test-pool",
			RequestedGB:  5,
		},
		10,
	)
	if err != nil {
		t.Fatalf("CloneVolume failed: %v", err)
	}

	updatedPool := k.getPool("test-pool")
	// Original: 100 allocated + 5 for clone = 105
	if updatedPool.Status.TotalAllocatedGB != 105 {
		t.Errorf("expected TotalAllocatedGB=105, got %d", updatedPool.Status.TotalAllocatedGB)
	}
	if updatedPool.Status.TotalPVCCount != 2 {
		t.Errorf("expected TotalPVCCount=2, got %d", updatedPool.Status.TotalPVCCount)
	}
}

// ---------------------------------------------------------------------------
// Group Snapshot Tests
// ---------------------------------------------------------------------------

func setupGroupSnapshotTest(t *testing.T) (*fakeK8sClient, *fakeNFSOperations, *Manager) {
	t.Helper()
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 3),
	)
	k.addPool(pool)

	// Add 3 source SubVolumes
	sv1 := newTestSubVolume("pvc-aaaa0001-0000-0000-0000-000000000001", "test-pool", "share-1", 5)
	sv2 := newTestSubVolume("pvc-aaaa0002-0000-0000-0000-000000000002", "test-pool", "share-1", 5)
	sv3 := newTestSubVolume("pvc-aaaa0003-0000-0000-0000-000000000003", "test-pool", "share-1", 5)
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)
	k.addSubVolume(sv3)

	mgr := newManagerForTest(k, vpc, nfs)
	return k, nfs, mgr
}

func TestCreateVolumeGroupSnapshot_AllSucceed(t *testing.T) {
	k, nfs, mgr := setupGroupSnapshotTest(t)

	req := GroupSnapshotRequest{
		GroupName: "test-group-snap",
		PoolName:  "test-pool",
		SourceVolumeIDs: []string{
			"test-pool/share-1/pvc-aaaa0001-0000-0000-0000-000000000001",
			"test-pool/share-1/pvc-aaaa0002-0000-0000-0000-000000000002",
			"test-pool/share-1/pvc-aaaa0003-0000-0000-0000-000000000003",
		},
		FailurePolicy: "Abort",
	}

	result, err := mgr.CreateVolumeGroupSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot failed: %v", err)
	}

	if result.GroupName != "test-group-snap" {
		t.Errorf("expected group name 'test-group-snap', got %q", result.GroupName)
	}
	if result.PoolName != "test-pool" {
		t.Errorf("expected pool name 'test-pool', got %q", result.PoolName)
	}
	if !result.ReadyToUse {
		t.Error("expected ReadyToUse=true")
	}
	if len(result.Members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(result.Members))
	}

	// Verify individual snapshots created
	if k.snapshotCount() != 3 {
		t.Errorf("expected 3 snapshot CRs, got %d", k.snapshotCount())
	}

	// Verify NFS copies happened
	if nfs.copyCount() != 3 {
		t.Errorf("expected 3 NFS copies, got %d", nfs.copyCount())
	}

	// Verify VolumeGroupSnapshot CR was created and is Complete
	vgs := k.getGroupSnapshot("test-group-snap")
	if vgs == nil {
		t.Fatal("VolumeGroupSnapshot CR not found")
	}
	if vgs.Status.Phase != "Complete" {
		t.Errorf("expected phase 'Complete', got %q", vgs.Status.Phase)
	}
	if vgs.Status.ReadyCount != 3 {
		t.Errorf("expected ReadyCount=3, got %d", vgs.Status.ReadyCount)
	}
	if vgs.Status.FailedCount != 0 {
		t.Errorf("expected FailedCount=0, got %d", vgs.Status.FailedCount)
	}
	if vgs.Status.ConsistencyLevel != "BestEffort" {
		t.Errorf("expected consistency 'BestEffort', got %q", vgs.Status.ConsistencyLevel)
	}
}

func TestCreateVolumeGroupSnapshot_AbortPolicy_RollbackOnFailure(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 3),
	)
	k.addPool(pool)

	sv1 := newTestSubVolume("pvc-aaaa0001-0000-0000-0000-000000000001", "test-pool", "share-1", 5)
	sv2 := newTestSubVolume("pvc-aaaa0002-0000-0000-0000-000000000002", "test-pool", "share-1", 5)
	sv3 := newTestSubVolume("pvc-aaaa0003-0000-0000-0000-000000000003", "test-pool", "share-1", 5)
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)
	k.addSubVolume(sv3)

	// Make NFS copy fail after the first successful copy
	nfs.CopyErr = fmt.Errorf("NFS copy failure")
	nfs.CopyErrAfterN = 1

	mgr := newManagerForTest(k, vpc, nfs)

	req := GroupSnapshotRequest{
		GroupName: "test-group-snap",
		PoolName:  "test-pool",
		SourceVolumeIDs: []string{
			"test-pool/share-1/pvc-aaaa0001-0000-0000-0000-000000000001",
			"test-pool/share-1/pvc-aaaa0002-0000-0000-0000-000000000002",
			"test-pool/share-1/pvc-aaaa0003-0000-0000-0000-000000000003",
		},
		FailurePolicy: "Abort",
	}

	_, err := mgr.CreateVolumeGroupSnapshot(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for Abort policy with member failure")
	}
	if !errors.Is(err, ErrGroupSnapshotFailed) {
		t.Errorf("expected ErrGroupSnapshotFailed, got %v", err)
	}

	// Verify rollback: the first snapshot that succeeded should have been deleted
	// (rollback deletes completed snapshots)
	if k.snapshotCount() != 0 {
		t.Errorf("expected 0 snapshots after rollback, got %d", k.snapshotCount())
	}

	// Verify group snapshot CR exists and is Failed
	vgs := k.getGroupSnapshot("test-group-snap")
	if vgs == nil {
		t.Fatal("VolumeGroupSnapshot CR not found")
	}
	if vgs.Status.Phase != "Failed" {
		t.Errorf("expected phase 'Failed', got %q", vgs.Status.Phase)
	}
}

func TestCreateVolumeGroupSnapshot_ContinuePolicy_PartialFailure(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()

	pool := newTestPool("test-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 100, 3),
	)
	k.addPool(pool)

	sv1 := newTestSubVolume("pvc-aaaa0001-0000-0000-0000-000000000001", "test-pool", "share-1", 5)
	sv2 := newTestSubVolume("pvc-aaaa0002-0000-0000-0000-000000000002", "test-pool", "share-1", 5)
	sv3 := newTestSubVolume("pvc-aaaa0003-0000-0000-0000-000000000003", "test-pool", "share-1", 5)
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)
	k.addSubVolume(sv3)

	// Make NFS copy fail after the first successful copy
	nfs.CopyErr = fmt.Errorf("NFS copy failure")
	nfs.CopyErrAfterN = 1

	mgr := newManagerForTest(k, vpc, nfs)

	req := GroupSnapshotRequest{
		GroupName: "test-group-snap",
		PoolName:  "test-pool",
		SourceVolumeIDs: []string{
			"test-pool/share-1/pvc-aaaa0001-0000-0000-0000-000000000001",
			"test-pool/share-1/pvc-aaaa0002-0000-0000-0000-000000000002",
			"test-pool/share-1/pvc-aaaa0003-0000-0000-0000-000000000003",
		},
		FailurePolicy: "Continue",
	}

	result, err := mgr.CreateVolumeGroupSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot with Continue policy should not error, got: %v", err)
	}

	// With Continue policy: first succeeds, second fails, third fails (CopyErr still active)
	if result.ReadyToUse {
		t.Error("expected ReadyToUse=false for partial failure")
	}
	if len(result.Members) != 1 {
		t.Errorf("expected 1 successful member, got %d", len(result.Members))
	}

	// Verify group snapshot CR shows PartialFailure
	vgs := k.getGroupSnapshot("test-group-snap")
	if vgs == nil {
		t.Fatal("VolumeGroupSnapshot CR not found")
	}
	if vgs.Status.Phase != "PartialFailure" {
		t.Errorf("expected phase 'PartialFailure', got %q", vgs.Status.Phase)
	}
	if vgs.Status.ReadyCount != 1 {
		t.Errorf("expected ReadyCount=1, got %d", vgs.Status.ReadyCount)
	}
	if vgs.Status.FailedCount != 2 {
		t.Errorf("expected FailedCount=2, got %d", vgs.Status.FailedCount)
	}
}

func TestCreateVolumeGroupSnapshot_EmptySourceList(t *testing.T) {
	_, _, mgr := setupGroupSnapshotTest(t)

	req := GroupSnapshotRequest{
		GroupName:       "test-group-snap",
		PoolName:        "test-pool",
		SourceVolumeIDs: []string{},
		FailurePolicy:   "Abort",
	}

	_, err := mgr.CreateVolumeGroupSnapshot(context.Background(), req)
	if !errors.Is(err, ErrEmptySourceList) {
		t.Errorf("expected ErrEmptySourceList, got %v", err)
	}
}

func TestCreateVolumeGroupSnapshot_Idempotent(t *testing.T) {
	k, _, mgr := setupGroupSnapshotTest(t)

	req := GroupSnapshotRequest{
		GroupName: "test-group-snap",
		PoolName:  "test-pool",
		SourceVolumeIDs: []string{
			"test-pool/share-1/pvc-aaaa0001-0000-0000-0000-000000000001",
			"test-pool/share-1/pvc-aaaa0002-0000-0000-0000-000000000002",
			"test-pool/share-1/pvc-aaaa0003-0000-0000-0000-000000000003",
		},
		FailurePolicy: "Abort",
	}

	// First call
	result1, err := mgr.CreateVolumeGroupSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("First CreateVolumeGroupSnapshot failed: %v", err)
	}

	initialSnapshotCount := k.snapshotCount()

	// Second call (idempotent)
	result2, err := mgr.CreateVolumeGroupSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("Second CreateVolumeGroupSnapshot failed: %v", err)
	}

	// Should return the same result without creating new snapshots
	if result2.GroupName != result1.GroupName {
		t.Errorf("idempotent call returned different group name: %q vs %q", result2.GroupName, result1.GroupName)
	}
	if len(result2.Members) != len(result1.Members) {
		t.Errorf("idempotent call returned different member count: %d vs %d", len(result2.Members), len(result1.Members))
	}

	// No new snapshots should have been created
	if k.snapshotCount() != initialSnapshotCount {
		t.Errorf("expected snapshot count to remain %d, got %d", initialSnapshotCount, k.snapshotCount())
	}
}

func TestDeleteVolumeGroupSnapshot_DeletesAllMembers(t *testing.T) {
	k, _, mgr := setupGroupSnapshotTest(t)

	// Create a group snapshot first
	req := GroupSnapshotRequest{
		GroupName: "test-group-snap",
		PoolName:  "test-pool",
		SourceVolumeIDs: []string{
			"test-pool/share-1/pvc-aaaa0001-0000-0000-0000-000000000001",
			"test-pool/share-1/pvc-aaaa0002-0000-0000-0000-000000000002",
			"test-pool/share-1/pvc-aaaa0003-0000-0000-0000-000000000003",
		},
		FailurePolicy: "Abort",
	}

	_, err := mgr.CreateVolumeGroupSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot failed: %v", err)
	}

	if k.snapshotCount() != 3 {
		t.Fatalf("expected 3 snapshots before delete, got %d", k.snapshotCount())
	}
	if k.groupSnapshotCount() != 1 {
		t.Fatalf("expected 1 group snapshot before delete, got %d", k.groupSnapshotCount())
	}

	// Delete the group snapshot
	err = mgr.DeleteVolumeGroupSnapshot(context.Background(), "test-group-snap")
	if err != nil {
		t.Fatalf("DeleteVolumeGroupSnapshot failed: %v", err)
	}

	// All individual snapshots should be deleted
	if k.snapshotCount() != 0 {
		t.Errorf("expected 0 snapshots after delete, got %d", k.snapshotCount())
	}

	// Group snapshot CR should be deleted
	if k.groupSnapshotCount() != 0 {
		t.Errorf("expected 0 group snapshots after delete, got %d", k.groupSnapshotCount())
	}
}

func TestDeleteVolumeGroupSnapshot_IdempotentNotFound(t *testing.T) {
	_, _, mgr := setupGroupSnapshotTest(t)

	// Delete a non-existent group snapshot — should return nil (idempotent)
	err := mgr.DeleteVolumeGroupSnapshot(context.Background(), "nonexistent")
	if err != nil {
		t.Errorf("expected nil error for non-existent group, got %v", err)
	}
}

func TestCreateVolumeGroupSnapshot_CopyOrderRespected(t *testing.T) {
	k, nfs, mgr := setupGroupSnapshotTest(t)

	// Specify CopyOrder: 3, 1, 2 (reverse of natural alphabetical)
	req := GroupSnapshotRequest{
		GroupName: "test-group-snap",
		PoolName:  "test-pool",
		SourceVolumeIDs: []string{
			"test-pool/share-1/pvc-aaaa0001-0000-0000-0000-000000000001",
			"test-pool/share-1/pvc-aaaa0002-0000-0000-0000-000000000002",
			"test-pool/share-1/pvc-aaaa0003-0000-0000-0000-000000000003",
		},
		CopyOrder: []string{
			"pvc-aaaa0003-0000-0000-0000-000000000003",
			"pvc-aaaa0001-0000-0000-0000-000000000001",
			"pvc-aaaa0002-0000-0000-0000-000000000002",
		},
		FailurePolicy: "Abort",
	}

	result, err := mgr.CreateVolumeGroupSnapshot(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot failed: %v", err)
	}

	// Verify members are in the specified copy order
	if len(result.Members) != 3 {
		t.Fatalf("expected 3 members, got %d", len(result.Members))
	}

	// Verify the VolumeGroupSnapshot CR members are in order
	vgs := k.getGroupSnapshot("test-group-snap")
	if vgs == nil {
		t.Fatal("VolumeGroupSnapshot CR not found")
	}
	expectedOrder := []string{
		"pvc-aaaa0003-0000-0000-0000-000000000003",
		"pvc-aaaa0001-0000-0000-0000-000000000001",
		"pvc-aaaa0002-0000-0000-0000-000000000002",
	}
	for i, member := range vgs.Status.Members {
		if member.SubVolumeName != expectedOrder[i] {
			t.Errorf("member %d: expected SubVolumeName %q, got %q", i, expectedOrder[i], member.SubVolumeName)
		}
	}

	// Verify NFS copies happened (3 copies)
	if nfs.copyCount() != 3 {
		t.Errorf("expected 3 NFS copies, got %d", nfs.copyCount())
	}
}

func TestCreateVolumeGroupSnapshot_SourceNotFound(t *testing.T) {
	_, _, mgr := setupGroupSnapshotTest(t)

	req := GroupSnapshotRequest{
		GroupName: "test-group-snap",
		PoolName:  "test-pool",
		SourceVolumeIDs: []string{
			"test-pool/share-1/pvc-aaaa0001-0000-0000-0000-000000000001",
			"test-pool/share-1/pvc-nonexistent-0000-0000-000000000000", // doesn't exist
		},
		FailurePolicy: "Abort",
	}

	_, err := mgr.CreateVolumeGroupSnapshot(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for missing source volume")
	}
	if !errors.Is(err, ErrSourceNotFound) {
		t.Errorf("expected ErrSourceNotFound, got %v", err)
	}
}

func TestCreateVolumeGroupSnapshot_CrossPoolRejected(t *testing.T) {
	k, _, mgr := setupGroupSnapshotTest(t)

	// Add a SubVolume in a different pool
	otherPoolSV := newTestSubVolume("pvc-bbbb0001-0000-0000-0000-000000000001", "other-pool", "share-2", 5)
	k.addSubVolume(otherPoolSV)

	req := GroupSnapshotRequest{
		GroupName: "test-group-snap",
		PoolName:  "test-pool",
		SourceVolumeIDs: []string{
			"test-pool/share-1/pvc-aaaa0001-0000-0000-0000-000000000001",
			"test-pool/share-2/pvc-bbbb0001-0000-0000-0000-000000000001", // different pool
		},
		FailurePolicy: "Abort",
	}

	_, err := mgr.CreateVolumeGroupSnapshot(context.Background(), req)
	if err == nil {
		t.Fatal("expected error for cross-pool source")
	}
	if !contains(err.Error(), "belongs to pool") {
		t.Errorf("expected cross-pool error, got %v", err)
	}
}

// ---------------------------------------------------------------------------
// Helper
// ---------------------------------------------------------------------------

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
