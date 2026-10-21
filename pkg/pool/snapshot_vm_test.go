package pool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const vmSnapName = "my-snapshot"

// vmSnapEnv is a manager with one SubVolume (claim default/my-pvc) that is the
// root disk of the running VM "web".
type vmSnapEnv struct {
	t     *testing.T
	k     *fakeK8sClient
	mgr   *Manager
	sd    *fakeShareData
	dc    client.Client
	cop   *fakeVMCopier
	clock time.Time
	ctx   context.Context
}

func newVMSnapEnv(t *testing.T) *vmSnapEnv {
	t.Helper()
	k, _, mgr := newShareDataTestManager(t)
	e := &vmSnapEnv{t: t, k: k, mgr: mgr, sd: mgr.shareData.(*fakeShareData), dc: newVMCopyDirectClient(),
		cop: newFakeVMCopier(), ctx: context.Background(), clock: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)}
	mgr.SetVMSnapshots(e.cop, e.dc)
	mgr.vmNow = func() time.Time { return e.clock }
	e.cop.state = vmcopy.Running
	return e
}

func (e *vmSnapEnv) startVM() {
	e.t.Helper()
	if err := e.dc.Create(e.ctx, newLauncherPod("virt-launcher-web-abcde", "pod-uid-1", "web", map[string]string{"rootdisk": "my-pvc"})); err != nil {
		e.t.Fatal(err)
	}
}

func (e *vmSnapEnv) stopVM() {
	e.t.Helper()
	pod := newLauncherPod("virt-launcher-web-abcde", "pod-uid-1", "web", nil)
	if err := e.dc.Delete(e.ctx, pod); err != nil {
		e.t.Fatal(err)
	}
}

func (e *vmSnapEnv) migrate() {
	e.t.Helper()
	mig := &unstructured.Unstructured{}
	mig.SetGroupVersionKind(vmimGVK)
	mig.SetNamespace("default")
	mig.SetName("web-migration")
	if err := unstructured.SetNestedField(mig.Object, "web", "spec", "vmiName"); err != nil {
		e.t.Fatal(err)
	}
	if err := unstructured.SetNestedField(mig.Object, "Running", "status", "phase"); err != nil {
		e.t.Fatal(err)
	}
	if err := e.dc.Create(e.ctx, mig); err != nil {
		e.t.Fatal(err)
	}
}

func (e *vmSnapEnv) create(params map[string]string) *SnapshotResult {
	e.t.Helper()
	res, err := e.mgr.CreateSnapshot(e.ctx, vmSnapName, "test-pool/share-1/"+testPVName, params)
	if err != nil {
		e.t.Fatalf("CreateSnapshot: %v", err)
	}
	return res
}

// tick runs one pass of the copy loop a little later than the last one.
func (e *vmSnapEnv) tick() {
	e.clock = e.clock.Add(30 * time.Second)
	e.mgr.AdvanceVMSnapshots(e.ctx)
}

func (e *vmSnapEnv) snap() *v1alpha1.Snapshot {
	e.t.Helper()
	s := e.k.getSnapshot(vmSnapName)
	if s == nil {
		e.t.Fatal("no Snapshot CR")
	}
	return s
}

func TestCreateSnapshot_RunningVM_TakesVMPathAndDoesNotFileCopy(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	res := e.create(map[string]string{"vmCopySpeedLimitMBps": "50"})
	if res.ReadyToUse {
		t.Error("a snapshot of a running VM must not be ready before the copy")
	}
	if len(e.sd.copies) != 0 {
		t.Errorf("the file copy ran %d times for a running VM", len(e.sd.copies))
	}
	s := e.snap()
	if s.Annotations[snapshotMethodAnnotation] != snapshotMethodVM {
		t.Errorf("method = %q, want %q", s.Annotations[snapshotMethodAnnotation], snapshotMethodVM)
	}
	if s.Annotations[snapshotSpeedAnnotation] != "50" {
		t.Errorf("speed annotation = %q, want 50", s.Annotations[snapshotSpeedAnnotation])
	}
	// A repeated call changes nothing.
	if again := e.create(nil); again.ReadyToUse || len(e.sd.copies) != 0 {
		t.Error("a repeated CreateSnapshot must not copy files or become ready")
	}
}

func TestCreateSnapshot_RejectsBadSpeedLimit(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	for _, v := range []string{"fast", "-5", "0"} {
		_, err := e.mgr.CreateSnapshot(e.ctx, "bad-"+v, "test-pool/share-1/"+testPVName, map[string]string{"vmCopySpeedLimitMBps": v})
		if err == nil {
			t.Errorf("speed %q: want an error", v)
		}
	}
}

func TestAdvanceVMSnapshots_FullRunToReady(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(map[string]string{"vmCopySpeedLimitMBps": "50"})

	e.tick() // starts the QEMU copy
	if len(e.cop.starts) != 1 {
		t.Fatalf("starts = %d, want 1", len(e.cop.starts))
	}
	st := e.cop.starts[0]
	if st.vm.Pod != "virt-launcher-web-abcde" || st.vm.Name != "web" || st.vm.Namespace != "default" {
		t.Errorf("copy started on %+v", st.vm)
	}
	if len(st.volumes) != 1 || st.volumes[0] != "rootdisk" {
		t.Errorf("volumes = %v, want [rootdisk]", st.volumes)
	}
	if st.speed != 50<<20 {
		t.Errorf("speed = %d, want %d", st.speed, 50<<20)
	}
	if st.base != "" {
		t.Errorf("base = %q: a snapshot is always a full copy", st.base)
	}
	if !strings.HasPrefix(st.id, vmcopy.SnapshotIDPrefix) {
		t.Errorf("copy id %q must start with %q, so replication leaves the copy alone", st.id, vmcopy.SnapshotIDPrefix)
	}
	if len(st.id) > 16 {
		t.Errorf("copy id %q is longer than the 16 characters vmcopy accepts", st.id)
	}
	if e.cop.removeStale != 0 {
		t.Error("a snapshot must not remove the change lists of other copies")
	}
	if e.cop.cleanStale != 1 {
		t.Errorf("cleanStale = %d: a snapshot clears the leftovers of an earlier try", e.cop.cleanStale)
	}
	vc := e.snap().Status.VMCopy
	if vc == nil || vc.Phase != vmCopyPhaseCopying || vc.StagingFile == "" || vc.PodUID != "pod-uid-1" {
		t.Fatalf("VMCopy = %+v", vc)
	}

	e.tick() // still running
	if e.snap().Status.VMCopy.Phase != vmCopyPhaseCopying || len(e.sd.applies) != 0 {
		t.Fatal("must wait while QEMU copies")
	}

	e.cop.state = vmcopy.Done
	e.sd.setInProgress(true)
	e.tick() // QEMU done: the apply Job starts
	if e.snap().Status.VMCopy.Phase != vmCopyPhaseSending {
		t.Fatalf("phase = %q, want Sending", e.snap().Status.VMCopy.Phase)
	}
	if len(e.cop.finishes) != 1 || e.cop.finishes[0] {
		t.Errorf("finishes = %v, want one Finish that keeps the staging file", e.cop.finishes)
	}
	if len(e.sd.applies) == 0 {
		t.Fatal("no apply Job")
	}
	a := e.sd.applies[0]
	if a.key != "snap-"+vmSnapName || a.volPath != "/pvcs/"+testPVName || a.dstPath != "/pvcs/.snapshots/snap-"+vmSnapName ||
		a.stagingFile != vc.StagingFile || !a.moment.Equal(vc.Moment.Time) {
		t.Errorf("apply call = %+v", a)
	}
	if e.snap().Status.ReadyToUse {
		t.Fatal("ready before the apply Job finished")
	}

	e.sd.setInProgress(false)
	e.tick() // the apply Job is done
	s := e.snap()
	if !s.Status.ReadyToUse || s.Status.Phase != "Ready" {
		t.Fatalf("status = %+v, want ready", s.Status)
	}
	if s.Status.VMCopy != nil {
		t.Error("VMCopy must be cleared once the snapshot is ready")
	}
	if s.Status.Consistency != consistencyFileSystem || s.Status.CopyMoment == nil || !s.Status.CopyMoment.Time.Equal(vc.Moment.Time) {
		t.Errorf("consistency %q, moment %v", s.Status.Consistency, s.Status.CopyMoment)
	}
	if last := e.cop.finishes[len(e.cop.finishes)-1]; !last {
		t.Error("the last Finish must drop the change list and staging file")
	}
	// A ready snapshot is left alone.
	n := len(e.cop.starts)
	e.tick()
	if len(e.cop.starts) != n {
		t.Error("a ready snapshot was copied again")
	}
}

func TestAdvanceVMSnapshots_CrashConsistentIsStillReady(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.cop.freezeErr = "no SELinux label"
	e.create(nil)
	e.tick()
	e.cop.state = vmcopy.Done
	e.tick()
	e.tick()
	s := e.snap()
	if !s.Status.ReadyToUse {
		t.Fatal("a crash-consistent snapshot must still be ready")
	}
	if s.Status.Consistency != consistencyCrash || s.Status.ConsistencyReason != "no SELinux label" {
		t.Errorf("consistency = %q (%q)", s.Status.Consistency, s.Status.ConsistencyReason)
	}
}

func TestAdvanceVMSnapshots_DefaultSpeedIs20(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	e.tick()
	if e.cop.starts[0].speed != 20<<20 {
		t.Errorf("speed = %d, want %d", e.cop.starts[0].speed, 20<<20)
	}
}

func TestAdvanceVMSnapshots_MigrationCancelsAndRetriesThreeTimes(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	e.tick() // copy starts
	e.migrate()

	e.tick() // the migration is seen
	if e.cop.cancels != 1 {
		t.Fatalf("cancels = %d, want 1", e.cop.cancels)
	}
	s := e.snap()
	if s.Status.VMCopy != nil || s.Status.VMCopyAttempts != 1 || s.Status.VMCopyRetryAfter == nil {
		t.Fatalf("status after one cancel: %+v", s.Status)
	}
	if !strings.Contains(s.Status.Message, "migrat") && !strings.Contains(s.Status.Message, "moving") {
		t.Errorf("message = %q, want it to name the move", s.Status.Message)
	}

	// The retry waits a minute.
	starts := len(e.cop.starts)
	e.tick() // 30 s after the cancel
	if len(e.cop.starts) != starts {
		t.Error("retried too early")
	}
	// While the VM moves, no copy starts either.
	e.clock = e.clock.Add(2 * time.Minute)
	e.tick()
	if len(e.cop.starts) != starts {
		t.Error("started a copy while the VM moves")
	}
}

func TestAdvanceVMSnapshots_ThreeLostCopiesFailTheSnapshot(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	for attempt := 1; attempt <= maxVMSnapshotAttempts; attempt++ {
		e.clock = e.clock.Add(vmSnapshotRetryDelay)
		e.tick() // starts a copy
		e.cop.state = vmcopy.Lost
		e.tick() // QEMU has no record of it
		e.cop.state = vmcopy.Running
	}
	s := e.snap()
	if s.Status.Phase != "Failed" || s.Status.ReadyToUse {
		t.Fatalf("status = %+v, want Failed", s.Status)
	}
	if !strings.Contains(s.Status.Message, "3") {
		t.Errorf("message = %q, want it to say how many tries", s.Status.Message)
	}
	// A failed snapshot is not copied again, and CreateSnapshot says so.
	n := len(e.cop.starts)
	e.clock = e.clock.Add(time.Hour)
	e.tick()
	if len(e.cop.starts) != n {
		t.Error("a failed snapshot was copied again")
	}
	if _, err := e.mgr.CreateSnapshot(e.ctx, vmSnapName, "test-pool/share-1/"+testPVName, nil); err == nil {
		t.Error("CreateSnapshot on a failed snapshot must return an error")
	}
}

func TestAdvanceVMSnapshots_FailedQEMUCopyCountsAsAttempt(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	e.tick()
	e.cop.state, e.cop.stateMsg = vmcopy.Failed, "rootdisk: no space left"
	e.tick()
	s := e.snap()
	if s.Status.VMCopyAttempts != 1 || s.Status.VMCopy != nil {
		t.Fatalf("status = %+v", s.Status)
	}
	if !strings.Contains(s.Status.Message, "no space left") {
		t.Errorf("message = %q, want QEMU's error", s.Status.Message)
	}
	if last := e.cop.finishes[len(e.cop.finishes)-1]; !last {
		t.Error("a failed copy must be cleaned up with its staging file")
	}
}

func TestAdvanceVMSnapshots_VMPodReplacedCountsAsAttempt(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	e.tick()
	e.stopVM()
	if err := e.dc.Create(e.ctx, newLauncherPod("virt-launcher-web-abcde", "pod-uid-2", "web", map[string]string{"rootdisk": "my-pvc"})); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if got := e.snap().Status.VMCopyAttempts; got != 1 {
		t.Errorf("attempts = %d, want 1", got)
	}
	if e.cop.cancels != 0 {
		t.Error("QEMU lost the copy with the pod, so there is nothing to cancel")
	}
}

func TestAdvanceVMSnapshots_StoppedVMFallsBackToFileCopy(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	e.stopVM()
	e.tick()
	if len(e.cop.starts) != 0 {
		t.Error("started a QEMU copy for a VM that is not running")
	}
	s := e.snap()
	if s.Annotations[snapshotMethodAnnotation] != snapshotMethodFile {
		t.Errorf("method = %q, want %q", s.Annotations[snapshotMethodAnnotation], snapshotMethodFile)
	}
	if len(e.sd.copies) != 1 {
		t.Fatalf("file copies = %d, want 1", len(e.sd.copies))
	}
	if !s.Status.ReadyToUse {
		t.Error("the file copy finished, so the snapshot is ready")
	}
}

func TestAdvanceVMSnapshots_WaitsWhileAnotherCopyRunsInTheVM(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	e.cop.idleErr = fmt.Errorf("copy job repl-x-0 is still in QEMU: %w", vmcopy.ErrBusy)
	e.tick()
	s := e.snap()
	if s.Status.VMCopyAttempts != 0 {
		t.Errorf("attempts = %d: waiting for another copy is not a failed try", s.Status.VMCopyAttempts)
	}
	if !strings.Contains(s.Status.Message, "another copy") {
		t.Errorf("message = %q", s.Status.Message)
	}
	e.cop.idleErr = nil
	e.tick()
	if len(e.cop.starts) != 1 {
		t.Error("the copy did not start once the VM was free")
	}
}

func TestAdvanceVMSnapshots_StartErrorCountsAsAttempt(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	e.cop.startErr = errors.New("exec failed")
	e.tick()
	s := e.snap()
	if s.Status.VMCopyAttempts != 1 || !strings.Contains(s.Status.Message, "exec failed") {
		t.Errorf("status = %+v", s.Status)
	}
}

func TestAdvanceVMSnapshots_SlowCopyIsCancelledAfterItsDeadline(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	e.tick()
	e.clock = e.clock.Add(vmSnapshotDeadline(10, 20) + time.Minute)
	e.tick()
	if e.cop.cancels != 1 || e.snap().Status.VMCopyAttempts != 1 {
		t.Errorf("cancels %d, attempts %d: want a cancel and one try used", e.cop.cancels, e.snap().Status.VMCopyAttempts)
	}
}

func TestVMSnapshotDeadline_GrowsWithSize(t *testing.T) {
	small, big := vmSnapshotDeadline(10, 20), vmSnapshotDeadline(2000, 20)
	if small < 2*time.Hour {
		t.Errorf("small deadline = %v, want at least 2h", small)
	}
	if big <= small {
		t.Errorf("a big disk must get a longer deadline: %v vs %v", big, small)
	}
}

func TestCreateSnapshot_VMStartsDuringFileCopySwitchesToVMPath(t *testing.T) {
	e := newVMSnapEnv(t)
	e.sd.setInProgress(true)
	e.create(nil) // no VM yet: the file copy starts
	if e.snap().Annotations[snapshotMethodAnnotation] == snapshotMethodVM {
		t.Fatal("no VM was running, so the file copy was expected")
	}
	e.startVM()
	e.sd.setInProgress(false)
	res := e.create(nil) // the file copy has now finished
	if res.ReadyToUse {
		t.Fatal("a VM started during the copy, so the file copy must be discarded")
	}
	if e.snap().Annotations[snapshotMethodAnnotation] != snapshotMethodVM {
		t.Error("the snapshot must switch to the VM path")
	}
	e.tick()
	if len(e.cop.starts) != 1 {
		t.Errorf("starts = %d, want the QEMU copy to start", len(e.cop.starts))
	}
}

func TestDeleteSnapshot_CancelsACopyInProgress(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	e.tick() // copying
	if err := e.mgr.DeleteSnapshot(e.ctx, vmSnapName); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if e.cop.cancels != 1 {
		t.Errorf("cancels = %d, want 1", e.cop.cancels)
	}
	if e.k.getSnapshot(vmSnapName) != nil {
		t.Error("the Snapshot CR is still there")
	}
}

func TestDeleteSnapshot_StopsTheApplyJob(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	e.tick()
	e.cop.state = vmcopy.Done
	e.sd.setInProgress(true)
	e.tick() // Sending
	if err := e.mgr.DeleteSnapshot(e.ctx, vmSnapName); err != nil && !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	found := false
	for _, k := range e.sd.stops {
		if k == "snap-"+vmSnapName {
			found = true
		}
	}
	if !found {
		t.Error("the apply Job was not stopped")
	}
	if last := e.cop.finishes[len(e.cop.finishes)-1]; !last {
		t.Error("the staging file must be removed")
	}
}

func TestAdvanceVMSnapshots_FailedApplyJobCountsAsAttempt(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	e.tick()
	e.cop.state = vmcopy.Done
	e.sd.setInProgress(true)
	e.tick() // the apply Job starts and runs
	e.sd.setInProgress(false)
	e.sd.applyErr = errors.New("disk full")
	e.tick()
	s := e.snap()
	if s.Status.VMCopyAttempts != 1 || s.Status.VMCopy != nil || s.Status.ReadyToUse {
		t.Fatalf("status = %+v", s.Status)
	}
	if last := e.cop.finishes[len(e.cop.finishes)-1]; !last {
		t.Error("a failed apply must remove the staging file")
	}
}

func TestSnapshotsOfOtherVolumesAreNotTouchedByTheLoop(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	cop := newFakeVMCopier()
	mgr.SetVMSnapshots(cop, newVMCopyDirectClient())
	addReadySnapshot(k)
	mgr.AdvanceVMSnapshots(context.Background())
	if len(cop.starts) != 0 {
		t.Error("the loop copied a snapshot that is not a VM snapshot")
	}
}

func (f *fakeVMCopier) startCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.starts)
}

func TestRunVMSnapshots_AdvancesOnATimerAndStopsWithItsContext(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	ctx, cancel := context.WithCancel(e.ctx)
	done := make(chan struct{})
	go func() {
		e.mgr.RunVMSnapshots(ctx, 5*time.Millisecond)
		close(done)
	}()
	deadline := time.Now().Add(5 * time.Second)
	for e.cop.startCount() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the loop did not stop with its context")
	}
	if e.cop.startCount() == 0 {
		t.Error("the loop never started the copy")
	}
}

func TestAdvanceVMSnapshots_OneSnapshotCopyPerVM(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	if _, err := e.mgr.CreateSnapshot(e.ctx, "second-snapshot", "test-pool/share-1/"+testPVName, nil); err != nil {
		t.Fatal(err)
	}
	e.tick()
	if len(e.cop.starts) != 1 {
		t.Fatalf("starts = %d after the first pass, want 1: a second snapshot of the same VM must wait", len(e.cop.starts))
	}
	waiting := e.k.getSnapshot(vmSnapName)
	if waiting.Status.VMCopy != nil {
		waiting = e.k.getSnapshot("second-snapshot")
	}
	if !strings.Contains(waiting.Status.Message, "another snapshot") {
		t.Errorf("the waiting snapshot says %q, want it to name the other snapshot", waiting.Status.Message)
	}

	// When the first is ready, the second starts.
	e.cop.state = vmcopy.Done
	e.tick()
	e.tick()
	e.cop.state = vmcopy.Running
	e.tick()
	if len(e.cop.starts) != 2 {
		t.Errorf("starts = %d, want the second copy to start once the first snapshot was ready", len(e.cop.starts))
	}
}
