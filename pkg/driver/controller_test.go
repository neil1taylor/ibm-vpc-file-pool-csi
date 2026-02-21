package driver

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Mock Pool Manager
// ---------------------------------------------------------------------------

type mockPoolManager struct {
	allocateResult *pool.AllocationResult
	allocateErr    error
	deallocateErr  error
	expandErr      error

	lastAllocateReq    pool.AllocationRequest
	lastDeallocateName string
	lastExpandName     string
	lastExpandSizeGB   int64

	// Snapshot fields
	createSnapshotResult  *pool.SnapshotResult
	createSnapshotErr     error
	deleteSnapshotErr     error
	listSnapshotsResults  []pool.SnapshotResult
	listSnapshotsErr      error
	restoreSnapshotResult *pool.AllocationResult
	restoreSnapshotErr    error

	lastCreateSnapshotName     string
	lastCreateSnapshotSourceID string
	lastDeleteSnapshotName     string
	lastListSnapshotsSourceID  string
	lastRestoreSnapshotName    string
	lastRestoreSnapshotReq     pool.AllocationRequest

	// Clone fields
	cloneVolumeResult        *pool.AllocationResult
	cloneVolumeErr           error
	lastCloneSourceVolumeID  string
	lastCloneReq             pool.AllocationRequest
	lastCloneSyncThresholdGB int64

	// Group snapshot fields
	createGroupSnapshotResult *pool.GroupSnapshotResult
	createGroupSnapshotErr    error
	deleteGroupSnapshotErr    error

	lastCreateGroupSnapshotReq  pool.GroupSnapshotRequest
	lastDeleteGroupSnapshotName string
}

func (m *mockPoolManager) Allocate(_ context.Context, req pool.AllocationRequest) (*pool.AllocationResult, error) {
	m.lastAllocateReq = req
	if m.allocateErr != nil {
		return nil, m.allocateErr
	}
	return m.allocateResult, nil
}

func (m *mockPoolManager) Deallocate(_ context.Context, subVolumeName string) error {
	m.lastDeallocateName = subVolumeName
	return m.deallocateErr
}

func (m *mockPoolManager) Expand(_ context.Context, subVolumeName string, newSizeGB int64) error {
	m.lastExpandName = subVolumeName
	m.lastExpandSizeGB = newSizeGB
	return m.expandErr
}

func (m *mockPoolManager) CreateSnapshot(_ context.Context, snapshotName, sourceVolumeID string, _ map[string]string) (*pool.SnapshotResult, error) {
	m.lastCreateSnapshotName = snapshotName
	m.lastCreateSnapshotSourceID = sourceVolumeID
	if m.createSnapshotErr != nil {
		return nil, m.createSnapshotErr
	}
	return m.createSnapshotResult, nil
}

func (m *mockPoolManager) DeleteSnapshot(_ context.Context, snapshotName string) error {
	m.lastDeleteSnapshotName = snapshotName
	return m.deleteSnapshotErr
}

func (m *mockPoolManager) ListSnapshots(_ context.Context, sourceVolumeID string) ([]pool.SnapshotResult, error) {
	m.lastListSnapshotsSourceID = sourceVolumeID
	if m.listSnapshotsErr != nil {
		return nil, m.listSnapshotsErr
	}
	return m.listSnapshotsResults, nil
}

func (m *mockPoolManager) RestoreSnapshot(_ context.Context, snapshotName string, req pool.AllocationRequest) (*pool.AllocationResult, error) {
	m.lastRestoreSnapshotName = snapshotName
	m.lastRestoreSnapshotReq = req
	if m.restoreSnapshotErr != nil {
		return nil, m.restoreSnapshotErr
	}
	return m.restoreSnapshotResult, nil
}

func (m *mockPoolManager) CloneVolume(_ context.Context, sourceVolumeID string, req pool.AllocationRequest, syncThresholdGB int64) (*pool.AllocationResult, error) {
	m.lastCloneSourceVolumeID = sourceVolumeID
	m.lastCloneReq = req
	m.lastCloneSyncThresholdGB = syncThresholdGB
	if m.cloneVolumeErr != nil {
		return nil, m.cloneVolumeErr
	}
	return m.cloneVolumeResult, nil
}

func (m *mockPoolManager) CreateVolumeGroupSnapshot(_ context.Context, req pool.GroupSnapshotRequest) (*pool.GroupSnapshotResult, error) {
	m.lastCreateGroupSnapshotReq = req
	if m.createGroupSnapshotErr != nil {
		return nil, m.createGroupSnapshotErr
	}
	return m.createGroupSnapshotResult, nil
}

func (m *mockPoolManager) DeleteVolumeGroupSnapshot(_ context.Context, groupName string) error {
	m.lastDeleteGroupSnapshotName = groupName
	return m.deleteGroupSnapshotErr
}

// ---------------------------------------------------------------------------
// Mock K8s Client (only GetSubVolume needed for controller idempotency path)
// ---------------------------------------------------------------------------

type mockK8sClient struct {
	subVolumes     map[string]*v1alpha1.SubVolume
	snapshots      map[string]*v1alpha1.Snapshot
	groupSnapshots map[string]*v1alpha1.VolumeGroupSnapshot
	getErr         error
}

func newMockK8sClient() *mockK8sClient {
	return &mockK8sClient{
		subVolumes:     make(map[string]*v1alpha1.SubVolume),
		snapshots:      make(map[string]*v1alpha1.Snapshot),
		groupSnapshots: make(map[string]*v1alpha1.VolumeGroupSnapshot),
	}
}

func (m *mockK8sClient) GetFileSharePool(_ context.Context, _ string) (*v1alpha1.FileSharePool, error) {
	return nil, fmt.Errorf("not implemented")
}

func (m *mockK8sClient) ListFileSharePools(_ context.Context) ([]v1alpha1.FileSharePool, error) {
	return nil, nil
}

func (m *mockK8sClient) UpdateFileSharePoolStatus(_ context.Context, _ *v1alpha1.FileSharePool) error {
	return nil
}

func (m *mockK8sClient) UpdateFileSharePool(_ context.Context, _ *v1alpha1.FileSharePool) error {
	return nil
}

func (m *mockK8sClient) GetSubVolume(_ context.Context, name string) (*v1alpha1.SubVolume, error) {
	if m.getErr != nil {
		return nil, m.getErr
	}
	sv, ok := m.subVolumes[name]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return sv, nil
}

func (m *mockK8sClient) ListSubVolumes(_ context.Context, _ string) ([]v1alpha1.SubVolume, error) {
	return nil, nil
}

func (m *mockK8sClient) ListSubVolumesByShare(_ context.Context, _ string) ([]v1alpha1.SubVolume, error) {
	return nil, nil
}

func (m *mockK8sClient) ListCloneSubVolumes(_ context.Context) ([]v1alpha1.SubVolume, error) {
	return nil, nil
}

func (m *mockK8sClient) CreateSubVolume(_ context.Context, _ *v1alpha1.SubVolume) error {
	return nil
}

func (m *mockK8sClient) UpdateSubVolume(_ context.Context, _ *v1alpha1.SubVolume) error {
	return nil
}

func (m *mockK8sClient) UpdateSubVolumeStatus(_ context.Context, _ *v1alpha1.SubVolume) error {
	return nil
}

func (m *mockK8sClient) DeleteSubVolume(_ context.Context, _ string) error {
	return nil
}

func (m *mockK8sClient) GetConfigMapValue(_ context.Context, _, _, _ string) (string, error) {
	return "", fmt.Errorf("not implemented")
}

func (m *mockK8sClient) GetNodeZone(_ context.Context, _ string) (string, error) {
	return "us-south-1", nil
}

func (m *mockK8sClient) GetSnapshot(_ context.Context, name string) (*v1alpha1.Snapshot, error) {
	snap, ok := m.snapshots[name]
	if !ok {
		return nil, fmt.Errorf("not found")
	}
	return snap, nil
}

func (m *mockK8sClient) ListSnapshots(_ context.Context, _ string) ([]v1alpha1.Snapshot, error) {
	return nil, nil
}

func (m *mockK8sClient) ListSnapshotsByShare(_ context.Context, _ string) ([]v1alpha1.Snapshot, error) {
	return nil, nil
}

func (m *mockK8sClient) ListSnapshotsBySource(_ context.Context, _ string) ([]v1alpha1.Snapshot, error) {
	return nil, nil
}

func (m *mockK8sClient) CreateSnapshot(_ context.Context, _ *v1alpha1.Snapshot) error {
	return nil
}

func (m *mockK8sClient) UpdateSnapshot(_ context.Context, _ *v1alpha1.Snapshot) error {
	return nil
}

func (m *mockK8sClient) UpdateSnapshotStatus(_ context.Context, _ *v1alpha1.Snapshot) error {
	return nil
}

func (m *mockK8sClient) DeleteSnapshot(_ context.Context, _ string) error {
	return nil
}
func (m *mockK8sClient) GetVolumeGroupSnapshot(_ context.Context, name string) (*v1alpha1.VolumeGroupSnapshot, error) {
	vgs, ok := m.groupSnapshots[name]
	if !ok {
		return nil, fmt.Errorf("group snapshot %q not found", name)
	}
	return vgs, nil
}
func (m *mockK8sClient) CreateVolumeGroupSnapshot(_ context.Context, _ *v1alpha1.VolumeGroupSnapshot) error {
	return nil
}
func (m *mockK8sClient) UpdateVolumeGroupSnapshotStatus(_ context.Context, _ *v1alpha1.VolumeGroupSnapshot) error {
	return nil
}
func (m *mockK8sClient) DeleteVolumeGroupSnapshot(_ context.Context, _ string) error {
	return nil
}
func (m *mockK8sClient) ListVolumeGroupSnapshots(_ context.Context, _ string) ([]v1alpha1.VolumeGroupSnapshot, error) {
	return nil, nil
}
func (m *mockK8sClient) GetReplicationPolicy(_ context.Context, _ string) (*v1alpha1.ReplicationPolicy, error) {
	return nil, fmt.Errorf("not implemented")
}
func (m *mockK8sClient) ListReplicationPolicies(_ context.Context) ([]v1alpha1.ReplicationPolicy, error) {
	return nil, nil
}
func (m *mockK8sClient) CreateReplicationPolicy(_ context.Context, _ *v1alpha1.ReplicationPolicy) error {
	return nil
}
func (m *mockK8sClient) UpdateReplicationPolicyStatus(_ context.Context, _ *v1alpha1.ReplicationPolicy) error {
	return nil
}
func (m *mockK8sClient) DeleteReplicationPolicy(_ context.Context, _ string) error { return nil }

func (m *mockK8sClient) GetStorageClass(_ context.Context, _ string) (*storagev1.StorageClass, error) {
	return nil, fmt.Errorf("not found")
}
func (m *mockK8sClient) CreateStorageClass(_ context.Context, _ *storagev1.StorageClass) error {
	return nil
}

// ---------------------------------------------------------------------------
// Test Helper
// ---------------------------------------------------------------------------

// closedChan returns a pre-closed channel for tests that need a ready driver.
func closedChan() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func newTestDriver(pm pool.PoolManager, k8s *mockK8sClient) *Driver {
	d := &Driver{
		name:        DriverName,
		version:     "test",
		nodeID:      "test-node",
		mode:        "controller",
		poolManager: pm,
		ready:       closedChan(),
	}
	if k8s != nil {
		d.k8sClient = k8s
	}
	return d
}

func newTestDriverNotReady(pm pool.PoolManager, k8s *mockK8sClient) *Driver {
	d := &Driver{
		name:        DriverName,
		version:     "test",
		nodeID:      "test-node",
		mode:        "controller",
		poolManager: pm,
		ready:       make(chan struct{}), // never closed
	}
	if k8s != nil {
		d.k8sClient = k8s
	}
	return d
}

func assertGRPCCode(t *testing.T, err error, expected codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected gRPC error with code %v, got nil", expected)
	}
	st, ok := status.FromError(err)
	if !ok {
		t.Fatalf("expected gRPC status error, got: %v", err)
	}
	if st.Code() != expected {
		t.Errorf("expected gRPC code %v, got %v (message: %s)", expected, st.Code(), st.Message())
	}
}

func gbToBytes(gb int64) int64 {
	return gb * (1 << 30)
}

// ---------------------------------------------------------------------------
// CreateVolume Tests
// ---------------------------------------------------------------------------

func TestCreateVolume_MissingName(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		// Name intentionally empty
		Parameters: map[string]string{"pool": "test-pool"},
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestCreateVolume_MissingPoolParameter(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{}, // no "pool" key
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestCreateVolume_NilParameters(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-test",
		// Parameters is nil
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestCreateVolume_PoolNotFound(t *testing.T) {
	pm := &mockPoolManager{
		allocateErr: pool.ErrPoolNotFound,
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "nonexistent"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
	})

	assertGRPCCode(t, err, codes.NotFound)
}

func TestCreateVolume_PoolExhausted(t *testing.T) {
	pm := &mockPoolManager{
		allocateErr: pool.ErrPoolExhausted,
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "full-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
	})

	assertGRPCCode(t, err, codes.ResourceExhausted)
}

func TestCreateVolume_ShareCreationPending(t *testing.T) {
	pm := &mockPoolManager{
		allocateErr: pool.ErrShareCreationPending,
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "expanding-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
	})

	assertGRPCCode(t, err, codes.Unavailable)
}

func TestCreateVolume_InternalError(t *testing.T) {
	pm := &mockPoolManager{
		allocateErr: fmt.Errorf("unexpected internal failure"),
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
	})

	assertGRPCCode(t, err, codes.Internal)
}

func TestCreateVolume_Success(t *testing.T) {
	pm := &mockPoolManager{
		allocateResult: &pool.AllocationResult{
			ShareID:       "r006-share-001",
			MountTargetIP: "10.240.0.1",
			SubPath:       "/pvcs/pvc-test",
			SharePath:     "/",
		},
	}
	d := newTestDriver(pm, nil)

	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	vol := resp.GetVolume()
	if vol == nil {
		t.Fatal("expected volume in response")
	}

	// Volume ID: pool/shareID/pvName
	expectedID := "test-pool/r006-share-001/pvc-test"
	if vol.VolumeId != expectedID {
		t.Errorf("expected volumeID %q, got %q", expectedID, vol.VolumeId)
	}

	if vol.CapacityBytes != gbToBytes(10) {
		t.Errorf("expected capacity %d, got %d", gbToBytes(10), vol.CapacityBytes)
	}

	// Volume context
	ctx := vol.VolumeContext
	if ctx["server"] != "10.240.0.1" {
		t.Errorf("expected server '10.240.0.1', got %q", ctx["server"])
	}
	if ctx["share"] != "/" {
		t.Errorf("expected share '/', got %q", ctx["share"])
	}
	if ctx["subDir"] != "/pvcs/pvc-test" {
		t.Errorf("expected subDir '/pvcs/pvc-test', got %q", ctx["subDir"])
	}
	if ctx["pool"] != "test-pool" {
		t.Errorf("expected pool 'test-pool', got %q", ctx["pool"])
	}
	if ctx["shareID"] != "r006-share-001" {
		t.Errorf("expected shareID 'r006-share-001', got %q", ctx["shareID"])
	}
}

func TestCreateVolume_Success_VerifiesAllocationRequest(t *testing.T) {
	pm := &mockPoolManager{
		allocateResult: &pool.AllocationResult{
			ShareID:       "share-1",
			MountTargetIP: "10.240.0.1",
			SubPath:       "/pvcs/pvc-test",
			SharePath:     "/",
		},
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-test",
		Parameters: map[string]string{
			"pool":                             "my-pool",
			"csi.storage.k8s.io/pvc/name":      "my-pvc",
			"csi.storage.k8s.io/pvc/namespace": "my-ns",
			"uid":                              "1000",
			"gid":                              "2000",
			"permissions":                      "0750",
		},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(5),
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{
				{Segments: map[string]string{"topology.kubernetes.io/zone": "us-south-1"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	req := pm.lastAllocateReq
	if req.PVName != "pvc-test" {
		t.Errorf("expected PVName 'pvc-test', got %q", req.PVName)
	}
	if req.PoolName != "my-pool" {
		t.Errorf("expected PoolName 'my-pool', got %q", req.PoolName)
	}
	if req.PVCName != "my-pvc" {
		t.Errorf("expected PVCName 'my-pvc', got %q", req.PVCName)
	}
	if req.PVCNamespace != "my-ns" {
		t.Errorf("expected PVCNamespace 'my-ns', got %q", req.PVCNamespace)
	}
	if req.RequestedGB != 5 {
		t.Errorf("expected RequestedGB 5, got %d", req.RequestedGB)
	}
	if req.Zone != "us-south-1" {
		t.Errorf("expected Zone 'us-south-1', got %q", req.Zone)
	}
	if req.UID == nil || *req.UID != 1000 {
		t.Errorf("expected UID 1000, got %v", req.UID)
	}
	if req.GID == nil || *req.GID != 2000 {
		t.Errorf("expected GID 2000, got %v", req.GID)
	}
	if req.Permissions != "0750" {
		t.Errorf("expected Permissions '0750', got %q", req.Permissions)
	}
}

func TestCreateVolume_Success_TopologyInResponse(t *testing.T) {
	pm := &mockPoolManager{
		allocateResult: &pool.AllocationResult{
			ShareID:       "share-1",
			MountTargetIP: "10.240.0.1",
			SubPath:       "/pvcs/pvc-test",
			SharePath:     "/",
		},
	}
	d := newTestDriver(pm, nil)

	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{
				{Segments: map[string]string{"topology.kubernetes.io/zone": "us-south-2"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	topos := resp.GetVolume().GetAccessibleTopology()
	if len(topos) != 1 {
		t.Fatalf("expected 1 topology, got %d", len(topos))
	}
	if zone := topos[0].GetSegments()["topology.kubernetes.io/zone"]; zone != "us-south-2" {
		t.Errorf("expected zone 'us-south-2', got %q", zone)
	}
}

func TestCreateVolume_MinimumCapacity(t *testing.T) {
	pm := &mockPoolManager{
		allocateResult: &pool.AllocationResult{
			ShareID:       "share-1",
			MountTargetIP: "10.240.0.1",
			SubPath:       "/pvcs/pvc-test",
			SharePath:     "/",
		},
	}
	d := newTestDriver(pm, nil)

	// Request with 0 bytes → should default to 1 GB
	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: 0,
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	if pm.lastAllocateReq.RequestedGB != 1 {
		t.Errorf("expected minimum 1 GB, got %d", pm.lastAllocateReq.RequestedGB)
	}
}

func TestCreateVolume_RoundsUpToGB(t *testing.T) {
	pm := &mockPoolManager{
		allocateResult: &pool.AllocationResult{
			ShareID:       "share-1",
			MountTargetIP: "10.240.0.1",
			SubPath:       "/pvcs/pvc-test",
			SharePath:     "/",
		},
	}
	d := newTestDriver(pm, nil)

	// 1.5 GB → should round up to 2 GB
	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(1) + 500*1024*1024, // 1.5 GB
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	if pm.lastAllocateReq.RequestedGB != 2 {
		t.Errorf("expected 2 GB (rounded up), got %d", pm.lastAllocateReq.RequestedGB)
	}
}

func TestCreateVolume_Idempotent_ViaK8sClient(t *testing.T) {
	pm := &mockPoolManager{} // not called
	k := newMockK8sClient()

	// Pre-existing SubVolume
	k.subVolumes["pvc-existing"] = &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-existing"},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           "test-pool",
			ShareID:            "share-1",
			ShareMountTargetIP: "10.240.0.1",
			SubPath:            "/pvcs/pvc-existing",
			RequestedGB:        10,
			PVName:             "pvc-existing",
			PVCName:            "my-pvc",
			PVCNamespace:       "default",
		},
	}

	d := newTestDriver(pm, k)

	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-existing",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
	})
	if err != nil {
		t.Fatalf("idempotent CreateVolume failed: %v", err)
	}

	vol := resp.GetVolume()
	expectedID := "test-pool/share-1/pvc-existing"
	if vol.VolumeId != expectedID {
		t.Errorf("expected volumeID %q, got %q", expectedID, vol.VolumeId)
	}
	if vol.CapacityBytes != gbToBytes(10) {
		t.Errorf("expected capacity %d, got %d", gbToBytes(10), vol.CapacityBytes)
	}
	if vol.VolumeContext["subDir"] != "/pvcs/pvc-existing" {
		t.Errorf("expected subDir '/pvcs/pvc-existing', got %q", vol.VolumeContext["subDir"])
	}
}

func TestCreateVolume_Idempotent_TwoCalls(t *testing.T) {
	pm := &mockPoolManager{
		allocateResult: &pool.AllocationResult{
			ShareID:       "share-1",
			MountTargetIP: "10.240.0.1",
			SubPath:       "/pvcs/pvc-test",
			SharePath:     "/",
		},
	}
	// No k8sClient → won't short-circuit on first call; tests pool manager idempotency handling
	d := newTestDriver(pm, nil)

	resp1, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
	})
	if err != nil {
		t.Fatalf("first CreateVolume failed: %v", err)
	}

	resp2, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
	})
	if err != nil {
		t.Fatalf("second CreateVolume failed: %v", err)
	}

	if resp1.GetVolume().VolumeId != resp2.GetVolume().VolumeId {
		t.Errorf("idempotent calls should return same volume ID: %q vs %q",
			resp1.GetVolume().VolumeId, resp2.GetVolume().VolumeId)
	}
}

// ---------------------------------------------------------------------------
// DeleteVolume Tests
// ---------------------------------------------------------------------------

func TestDeleteVolume_MissingVolumeID(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "",
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestDeleteVolume_InvalidVolumeID(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "invalid-no-slashes",
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestDeleteVolume_InvalidVolumeID_TwoParts(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "pool/share", // only 2 parts
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestDeleteVolume_NotFound(t *testing.T) {
	pm := &mockPoolManager{
		deallocateErr: pool.ErrSubVolumeNotFound,
	}
	d := newTestDriver(pm, nil)

	resp, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "test-pool/share-1/pvc-missing",
	})

	// Not found should be idempotent success
	if err != nil {
		t.Fatalf("expected idempotent success for not-found, got: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestDeleteVolume_Success(t *testing.T) {
	pm := &mockPoolManager{
		deallocateErr: nil,
	}
	d := newTestDriver(pm, nil)

	resp, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "test-pool/share-1/pvc-test",
	})
	if err != nil {
		t.Fatalf("DeleteVolume failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	if pm.lastDeallocateName != "pvc-test" {
		t.Errorf("expected deallocate called with 'pvc-test', got %q", pm.lastDeallocateName)
	}
}

func TestDeleteVolume_InternalError(t *testing.T) {
	pm := &mockPoolManager{
		deallocateErr: fmt.Errorf("NFS failure"),
	}
	d := newTestDriver(pm, nil)

	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "test-pool/share-1/pvc-test",
	})

	assertGRPCCode(t, err, codes.Internal)
}

func TestDeleteVolume_ParsesVolumeIDCorrectly(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "my-pool/r006-share-abc/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab",
	})
	if err != nil {
		t.Fatalf("DeleteVolume failed: %v", err)
	}

	if pm.lastDeallocateName != "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab" {
		t.Errorf("expected pv name parsed from volume ID, got %q", pm.lastDeallocateName)
	}
}

// ---------------------------------------------------------------------------
// ControllerExpandVolume Tests
// ---------------------------------------------------------------------------

func TestExpandVolume_MissingVolumeID(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "",
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestExpandVolume_InvalidVolumeID(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "bad-id",
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestExpandVolume_Success(t *testing.T) {
	pm := &mockPoolManager{expandErr: nil}
	d := newTestDriver(pm, nil)

	resp, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "test-pool/share-1/pvc-test",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(50),
		},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume failed: %v", err)
	}

	if resp.CapacityBytes != gbToBytes(50) {
		t.Errorf("expected capacity %d, got %d", gbToBytes(50), resp.CapacityBytes)
	}
	if resp.NodeExpansionRequired {
		t.Error("expected NodeExpansionRequired=false for NFS subdirectory expansion")
	}

	if pm.lastExpandName != "pvc-test" {
		t.Errorf("expected Expand called with 'pvc-test', got %q", pm.lastExpandName)
	}
	if pm.lastExpandSizeGB != 50 {
		t.Errorf("expected Expand called with 50 GB, got %d", pm.lastExpandSizeGB)
	}
}

func TestExpandVolume_InsufficientCapacity(t *testing.T) {
	pm := &mockPoolManager{
		expandErr: pool.ErrInsufficientShareCapacity,
	}
	d := newTestDriver(pm, nil)

	_, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "test-pool/share-1/pvc-test",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(5000),
		},
	})

	assertGRPCCode(t, err, codes.ResourceExhausted)
}

func TestExpandVolume_InternalError(t *testing.T) {
	pm := &mockPoolManager{
		expandErr: fmt.Errorf("unexpected error"),
	}
	d := newTestDriver(pm, nil)

	_, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "test-pool/share-1/pvc-test",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(50),
		},
	})

	assertGRPCCode(t, err, codes.Internal)
}

func TestExpandVolume_RoundsUpToGB(t *testing.T) {
	pm := &mockPoolManager{expandErr: nil}
	d := newTestDriver(pm, nil)

	_, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "test-pool/share-1/pvc-test",
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10) + 1, // slightly over 10 GB
		},
	})
	if err != nil {
		t.Fatalf("ControllerExpandVolume failed: %v", err)
	}

	if pm.lastExpandSizeGB != 11 {
		t.Errorf("expected 11 GB (rounded up), got %d", pm.lastExpandSizeGB)
	}
}

// ---------------------------------------------------------------------------
// ValidateVolumeCapabilities Tests
// ---------------------------------------------------------------------------

func TestValidateVolumeCapabilities_MissingVolumeID(t *testing.T) {
	d := newTestDriver(&mockPoolManager{}, nil)

	_, err := d.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "",
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestValidateVolumeCapabilities_AllSupportedModes(t *testing.T) {
	d := newTestDriver(&mockPoolManager{}, nil)

	modes := []csi.VolumeCapability_AccessMode_Mode{
		csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER,
		csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY,
		csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER,
		csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER,
	}

	for _, mode := range modes {
		t.Run(mode.String(), func(t *testing.T) {
			resp, err := d.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
				VolumeId: "pool/share/pvc",
				VolumeCapabilities: []*csi.VolumeCapability{
					{
						AccessMode: &csi.VolumeCapability_AccessMode{Mode: mode},
					},
				},
			})
			if err != nil {
				t.Fatalf("ValidateVolumeCapabilities failed: %v", err)
			}
			if resp.GetConfirmed() == nil {
				t.Errorf("expected Confirmed for mode %v", mode)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// ControllerGetCapabilities Tests
// ---------------------------------------------------------------------------

func TestControllerGetCapabilities(t *testing.T) {
	d := newTestDriver(&mockPoolManager{}, nil)

	resp, err := d.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ControllerGetCapabilities failed: %v", err)
	}

	caps := resp.GetCapabilities()
	if len(caps) != 5 {
		t.Fatalf("expected 5 capabilities, got %d", len(caps))
	}

	foundCreate := false
	foundExpand := false
	foundCreateSnap := false
	foundListSnap := false
	foundClone := false
	for _, cap := range caps {
		switch cap.GetRpc().GetType() {
		case csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME:
			foundCreate = true
		case csi.ControllerServiceCapability_RPC_EXPAND_VOLUME:
			foundExpand = true
		case csi.ControllerServiceCapability_RPC_CREATE_DELETE_SNAPSHOT:
			foundCreateSnap = true
		case csi.ControllerServiceCapability_RPC_LIST_SNAPSHOTS:
			foundListSnap = true
		case csi.ControllerServiceCapability_RPC_CLONE_VOLUME:
			foundClone = true
		}
	}

	if !foundCreate {
		t.Error("missing CREATE_DELETE_VOLUME capability")
	}
	if !foundExpand {
		t.Error("missing EXPAND_VOLUME capability")
	}
	if !foundCreateSnap {
		t.Error("missing CREATE_DELETE_SNAPSHOT capability")
	}
	if !foundListSnap {
		t.Error("missing LIST_SNAPSHOTS capability")
	}
	if !foundClone {
		t.Error("missing CLONE_VOLUME capability")
	}
}

// ---------------------------------------------------------------------------
// Cross-Zone Tests
// ---------------------------------------------------------------------------

func TestCreateVolume_CrossZone_VolumeContext(t *testing.T) {
	pm := &mockPoolManager{
		allocateResult: &pool.AllocationResult{
			ShareID:       "share-1",
			MountTargetIP: "10.0.0.1",
			SubPath:       "/pvcs/pvc-test",
			SharePath:     "/",
			MountTargets: map[string]string{
				"us-south-1": "10.0.0.1",
				"us-south-2": "10.0.1.1",
				"us-south-3": "10.0.2.1",
			},
			AccessibleZones: []string{"us-south-1", "us-south-2", "us-south-3"},
		},
	}
	d := newTestDriver(pm, nil)

	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	ctx := resp.GetVolume().VolumeContext
	if ctx["server.us-south-1"] != "10.0.0.1" {
		t.Errorf("expected server.us-south-1='10.0.0.1', got %q", ctx["server.us-south-1"])
	}
	if ctx["server.us-south-2"] != "10.0.1.1" {
		t.Errorf("expected server.us-south-2='10.0.1.1', got %q", ctx["server.us-south-2"])
	}
	if ctx["server.us-south-3"] != "10.0.2.1" {
		t.Errorf("expected server.us-south-3='10.0.2.1', got %q", ctx["server.us-south-3"])
	}
	// Primary server should still be set
	if ctx["server"] != "10.0.0.1" {
		t.Errorf("expected server='10.0.0.1', got %q", ctx["server"])
	}

	// Should have 3 topology entries
	topos := resp.GetVolume().GetAccessibleTopology()
	if len(topos) != 3 {
		t.Fatalf("expected 3 topologies, got %d", len(topos))
	}

	zones := make(map[string]bool)
	for _, topo := range topos {
		zones[topo.GetSegments()["topology.kubernetes.io/zone"]] = true
	}
	for _, z := range []string{"us-south-1", "us-south-2", "us-south-3"} {
		if !zones[z] {
			t.Errorf("expected zone %q in topologies", z)
		}
	}
}

func TestCreateVolume_SingleZone_BackwardCompat(t *testing.T) {
	pm := &mockPoolManager{
		allocateResult: &pool.AllocationResult{
			ShareID:       "share-1",
			MountTargetIP: "10.0.0.1",
			SubPath:       "/pvcs/pvc-test",
			SharePath:     "/",
			// No MountTargets or AccessibleZones (single-zone)
		},
	}
	d := newTestDriver(pm, nil)

	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{
				{Segments: map[string]string{"topology.kubernetes.io/zone": "us-south-1"}},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	// Should have single topology (backward compat)
	topos := resp.GetVolume().GetAccessibleTopology()
	if len(topos) != 1 {
		t.Fatalf("expected 1 topology, got %d", len(topos))
	}
	if zone := topos[0].GetSegments()["topology.kubernetes.io/zone"]; zone != "us-south-1" {
		t.Errorf("expected zone 'us-south-1', got %q", zone)
	}

	// No server.<zone> keys
	ctx := resp.GetVolume().VolumeContext
	for key := range ctx {
		if key != "server" && len(key) > 7 && key[:7] == "server." {
			t.Errorf("unexpected zone-specific key %q in single-zone mode", key)
		}
	}
}

// ---------------------------------------------------------------------------
// parseVolumeID Tests
// ---------------------------------------------------------------------------

func TestParseVolumeID_Valid(t *testing.T) {
	poolName, shareID, pvName, err := parseVolumeID("my-pool/r006-share-abc/pvc-xyz")
	if err != nil {
		t.Fatalf("parseVolumeID failed: %v", err)
	}
	if poolName != "my-pool" {
		t.Errorf("expected poolName 'my-pool', got %q", poolName)
	}
	if shareID != "r006-share-abc" {
		t.Errorf("expected shareID 'r006-share-abc', got %q", shareID)
	}
	if pvName != "pvc-xyz" {
		t.Errorf("expected pvName 'pvc-xyz', got %q", pvName)
	}
}

func TestParseVolumeID_WithSlashesInPVName(t *testing.T) {
	// SplitN with 3 means the third part gets everything remaining
	poolName, shareID, pvName, err := parseVolumeID("pool/share/pvc/with/extra")
	if err != nil {
		t.Fatalf("parseVolumeID failed: %v", err)
	}
	if poolName != "pool" {
		t.Errorf("expected poolName 'pool', got %q", poolName)
	}
	if shareID != "share" {
		t.Errorf("expected shareID 'share', got %q", shareID)
	}
	if pvName != "pvc/with/extra" {
		t.Errorf("expected pvName 'pvc/with/extra', got %q", pvName)
	}
}

func TestParseVolumeID_Invalid(t *testing.T) {
	tests := []struct {
		name string
		id   string
	}{
		{"empty", ""},
		{"no slashes", "noSlashes"},
		{"one slash", "pool/share"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, _, err := parseVolumeID(tt.id)
			if err == nil {
				t.Errorf("expected error for volume ID %q", tt.id)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// parseOptionalInt64 Tests
// ---------------------------------------------------------------------------

func TestParseOptionalInt64(t *testing.T) {
	tests := []struct {
		input    string
		expected *int64
	}{
		{"", nil},
		{"abc", nil},
		{"1000", int64Ptr(1000)},
		{"0", int64Ptr(0)},
		{"-1", int64Ptr(-1)},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("input=%q", tt.input), func(t *testing.T) {
			got := parseOptionalInt64(tt.input)
			if tt.expected == nil {
				if got != nil {
					t.Errorf("expected nil, got %d", *got)
				}
			} else {
				if got == nil {
					t.Errorf("expected %d, got nil", *tt.expected)
				} else if *got != *tt.expected {
					t.Errorf("expected %d, got %d", *tt.expected, *got)
				}
			}
		})
	}
}

func int64Ptr(v int64) *int64 {
	return &v
}

// ---------------------------------------------------------------------------
// CreateSnapshot Tests
// ---------------------------------------------------------------------------

func TestCreateSnapshot_MissingName(t *testing.T) {
	d := newTestDriver(&mockPoolManager{}, nil)

	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		SourceVolumeId: "pool/share/pvc-test",
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestCreateSnapshot_MissingSourceVolumeID(t *testing.T) {
	d := newTestDriver(&mockPoolManager{}, nil)

	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name: "snap-test",
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestCreateSnapshot_SourceNotFound(t *testing.T) {
	pm := &mockPoolManager{
		createSnapshotErr: pool.ErrSourceNotFound,
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name:           "snap-test",
		SourceVolumeId: "pool/share/pvc-missing",
	})

	assertGRPCCode(t, err, codes.NotFound)
}

func TestCreateSnapshot_InternalError(t *testing.T) {
	pm := &mockPoolManager{
		createSnapshotErr: fmt.Errorf("NFS copy failed"),
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name:           "snap-test",
		SourceVolumeId: "pool/share/pvc-test",
	})

	assertGRPCCode(t, err, codes.Internal)
}

func TestCreateSnapshot_Success(t *testing.T) {
	now := time.Now()
	pm := &mockPoolManager{
		createSnapshotResult: &pool.SnapshotResult{
			SnapshotName:  "snap-test",
			PoolName:      "test-pool",
			ShareID:       "share-1",
			SnapshotPath:  "/pvcs/.snapshots/snap-snap-test",
			SourceSubPath: "/pvcs/pvc-test",
			SizeBytes:     10 * (1 << 30),
			CreationTime:  now,
			ReadyToUse:    true,
		},
	}
	d := newTestDriver(pm, nil)

	resp, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name:           "snap-test",
		SourceVolumeId: "test-pool/share-1/pvc-test",
	})
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}

	snap := resp.GetSnapshot()
	if snap == nil {
		t.Fatal("expected snapshot in response")
	}

	expectedID := "test-pool/share-1/snap-test"
	if snap.SnapshotId != expectedID {
		t.Errorf("expected snapshotID %q, got %q", expectedID, snap.SnapshotId)
	}
	if snap.SourceVolumeId != "test-pool/share-1/pvc-test" {
		t.Errorf("expected sourceVolumeId 'test-pool/share-1/pvc-test', got %q", snap.SourceVolumeId)
	}
	if snap.SizeBytes != 10*(1<<30) {
		t.Errorf("expected sizeBytes %d, got %d", 10*(1<<30), snap.SizeBytes)
	}
	if !snap.ReadyToUse {
		t.Error("expected ReadyToUse=true")
	}

	if pm.lastCreateSnapshotName != "snap-test" {
		t.Errorf("expected snapshot name 'snap-test', got %q", pm.lastCreateSnapshotName)
	}
	if pm.lastCreateSnapshotSourceID != "test-pool/share-1/pvc-test" {
		t.Errorf("expected source ID 'test-pool/share-1/pvc-test', got %q", pm.lastCreateSnapshotSourceID)
	}
}

// ---------------------------------------------------------------------------
// DeleteSnapshot Tests
// ---------------------------------------------------------------------------

func TestDeleteSnapshot_MissingSnapshotID(t *testing.T) {
	d := newTestDriver(&mockPoolManager{}, nil)

	_, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{
		SnapshotId: "",
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestDeleteSnapshot_InvalidSnapshotID(t *testing.T) {
	d := newTestDriver(&mockPoolManager{}, nil)

	_, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{
		SnapshotId: "invalid-no-slashes",
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestDeleteSnapshot_NotFound_Idempotent(t *testing.T) {
	pm := &mockPoolManager{
		deleteSnapshotErr: pool.ErrSnapshotNotFound,
	}
	d := newTestDriver(pm, nil)

	resp, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{
		SnapshotId: "pool/share/snap-missing",
	})

	if err != nil {
		t.Fatalf("expected idempotent success for not-found, got: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestDeleteSnapshot_Success(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	resp, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{
		SnapshotId: "test-pool/share-1/snap-test",
	})
	if err != nil {
		t.Fatalf("DeleteSnapshot failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	if pm.lastDeleteSnapshotName != "snap-test" {
		t.Errorf("expected delete called with 'snap-test', got %q", pm.lastDeleteSnapshotName)
	}
}

func TestDeleteSnapshot_InternalError(t *testing.T) {
	pm := &mockPoolManager{
		deleteSnapshotErr: fmt.Errorf("NFS failure"),
	}
	d := newTestDriver(pm, nil)

	_, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{
		SnapshotId: "pool/share/snap-test",
	})

	assertGRPCCode(t, err, codes.Internal)
}

// ---------------------------------------------------------------------------
// ListSnapshots Tests
// ---------------------------------------------------------------------------

func TestListSnapshots_BySnapshotID(t *testing.T) {
	k := newMockK8sClient()
	now := metav1.Now()
	k.snapshots["snap-test"] = &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "snap-test"},
		Spec: v1alpha1.SnapshotSpec{
			SourceSubVolume: "pvc-source",
			PoolName:        "test-pool",
			ShareID:         "share-1",
			SizeGB:          10,
		},
		Status: v1alpha1.SnapshotStatus{
			ReadyToUse:   true,
			CreationTime: &now,
		},
	}
	d := newTestDriver(&mockPoolManager{}, k)

	resp, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SnapshotId: "test-pool/share-1/snap-test",
	})
	if err != nil {
		t.Fatalf("ListSnapshots failed: %v", err)
	}

	entries := resp.GetEntries()
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}

	snap := entries[0].GetSnapshot()
	if snap.SnapshotId != "test-pool/share-1/snap-test" {
		t.Errorf("expected snapshotID 'test-pool/share-1/snap-test', got %q", snap.SnapshotId)
	}
	if snap.SourceVolumeId != "test-pool/share-1/pvc-source" {
		t.Errorf("expected sourceVolumeId 'test-pool/share-1/pvc-source', got %q", snap.SourceVolumeId)
	}
	if !snap.ReadyToUse {
		t.Error("expected ReadyToUse=true")
	}
	if snap.SizeBytes != 10*(1<<30) {
		t.Errorf("expected sizeBytes %d, got %d", 10*(1<<30), snap.SizeBytes)
	}
}

func TestListSnapshots_BySnapshotID_NotFound(t *testing.T) {
	k := newMockK8sClient()
	d := newTestDriver(&mockPoolManager{}, k)

	resp, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SnapshotId: "pool/share/snap-missing",
	})
	if err != nil {
		t.Fatalf("ListSnapshots failed: %v", err)
	}

	if len(resp.GetEntries()) != 0 {
		t.Errorf("expected 0 entries for missing snapshot, got %d", len(resp.GetEntries()))
	}
}

func TestListSnapshots_BySourceVolumeID(t *testing.T) {
	now := time.Now()
	pm := &mockPoolManager{
		listSnapshotsResults: []pool.SnapshotResult{
			{
				SnapshotName: "snap-1",
				PoolName:     "test-pool",
				ShareID:      "share-1",
				SizeBytes:    10 * (1 << 30),
				CreationTime: now,
				ReadyToUse:   true,
			},
			{
				SnapshotName: "snap-2",
				PoolName:     "test-pool",
				ShareID:      "share-1",
				SizeBytes:    5 * (1 << 30),
				CreationTime: now,
				ReadyToUse:   true,
			},
		},
	}
	d := newTestDriver(pm, nil)

	resp, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SourceVolumeId: "test-pool/share-1/pvc-source",
	})
	if err != nil {
		t.Fatalf("ListSnapshots failed: %v", err)
	}

	entries := resp.GetEntries()
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	if entries[0].GetSnapshot().SnapshotId != "test-pool/share-1/snap-1" {
		t.Errorf("expected first snapshot ID 'test-pool/share-1/snap-1', got %q", entries[0].GetSnapshot().SnapshotId)
	}
	if entries[1].GetSnapshot().SnapshotId != "test-pool/share-1/snap-2" {
		t.Errorf("expected second snapshot ID 'test-pool/share-1/snap-2', got %q", entries[1].GetSnapshot().SnapshotId)
	}
}

func TestListSnapshots_Pagination(t *testing.T) {
	now := time.Now()
	pm := &mockPoolManager{
		listSnapshotsResults: []pool.SnapshotResult{
			{SnapshotName: "snap-1", PoolName: "pool", ShareID: "share", SizeBytes: 1, CreationTime: now, ReadyToUse: true},
			{SnapshotName: "snap-2", PoolName: "pool", ShareID: "share", SizeBytes: 2, CreationTime: now, ReadyToUse: true},
			{SnapshotName: "snap-3", PoolName: "pool", ShareID: "share", SizeBytes: 3, CreationTime: now, ReadyToUse: true},
		},
	}
	d := newTestDriver(pm, nil)

	// First page: max 2 entries
	resp, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SourceVolumeId: "pool/share/pvc-source",
		MaxEntries:     2,
	})
	if err != nil {
		t.Fatalf("ListSnapshots page 1 failed: %v", err)
	}

	if len(resp.GetEntries()) != 2 {
		t.Fatalf("expected 2 entries on page 1, got %d", len(resp.GetEntries()))
	}
	if resp.NextToken == "" {
		t.Fatal("expected non-empty NextToken for pagination")
	}

	// Second page: use token
	resp2, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SourceVolumeId: "pool/share/pvc-source",
		MaxEntries:     2,
		StartingToken:  resp.NextToken,
	})
	if err != nil {
		t.Fatalf("ListSnapshots page 2 failed: %v", err)
	}

	if len(resp2.GetEntries()) != 1 {
		t.Fatalf("expected 1 entry on page 2, got %d", len(resp2.GetEntries()))
	}
	if resp2.NextToken != "" {
		t.Errorf("expected empty NextToken on last page, got %q", resp2.NextToken)
	}
}

// ---------------------------------------------------------------------------
// CreateVolume from Snapshot Tests
// ---------------------------------------------------------------------------

func TestCreateVolume_FromSnapshot_Success(t *testing.T) {
	pm := &mockPoolManager{
		restoreSnapshotResult: &pool.AllocationResult{
			ShareID:       "share-1",
			MountTargetIP: "10.240.0.1",
			SubPath:       "/pvcs/pvc-restored",
			SharePath:     "/",
		},
	}
	d := newTestDriver(pm, nil)

	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-restored",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{
					SnapshotId: "test-pool/share-1/snap-source",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume from snapshot failed: %v", err)
	}

	vol := resp.GetVolume()
	if vol == nil {
		t.Fatal("expected volume in response")
	}

	expectedID := "test-pool/share-1/pvc-restored"
	if vol.VolumeId != expectedID {
		t.Errorf("expected volumeID %q, got %q", expectedID, vol.VolumeId)
	}

	if vol.ContentSource == nil {
		t.Fatal("expected content source in response")
	}
	snapSource := vol.ContentSource.GetSnapshot()
	if snapSource == nil {
		t.Fatal("expected snapshot content source")
	}
	if snapSource.SnapshotId != "test-pool/share-1/snap-source" {
		t.Errorf("expected content source snapshot ID 'test-pool/share-1/snap-source', got %q", snapSource.SnapshotId)
	}

	if pm.lastRestoreSnapshotName != "snap-source" {
		t.Errorf("expected restore called with 'snap-source', got %q", pm.lastRestoreSnapshotName)
	}
}

func TestCreateVolume_FromSnapshot_SnapshotNotFound(t *testing.T) {
	pm := &mockPoolManager{
		restoreSnapshotErr: pool.ErrSnapshotNotFound,
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-restored",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{
					SnapshotId: "pool/share/snap-missing",
				},
			},
		},
	})

	assertGRPCCode(t, err, codes.NotFound)
}

func TestCreateVolume_FromSnapshot_PoolExhausted(t *testing.T) {
	pm := &mockPoolManager{
		restoreSnapshotErr: pool.ErrPoolExhausted,
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-restored",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{
					SnapshotId: "pool/share/snap-test",
				},
			},
		},
	})

	assertGRPCCode(t, err, codes.ResourceExhausted)
}

func TestCreateVolume_FromSnapshot_InvalidSnapshotID(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-restored",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{
					SnapshotId: "invalid-id",
				},
			},
		},
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

// ---------------------------------------------------------------------------
// CreateVolume from Clone Tests
// ---------------------------------------------------------------------------

func TestCreateVolume_FromClone_Success(t *testing.T) {
	pm := &mockPoolManager{
		cloneVolumeResult: &pool.AllocationResult{
			ShareID:       "share-1",
			MountTargetIP: "10.240.0.1",
			SubPath:       "/pvcs/pvc-clone",
			SharePath:     "/",
		},
	}
	d := newTestDriver(pm, nil)

	resp, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-clone",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Volume{
				Volume: &csi.VolumeContentSource_VolumeSource{
					VolumeId: "test-pool/share-1/pvc-source",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume from clone failed: %v", err)
	}

	vol := resp.GetVolume()
	if vol == nil {
		t.Fatal("expected volume in response")
	}

	expectedID := "test-pool/share-1/pvc-clone"
	if vol.VolumeId != expectedID {
		t.Errorf("expected volumeID %q, got %q", expectedID, vol.VolumeId)
	}

	if vol.ContentSource == nil {
		t.Fatal("expected content source in response")
	}
	volSource := vol.ContentSource.GetVolume()
	if volSource == nil {
		t.Fatal("expected volume content source")
	}
	if volSource.VolumeId != "test-pool/share-1/pvc-source" {
		t.Errorf("expected content source volume ID 'test-pool/share-1/pvc-source', got %q", volSource.VolumeId)
	}

	if pm.lastCloneSourceVolumeID != "test-pool/share-1/pvc-source" {
		t.Errorf("expected clone called with source 'test-pool/share-1/pvc-source', got %q", pm.lastCloneSourceVolumeID)
	}
	if pm.lastCloneReq.PVName != "pvc-clone" {
		t.Errorf("expected clone PVName 'pvc-clone', got %q", pm.lastCloneReq.PVName)
	}
}

func TestCreateVolume_FromClone_SourceNotFound(t *testing.T) {
	pm := &mockPoolManager{
		cloneVolumeErr: pool.ErrSourceNotFound,
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-clone",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Volume{
				Volume: &csi.VolumeContentSource_VolumeSource{
					VolumeId: "pool/share/pvc-missing",
				},
			},
		},
	})

	assertGRPCCode(t, err, codes.NotFound)
}

func TestCreateVolume_FromClone_PoolExhausted(t *testing.T) {
	pm := &mockPoolManager{
		cloneVolumeErr: pool.ErrPoolExhausted,
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-clone",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Volume{
				Volume: &csi.VolumeContentSource_VolumeSource{
					VolumeId: "pool/share/pvc-source",
				},
			},
		},
	})

	assertGRPCCode(t, err, codes.ResourceExhausted)
}

func TestCreateVolume_FromClone_InternalError(t *testing.T) {
	pm := &mockPoolManager{
		cloneVolumeErr: fmt.Errorf("NFS copy failed"),
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-clone",
		Parameters: map[string]string{"pool": "test-pool"},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Volume{
				Volume: &csi.VolumeContentSource_VolumeSource{
					VolumeId: "pool/share/pvc-source",
				},
			},
		},
	})

	assertGRPCCode(t, err, codes.Internal)
}

func TestCreateVolume_FromClone_CustomSyncThreshold(t *testing.T) {
	pm := &mockPoolManager{
		cloneVolumeResult: &pool.AllocationResult{
			ShareID:       "share-1",
			MountTargetIP: "10.240.0.1",
			SubPath:       "/pvcs/pvc-clone",
			SharePath:     "/",
		},
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name: "pvc-clone",
		Parameters: map[string]string{
			"pool":                 "test-pool",
			"cloneSyncThresholdGB": "20",
		},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(10),
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Volume{
				Volume: &csi.VolumeContentSource_VolumeSource{
					VolumeId: "pool/share/pvc-source",
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("CreateVolume from clone failed: %v", err)
	}

	if pm.lastCloneSyncThresholdGB != 20 {
		t.Errorf("expected sync threshold 20, got %d", pm.lastCloneSyncThresholdGB)
	}
}

// ---------------------------------------------------------------------------
// Group Snapshot CSI Handler Tests
// ---------------------------------------------------------------------------

func TestCreateVolumeGroupSnapshot_MissingName(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		// Name intentionally empty
		SourceVolumeIds: []string{"pool/share/pvc-1"},
		Parameters:      map[string]string{"pool": "test-pool"},
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestCreateVolumeGroupSnapshot_NoSourceVolumes(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "test-group",
		SourceVolumeIds: []string{},
		Parameters:      map[string]string{"pool": "test-pool"},
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestCreateVolumeGroupSnapshot_Success(t *testing.T) {
	pm := &mockPoolManager{
		createGroupSnapshotResult: &pool.GroupSnapshotResult{
			GroupName:  "test-group",
			PoolName:   "test-pool",
			ReadyToUse: true,
			Members: []pool.SnapshotResult{
				{
					SnapshotName:   "snap-1",
					PoolName:       "test-pool",
					ShareID:        "share-1",
					SizeBytes:      gbToBytes(10),
					ReadyToUse:     true,
					SourceVolumeID: "test-pool/share-1/pvc-1",
					CreationTime:   time.Now(),
				},
				{
					SnapshotName:   "snap-2",
					PoolName:       "test-pool",
					ShareID:        "share-1",
					SizeBytes:      gbToBytes(5),
					ReadyToUse:     true,
					SourceVolumeID: "test-pool/share-1/pvc-2",
					CreationTime:   time.Now(),
				},
			},
			CreationTime: time.Now(),
		},
	}
	d := newTestDriver(pm, nil)

	resp, err := d.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "test-group",
		SourceVolumeIds: []string{"test-pool/share-1/pvc-1", "test-pool/share-1/pvc-2"},
		Parameters:      map[string]string{"pool": "test-pool"},
	})
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot failed: %v", err)
	}

	gs := resp.GetGroupSnapshot()
	if gs == nil {
		t.Fatal("expected group snapshot in response")
	}
	if !gs.ReadyToUse {
		t.Error("expected ReadyToUse=true")
	}
	if len(gs.Snapshots) != 2 {
		t.Errorf("expected 2 snapshots, got %d", len(gs.Snapshots))
	}

	// Verify pool manager received correct request
	if pm.lastCreateGroupSnapshotReq.GroupName != "test-group" {
		t.Errorf("expected group name 'test-group', got %q", pm.lastCreateGroupSnapshotReq.GroupName)
	}
	if pm.lastCreateGroupSnapshotReq.PoolName != "test-pool" {
		t.Errorf("expected pool name 'test-pool', got %q", pm.lastCreateGroupSnapshotReq.PoolName)
	}
}

func TestCreateVolumeGroupSnapshot_PoolFromVolumeID(t *testing.T) {
	pm := &mockPoolManager{
		createGroupSnapshotResult: &pool.GroupSnapshotResult{
			GroupName:    "test-group",
			PoolName:     "my-pool",
			ReadyToUse:   true,
			Members:      []pool.SnapshotResult{},
			CreationTime: time.Now(),
		},
	}
	d := newTestDriver(pm, nil)

	// No "pool" parameter -- extracted from volume ID
	_, err := d.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "test-group",
		SourceVolumeIds: []string{"my-pool/share-1/pvc-1"},
		Parameters:      map[string]string{},
	})
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot failed: %v", err)
	}

	if pm.lastCreateGroupSnapshotReq.PoolName != "my-pool" {
		t.Errorf("expected pool name 'my-pool', got %q", pm.lastCreateGroupSnapshotReq.PoolName)
	}
}

func TestCreateVolumeGroupSnapshot_GroupSnapshotFailed(t *testing.T) {
	pm := &mockPoolManager{
		createGroupSnapshotErr: pool.ErrGroupSnapshotFailed,
	}
	d := newTestDriver(pm, nil)

	_, err := d.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "test-group",
		SourceVolumeIds: []string{"test-pool/share-1/pvc-1"},
		Parameters:      map[string]string{"pool": "test-pool"},
	})

	assertGRPCCode(t, err, codes.Internal)
}

func TestDeleteVolumeGroupSnapshot_MissingID(t *testing.T) {
	pm := &mockPoolManager{}
	d := newTestDriver(pm, nil)

	_, err := d.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
		// GroupSnapshotId intentionally empty
	})

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestDeleteVolumeGroupSnapshot_Success(t *testing.T) {
	pm := &mockPoolManager{
		deleteGroupSnapshotErr: nil,
	}
	d := newTestDriver(pm, nil)

	_, err := d.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: "test-pool/test-group",
	})
	if err != nil {
		t.Fatalf("DeleteVolumeGroupSnapshot failed: %v", err)
	}

	if pm.lastDeleteGroupSnapshotName != "test-group" {
		t.Errorf("expected group name 'test-group', got %q", pm.lastDeleteGroupSnapshotName)
	}
}

func TestDeleteVolumeGroupSnapshot_InternalError(t *testing.T) {
	pm := &mockPoolManager{
		deleteGroupSnapshotErr: fmt.Errorf("VPC API error"),
	}
	d := newTestDriver(pm, nil)

	_, err := d.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: "test-pool/test-group",
	})

	assertGRPCCode(t, err, codes.Internal)
}

func TestGetVolumeGroupSnapshot_Success(t *testing.T) {
	k8sMock := newMockK8sClient()
	now := metav1.Now()
	k8sMock.groupSnapshots = map[string]*v1alpha1.VolumeGroupSnapshot{
		"test-group": {
			ObjectMeta: metav1.ObjectMeta{Name: "test-group"},
			Spec: v1alpha1.VolumeGroupSnapshotSpec{
				PoolName: "test-pool",
			},
			Status: v1alpha1.VolumeGroupSnapshotStatus{
				Phase: "Complete",
				Members: []v1alpha1.GroupSnapshotMember{
					{SubVolumeName: "pvc-1", SnapshotName: "snap-1", Phase: "Ready"},
					{SubVolumeName: "pvc-2", SnapshotName: "snap-2", Phase: "Ready"},
				},
				MemberCount:  2,
				ReadyCount:   2,
				CreationTime: &now,
			},
		},
	}
	pm := &mockPoolManager{}
	d := newTestDriver(pm, k8sMock)

	resp, err := d.GetVolumeGroupSnapshot(context.Background(), &csi.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: "test-pool/test-group",
	})
	if err != nil {
		t.Fatalf("GetVolumeGroupSnapshot failed: %v", err)
	}

	gs := resp.GetGroupSnapshot()
	if gs == nil {
		t.Fatal("expected group snapshot in response")
	}
	if !gs.ReadyToUse {
		t.Error("expected ReadyToUse=true")
	}
	if len(gs.Snapshots) != 2 {
		t.Errorf("expected 2 snapshots, got %d", len(gs.Snapshots))
	}
}

// ---------------------------------------------------------------------------
// Readiness Guard Tests — not-ready driver returns Unavailable
// ---------------------------------------------------------------------------

func TestReadinessGuard_CreateVolume(t *testing.T) {
	d := newTestDriverNotReady(&mockPoolManager{}, nil)
	_, err := d.CreateVolume(context.Background(), &csi.CreateVolumeRequest{
		Name:       "pvc-test",
		Parameters: map[string]string{"pool": "test"},
	})
	assertGRPCCode(t, err, codes.Unavailable)
}

func TestReadinessGuard_DeleteVolume(t *testing.T) {
	d := newTestDriverNotReady(&mockPoolManager{}, nil)
	_, err := d.DeleteVolume(context.Background(), &csi.DeleteVolumeRequest{
		VolumeId: "pool/share/pvc",
	})
	assertGRPCCode(t, err, codes.Unavailable)
}

func TestReadinessGuard_ControllerExpandVolume(t *testing.T) {
	d := newTestDriverNotReady(&mockPoolManager{}, nil)
	_, err := d.ControllerExpandVolume(context.Background(), &csi.ControllerExpandVolumeRequest{
		VolumeId: "pool/share/pvc",
	})
	assertGRPCCode(t, err, codes.Unavailable)
}

func TestReadinessGuard_CreateSnapshot(t *testing.T) {
	d := newTestDriverNotReady(&mockPoolManager{}, nil)
	_, err := d.CreateSnapshot(context.Background(), &csi.CreateSnapshotRequest{
		Name:           "snap",
		SourceVolumeId: "pool/share/pvc",
	})
	assertGRPCCode(t, err, codes.Unavailable)
}

func TestReadinessGuard_DeleteSnapshot(t *testing.T) {
	d := newTestDriverNotReady(&mockPoolManager{}, nil)
	_, err := d.DeleteSnapshot(context.Background(), &csi.DeleteSnapshotRequest{
		SnapshotId: "pool/share/snap",
	})
	assertGRPCCode(t, err, codes.Unavailable)
}

func TestReadinessGuard_ListSnapshots(t *testing.T) {
	d := newTestDriverNotReady(&mockPoolManager{}, nil)
	_, err := d.ListSnapshots(context.Background(), &csi.ListSnapshotsRequest{
		SourceVolumeId: "pool/share/pvc",
	})
	assertGRPCCode(t, err, codes.Unavailable)
}

func TestReadinessGuard_CreateVolumeGroupSnapshot(t *testing.T) {
	d := newTestDriverNotReady(&mockPoolManager{}, nil)
	_, err := d.CreateVolumeGroupSnapshot(context.Background(), &csi.CreateVolumeGroupSnapshotRequest{
		Name:            "group",
		SourceVolumeIds: []string{"pool/share/pvc"},
	})
	assertGRPCCode(t, err, codes.Unavailable)
}

func TestReadinessGuard_DeleteVolumeGroupSnapshot(t *testing.T) {
	d := newTestDriverNotReady(&mockPoolManager{}, nil)
	_, err := d.DeleteVolumeGroupSnapshot(context.Background(), &csi.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: "pool/group",
	})
	assertGRPCCode(t, err, codes.Unavailable)
}

func TestReadinessGuard_GetVolumeGroupSnapshot(t *testing.T) {
	d := newTestDriverNotReady(&mockPoolManager{}, nil)
	_, err := d.GetVolumeGroupSnapshot(context.Background(), &csi.GetVolumeGroupSnapshotRequest{
		GroupSnapshotId: "pool/group",
	})
	assertGRPCCode(t, err, codes.Unavailable)
}

func TestReadinessGuard_ControllerGetCapabilities_AlwaysWorks(t *testing.T) {
	d := newTestDriverNotReady(&mockPoolManager{}, nil)
	resp, err := d.ControllerGetCapabilities(context.Background(), &csi.ControllerGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("ControllerGetCapabilities should work even when not ready: %v", err)
	}
	if len(resp.GetCapabilities()) == 0 {
		t.Error("expected capabilities")
	}
}

func TestReadinessGuard_ValidateVolumeCapabilities_AlwaysWorks(t *testing.T) {
	d := newTestDriverNotReady(&mockPoolManager{}, nil)
	_, err := d.ValidateVolumeCapabilities(context.Background(), &csi.ValidateVolumeCapabilitiesRequest{
		VolumeId: "pool/share/pvc",
		VolumeCapabilities: []*csi.VolumeCapability{
			{AccessMode: &csi.VolumeCapability_AccessMode{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER}},
		},
	})
	if err != nil {
		t.Fatalf("ValidateVolumeCapabilities should work even when not ready: %v", err)
	}
}
