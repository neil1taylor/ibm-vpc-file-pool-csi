package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeShareData applies share data calls to a fakeNFSOperations rooted at
// base, so tests can check folders the same way for every operation. When
// inProgress is set, calls report "not done" and change nothing.
type fakeShareData struct {
	mu         sync.Mutex
	nfs        *fakeNFSOperations
	base       string
	inProgress bool
	removes    []shareDataCall
	copies     []shareDataCall
}

type shareDataCall struct {
	key      string
	src, dst ShareMount
	srcPath  string
	dstPath  string
	owner    *Ownership
}

func (f *fakeShareData) RemoveDir(_ context.Context, key string, share ShareMount, path string) (bool, error) {
	f.mu.Lock()
	f.removes = append(f.removes, shareDataCall{key: key, dst: share, dstPath: path})
	inProgress := f.inProgress
	f.mu.Unlock()
	if inProgress {
		return false, nil
	}
	if err := f.nfs.RemoveAll(f.base + path); err != nil {
		return false, err
	}
	return true, nil
}

func (f *fakeShareData) CopyDir(_ context.Context, key string, src ShareMount, srcPath string, dst ShareMount, dstPath string, owner *Ownership) (bool, error) {
	f.mu.Lock()
	f.copies = append(f.copies, shareDataCall{key: key, src: src, srcPath: srcPath, dst: dst, dstPath: dstPath, owner: owner})
	inProgress := f.inProgress
	f.mu.Unlock()
	if inProgress {
		return false, nil
	}
	if err := f.nfs.CopyDir(f.base+srcPath, f.base+dstPath); err != nil {
		return false, err
	}
	return true, nil
}

func (f *fakeShareData) setInProgress(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.inProgress = v
}

func (f *fakeShareData) lastRemove(t *testing.T) shareDataCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.removes) == 0 {
		t.Fatal("no RemoveDir call")
	}
	return f.removes[len(f.removes)-1]
}

func (f *fakeShareData) lastCopy(t *testing.T) shareDataCall {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.copies) == 0 {
		t.Fatal("no CopyDir call")
	}
	return f.copies[len(f.copies)-1]
}

func shareDataOf(m *Manager) *fakeShareData {
	return m.shareData.(*fakeShareData)
}

const testRestorePVName = "pvc-b2c3d4e5-6789-01ab-cdef-234567890abc"

func newShareDataTestManager(t *testing.T) (*fakeK8sClient, *fakeNFSOperations, *Manager) {
	t.Helper()
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()
	share := newStableShare("share-1", "s1", 1000, 100, 1)
	share.ExportPath = "/share_one"
	k.addPool(newTestPool("test-pool", "spread", 1000, share))
	sv := newTestSubVolume(testPVName, "test-pool", "share-1", 10)
	sv.Spec.ShareExportPath = "/share_one"
	k.addSubVolume(sv)
	return k, nfs, newManagerForTest(k, fake.NewFakeVPCClient(), nfs)
}

func addReadySnapshot(k *fakeK8sClient) {
	now := metav1.Now()
	k.addSnapshot(&v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: "my-snapshot"},
		Spec: v1alpha1.SnapshotSpec{
			SourceSubVolume:    testPVName,
			PoolName:           "test-pool",
			ShareID:            "share-1",
			ShareMountTargetIP: "10.240.0.1",
			SnapshotPath:       "/pvcs/.snapshots/snap-my-snapshot",
			SourceSubPath:      "/pvcs/" + testPVName,
			SizeGB:             10,
		},
		Status: v1alpha1.SnapshotStatus{Phase: "Ready", ReadyToUse: true, CreationTime: &now},
	})
}

var shareOne = ShareMount{MountTargetIP: "10.240.0.1", ExportPath: "/share_one"}

func TestDeallocate_RemovesFolderOnItsShare(t *testing.T) {
	_, _, mgr := newShareDataTestManager(t)

	if err := mgr.Deallocate(context.Background(), testPVName); err != nil {
		t.Fatalf("Deallocate: %v", err)
	}
	call := shareDataOf(mgr).lastRemove(t)
	if call.dst != shareOne || call.dstPath != "/pvcs/"+testPVName {
		t.Errorf("removed %s on %+v, want /pvcs/%s on %+v", call.dstPath, call.dst, testPVName, shareOne)
	}
}

func TestDeallocate_RemoveInProgress_KeepsVolumeAndCapacity(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	shareDataOf(mgr).setInProgress(true)

	err := mgr.Deallocate(context.Background(), testPVName)
	if !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("Deallocate() error = %v, want ErrOperationInProgress", err)
	}
	if k.getSubVolume(testPVName) == nil {
		t.Error("SubVolume deleted before its folder was removed")
	}
	if got := k.getPool("test-pool").Status.Shares[0].AllocatedGB; got != 100 {
		t.Errorf("capacity freed before the folder was removed: AllocatedGB = %d, want 100", got)
	}

	shareDataOf(mgr).setInProgress(false)
	if err := mgr.Deallocate(context.Background(), testPVName); err != nil {
		t.Fatalf("Deallocate after removal: %v", err)
	}
	if k.getSubVolume(testPVName) != nil {
		t.Error("SubVolume kept after its folder was removed")
	}
}

func TestCreateSnapshot_CopiesVolumeOnItsShare(t *testing.T) {
	_, _, mgr := newShareDataTestManager(t)

	_, err := mgr.CreateSnapshot(context.Background(), "my-snapshot", "test-pool/share-1/"+testPVName, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	call := shareDataOf(mgr).lastCopy(t)
	if call.src != shareOne || call.dst != shareOne {
		t.Errorf("copy between %+v and %+v, want both on %+v", call.src, call.dst, shareOne)
	}
	if call.srcPath != "/pvcs/"+testPVName || call.dstPath != "/pvcs/.snapshots/snap-my-snapshot" {
		t.Errorf("copy %s -> %s", call.srcPath, call.dstPath)
	}
	if call.owner != nil {
		t.Errorf("snapshot copy must keep the copied owners, got %+v", call.owner)
	}
}

func TestCreateSnapshot_CopyInProgress_NotReadyUntilCopied(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	sd := shareDataOf(mgr)
	sd.setInProgress(true)
	ctx := context.Background()
	src := "test-pool/share-1/" + testPVName

	result, err := mgr.CreateSnapshot(ctx, "my-snapshot", src, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	if result.ReadyToUse {
		t.Error("snapshot reported ready before its copy finished")
	}
	snap := k.getSnapshot("my-snapshot")
	if snap == nil || snap.Status.ReadyToUse {
		t.Fatalf("Snapshot record = %+v, want present and not ready", snap)
	}

	sd.setInProgress(false)
	result, err = mgr.CreateSnapshot(ctx, "my-snapshot", src, nil)
	if err != nil {
		t.Fatalf("CreateSnapshot after copy: %v", err)
	}
	if !result.ReadyToUse || !k.getSnapshot("my-snapshot").Status.ReadyToUse {
		t.Error("snapshot not ready after its copy finished")
	}
	if got := k.getPool("test-pool").Status.Shares[0].AllocatedGB; got != 110 {
		t.Errorf("snapshot capacity counted %d times: AllocatedGB = %d, want 110", (got-100)/10, got)
	}
}

func TestDeleteSnapshot_RemovesFolderOnItsShare(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	addReadySnapshot(k)

	if err := mgr.DeleteSnapshot(context.Background(), "my-snapshot"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	call := shareDataOf(mgr).lastRemove(t)
	if call.dst != shareOne || call.dstPath != "/pvcs/.snapshots/snap-my-snapshot" {
		t.Errorf("removed %s on %+v", call.dstPath, call.dst)
	}
}

func TestDeleteSnapshot_RemoveInProgress_KeepsRecord(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	addReadySnapshot(k)
	shareDataOf(mgr).setInProgress(true)

	err := mgr.DeleteSnapshot(context.Background(), "my-snapshot")
	if !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("DeleteSnapshot() error = %v, want ErrOperationInProgress", err)
	}
	if k.getSnapshot("my-snapshot") == nil {
		t.Error("Snapshot record deleted before its folder was removed")
	}
}

func restoreRequest() AllocationRequest {
	return AllocationRequest{
		PVName:       testRestorePVName,
		PVCName:      "restore-pvc",
		PVCNamespace: "default",
		PoolName:     "test-pool",
		RequestedGB:  10,
	}
}

func TestRestoreSnapshot_CopiesSnapshotIntoNewVolumeWithOwner(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	addReadySnapshot(k)
	pool := k.getPool("test-pool")
	uid, gid := int64(107), int64(107)
	pool.Spec.DefaultUID, pool.Spec.DefaultGID, pool.Spec.DefaultPermissions = &uid, &gid, "0777"
	k.addPool(pool)

	if _, err := mgr.RestoreSnapshot(context.Background(), "my-snapshot", restoreRequest()); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	call := shareDataOf(mgr).lastCopy(t)
	if call.src != shareOne || call.srcPath != "/pvcs/.snapshots/snap-my-snapshot" {
		t.Errorf("copy source %s on %+v", call.srcPath, call.src)
	}
	if call.dst != shareOne || call.dstPath != "/pvcs/"+testRestorePVName {
		t.Errorf("copy target %s on %+v", call.dstPath, call.dst)
	}
	if call.owner == nil || call.owner.UID == nil || *call.owner.UID != 107 || call.owner.Permissions != "0777" {
		t.Errorf("restore owner = %+v, want 107:107 0777", call.owner)
	}
}

func TestRestoreSnapshot_CopyInProgress_VolumeNotReturned(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	addReadySnapshot(k)
	sd := shareDataOf(mgr)
	sd.setInProgress(true)
	ctx := context.Background()

	_, err := mgr.RestoreSnapshot(ctx, "my-snapshot", restoreRequest())
	if !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("RestoreSnapshot() error = %v, want ErrOperationInProgress", err)
	}
	sv := k.getSubVolume(testRestorePVName)
	if sv == nil || sv.Status.Phase != "Creating" {
		t.Fatalf("SubVolume = %+v, want phase Creating (keeps the share reservation)", sv)
	}

	sd.setInProgress(false)
	result, err := mgr.RestoreSnapshot(ctx, "my-snapshot", restoreRequest())
	if err != nil {
		t.Fatalf("RestoreSnapshot after copy: %v", err)
	}
	if result.SubPath != "/pvcs/"+testRestorePVName {
		t.Errorf("SubPath = %q", result.SubPath)
	}
	if got := k.getSubVolume(testRestorePVName).Status.Phase; got != "Bound" {
		t.Errorf("phase after copy = %q, want Bound", got)
	}
	if got := k.getPool("test-pool").Status.Shares[0].AllocatedGB; got != 110 {
		t.Errorf("restore capacity counted wrongly: AllocatedGB = %d, want 110", got)
	}
}

func TestRestoreSnapshot_CopyFails_RetriesOnNextCall(t *testing.T) {
	k, nfs, mgr := newShareDataTestManager(t)
	addReadySnapshot(k)
	nfs.CopyErr = fmt.Errorf("copy failed")
	ctx := context.Background()

	_, err := mgr.RestoreSnapshot(ctx, "my-snapshot", restoreRequest())
	if err == nil {
		t.Fatal("RestoreSnapshot succeeded with a failed copy")
	}
	if sv := k.getSubVolume(testRestorePVName); sv == nil || sv.Status.Phase != "Creating" {
		t.Fatalf("SubVolume = %+v, want kept in phase Creating for a retry", sv)
	}

	nfs.CopyErr = nil
	if _, err := mgr.RestoreSnapshot(ctx, "my-snapshot", restoreRequest()); err != nil {
		t.Fatalf("retry RestoreSnapshot: %v", err)
	}
}
