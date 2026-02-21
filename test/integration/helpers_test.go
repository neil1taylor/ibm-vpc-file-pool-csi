//go:build integration

package integration

import (
	"context"
	"fmt"
	"os"
	"sync"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/driver"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud/fake"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Fake K8s Client (implements k8s.Client for integration tests)
// ---------------------------------------------------------------------------

// Compile-time check that fakeK8sClient implements k8s.Client.
var _ k8s.Client = (*fakeK8sClient)(nil)

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
}

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

func (f *fakeK8sClient) ListFileSharePools(_ context.Context) ([]v1alpha1.FileSharePool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []v1alpha1.FileSharePool
	for _, p := range f.pools {
		result = append(result, *p.DeepCopy())
	}
	return result, nil
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

func (f *fakeK8sClient) UpdateFileSharePool(_ context.Context, pool *v1alpha1.FileSharePool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.pools[pool.Name]; !ok {
		return fmt.Errorf("pool %q not found", pool.Name)
	}
	f.pools[pool.Name] = pool.DeepCopy()
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

// Snapshot operations

func (f *fakeK8sClient) GetSnapshot(_ context.Context, name string) (*v1alpha1.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	snap, ok := f.snapshots[name]
	if !ok {
		return nil, fmt.Errorf("snapshot %q not found", name)
	}
	return snap.DeepCopy(), nil
}

func (f *fakeK8sClient) ListSnapshots(_ context.Context, _ string) ([]v1alpha1.Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var result []v1alpha1.Snapshot
	for _, snap := range f.snapshots {
		result = append(result, *snap.DeepCopy())
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

func (f *fakeK8sClient) GetConfigMapValue(_ context.Context, _, _, _ string) (string, error) {
	return "", fmt.Errorf("not implemented in fake")
}

func (f *fakeK8sClient) GetNodeZone(_ context.Context, _ string) (string, error) {
	return "us-south-1", nil
}

// Test state inspection helpers

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
	if p, ok := f.pools[name]; ok {
		return p.DeepCopy()
	}
	return nil
}

func (f *fakeK8sClient) getSubVolume(name string) *v1alpha1.SubVolume {
	f.mu.Lock()
	defer f.mu.Unlock()
	if sv, ok := f.subVolumes[name]; ok {
		return sv.DeepCopy()
	}
	return nil
}

func (f *fakeK8sClient) subVolumeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.subVolumes)
}

func (f *fakeK8sClient) subVolumeNames() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	names := make([]string, 0, len(f.subVolumes))
	for name := range f.subVolumes {
		names = append(names, name)
	}
	return names
}

func (f *fakeK8sClient) snapshotCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.snapshots)
}

func (f *fakeK8sClient) getSnapshot(name string) *v1alpha1.Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	if snap, ok := f.snapshots[name]; ok {
		return snap.DeepCopy()
	}
	return nil
}

func (f *fakeK8sClient) getGroupSnapshot(name string) *v1alpha1.VolumeGroupSnapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	if vgs, ok := f.groupSnapshots[name]; ok {
		return vgs.DeepCopy()
	}
	return nil
}

func (f *fakeK8sClient) groupSnapshotCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.groupSnapshots)
}

// ---------------------------------------------------------------------------
// Fake NFS Operations
// ---------------------------------------------------------------------------

type fakeNFSOperations struct {
	mu            sync.Mutex
	dirs          map[string]os.FileMode
	chowns        map[string][2]int
	copies        map[string]string // dst → src
	copyCallCount int              // tracks total CopyDir calls
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

func (f *fakeNFSOperations) MkdirAsUser(path string, perm os.FileMode, uid, gid uint32) error {
	if err := f.MkdirAll(path, perm); err != nil {
		return err
	}
	return f.Chown(path, int(uid), int(gid))
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

func (f *fakeNFSOperations) dirCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.dirs)
}

// ---------------------------------------------------------------------------
// Test Fixture Builders
// ---------------------------------------------------------------------------

// testHarness bundles all the fakes and real components for integration tests.
type testHarness struct {
	k8sClient *fakeK8sClient
	vpcClient *fake.FakeVPCClient
	nfsOps    *fakeNFSOperations
	manager   *pool.Manager
	driver    *driver.Driver
}

func newTestHarness(t *testing.T) *testHarness {
	t.Helper()

	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	nfs := newFakeNFSOperations()
	mgr := pool.NewManager(k, vpc, nfs, "/mnt/staging")

	d, err := driver.NewDriver(driver.Config{
		Name:        driver.DriverName,
		Version:     "test",
		NodeID:      "test-node",
		Endpoint:    "unix:///tmp/integration-test.sock",
		Mode:        "controller",
		PoolManager: mgr,
		K8sClient:   k,
	})
	if err != nil {
		t.Fatalf("failed to create driver: %v", err)
	}

	return &testHarness{
		k8sClient: k,
		vpcClient: vpc,
		nfsOps:    nfs,
		manager:   mgr,
		driver:    d,
	}
}

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

func gbToBytes(gb int64) int64 {
	return gb * (1 << 30)
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

func createVolumeRequest(pvName, poolName string, sizeGB int64) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:       pvName,
		Parameters: map[string]string{"pool": poolName},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(sizeGB),
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{
				{Segments: map[string]string{"topology.kubernetes.io/zone": "us-south-1"}},
			},
		},
	}
}

func deleteVolumeRequest(volumeID string) *csi.DeleteVolumeRequest {
	return &csi.DeleteVolumeRequest{
		VolumeId: volumeID,
	}
}

func createSnapshotRequest(snapshotName, sourceVolumeID string) *csi.CreateSnapshotRequest {
	return &csi.CreateSnapshotRequest{
		Name:           snapshotName,
		SourceVolumeId: sourceVolumeID,
	}
}

func deleteSnapshotRequest(snapshotID string) *csi.DeleteSnapshotRequest {
	return &csi.DeleteSnapshotRequest{
		SnapshotId: snapshotID,
	}
}

func listSnapshotsRequest(sourceVolumeID string) *csi.ListSnapshotsRequest {
	return &csi.ListSnapshotsRequest{
		SourceVolumeId: sourceVolumeID,
	}
}

func createVolumeFromSnapshotRequest(pvName, poolName string, sizeGB int64, snapshotID string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:       pvName,
		Parameters: map[string]string{"pool": poolName},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(sizeGB),
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Snapshot{
				Snapshot: &csi.VolumeContentSource_SnapshotSource{
					SnapshotId: snapshotID,
				},
			},
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{
				{Segments: map[string]string{"topology.kubernetes.io/zone": "us-south-1"}},
			},
		},
	}
}

func createVolumeFromCloneRequest(pvName, poolName string, sizeGB int64, sourceVolumeID string) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name:       pvName,
		Parameters: map[string]string{"pool": poolName},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(sizeGB),
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Volume{
				Volume: &csi.VolumeContentSource_VolumeSource{
					VolumeId: sourceVolumeID,
				},
			},
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{
				{Segments: map[string]string{"topology.kubernetes.io/zone": "us-south-1"}},
			},
		},
	}
}

func createVolumeFromCloneRequestWithThreshold(pvName, poolName string, sizeGB int64, sourceVolumeID string, thresholdGB int64) *csi.CreateVolumeRequest {
	return &csi.CreateVolumeRequest{
		Name: pvName,
		Parameters: map[string]string{
			"pool":                 poolName,
			"cloneSyncThresholdGB": fmt.Sprintf("%d", thresholdGB),
		},
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(sizeGB),
		},
		VolumeContentSource: &csi.VolumeContentSource{
			Type: &csi.VolumeContentSource_Volume{
				Volume: &csi.VolumeContentSource_VolumeSource{
					VolumeId: sourceVolumeID,
				},
			},
		},
		AccessibilityRequirements: &csi.TopologyRequirement{
			Preferred: []*csi.Topology{
				{Segments: map[string]string{"topology.kubernetes.io/zone": "us-south-1"}},
			},
		},
	}
}

func createVolumeGroupSnapshotRequest(name string, sourceVolumeIDs []string, params map[string]string) *csi.CreateVolumeGroupSnapshotRequest {
	return &csi.CreateVolumeGroupSnapshotRequest{
		Name:            name,
		SourceVolumeIds: sourceVolumeIDs,
		Parameters:      params,
	}
}

func deleteVolumeGroupSnapshotRequest(groupSnapshotID string) *csi.DeleteVolumeGroupSnapshotRequest {
	return &csi.DeleteVolumeGroupSnapshotRequest{
		GroupSnapshotId: groupSnapshotID,
	}
}
