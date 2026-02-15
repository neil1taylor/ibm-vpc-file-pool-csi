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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Fake NFS Operations
// ---------------------------------------------------------------------------

type fakeNFSOperations struct {
	mu       sync.Mutex
	dirs     map[string]os.FileMode
	chowns   map[string][2]int // path → [uid, gid]
	MkdirErr error
	RemoveErr error
	ChownErr error
	ChmodErr error
}

func newFakeNFSOperations() *fakeNFSOperations {
	return &fakeNFSOperations{
		dirs:   make(map[string]os.FileMode),
		chowns: make(map[string][2]int),
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
	mu         sync.Mutex
	pools      map[string]*v1alpha1.FileSharePool
	subVolumes map[string]*v1alpha1.SubVolume

	GetPoolErr            error
	UpdatePoolStatusErr   error
	GetSubVolumeErr       error
	CreateSubVolumeErr    error
	UpdateSubVolumeErr    error
	DeleteSubVolumeErr    error
}

var _ = (interface{ GetFileSharePool(context.Context, string) (*v1alpha1.FileSharePool, error) })((*fakeK8sClient)(nil))

func newFakeK8sClient() *fakeK8sClient {
	return &fakeK8sClient{
		pools:      make(map[string]*v1alpha1.FileSharePool),
		subVolumes: make(map[string]*v1alpha1.SubVolume),
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
	existing.Status = sv.Status
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
			ShareCount:       int32(len(shares)),
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

	got, err := selectShare("spread", shares, 10)
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

	got, err := selectShare("binpack", shares, 10)
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

	_, err := selectShare("spread", shares, 10)
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

	got, err := selectShare("spread", shares, 10)
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

	_, err := selectShare("unknown", shares, 10)
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
