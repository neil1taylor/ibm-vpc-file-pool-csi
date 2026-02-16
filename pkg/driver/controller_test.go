package driver

import (
	"context"
	"fmt"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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

// ---------------------------------------------------------------------------
// Mock K8s Client (only GetSubVolume needed for controller idempotency path)
// ---------------------------------------------------------------------------

type mockK8sClient struct {
	subVolumes map[string]*v1alpha1.SubVolume
	getErr     error
}

func newMockK8sClient() *mockK8sClient {
	return &mockK8sClient{
		subVolumes: make(map[string]*v1alpha1.SubVolume),
	}
}

func (m *mockK8sClient) GetFileSharePool(_ context.Context, _ string) (*v1alpha1.FileSharePool, error) {
	return nil, fmt.Errorf("not implemented")
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

// ---------------------------------------------------------------------------
// Test Helper
// ---------------------------------------------------------------------------

func newTestDriver(pm pool.PoolManager, k8s *mockK8sClient) *Driver {
	d := &Driver{
		name:        DriverName,
		version:     "test",
		nodeID:      "test-node",
		mode:        "controller",
		poolManager: pm,
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
	if len(caps) != 2 {
		t.Fatalf("expected 2 capabilities, got %d", len(caps))
	}

	foundCreate := false
	foundExpand := false
	for _, cap := range caps {
		switch cap.GetRpc().GetType() {
		case csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME:
			foundCreate = true
		case csi.ControllerServiceCapability_RPC_EXPAND_VOLUME:
			foundExpand = true
		}
	}

	if !foundCreate {
		t.Error("missing CREATE_DELETE_VOLUME capability")
	}
	if !foundExpand {
		t.Error("missing EXPAND_VOLUME capability")
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
