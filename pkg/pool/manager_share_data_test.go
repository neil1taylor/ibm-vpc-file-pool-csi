package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeShareData applies share data calls to a fakeNFSOperations rooted at
// base, so tests can check folders the same way for every operation. When
// inProgress is set, calls report "not done" and change nothing. A failed
// fake copy is reported as a failed Job, like the real runner. apiErr makes
// every call fail as if the Kubernetes API did not answer. running lists the
// keys of Jobs that Stop must wait for.
type fakeShareData struct {
	mu         sync.Mutex
	nfs        *fakeNFSOperations
	base       string
	inProgress bool
	apiErr     error
	running    map[string]bool
	removes    []shareDataCall
	copies     []shareDataCall
	stops      []string
	applies    []applyCall
	// applyErr makes the next ApplyVMDisk calls fail as a failed Job.
	applyErr error
	restores []restoreCall
	// restoreErr makes RestoreBackup fail as a failed Job; restoreOutcome is
	// what a finished restore reports.
	restoreErr     error
	restoreOutcome RestoreOutcome
	// usagePercent is what ShareUsage reports; usageErr makes it fail as a
	// failed Job; usageCalls counts the calls.
	usagePercent int
	usageErr     error
	usageCalls   int
}

type restoreCall struct {
	key         string
	volPath     string
	momentsPath string
	dstPath     string
	moment      time.Time
}

type applyCall struct {
	key         string
	volPath     string
	stagingFile string
	dstPath     string
	moment      time.Time
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
	inProgress, apiErr := f.inProgress, f.apiErr
	f.mu.Unlock()
	if apiErr != nil {
		return false, apiErr
	}
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
	inProgress, apiErr := f.inProgress, f.apiErr
	f.mu.Unlock()
	if apiErr != nil {
		return false, apiErr
	}
	if inProgress {
		return false, nil
	}
	if err := f.nfs.CopyDir(f.base+srcPath, f.base+dstPath); err != nil {
		return false, fmt.Errorf("%w: %v", ErrShareJobFailed, err)
	}
	return true, nil
}

// ApplyVMDisk records the call and reports done when the call is not held up.
func (f *fakeShareData) ApplyVMDisk(_ context.Context, key string, _ ShareMount, volPath, stagingFile, dstPath string, moment time.Time) (bool, error) {
	f.mu.Lock()
	f.applies = append(f.applies, applyCall{key: key, volPath: volPath, stagingFile: stagingFile, dstPath: dstPath, moment: moment})
	inProgress, apiErr, applyErr := f.inProgress, f.apiErr, f.applyErr
	f.mu.Unlock()
	if apiErr != nil {
		return false, apiErr
	}
	if applyErr != nil {
		return false, fmt.Errorf("%w: %v", ErrShareJobFailed, applyErr)
	}
	if inProgress {
		return false, nil
	}
	return true, nil
}

// RestoreBackup records the call and reports done when the call is not held
// up. The outcome is the latest backup, unless restoreOutcome says otherwise.
func (f *fakeShareData) RestoreBackup(_ context.Context, key string, _ ShareMount, volPath, momentsPath string, _ ShareMount, dstPath string, moment time.Time, _ *Ownership) (bool, RestoreOutcome, error) {
	f.mu.Lock()
	f.restores = append(f.restores, restoreCall{key: key, volPath: volPath, momentsPath: momentsPath, dstPath: dstPath, moment: moment})
	inProgress, apiErr, restoreErr, outcome := f.inProgress, f.apiErr, f.restoreErr, f.restoreOutcome
	f.mu.Unlock()
	if apiErr != nil {
		return false, RestoreOutcome{}, apiErr
	}
	if restoreErr != nil {
		return false, RestoreOutcome{}, fmt.Errorf("%w: %v", ErrShareJobFailed, restoreErr)
	}
	if inProgress {
		return false, RestoreOutcome{}, nil
	}
	return true, outcome, nil
}

func (f *fakeShareData) ShareUsage(_ context.Context, _ string, _ ShareMount) (bool, int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.usageCalls++
	if f.apiErr != nil {
		return false, 0, f.apiErr
	}
	if f.usageErr != nil {
		return false, 0, fmt.Errorf("%w: %v", ErrShareJobFailed, f.usageErr)
	}
	if f.inProgress {
		return false, 0, nil
	}
	return true, f.usagePercent, nil
}

// Stop reports done once no Job runs for key. The first call on a running
// key stops it; the next call sees it gone.
func (f *fakeShareData) Stop(_ context.Context, key string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stops = append(f.stops, key)
	if f.running[key] {
		delete(f.running, key)
		return false, nil
	}
	return true, nil
}

func (f *fakeShareData) setRunning(key string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.running == nil {
		f.running = map[string]bool{}
	}
	f.running[key] = true
}

func (f *fakeShareData) removeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.removes)
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
	sd, _ := m.shareData.(*fakeShareData)
	return sd
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

func TestDeallocate_DuringRestoreCopy_StopsCopyBeforeRemoving(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	addReadySnapshot(k)
	sd := shareDataOf(mgr)
	sd.setInProgress(true)
	ctx := context.Background()
	_, _ = mgr.RestoreSnapshot(ctx, "my-snapshot", restoreRequest())
	sd.setInProgress(false)
	sd.setRunning("restore-" + testRestorePVName)
	removesBefore := sd.removeCount()

	err := mgr.Deallocate(ctx, testRestorePVName)
	if !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("Deallocate() error = %v, want ErrOperationInProgress while the copy stops", err)
	}
	if sd.removeCount() != removesBefore {
		t.Error("folder removal started while the restore copy was still running")
	}

	if err := mgr.Deallocate(ctx, testRestorePVName); err != nil {
		t.Fatalf("Deallocate after the copy stopped: %v", err)
	}
	if sd.removeCount() != removesBefore+1 {
		t.Error("folder not removed after the restore copy stopped")
	}
}

func TestDeleteSnapshot_DuringCopy_StopsCopyBeforeRemoving(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	sd := shareDataOf(mgr)
	sd.setInProgress(true)
	ctx := context.Background()
	_, _ = mgr.CreateSnapshot(ctx, "my-snapshot", "test-pool/share-1/"+testPVName, nil)
	sd.setInProgress(false)
	sd.setRunning("snap-my-snapshot")

	err := mgr.DeleteSnapshot(ctx, "my-snapshot")
	if !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("DeleteSnapshot() error = %v, want ErrOperationInProgress while the copy stops", err)
	}
	if sd.removeCount() != 0 {
		t.Error("folder removal started while the snapshot copy was still running")
	}
	if k.getSnapshot("my-snapshot") == nil {
		t.Error("Snapshot record deleted while its copy was still running")
	}

	if err := mgr.DeleteSnapshot(ctx, "my-snapshot"); err != nil {
		t.Fatalf("DeleteSnapshot after the copy stopped: %v", err)
	}
}

func TestCreateSnapshot_APIErrorDuringCopy_KeepsSnapshot(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	sd := shareDataOf(mgr)
	sd.setInProgress(true)
	ctx := context.Background()
	src := "test-pool/share-1/" + testPVName
	_, _ = mgr.CreateSnapshot(ctx, "my-snapshot", src, nil)

	sd.mu.Lock()
	sd.apiErr = errors.New("connection refused")
	sd.mu.Unlock()
	if _, err := mgr.CreateSnapshot(ctx, "my-snapshot", src, nil); err == nil {
		t.Fatal("CreateSnapshot hid an API error")
	}
	if k.getSnapshot("my-snapshot") == nil {
		t.Error("a short API error discarded the snapshot")
	}
	if got := k.getPool("test-pool").Status.Shares[0].AllocatedGB; got != 110 {
		t.Errorf("a short API error gave back the snapshot's capacity: AllocatedGB = %d, want 110", got)
	}
}

func TestCreateVolumeGroupSnapshot_NotReadyUntilEveryCopyFinished(t *testing.T) {
	k, _, mgr := setupGroupSnapshotTest(t)
	sd := shareDataOf(mgr)
	sd.setInProgress(true)
	ctx := context.Background()
	req := GroupSnapshotRequest{
		GroupName: "test-group-snap",
		PoolName:  "test-pool",
		SourceVolumeIDs: []string{
			"test-pool/share-1/pvc-aaaa0001-0000-0000-0000-000000000001",
			"test-pool/share-1/pvc-aaaa0002-0000-0000-0000-000000000002",
		},
		FailurePolicy: "Abort",
	}

	result, err := mgr.CreateVolumeGroupSnapshot(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot: %v", err)
	}
	if result.ReadyToUse {
		t.Error("group reported ready before its copies finished")
	}
	vgs := k.getGroupSnapshot("test-group-snap")
	if vgs.Status.Phase == "Complete" || vgs.Status.ReadyCount != 0 {
		t.Errorf("group phase %q with %d ready members before any copy finished", vgs.Status.Phase, vgs.Status.ReadyCount)
	}

	sd.setInProgress(false)
	result, err = mgr.CreateVolumeGroupSnapshot(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot after copies: %v", err)
	}
	if !result.ReadyToUse || len(result.Members) != 2 {
		t.Errorf("after copies: ReadyToUse=%v members=%d, want true and 2", result.ReadyToUse, len(result.Members))
	}
	if vgs := k.getGroupSnapshot("test-group-snap"); vgs.Status.Phase != "Complete" || vgs.Status.ReadyCount != 2 {
		t.Errorf("after copies: phase %q ready %d, want Complete and 2", vgs.Status.Phase, vgs.Status.ReadyCount)
	}
}

// newUnfinishedRestore records a restored volume whose status write was lost:
// it has the restore label but no phase.
func newUnfinishedRestore(k *fakeK8sClient) {
	sv := newTestSubVolume(testRestorePVName, "test-pool", "share-1", 10)
	sv.Spec.ShareExportPath = "/share_one"
	sv.Labels[restoreSnapshotLabel] = "my-snapshot"
	sv.Status.Phase = ""
	k.addSubVolume(sv)
}

func TestRestoreSnapshot_LostStatusWrite_StillCopies(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	addReadySnapshot(k)
	newUnfinishedRestore(k)
	sd := shareDataOf(mgr)
	sd.setInProgress(true)

	_, err := mgr.RestoreSnapshot(context.Background(), "my-snapshot", restoreRequest())
	if !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("RestoreSnapshot() error = %v, want ErrOperationInProgress (the copy has not finished)", err)
	}
}

func TestDeallocate_LostStatusWrite_StillStopsRestoreCopy(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	addReadySnapshot(k)
	newUnfinishedRestore(k)
	sd := shareDataOf(mgr)
	sd.setRunning("restore-" + testRestorePVName)

	err := mgr.Deallocate(context.Background(), testRestorePVName)
	if !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("Deallocate() error = %v, want ErrOperationInProgress while the copy stops", err)
	}
	if sd.removeCount() != 0 {
		t.Error("folder removal started while the restore copy was still running")
	}
}

func TestDeleteSnapshot_ReadySnapshot_StillChecksForCopy(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	addReadySnapshot(k)
	sd := shareDataOf(mgr)
	sd.setRunning("snap-my-snapshot")

	err := mgr.DeleteSnapshot(context.Background(), "my-snapshot")
	if !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("DeleteSnapshot() error = %v, want ErrOperationInProgress while a copy Job remains", err)
	}
}

func TestDeallocate_DuringCloneCopy_StopsCopyBeforeRemoving(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	clone := newPendingClone(testRestorePVName, "test-pool", "share-1", testPVName, 10)
	clone.Spec.ShareExportPath = "/share_one"
	k.addSubVolume(clone)
	sd := shareDataOf(mgr)
	sd.setRunning("clone-" + testRestorePVName)

	err := mgr.Deallocate(context.Background(), testRestorePVName)
	if !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("Deallocate() error = %v, want ErrOperationInProgress while the clone copy stops", err)
	}
	if sd.removeCount() != 0 {
		t.Error("folder removal started while the clone copy was still running")
	}
	if err := mgr.Deallocate(context.Background(), testRestorePVName); err != nil {
		t.Fatalf("Deallocate after the copy stopped: %v", err)
	}
}
