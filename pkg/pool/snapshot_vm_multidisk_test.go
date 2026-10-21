package pool

import (
	"reflect"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
)

// The tests in this file are about a VM with two pool disks: the root disk
// (claim my-pvc, volume rootdisk) and a data disk (claim data-pvc, volume
// datadisk). A snapshot of each disk must show one moment, whichever disk's
// request arrives first. See docs/specs/2026-10-21-multi-disk-vm-snapshots.md.

const (
	dataPVName   = "pvc-b2b2b2b2-5678-90ab-cdef-1234567890ab"
	dataSnapName = "data-snapshot"
)

// twoDiskEnv is a vmSnapEnv whose VM has a second pool disk.
func newTwoDiskEnv(t *testing.T) *vmSnapEnv {
	t.Helper()
	e := newVMSnapEnv(t)
	sv := newTestSubVolume(dataPVName, "test-pool", "share-1", 10)
	sv.Spec.ShareExportPath = "/share_one"
	sv.Spec.PVCName = "data-pvc"
	e.k.addSubVolume(sv)
	return e
}

func (e *vmSnapEnv) startTwoDiskVM(extraClaims map[string]string) {
	e.t.Helper()
	claims := map[string]string{"rootdisk": "my-pvc", "datadisk": "data-pvc"}
	for v, c := range extraClaims {
		claims[v] = c
	}
	if err := e.dc.Create(e.ctx, newLauncherPod("virt-launcher-web-abcde", "pod-uid-1", "web", claims)); err != nil {
		e.t.Fatal(err)
	}
}

func (e *vmSnapEnv) createData() {
	e.t.Helper()
	if _, err := e.mgr.CreateSnapshot(e.ctx, dataSnapName, "test-pool/share-1/"+dataPVName, nil); err != nil {
		e.t.Fatalf("CreateSnapshot (data disk): %v", err)
	}
}

func (e *vmSnapEnv) dataSnap() *v1alpha1.Snapshot {
	e.t.Helper()
	s := e.k.getSnapshot(dataSnapName)
	if s == nil {
		e.t.Fatal("no Snapshot CR for the data disk")
	}
	return s
}

// owner returns the name of the snapshot that owns the QEMU copy, and the name
// of the one that joined it. Which snapshot starts the copy depends on the order
// in which the controller lists them.
func (e *vmSnapEnv) owner() (owner, joined string) {
	e.t.Helper()
	for _, name := range []string{vmSnapName, dataSnapName} {
		vc := e.k.getSnapshot(name).Status.VMCopy
		if vc == nil {
			e.t.Fatalf("%s holds no copy", name)
		}
		if vc.Leader == "" {
			owner = name
		} else {
			joined = name
		}
	}
	if owner == "" || joined == "" {
		e.t.Fatalf("owner %q, joined %q: want one of each", owner, joined)
	}
	return owner, joined
}

func TestMultiDisk_FirstSnapshotCopiesEveryPoolDiskAtOnce(t *testing.T) {
	e := newTwoDiskEnv(t)
	e.startTwoDiskVM(map[string]string{"cloudinit": "claim-without-a-pool-volume"})
	e.create(nil)
	e.createData()

	e.tick()

	if len(e.cop.starts) != 1 {
		t.Fatalf("starts = %d, want one QEMU copy for both disks", len(e.cop.starts))
	}
	if got, want := e.cop.starts[0].volumes, []string{"rootdisk", "datadisk"}; !reflect.DeepEqual(got, want) {
		t.Errorf("volumes = %v, want %v: every pool disk, in SubVolume name order, and no disk that is not on a pool", got, want)
	}
}

func TestMultiDisk_SecondDiskJoinsTheCopyAndShowsTheSameMoment(t *testing.T) {
	e := newTwoDiskEnv(t)
	e.startTwoDiskVM(nil)
	e.create(nil)
	e.createData()

	e.tick()

	root, data := e.snap().Status.VMCopy, e.dataSnap().Status.VMCopy
	if root == nil || data == nil {
		t.Fatalf("VMCopy: root %+v, data %+v: both snapshots must hold the copy", root, data)
	}
	if data.ID != root.ID || !data.Moment.Equal(&root.Moment) {
		t.Errorf("data copy %s at %v, root copy %s at %v: they must be one copy", data.ID, data.Moment, root.ID, root.Moment)
	}
	owner, joined := e.owner()
	if e.k.getSnapshot(joined).Status.VMCopy.Leader != owner {
		t.Errorf("the joined snapshot names leader %q, want %q", e.k.getSnapshot(joined).Status.VMCopy.Leader, owner)
	}
	if root.Volume != "rootdisk" || data.Volume != "datadisk" || root.DiskIndex != 0 || data.DiskIndex != 1 {
		t.Errorf("volumes: root %s/%d, data %s/%d", root.Volume, root.DiskIndex, data.Volume, data.DiskIndex)
	}
	if root.StagingFile == "" || data.StagingFile == "" {
		t.Errorf("staging files: root %q, data %q", root.StagingFile, data.StagingFile)
	}
	if e.cop.cleanStale != 1 {
		t.Errorf("cleanStale = %d: only the snapshot that starts the copy clears leftovers", e.cop.cleanStale)
	}
}

func TestMultiDisk_FullRunGivesBothSnapshotsOneMoment(t *testing.T) {
	e := newTwoDiskEnv(t)
	e.startTwoDiskVM(nil)
	e.create(nil)
	e.createData()
	e.tick() // starts the copy; the data disk joins

	e.cop.state = vmcopy.Done
	e.sd.setInProgress(true)
	e.tick()
	e.tick() // QEMU is done: both apply Jobs start
	for name, s := range map[string]*v1alpha1.Snapshot{vmSnapName: e.snap(), dataSnapName: e.dataSnap()} {
		if s.Status.VMCopy == nil || s.Status.VMCopy.Phase != vmCopyPhaseSending {
			t.Fatalf("%s: VMCopy = %+v, want phase Sending", name, s.Status.VMCopy)
		}
	}
	// The QEMU copy is released once, for both disks, and keeps the staging files.
	var keeps [][]string
	for i, remove := range e.cop.finishes {
		if !remove {
			keeps = append(keeps, e.cop.finishDisks[i])
		}
	}
	if len(keeps) != 1 || !reflect.DeepEqual(keeps[0], []string{"rootdisk", "datadisk"}) {
		t.Errorf("Finish calls that keep staging: %v, want one for [rootdisk datadisk]", keeps)
	}

	e.sd.setInProgress(false)
	e.tick()
	e.tick()
	root, data := e.snap(), e.dataSnap()
	if !root.Status.ReadyToUse || !data.Status.ReadyToUse {
		t.Fatalf("ready: root %v, data %v", root.Status.ReadyToUse, data.Status.ReadyToUse)
	}
	if root.Status.CopyMoment == nil || data.Status.CopyMoment == nil || !root.Status.CopyMoment.Equal(data.Status.CopyMoment) {
		t.Errorf("moments differ: root %v, data %v", root.Status.CopyMoment, data.Status.CopyMoment)
	}
	if root.Status.CopyID == "" || root.Status.CopyID != data.Status.CopyID {
		t.Errorf("CopyID: root %q, data %q", root.Status.CopyID, data.Status.CopyID)
	}
	// Each snapshot releases only its own disk.
	removed := map[string]bool{}
	for i, remove := range e.cop.finishes {
		if remove && len(e.cop.finishDisks[i]) == 1 {
			removed[e.cop.finishDisks[i][0]] = true
		}
	}
	if !removed["rootdisk"] || !removed["datadisk"] {
		t.Errorf("Finish(remove staging) per disk: %v, want rootdisk and datadisk", removed)
	}
}

func TestMultiDisk_DiskThatNoSnapshotClaimsIsReleasedAfterTheJoinWindow(t *testing.T) {
	e := newTwoDiskEnv(t)
	e.startTwoDiskVM(nil)
	e.create(nil) // only the root disk is asked for

	e.tick()
	e.cop.state = vmcopy.Done
	e.sd.setInProgress(true)
	e.tick()
	e.sd.setInProgress(false)
	e.tick()
	if !e.snap().Status.ReadyToUse {
		t.Fatal("the root snapshot should be ready")
	}
	before := len(e.cop.finishes)

	// Inside the join window the data disk's staging file is kept.
	e.clock = e.clock.Add(5 * time.Minute)
	e.mgr.AdvanceVMSnapshots(e.ctx)
	for i := before; i < len(e.cop.finishes); i++ {
		if e.cop.finishes[i] && reflect.DeepEqual(e.cop.finishDisks[i], []string{"datadisk"}) {
			t.Fatal("the data disk was released inside the join window")
		}
	}

	e.clock = e.clock.Add(6 * time.Minute)
	e.mgr.AdvanceVMSnapshots(e.ctx)
	released := false
	for i := before; i < len(e.cop.finishes); i++ {
		if e.cop.finishes[i] && reflect.DeepEqual(e.cop.finishDisks[i], []string{"datadisk"}) {
			released = true
		}
	}
	if !released {
		t.Errorf("finishes %v %v: the unclaimed data disk must be released after the join window", e.cop.finishes, e.cop.finishDisks)
	}
	if e.snap().Status.VMCopySet != nil {
		t.Error("the copy set must be cleared once its unclaimed disks are released")
	}
}

func TestMultiDisk_RequestAfterTheJoinWindowStartsANewCopy(t *testing.T) {
	e := newTwoDiskEnv(t)
	e.startTwoDiskVM(nil)
	e.create(nil)
	e.tick()
	e.cop.state = vmcopy.Done
	e.tick()
	e.tick()
	e.clock = e.clock.Add(11 * time.Minute)
	e.mgr.AdvanceVMSnapshots(e.ctx)
	if !e.snap().Status.ReadyToUse {
		t.Fatal("the root snapshot should be ready")
	}

	e.cop.state = vmcopy.Running
	e.createData()
	e.tick()
	if len(e.cop.starts) != 2 {
		t.Fatalf("starts = %d, want a new copy for a request after the join window", len(e.cop.starts))
	}
	if e.dataSnap().Status.VMCopy == nil || e.dataSnap().Status.VMCopy.Leader != "" {
		t.Errorf("the late snapshot must own its copy: %+v", e.dataSnap().Status.VMCopy)
	}
}

func TestMultiDisk_DiskJoinsAfterTheFirstSnapshotIsReady(t *testing.T) {
	e := newTwoDiskEnv(t)
	e.startTwoDiskVM(nil)
	e.create(nil)
	e.tick()
	e.cop.state = vmcopy.Done
	e.tick()
	e.tick()
	if !e.snap().Status.ReadyToUse {
		t.Fatal("the root snapshot should be ready")
	}

	e.createData() // within the join window
	e.tick()
	if len(e.cop.starts) != 1 {
		t.Fatalf("starts = %d: the data disk must use the existing copy", len(e.cop.starts))
	}
	d := e.dataSnap().Status.VMCopy
	if d == nil || d.Phase != vmCopyPhaseSending || d.Leader != vmSnapName {
		t.Fatalf("data VMCopy = %+v, want phase Sending, joined to %s", d, vmSnapName)
	}
	if !d.Moment.Equal(e.snap().Status.CopyMoment) {
		t.Errorf("moments differ: %v and %v", d.Moment, e.snap().Status.CopyMoment)
	}
	e.tick()
	if !e.dataSnap().Status.ReadyToUse {
		t.Fatal("the data snapshot should be ready")
	}
}

func TestMultiDisk_MigrationCancelsTheWholeCopyAndBothSnapshotsRetry(t *testing.T) {
	e := newTwoDiskEnv(t)
	e.startTwoDiskVM(nil)
	e.create(nil)
	e.createData()
	e.tick()

	e.migrate()
	e.tick()
	e.tick()
	if len(e.cop.cancelDisks) != 1 || !reflect.DeepEqual(e.cop.cancelDisks[0], []string{"rootdisk", "datadisk"}) {
		t.Fatalf("cancels = %v, want one Cancel of both disks", e.cop.cancelDisks)
	}
	for name, s := range map[string]*v1alpha1.Snapshot{vmSnapName: e.snap(), dataSnapName: e.dataSnap()} {
		if s.Status.VMCopy != nil || s.Status.VMCopyAttempts != 1 {
			t.Errorf("%s: VMCopy %+v, attempts %d; want the try counted and the copy cleared", name, s.Status.VMCopy, s.Status.VMCopyAttempts)
		}
		if !strings.Contains(s.Status.Message, "trying again") {
			t.Errorf("%s: message %q", name, s.Status.Message)
		}
	}
	if e.snap().Status.VMCopySet != nil {
		t.Error("a cancelled copy leaves no set behind")
	}
}

func TestMultiDisk_DeletingTheOwnerWhileCopyingCancelsTheJoinedSnapshot(t *testing.T) {
	e := newTwoDiskEnv(t)
	e.startTwoDiskVM(nil)
	e.create(nil)
	e.createData()
	e.tick()
	owner, joined := e.owner()

	if err := e.mgr.DeleteSnapshot(e.ctx, owner); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if len(e.cop.cancelDisks) != 1 || !reflect.DeepEqual(e.cop.cancelDisks[0], []string{"rootdisk", "datadisk"}) {
		t.Fatalf("cancels = %v, want one Cancel of both disks", e.cop.cancelDisks)
	}
	e.tick()
	if j := e.k.getSnapshot(joined); j.Status.VMCopy != nil || j.Status.VMCopyAttempts != 1 {
		t.Errorf("joined snapshot: VMCopy %+v, attempts %d; want the try counted", j.Status.VMCopy, j.Status.VMCopyAttempts)
	}
}

func TestMultiDisk_DeletingAJoinedSnapshotWhileCopyingLeavesTheCopyRunning(t *testing.T) {
	e := newTwoDiskEnv(t)
	e.startTwoDiskVM(nil)
	e.create(nil)
	e.createData()
	e.tick()
	owner, joined := e.owner()

	if err := e.mgr.DeleteSnapshot(e.ctx, joined); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if e.cop.cancels != 0 {
		t.Errorf("cancels = %d: the owner's copy must keep running", e.cop.cancels)
	}
	if e.k.getSnapshot(owner).Status.VMCopy == nil {
		t.Error("the owner lost its copy")
	}
}

func TestMultiDisk_OneDiskVMIsUnchanged(t *testing.T) {
	e := newVMSnapEnv(t)
	e.startVM()
	e.create(nil)
	e.tick()
	if got := e.cop.starts[0].volumes; !reflect.DeepEqual(got, []string{"rootdisk"}) {
		t.Errorf("volumes = %v, want [rootdisk]", got)
	}
}

func TestMultiDisk_DiskJoinsWhileTheFirstSnapshotWritesItsImage(t *testing.T) {
	e := newTwoDiskEnv(t)
	e.startTwoDiskVM(nil)
	e.create(nil)
	e.tick()
	e.cop.state = vmcopy.Done
	e.sd.setInProgress(true)
	e.tick() // the first snapshot's apply Job runs

	e.createData()
	e.tick()
	if len(e.cop.starts) != 1 {
		t.Fatalf("starts = %d, want the data disk to use the copy", len(e.cop.starts))
	}
	if d := e.dataSnap().Status.VMCopy; d == nil || d.Phase != vmCopyPhaseSending || d.Leader != vmSnapName {
		t.Fatalf("data VMCopy = %+v, want phase Sending", d)
	}
	e.sd.setInProgress(false)
	e.tick()
	e.tick()
	if !e.snap().Status.ReadyToUse || !e.dataSnap().Status.ReadyToUse {
		t.Fatal("both snapshots should be ready")
	}
}
