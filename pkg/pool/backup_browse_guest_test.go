package pool

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// guestEnv is a browse environment with a VM "web" that owns the claim apps/db.
func newGuestEnv(t *testing.T) *browseEnv {
	t.Helper()
	e := newBrowseEnv(t)
	e.jobs.guest = true
	e.addVM("apps", "web", "db")
	e.reconcile() // the policy now knows that the volume is a VM disk
	return e
}

func (e *browseEnv) openGuest(name string) {
	e.t.Helper()
	e.open(name, func(b *v1alpha1.BackupBrowse) { b.Spec.Guest = true })
}

func (e *browseEnv) holdOf() string {
	e.t.Helper()
	return e.sv(e.claim.Name).Annotations[browseHoldAnnotation]
}

// markCopying says that the replication controller is copying the claim's disk now.
func (e *browseEnv) markCopying(on bool) {
	e.t.Helper()
	rps := e.repls()
	if len(rps) == 0 {
		e.t.Fatal("no replication policy")
	}
	rp := rps[0]
	rp.Status.SubVolumeStatuses = nil
	if on {
		rp.Status.SubVolumeStatuses = []v1alpha1.SubVolumeReplicationStatus{{SubVolumeName: e.claim.Name, LastError: "sync in progress"}}
	}
	if err := e.c.Status().Update(e.ctx, &rp); err != nil {
		e.t.Fatal(err)
	}
}

func TestBrowseGuest_HoldsTheDiskAndStartsTheHelper(t *testing.T) {
	e := newGuestEnv(t)
	e.openGuest("bb1")
	e.reconcile()

	bb := e.bb("bb1")
	if bb.Status.Phase != "Starting" {
		t.Fatalf("status = %+v", bb.Status)
	}
	holds := parseBrowseHolds(e.holdOf())
	if len(holds) != 1 || holds[0].session != "uid-bb1" || !holds[0].since.Equal(e.now) {
		t.Fatalf("holds = %+v (%q)", holds, e.holdOf())
	}
	req := e.jobs.started["uid-bb1"]
	if !req.Guest {
		t.Error("the helper was not asked to read the guest file systems")
	}
	if want := "/backup/nightly" + e.claim.Spec.SubPath; req.LatestPath != want {
		t.Errorf("LatestPath = %q, want %q", req.LatestPath, want)
	}
	if req.MomentsPath != "" {
		t.Errorf("a VM disk keeps its earlier moments next to the disk, not in a moments folder: %q", req.MomentsPath)
	}
}

func TestBrowseGuest_AMomentIsPassedToTheHelper(t *testing.T) {
	e := newGuestEnv(t)
	e.setRestorePoints(3)
	at := metav1.NewTime(time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC))
	e.open("bb1", func(b *v1alpha1.BackupBrowse) { b.Spec.Guest = true; b.Spec.Moment = &at })
	e.reconcile()
	req := e.jobs.started["uid-bb1"]
	if !req.At.Equal(at.Time) || req.MomentsPath != "" {
		t.Errorf("request = %+v", req)
	}
}

func TestBrowseGuest_WaitsWhileTheDiskIsBeingCopied(t *testing.T) {
	e := newGuestEnv(t)
	e.markCopying(true)
	e.openGuest("bb1")
	e.reconcile()

	bb := e.bb("bb1")
	if bb.Status.Phase != "Pending" || !strings.Contains(bb.Status.Message, "copy") {
		t.Fatalf("status = %+v", bb.Status)
	}
	if len(e.jobs.started) != 0 {
		t.Fatal("a helper was started while a copy runs")
	}
	if e.holdOf() == "" {
		t.Fatal("the hold is taken first, so that no new copy can start while the session waits")
	}

	e.markCopying(false)
	e.reconcile()
	if bb = e.bb("bb1"); bb.Status.Phase != "Starting" {
		t.Fatalf("status = %+v", bb.Status)
	}
	if _, ok := e.jobs.started["uid-bb1"]; !ok {
		t.Fatal("no helper after the copy ended")
	}
}

func TestBrowseGuest_ARunningVMCopyDelaysTheSession(t *testing.T) {
	e := newGuestEnv(t)
	rp := e.repls()[0]
	rp.Status.SubVolumeStatuses = []v1alpha1.SubVolumeReplicationStatus{{SubVolumeName: e.claim.Name, VMCopy: &v1alpha1.VMCopyState{}}}
	if err := e.c.Status().Update(e.ctx, &rp); err != nil {
		t.Fatal(err)
	}
	e.openGuest("bb1")
	e.reconcile()
	if bb := e.bb("bb1"); bb.Status.Phase != "Pending" {
		t.Fatalf("status = %+v", bb.Status)
	}
}

func TestBrowseGuest_NotAVMDiskFails(t *testing.T) {
	e := newBrowseEnv(t) // the claim has no VM
	e.jobs.guest = true
	e.openGuest("bb1")
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "not a VM disk") {
		t.Fatalf("status = %+v", bb.Status)
	}
	if len(e.jobs.started) != 0 || e.holdOf() != "" {
		t.Error("nothing may be started or held for a volume that is not a VM disk")
	}
}

func TestBrowseGuest_NotTurnedOnFailsInPlainWords(t *testing.T) {
	e := newGuestEnv(t)
	e.jobs.guest = false
	e.openGuest("bb1")
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "not turned on") {
		t.Fatalf("status = %+v", bb.Status)
	}
	if e.holdOf() != "" {
		t.Error("no hold may be taken for a session that cannot start")
	}
}

func TestBrowseGuest_COSIsNotYetSupported(t *testing.T) {
	e := newGuestEnv(t)
	e.open("bb1", func(b *v1alpha1.BackupBrowse) { b.Spec.Guest = true; b.Spec.Source = v1alpha1.BackupSourceCOS })
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "COS") {
		t.Fatalf("status = %+v", bb.Status)
	}
}

func TestBrowseGuest_TheHoldEndsWhenTheHelperIsGone(t *testing.T) {
	e := newGuestEnv(t)
	e.openGuest("bb1")
	e.reconcile()
	if e.holdOf() == "" {
		t.Fatal("no hold")
	}
	e.jobs.stopDone = false // the pod is still shutting down
	bb := e.bb("bb1")
	bb.Spec.Close = true
	if err := e.c.Update(e.ctx, bb); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if e.holdOf() == "" {
		t.Fatal("the hold must stay until the pod is gone: it still reads the disk")
	}
	e.jobs.stopDone = true
	e.reconcile()
	e.reconcile()
	if got := e.holdOf(); got != "" {
		t.Fatalf("hold = %q after the helper is gone", got)
	}
}

func TestBrowseGuest_TheHoldEndsAtTheHardLimit(t *testing.T) {
	e := newGuestEnv(t)
	e.openGuest("bb1")
	e.reconcile()
	e.jobs.state["uid-bb1"] = BrowseJobState{Exists: true, Ready: true, PodIP: "10.130.0.9"}
	e.reconcile()
	e.now = e.now.Add(2*time.Hour + time.Minute)
	e.reconcile()
	e.reconcile()
	if bb := e.bb("bb1"); bb.Status.Phase != "Closed" {
		t.Fatalf("status = %+v", bb.Status)
	}
	if got := e.holdOf(); got != "" {
		t.Fatalf("hold = %q after the session closed", got)
	}
}

func TestBrowseGuest_TwoSessionsKeepTheirOwnHolds(t *testing.T) {
	e := newGuestEnv(t)
	e.cfg.MaxPerNamespace = 2
	e.openGuest("bb1")
	e.openGuest("bb2")
	e.reconcile()
	if n := len(parseBrowseHolds(e.holdOf())); n != 2 {
		t.Fatalf("holds = %q", e.holdOf())
	}
	bb := e.bb("bb1")
	bb.Spec.Close = true
	if err := e.c.Update(e.ctx, bb); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.reconcile()
	holds := parseBrowseHolds(e.holdOf())
	if len(holds) != 1 || holds[0].session != "uid-bb2" {
		t.Fatalf("holds = %q, want only the open session's", e.holdOf())
	}
}

func TestBrowseGuest_ALeftOverHoldIsRemoved(t *testing.T) {
	e := newGuestEnv(t)
	sv := e.sv(e.claim.Name)
	sv.Annotations = map[string]string{browseHoldAnnotation: addBrowseHold(addBrowseHold("", "uid-gone", e.now), "uid-live", e.now)}
	if err := e.c.Update(e.ctx, sv); err != nil {
		t.Fatal(err)
	}
	e.jobs.orphans = []string{"uid-live"} // a helper Job of this session still exists
	e.reconcile()                         // stops the orphan Job
	e.reconcile()
	e.jobs.orphans = nil
	e.reconcile()
	if got := e.holdOf(); got != "" {
		t.Fatalf("hold = %q, want none: no session is open", got)
	}
}

func TestBrowseGuest_AHoldOfAnOpenSessionIsKept(t *testing.T) {
	e := newGuestEnv(t)
	e.openGuest("bb1")
	e.reconcile()
	for i := 0; i < 3; i++ {
		e.reconcile()
	}
	if len(parseBrowseHolds(e.holdOf())) != 1 {
		t.Fatalf("hold = %q", e.holdOf())
	}
}

func TestBrowseGuest_NoSubVolumeNoHold(t *testing.T) {
	// The volume's SubVolume is gone (the claim was deleted). Nothing copies the
	// disk any more, so there is nothing to hold, and the backup can be read.
	e := newGuestEnv(t)
	e.openGuest("bb1")
	if msg := e.bc.holdForGuest(e.ctx, e.bb("bb1"), "pvc-that-is-gone"); msg != "" {
		t.Fatalf("a volume that is gone must not make the session wait: %q", msg)
	}
}

func TestBrowseGuest_TheConsistencyOfTheNewestCopyGoesToTheHelper(t *testing.T) {
	e := newGuestEnv(t)
	bp := e.bp("nightly")
	for i := range bp.Status.Volumes {
		bp.Status.Volumes[i].Consistency = "CrashConsistent"
	}
	if err := e.c.Status().Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.openGuest("bb1")
	e.reconcile()
	// A pass of the policy controller keeps what the replication status says; with
	// none, the last value stays.
	if got := e.jobs.started["uid-bb1"].Consistency; got != "CrashConsistent" {
		t.Errorf("Consistency = %q", got)
	}
}

func TestBrowseGuest_AHelperThatIsNotReadyInTimeEndsInWords(t *testing.T) {
	e := newGuestEnv(t)
	e.openGuest("bb1")
	e.reconcile()
	e.jobs.state["uid-bb1"] = BrowseJobState{Exists: true} // the pod is up, the reader is not ready
	e.now = e.now.Add(defaultBrowseGuestStartTimeout + time.Minute)
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "not ready") {
		t.Fatalf("status = %+v", bb.Status)
	}
}
