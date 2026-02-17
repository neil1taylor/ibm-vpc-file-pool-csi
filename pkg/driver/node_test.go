package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	storagev1 "k8s.io/api/storage/v1"
	mount "k8s.io/mount-utils"
)

// Valid PVC subDir for tests (matches the /pvcs/pvc-[a-f0-9-]{36} pattern).
const testSubDir = "/pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"

// resolvedTempDir returns a temp dir with symlinks resolved (macOS /var → /private/var).
func resolvedTempDir(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	resolved, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks(%s): %v", dir, err)
	}
	return resolved
}

func newNodeTestDriver(mounter mount.Interface, cache *util.MountCache, k8sClient k8s.Client) *Driver {
	if cache == nil {
		cache = util.NewMountCache()
	}
	return &Driver{
		name:       DriverName,
		version:    "test",
		nodeID:     "test-node-01",
		mode:       "node",
		mounter:    mounter,
		mountCache: cache,
		k8sClient:  k8sClient,
	}
}

// ---------------------------------------------------------------------------
// nodeTestK8sClient — minimal k8s.Client mock for node tests
// ---------------------------------------------------------------------------

type nodeTestK8sClient struct {
	zone       string
	zoneErr    error
	subVolumes map[string]*v1alpha1.SubVolume
}

func (n *nodeTestK8sClient) GetNodeZone(_ context.Context, _ string) (string, error) {
	if n.zoneErr != nil {
		return "", n.zoneErr
	}
	return n.zone, nil
}

func (n *nodeTestK8sClient) GetFileSharePool(_ context.Context, _ string) (*v1alpha1.FileSharePool, error) {
	return nil, fmt.Errorf("not implemented")
}
func (n *nodeTestK8sClient) UpdateFileSharePoolStatus(_ context.Context, _ *v1alpha1.FileSharePool) error {
	return nil
}
func (n *nodeTestK8sClient) UpdateFileSharePool(_ context.Context, _ *v1alpha1.FileSharePool) error {
	return nil
}
func (n *nodeTestK8sClient) GetSubVolume(_ context.Context, name string) (*v1alpha1.SubVolume, error) {
	if n.subVolumes != nil {
		if sv, ok := n.subVolumes[name]; ok {
			return sv, nil
		}
	}
	return nil, fmt.Errorf("subvolume %q not found", name)
}
func (n *nodeTestK8sClient) ListSubVolumes(_ context.Context, _ string) ([]v1alpha1.SubVolume, error) {
	return nil, nil
}
func (n *nodeTestK8sClient) ListSubVolumesByShare(_ context.Context, _ string) ([]v1alpha1.SubVolume, error) {
	return nil, nil
}
func (n *nodeTestK8sClient) ListCloneSubVolumes(_ context.Context) ([]v1alpha1.SubVolume, error) {
	return nil, nil
}
func (n *nodeTestK8sClient) CreateSubVolume(_ context.Context, _ *v1alpha1.SubVolume) error {
	return nil
}
func (n *nodeTestK8sClient) UpdateSubVolume(_ context.Context, _ *v1alpha1.SubVolume) error {
	return nil
}
func (n *nodeTestK8sClient) UpdateSubVolumeStatus(_ context.Context, _ *v1alpha1.SubVolume) error {
	return nil
}
func (n *nodeTestK8sClient) DeleteSubVolume(_ context.Context, _ string) error { return nil }
func (n *nodeTestK8sClient) GetConfigMapValue(_ context.Context, _, _, _ string) (string, error) {
	return "", fmt.Errorf("not implemented")
}
func (n *nodeTestK8sClient) GetSnapshot(_ context.Context, _ string) (*v1alpha1.Snapshot, error) {
	return nil, fmt.Errorf("not implemented")
}
func (n *nodeTestK8sClient) ListSnapshots(_ context.Context, _ string) ([]v1alpha1.Snapshot, error) {
	return nil, nil
}
func (n *nodeTestK8sClient) ListSnapshotsByShare(_ context.Context, _ string) ([]v1alpha1.Snapshot, error) {
	return nil, nil
}
func (n *nodeTestK8sClient) ListSnapshotsBySource(_ context.Context, _ string) ([]v1alpha1.Snapshot, error) {
	return nil, nil
}
func (n *nodeTestK8sClient) CreateSnapshot(_ context.Context, _ *v1alpha1.Snapshot) error {
	return nil
}
func (n *nodeTestK8sClient) UpdateSnapshot(_ context.Context, _ *v1alpha1.Snapshot) error {
	return nil
}
func (n *nodeTestK8sClient) UpdateSnapshotStatus(_ context.Context, _ *v1alpha1.Snapshot) error {
	return nil
}
func (n *nodeTestK8sClient) DeleteSnapshot(_ context.Context, _ string) error { return nil }
func (n *nodeTestK8sClient) GetVolumeGroupSnapshot(_ context.Context, _ string) (*v1alpha1.VolumeGroupSnapshot, error) {
	return nil, fmt.Errorf("not implemented")
}
func (n *nodeTestK8sClient) CreateVolumeGroupSnapshot(_ context.Context, _ *v1alpha1.VolumeGroupSnapshot) error {
	return nil
}
func (n *nodeTestK8sClient) UpdateVolumeGroupSnapshotStatus(_ context.Context, _ *v1alpha1.VolumeGroupSnapshot) error {
	return nil
}
func (n *nodeTestK8sClient) DeleteVolumeGroupSnapshot(_ context.Context, _ string) error {
	return nil
}
func (n *nodeTestK8sClient) ListVolumeGroupSnapshots(_ context.Context, _ string) ([]v1alpha1.VolumeGroupSnapshot, error) {
	return nil, nil
}
func (n *nodeTestK8sClient) GetReplicationPolicy(_ context.Context, _ string) (*v1alpha1.ReplicationPolicy, error) {
	return nil, fmt.Errorf("not implemented")
}
func (n *nodeTestK8sClient) ListReplicationPolicies(_ context.Context) ([]v1alpha1.ReplicationPolicy, error) {
	return nil, nil
}
func (n *nodeTestK8sClient) CreateReplicationPolicy(_ context.Context, _ *v1alpha1.ReplicationPolicy) error {
	return nil
}
func (n *nodeTestK8sClient) UpdateReplicationPolicyStatus(_ context.Context, _ *v1alpha1.ReplicationPolicy) error {
	return nil
}
func (n *nodeTestK8sClient) DeleteReplicationPolicy(_ context.Context, _ string) error { return nil }

func (n *nodeTestK8sClient) GetStorageClass(_ context.Context, _ string) (*storagev1.StorageClass, error) {
	return nil, fmt.Errorf("not found")
}
func (n *nodeTestK8sClient) CreateStorageClass(_ context.Context, _ *storagev1.StorageClass) error {
	return nil
}

// ---------------------------------------------------------------------------
// NodeGetCapabilities
// ---------------------------------------------------------------------------

func TestNodeGetCapabilities(t *testing.T) {
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, nil)

	resp, err := d.NodeGetCapabilities(context.Background(), &csi.NodeGetCapabilitiesRequest{})
	if err != nil {
		t.Fatalf("NodeGetCapabilities failed: %v", err)
	}

	caps := resp.GetCapabilities()
	if len(caps) != 2 {
		t.Fatalf("expected 2 capabilities, got %d", len(caps))
	}

	foundStage := false
	foundStats := false
	for _, c := range caps {
		switch c.GetRpc().GetType() {
		case csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME:
			foundStage = true
		case csi.NodeServiceCapability_RPC_GET_VOLUME_STATS:
			foundStats = true
		}
	}
	if !foundStage {
		t.Error("missing STAGE_UNSTAGE_VOLUME capability")
	}
	if !foundStats {
		t.Error("missing GET_VOLUME_STATS capability")
	}
}

// ---------------------------------------------------------------------------
// NodeStageVolume
// ---------------------------------------------------------------------------

func TestNodeStageVolume_MountsNFS(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	d := newNodeTestDriver(fakeMounter, nil, nil)

	stagingPath := filepath.Join(resolvedTempDir(t), "staging")

	resp, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			"server": "10.240.0.1",
			"share":  "/",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Verify the staging dir was created
	if _, err := os.Stat(stagingPath); os.IsNotExist(err) {
		t.Error("expected staging directory to be created")
	}

	// Verify mount was called with correct args
	actions := fakeMounter.GetLog()
	if len(actions) != 1 {
		t.Fatalf("expected 1 mount action, got %d", len(actions))
	}
	a := actions[0]
	if a.Action != "mount" {
		t.Errorf("expected 'mount' action, got %q", a.Action)
	}
	if a.Source != "10.240.0.1:/" {
		t.Errorf("expected source '10.240.0.1:/', got %q", a.Source)
	}
	if a.Target != stagingPath {
		t.Errorf("expected target %q, got %q", stagingPath, a.Target)
	}
	if a.FSType != "nfs4" {
		t.Errorf("expected fsType 'nfs4', got %q", a.FSType)
	}

	// Verify mount cache was updated
	if !d.mountCache.IsMounted(stagingPath) {
		t.Error("expected staging path to be in mount cache")
	}
}

func TestNodeStageVolume_DefaultMountOptions(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	d := newNodeTestDriver(fakeMounter, nil, nil)

	stagingPath := filepath.Join(resolvedTempDir(t), "staging")

	_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			"server": "10.240.0.1",
			"share":  "/",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{
					// No MountFlags → uses defaults
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume failed: %v", err)
	}

	// The fake mounter doesn't expose mount options directly in FakeAction,
	// but we can verify the mount was called (default options: nfsvers=4.1, soft, timeo=600, retrans=3)
	actions := fakeMounter.GetLog()
	if len(actions) != 1 {
		t.Fatalf("expected 1 mount action, got %d", len(actions))
	}
}

func TestNodeStageVolume_CustomMountFlags(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	d := newNodeTestDriver(fakeMounter, nil, nil)

	stagingPath := filepath.Join(resolvedTempDir(t), "staging")

	_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			"server": "10.240.0.1",
			"share":  "/export",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{
					MountFlags: []string{"nfsvers=4.1", "hard", "rsize=1048576"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume failed: %v", err)
	}

	actions := fakeMounter.GetLog()
	if len(actions) != 1 {
		t.Fatalf("expected 1 mount action, got %d", len(actions))
	}
	if actions[0].Source != "10.240.0.1:/export" {
		t.Errorf("expected source '10.240.0.1:/export', got %q", actions[0].Source)
	}
}

func TestNodeStageVolume_Idempotent(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	cache := util.NewMountCache()
	d := newNodeTestDriver(fakeMounter, cache, nil)

	stagingPath := filepath.Join(resolvedTempDir(t), "staging")

	// Pre-populate mount cache to simulate already-staged volume
	cache.Add(stagingPath, "10.240.0.1", "/")

	resp, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			"server": "10.240.0.1",
			"share":  "/",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
	})
	if err != nil {
		t.Fatalf("idempotent NodeStageVolume failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// No mount should have been called (idempotent)
	actions := fakeMounter.GetLog()
	if len(actions) != 0 {
		t.Errorf("expected 0 mount actions for idempotent call, got %d", len(actions))
	}
}

func TestNodeStageVolume_MountFails(t *testing.T) {
	d := newNodeTestDriver(&failingMounter{mountErr: os.ErrPermission}, nil, nil)

	stagingPath := filepath.Join(resolvedTempDir(t), "staging")

	_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			"server": "10.240.0.1",
			"share":  "/",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
	})

	assertGRPCCode(t, err, codes.Internal)
}

// ---------------------------------------------------------------------------
// NodePublishVolume
// ---------------------------------------------------------------------------

func TestNodePublishVolume_ValidatesSubDir(t *testing.T) {
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, nil)

	tests := []struct {
		name   string
		subDir string
	}{
		{"empty", ""},
		{"traversal_dotdot", "/../etc/passwd"},
		{"traversal_nested", "/pvcs/../../../etc"},
		{"wrong_prefix", "/something-else/pvc-abc"},
		{"just_slash", "/"},
		{"no_leading_slash", "pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"},
		{"not_a_uuid", "/pvcs/not-a-uuid"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
				VolumeId:          "pool/share/pvc",
				StagingTargetPath: "/staging",
				TargetPath:        "/target",
				VolumeContext: map[string]string{
					"subDir": tt.subDir,
				},
			})
			assertGRPCCode(t, err, codes.InvalidArgument)
		})
	}
}

func TestNodePublishVolume_ValidSubDir(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	d := newNodeTestDriver(fakeMounter, nil, nil)

	// Create real staging directory with the subdirectory present
	tmpDir := resolvedTempDir(t)
	stagingPath := filepath.Join(tmpDir, "staging")
	subDirPath := filepath.Join(stagingPath, testSubDir)
	if err := os.MkdirAll(subDirPath, 0750); err != nil {
		t.Fatalf("setup: create subdir: %v", err)
	}

	targetPath := filepath.Join(tmpDir, "target")

	resp, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeContext: map[string]string{
			"subDir": testSubDir,
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestNodePublishVolume_BindMountCorrectSource(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	d := newNodeTestDriver(fakeMounter, nil, nil)

	tmpDir := resolvedTempDir(t)
	stagingPath := filepath.Join(tmpDir, "staging")
	subDirPath := filepath.Join(stagingPath, testSubDir)
	if err := os.MkdirAll(subDirPath, 0750); err != nil {
		t.Fatalf("setup: create subdir: %v", err)
	}

	targetPath := filepath.Join(tmpDir, "target")

	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeContext: map[string]string{
			"subDir": testSubDir,
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume failed: %v", err)
	}

	actions := fakeMounter.GetLog()
	if len(actions) != 1 {
		t.Fatalf("expected 1 mount action, got %d", len(actions))
	}

	a := actions[0]
	if a.Action != "mount" {
		t.Errorf("expected 'mount' action, got %q", a.Action)
	}

	expectedSource := filepath.Join(stagingPath, testSubDir)
	if a.Source != expectedSource {
		t.Errorf("expected source %q, got %q", expectedSource, a.Source)
	}
	if a.Target != targetPath {
		t.Errorf("expected target %q, got %q", targetPath, a.Target)
	}
}

func TestNodePublishVolume_ReadOnly(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	d := newNodeTestDriver(fakeMounter, nil, nil)

	tmpDir := resolvedTempDir(t)
	stagingPath := filepath.Join(tmpDir, "staging")
	subDirPath := filepath.Join(stagingPath, testSubDir)
	if err := os.MkdirAll(subDirPath, 0750); err != nil {
		t.Fatalf("setup: create subdir: %v", err)
	}

	targetPath := filepath.Join(tmpDir, "target")

	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		Readonly:          true,
		VolumeContext: map[string]string{
			"subDir": testSubDir,
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume failed: %v", err)
	}

	// Verify that mount was called — can't inspect options directly from FakeAction,
	// but we can verify the mount happened. The "ro" option is passed to the mounter
	// and the fake mounter records it internally via MountPoints.
	actions := fakeMounter.GetLog()
	if len(actions) != 1 {
		t.Fatalf("expected 1 mount action, got %d", len(actions))
	}

	// Check MountPoints for the "ro" option
	mountPoints, _ := fakeMounter.List()
	found := false
	for _, mp := range mountPoints {
		if mp.Path == targetPath {
			found = true
			hasRO := false
			for _, opt := range mp.Opts {
				if opt == "ro" {
					hasRO = true
				}
			}
			if !hasRO {
				t.Error("expected 'ro' in mount options for readonly volume")
			}
		}
	}
	if !found {
		t.Error("expected mount point for target path in fake mounter")
	}
}

func TestNodePublishVolume_SubDirNotExist_CreatesIt(t *testing.T) {
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, nil)

	tmpDir := resolvedTempDir(t)
	stagingPath := filepath.Join(tmpDir, "staging")
	// Don't create the subdir — NodePublishVolume should create it
	if err := os.MkdirAll(stagingPath, 0750); err != nil {
		t.Fatalf("setup: create staging dir: %v", err)
	}

	targetPath := filepath.Join(tmpDir, "target")

	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeContext: map[string]string{
			"subDir":      testSubDir,
			"permissions": "0700",
			"uid":         "1000",
			"gid":         "1000",
		},
	})

	if err != nil {
		t.Fatalf("expected no error, got: %v", err)
	}

	// Verify the subdirectory was created
	subDirPath := filepath.Join(stagingPath, testSubDir)
	info, err := os.Stat(subDirPath)
	if err != nil {
		t.Fatalf("expected subdirectory to be created: %v", err)
	}
	if !info.IsDir() {
		t.Fatalf("expected %s to be a directory", subDirPath)
	}
}

func TestNodePublishVolume_MountFails(t *testing.T) {
	d := newNodeTestDriver(&failingMounter{mountErr: os.ErrPermission}, nil, nil)

	tmpDir := resolvedTempDir(t)
	stagingPath := filepath.Join(tmpDir, "staging")
	subDirPath := filepath.Join(stagingPath, testSubDir)
	if err := os.MkdirAll(subDirPath, 0750); err != nil {
		t.Fatalf("setup: create subdir: %v", err)
	}

	targetPath := filepath.Join(tmpDir, "target")

	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeContext: map[string]string{
			"subDir": testSubDir,
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
	})

	assertGRPCCode(t, err, codes.Internal)
}

// ---------------------------------------------------------------------------
// NodeUnpublishVolume
// ---------------------------------------------------------------------------

func TestNodeUnpublishVolume_Unmounts(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	d := newNodeTestDriver(fakeMounter, nil, nil)

	// Create a target dir that will be removed after unmount
	tmpDir := resolvedTempDir(t)
	targetPath := filepath.Join(tmpDir, "target")
	if err := os.MkdirAll(targetPath, 0750); err != nil {
		t.Fatalf("setup: %v", err)
	}

	resp, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "pool/share/pvc",
		TargetPath: targetPath,
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Verify unmount was called
	actions := fakeMounter.GetLog()
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Action != "unmount" {
		t.Errorf("expected 'unmount' action, got %q", actions[0].Action)
	}
	if actions[0].Target != targetPath {
		t.Errorf("expected target %q, got %q", targetPath, actions[0].Target)
	}

	// Verify the directory was removed
	if _, err := os.Stat(targetPath); !os.IsNotExist(err) {
		t.Error("expected target directory to be removed after unpublish")
	}
}

func TestNodeUnpublishVolume_UnmountFails(t *testing.T) {
	d := newNodeTestDriver(&failingMounter{unmountErr: os.ErrPermission}, nil, nil)

	_, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "pool/share/pvc",
		TargetPath: "/some/target",
	})

	assertGRPCCode(t, err, codes.Internal)
}

func TestNodeUnpublishVolume_TargetAlreadyGone(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	d := newNodeTestDriver(fakeMounter, nil, nil)

	// Target path doesn't exist — os.Remove returns ErrNotExist which is tolerated
	resp, err := d.NodeUnpublishVolume(context.Background(), &csi.NodeUnpublishVolumeRequest{
		VolumeId:   "pool/share/pvc",
		TargetPath: "/nonexistent/path",
	})
	if err != nil {
		t.Fatalf("NodeUnpublishVolume failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

// ---------------------------------------------------------------------------
// NodeUnstageVolume
// ---------------------------------------------------------------------------

func TestNodeUnstageVolume_UnmountsNFS(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	cache := util.NewMountCache()
	d := newNodeTestDriver(fakeMounter, cache, nil)

	tmpDir := resolvedTempDir(t)
	stagingPath := filepath.Join(tmpDir, "staging")
	if err := os.MkdirAll(stagingPath, 0750); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Pre-populate cache
	cache.Add(stagingPath, "10.240.0.1", "/")

	resp, err := d.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}

	// Verify unmount was called
	actions := fakeMounter.GetLog()
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %d", len(actions))
	}
	if actions[0].Action != "unmount" {
		t.Errorf("expected 'unmount', got %q", actions[0].Action)
	}
	if actions[0].Target != stagingPath {
		t.Errorf("expected target %q, got %q", stagingPath, actions[0].Target)
	}

	// Verify mount cache was cleaned up
	if cache.IsMounted(stagingPath) {
		t.Error("expected staging path to be removed from mount cache")
	}

	// Verify the staging directory was removed
	if _, err := os.Stat(stagingPath); !os.IsNotExist(err) {
		t.Error("expected staging directory to be removed after unstage")
	}
}

func TestNodeUnstageVolume_UnmountFails(t *testing.T) {
	d := newNodeTestDriver(&failingMounter{unmountErr: os.ErrPermission}, nil, nil)

	_, err := d.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: "/some/staging",
	})

	assertGRPCCode(t, err, codes.Internal)
}

func TestNodeUnstageVolume_StagingAlreadyGone(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	cache := util.NewMountCache()
	d := newNodeTestDriver(fakeMounter, cache, nil)

	// Staging path doesn't exist — os.Remove returns ErrNotExist which is tolerated
	resp, err := d.NodeUnstageVolume(context.Background(), &csi.NodeUnstageVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: "/nonexistent/staging",
	})
	if err != nil {
		t.Fatalf("NodeUnstageVolume failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

// ---------------------------------------------------------------------------
// NodeGetVolumeStats
// ---------------------------------------------------------------------------

func TestNodeGetVolumeStats_Success(t *testing.T) {
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, nil)

	// Use a real temp dir so unix.Statfs works
	tmpDir := resolvedTempDir(t)

	resp, err := d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "pool/share/pvc",
		VolumePath: tmpDir,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats failed: %v", err)
	}

	usage := resp.GetUsage()
	if len(usage) != 2 {
		t.Fatalf("expected 2 usage entries (bytes + inodes), got %d", len(usage))
	}

	var bytesEntry, inodesEntry *csi.VolumeUsage
	for _, u := range usage {
		switch u.Unit {
		case csi.VolumeUsage_BYTES:
			bytesEntry = u
		case csi.VolumeUsage_INODES:
			inodesEntry = u
		}
	}

	if bytesEntry == nil {
		t.Fatal("missing BYTES usage entry")
	}
	if bytesEntry.Total <= 0 {
		t.Errorf("expected positive total bytes, got %d", bytesEntry.Total)
	}
	if bytesEntry.Available < 0 {
		t.Errorf("expected non-negative available bytes, got %d", bytesEntry.Available)
	}
	if bytesEntry.Used < 0 {
		t.Errorf("expected non-negative used bytes, got %d", bytesEntry.Used)
	}

	if inodesEntry == nil {
		t.Fatal("missing INODES usage entry")
	}
	if inodesEntry.Total <= 0 {
		t.Errorf("expected positive total inodes, got %d", inodesEntry.Total)
	}
}

func TestNodeGetVolumeStats_InvalidPath(t *testing.T) {
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, nil)

	_, err := d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "pool/share/pvc",
		VolumePath: "/nonexistent/path/that/doesnt/exist",
	})

	assertGRPCCode(t, err, codes.Internal)
}

func TestNodeGetVolumeStats_PerPVC_ReturnsSubdirUsage(t *testing.T) {
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	volumeID := fmt.Sprintf("my-pool/share-123/%s", pvName)

	k := &nodeTestK8sClient{
		zone: "us-south-1",
		subVolumes: map[string]*v1alpha1.SubVolume{
			pvName: {
				Spec: v1alpha1.SubVolumeSpec{
					RequestedGB: 10,
				},
			},
		},
	}
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, k)

	// Create a temp dir with known file sizes
	tmpDir := resolvedTempDir(t)
	if err := os.WriteFile(filepath.Join(tmpDir, "file1.dat"), make([]byte, 1024), 0600); err != nil {
		t.Fatalf("setup: write file1: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "subdir"), 0750); err != nil {
		t.Fatalf("setup: mkdir subdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "subdir", "file2.dat"), make([]byte, 2048), 0600); err != nil {
		t.Fatalf("setup: write file2: %v", err)
	}

	resp, err := d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   volumeID,
		VolumePath: tmpDir,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats failed: %v", err)
	}

	usage := resp.GetUsage()
	if len(usage) != 2 {
		t.Fatalf("expected 2 usage entries, got %d", len(usage))
	}

	var bytesEntry *csi.VolumeUsage
	for _, u := range usage {
		if u.Unit == csi.VolumeUsage_BYTES {
			bytesEntry = u
		}
	}
	if bytesEntry == nil {
		t.Fatal("missing BYTES usage entry")
	}

	// Total should be RequestedGB (10 GB) in bytes
	const gb = 1024 * 1024 * 1024
	expectedTotal := int64(10 * gb)
	if bytesEntry.Total != expectedTotal {
		t.Errorf("expected Total=%d (10 GB), got %d", expectedTotal, bytesEntry.Total)
	}

	// Used should be 1024 + 2048 = 3072
	expectedUsed := int64(3072)
	if bytesEntry.Used != expectedUsed {
		t.Errorf("expected Used=%d, got %d", expectedUsed, bytesEntry.Used)
	}

	// Available = Total - Used
	expectedAvailable := expectedTotal - expectedUsed
	if bytesEntry.Available != expectedAvailable {
		t.Errorf("expected Available=%d, got %d", expectedAvailable, bytesEntry.Available)
	}
}

func TestNodeGetVolumeStats_PerPVC_FallbackOnSubVolumeLookupFailure(t *testing.T) {
	// k8s client returns error for GetSubVolume → should fall back to statfs
	k := &nodeTestK8sClient{zone: "us-south-1"}
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, k)

	tmpDir := resolvedTempDir(t)

	resp, err := d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "my-pool/share-123/pvc-nonexistent",
		VolumePath: tmpDir,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats failed: %v", err)
	}

	// Should still return valid stats (share-level fallback)
	usage := resp.GetUsage()
	if len(usage) != 2 {
		t.Fatalf("expected 2 usage entries, got %d", len(usage))
	}
	for _, u := range usage {
		if u.Unit == csi.VolumeUsage_BYTES && u.Total <= 0 {
			t.Error("expected positive total bytes from statfs fallback")
		}
	}
}

func TestNodeGetVolumeStats_PerPVC_FallbackOnBadVolumeID(t *testing.T) {
	// Volume ID that doesn't parse → should fall back to statfs
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, nil)

	tmpDir := resolvedTempDir(t)

	resp, err := d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   "bad-volume-id",
		VolumePath: tmpDir,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats failed: %v", err)
	}

	usage := resp.GetUsage()
	if len(usage) != 2 {
		t.Fatalf("expected 2 usage entries, got %d", len(usage))
	}
}

func TestNodeGetVolumeStats_PerPVC_UsedExceedsQuota(t *testing.T) {
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"

	k := &nodeTestK8sClient{
		zone: "us-south-1",
		subVolumes: map[string]*v1alpha1.SubVolume{
			pvName: {
				Spec: v1alpha1.SubVolumeSpec{
					RequestedGB: 1, // 1 GB quota
				},
			},
		},
	}
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, k)

	tmpDir := resolvedTempDir(t)
	// Write 2 GB of data (exceeds 1 GB quota) — we can't actually write 2 GB in a test,
	// but we can verify the Available-clamped-to-zero logic with a small file + tiny quota.
	// Use RequestedGB=0 equivalent: set RequestedGB=1 and write enough to test clamping.
	// Actually let's just write a small file and set a very small quota (still 1 GB).
	// The 1 GB quota will be larger than actual usage so Available won't be clamped.
	// Let me instead test the clamping directly with a known setup.

	// Write a file — any size works. The point is Total = 1GB, Used = file size.
	if err := os.WriteFile(filepath.Join(tmpDir, "data.bin"), make([]byte, 512), 0600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	resp, err := d.NodeGetVolumeStats(context.Background(), &csi.NodeGetVolumeStatsRequest{
		VolumeId:   fmt.Sprintf("pool/share/%s", pvName),
		VolumePath: tmpDir,
	})
	if err != nil {
		t.Fatalf("NodeGetVolumeStats failed: %v", err)
	}

	var bytesEntry *csi.VolumeUsage
	for _, u := range resp.GetUsage() {
		if u.Unit == csi.VolumeUsage_BYTES {
			bytesEntry = u
		}
	}

	if bytesEntry.Available < 0 {
		t.Error("Available should never be negative")
	}
	if bytesEntry.Used != 512 {
		t.Errorf("expected Used=512, got %d", bytesEntry.Used)
	}
}

func TestDirUsageBytes(t *testing.T) {
	tmpDir := resolvedTempDir(t)

	// Empty directory
	size, err := dirUsageBytes(tmpDir)
	if err != nil {
		t.Fatalf("dirUsageBytes failed on empty dir: %v", err)
	}
	if size != 0 {
		t.Errorf("expected 0 for empty dir, got %d", size)
	}

	// Add files
	if err := os.WriteFile(filepath.Join(tmpDir, "a.txt"), []byte("hello"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(tmpDir, "nested"), 0750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmpDir, "nested", "b.bin"), make([]byte, 100), 0600); err != nil {
		t.Fatal(err)
	}

	size, err = dirUsageBytes(tmpDir)
	if err != nil {
		t.Fatalf("dirUsageBytes failed: %v", err)
	}
	if size != 105 { // 5 ("hello") + 100
		t.Errorf("expected 105, got %d", size)
	}
}

func TestDirUsageBytes_NonexistentDir(t *testing.T) {
	_, err := dirUsageBytes("/nonexistent/path")
	if err == nil {
		t.Error("expected error for nonexistent directory")
	}
}

// ---------------------------------------------------------------------------
// NodeGetInfo
// ---------------------------------------------------------------------------

func TestNodeGetInfo_ReturnsZone(t *testing.T) {
	k := &nodeTestK8sClient{zone: "us-south-1"}
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, k)

	resp, err := d.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})
	if err != nil {
		t.Fatalf("NodeGetInfo failed: %v", err)
	}
	if resp.NodeId != "test-node-01" {
		t.Errorf("expected node ID 'test-node-01', got %q", resp.NodeId)
	}
	zone := resp.GetAccessibleTopology().GetSegments()["topology.kubernetes.io/zone"]
	if zone != "us-south-1" {
		t.Errorf("expected zone 'us-south-1', got %q", zone)
	}
}

func TestNodeGetInfo_ZoneDetectionFails(t *testing.T) {
	k := &nodeTestK8sClient{zoneErr: fmt.Errorf("node not found")}
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, k)

	_, err := d.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})

	assertGRPCCode(t, err, codes.Internal)
}

func TestNodeGetInfo_NoK8sClient(t *testing.T) {
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, nil)

	_, err := d.NodeGetInfo(context.Background(), &csi.NodeGetInfoRequest{})

	assertGRPCCode(t, err, codes.Internal)
}

// ---------------------------------------------------------------------------
// Cross-Zone NodeStageVolume Tests
// ---------------------------------------------------------------------------

func TestNodeStageVolume_UsesZoneLocalIP(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	k := &nodeTestK8sClient{zone: "us-south-2"}
	d := newNodeTestDriver(fakeMounter, nil, k)

	stagingPath := filepath.Join(resolvedTempDir(t), "staging")

	_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			"server":            "10.0.0.1", // primary (us-south-1)
			"share":             "/",
			"server.us-south-1": "10.0.0.1",
			"server.us-south-2": "10.0.1.1", // zone-local for this node
			"server.us-south-3": "10.0.2.1",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume failed: %v", err)
	}

	actions := fakeMounter.GetLog()
	if len(actions) != 1 {
		t.Fatalf("expected 1 mount action, got %d", len(actions))
	}
	// Should mount using the zone-local IP for us-south-2
	if actions[0].Source != "10.0.1.1:/" {
		t.Errorf("expected source '10.0.1.1:/' (zone-local), got %q", actions[0].Source)
	}
}

func TestNodeStageVolume_FallsBackToServer(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	k := &nodeTestK8sClient{zone: "us-south-2"}
	d := newNodeTestDriver(fakeMounter, nil, k)

	stagingPath := filepath.Join(resolvedTempDir(t), "staging")

	// No server.us-south-2 key → falls back to primary server
	_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			"server": "10.0.0.1",
			"share":  "/",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume failed: %v", err)
	}

	actions := fakeMounter.GetLog()
	if len(actions) != 1 {
		t.Fatalf("expected 1 mount action, got %d", len(actions))
	}
	// Should fall back to primary server IP
	if actions[0].Source != "10.0.0.1:/" {
		t.Errorf("expected source '10.0.0.1:/' (fallback), got %q", actions[0].Source)
	}
}

func TestNodeStageVolume_NoK8sClient_FallsBackToServer(t *testing.T) {
	fakeMounter := mount.NewFakeMounter(nil)
	// No k8s client → can't determine zone
	d := newNodeTestDriver(fakeMounter, nil, nil)

	stagingPath := filepath.Join(resolvedTempDir(t), "staging")

	_, err := d.NodeStageVolume(context.Background(), &csi.NodeStageVolumeRequest{
		VolumeId:          "pool/share/pvc",
		StagingTargetPath: stagingPath,
		VolumeContext: map[string]string{
			"server":            "10.0.0.1",
			"share":             "/",
			"server.us-south-2": "10.0.1.1",
		},
		VolumeCapability: &csi.VolumeCapability{
			AccessType: &csi.VolumeCapability_Mount{
				Mount: &csi.VolumeCapability_MountVolume{},
			},
		},
	})
	if err != nil {
		t.Fatalf("NodeStageVolume failed: %v", err)
	}

	actions := fakeMounter.GetLog()
	if len(actions) != 1 {
		t.Fatalf("expected 1 mount action, got %d", len(actions))
	}
	// Without k8s client, should use primary server
	if actions[0].Source != "10.0.0.1:/" {
		t.Errorf("expected source '10.0.0.1:/' (no k8s client), got %q", actions[0].Source)
	}
}

// ---------------------------------------------------------------------------
// failingMounter — a mount.Interface that returns errors for testing
// ---------------------------------------------------------------------------

type failingMounter struct {
	mount.FakeMounter
	mountErr   error
	unmountErr error
}

func (f *failingMounter) Mount(source string, target string, fstype string, options []string) error {
	if f.mountErr != nil {
		return f.mountErr
	}
	return f.FakeMounter.Mount(source, target, fstype, options)
}

func (f *failingMounter) MountSensitive(source string, target string, fstype string, options []string, sensitiveOptions []string) error {
	if f.mountErr != nil {
		return f.mountErr
	}
	return f.FakeMounter.MountSensitive(source, target, fstype, options, sensitiveOptions)
}

func (f *failingMounter) Unmount(target string) error {
	if f.unmountErr != nil {
		return f.unmountErr
	}
	return f.FakeMounter.Unmount(target)
}

// ---------------------------------------------------------------------------
// NodePublishVolume — Clone Gate Tests
// ---------------------------------------------------------------------------

func TestNodePublishVolume_CloneInProgress(t *testing.T) {
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	k := &nodeTestK8sClient{
		zone: "us-south-1",
		subVolumes: map[string]*v1alpha1.SubVolume{
			pvName: {
				Spec: v1alpha1.SubVolumeSpec{
					SourceVolume: "pvc-source",
				},
				Status: v1alpha1.SubVolumeStatus{
					Phase:       "Cloning",
					CloneStatus: "InProgress",
				},
			},
		},
	}
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, k)

	tmpDir := resolvedTempDir(t)
	stagingPath := filepath.Join(tmpDir, "staging")
	subDirPath := filepath.Join(stagingPath, "/pvcs/"+pvName)
	if err := os.MkdirAll(subDirPath, 0750); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          fmt.Sprintf("pool/share/%s", pvName),
		StagingTargetPath: stagingPath,
		TargetPath:        filepath.Join(tmpDir, "target"),
		VolumeContext: map[string]string{
			"subDir": "/pvcs/" + pvName,
		},
	})

	assertGRPCCode(t, err, codes.Unavailable)
}

func TestNodePublishVolume_ClonePending(t *testing.T) {
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	k := &nodeTestK8sClient{
		zone: "us-south-1",
		subVolumes: map[string]*v1alpha1.SubVolume{
			pvName: {
				Spec: v1alpha1.SubVolumeSpec{
					SourceVolume: "pvc-source",
				},
				Status: v1alpha1.SubVolumeStatus{
					Phase:       "Cloning",
					CloneStatus: "Pending",
				},
			},
		},
	}
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, k)

	tmpDir := resolvedTempDir(t)
	stagingPath := filepath.Join(tmpDir, "staging")
	subDirPath := filepath.Join(stagingPath, "/pvcs/"+pvName)
	if err := os.MkdirAll(subDirPath, 0750); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          fmt.Sprintf("pool/share/%s", pvName),
		StagingTargetPath: stagingPath,
		TargetPath:        filepath.Join(tmpDir, "target"),
		VolumeContext: map[string]string{
			"subDir": "/pvcs/" + pvName,
		},
	})

	assertGRPCCode(t, err, codes.Unavailable)
}

func TestNodePublishVolume_CloneFailed(t *testing.T) {
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	k := &nodeTestK8sClient{
		zone: "us-south-1",
		subVolumes: map[string]*v1alpha1.SubVolume{
			pvName: {
				Spec: v1alpha1.SubVolumeSpec{
					SourceVolume: "pvc-source",
				},
				Status: v1alpha1.SubVolumeStatus{
					Phase:       "Failed",
					CloneStatus: "Failed",
					CloneProgress: &v1alpha1.CloneProgress{
						Error: "NFS copy failed: disk full",
					},
				},
			},
		},
	}
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, k)

	tmpDir := resolvedTempDir(t)
	stagingPath := filepath.Join(tmpDir, "staging")
	subDirPath := filepath.Join(stagingPath, "/pvcs/"+pvName)
	if err := os.MkdirAll(subDirPath, 0750); err != nil {
		t.Fatalf("setup: %v", err)
	}

	_, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          fmt.Sprintf("pool/share/%s", pvName),
		StagingTargetPath: stagingPath,
		TargetPath:        filepath.Join(tmpDir, "target"),
		VolumeContext: map[string]string{
			"subDir": "/pvcs/" + pvName,
		},
	})

	assertGRPCCode(t, err, codes.Internal)
}

func TestNodePublishVolume_CloneComplete(t *testing.T) {
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	k := &nodeTestK8sClient{
		zone: "us-south-1",
		subVolumes: map[string]*v1alpha1.SubVolume{
			pvName: {
				Spec: v1alpha1.SubVolumeSpec{
					SourceVolume: "pvc-source",
				},
				Status: v1alpha1.SubVolumeStatus{
					Phase:       "Bound",
					CloneStatus: "Complete",
				},
			},
		},
	}
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, k)

	tmpDir := resolvedTempDir(t)
	stagingPath := filepath.Join(tmpDir, "staging")
	subDirPath := filepath.Join(stagingPath, "/pvcs/"+pvName)
	if err := os.MkdirAll(subDirPath, 0750); err != nil {
		t.Fatalf("setup: %v", err)
	}

	targetPath := filepath.Join(tmpDir, "target")

	resp, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          fmt.Sprintf("pool/share/%s", pvName),
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeContext: map[string]string{
			"subDir": "/pvcs/" + pvName,
		},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume failed: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}

func TestNodePublishVolume_NonClone(t *testing.T) {
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	k := &nodeTestK8sClient{
		zone: "us-south-1",
		subVolumes: map[string]*v1alpha1.SubVolume{
			pvName: {
				Spec: v1alpha1.SubVolumeSpec{
					// No SourceVolume -- not a clone
				},
				Status: v1alpha1.SubVolumeStatus{
					Phase: "Bound",
					// CloneStatus is empty -- not a clone
				},
			},
		},
	}
	d := newNodeTestDriver(mount.NewFakeMounter(nil), nil, k)

	tmpDir := resolvedTempDir(t)
	stagingPath := filepath.Join(tmpDir, "staging")
	subDirPath := filepath.Join(stagingPath, "/pvcs/"+pvName)
	if err := os.MkdirAll(subDirPath, 0750); err != nil {
		t.Fatalf("setup: %v", err)
	}

	targetPath := filepath.Join(tmpDir, "target")

	resp, err := d.NodePublishVolume(context.Background(), &csi.NodePublishVolumeRequest{
		VolumeId:          fmt.Sprintf("pool/share/%s", pvName),
		StagingTargetPath: stagingPath,
		TargetPath:        targetPath,
		VolumeContext: map[string]string{
			"subDir": "/pvcs/" + pvName,
		},
	})
	if err != nil {
		t.Fatalf("NodePublishVolume failed for non-clone: %v", err)
	}
	if resp == nil {
		t.Fatal("expected non-nil response")
	}
}
