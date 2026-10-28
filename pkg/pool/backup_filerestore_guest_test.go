package pool

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func newGuestRestoreEnv(t *testing.T) *fileRestoreEnv {
	t.Helper()
	e := newFileRestoreEnv(t)
	e.jobs.guest = true
	e.addVM("apps", "web", "db")
	e.reconcile() // the policy now knows that the volume is a VM disk
	return e
}

func (e *fileRestoreEnv) guestRestore(name string, mutate func(*v1alpha1.BackupFileRestore)) {
	e.t.Helper()
	e.request(name, func(fr *v1alpha1.BackupFileRestore) {
		fr.Spec.Guest = true
		fr.Spec.Paths = []string{"/sda1/etc/hosts"}
		if mutate != nil {
			mutate(fr)
		}
	})
}

func (e *fileRestoreEnv) holdOf() string {
	e.t.Helper()
	return e.sv(e.claim.Name).Annotations[browseHoldAnnotation]
}

func (e *fileRestoreEnv) markCopying(on bool) {
	e.t.Helper()
	rp := e.repls()[0]
	rp.Status.SubVolumeStatuses = nil
	if on {
		rp.Status.SubVolumeStatuses = []v1alpha1.SubVolumeReplicationStatus{{SubVolumeName: e.claim.Name, LastError: "sync in progress"}}
	}
	if err := e.c.Status().Update(e.ctx, &rp); err != nil {
		e.t.Fatal(err)
	}
}

func TestFileRestoreGuest_HoldsTheDiskAndMeasuresInTheGuestImage(t *testing.T) {
	e := newGuestRestoreEnv(t)
	e.guestRestore("f1", nil)
	e.jobs.answer[filerestore.ModeMeasure] = fakeJobAnswer{}
	e.reconcile()

	holds := parseBrowseHolds(e.holdOf())
	if len(holds) != 1 || holds[0].session != "uid-f1" {
		t.Fatalf("holds = %q: the restore holds the disk under its own UID", e.holdOf())
	}
	calls := e.jobs.callsFor(filerestore.ModeMeasure)
	if len(calls) == 0 {
		t.Fatal("no measuring Job")
	}
	req := calls[0]
	if req.Guest == nil || req.MomentsPath != "" || req.COS != nil {
		t.Errorf("request = %+v", req)
	}
	if want := "/backup/nightly" + e.claim.Spec.SubPath; req.LatestPath != want {
		t.Errorf("LatestPath = %q, want %q", req.LatestPath, want)
	}
	if e.fr("f1").Status.Phase != restorePhaseMeasuring {
		t.Errorf("phase = %q", e.fr("f1").Status.Phase)
	}
}

func TestFileRestoreGuest_WaitsForACopyBeforeItStarts(t *testing.T) {
	e := newGuestRestoreEnv(t)
	e.markCopying(true)
	e.guestRestore("f1", nil)
	e.reconcile()
	fr := e.fr("f1")
	if fr.Status.Phase != restorePhasePending || !strings.Contains(fr.Status.Message, "copy") {
		t.Fatalf("status = %+v", fr.Status)
	}
	if len(e.jobs.calls) != 0 {
		t.Fatal("a Job started while a copy runs")
	}
	if e.holdOf() == "" {
		t.Fatal("the hold comes first, so that no new copy can start")
	}
	e.markCopying(false)
	e.reconcile()
	if len(e.jobs.calls) == 0 {
		t.Fatal("no Job after the copy ended")
	}
}

func TestFileRestoreGuest_ARunningRestoreIsNotStoppedByACopy(t *testing.T) {
	e := newGuestRestoreEnv(t)
	e.guestRestore("f1", nil)
	e.jobs.answer[filerestore.ModeMeasure] = fakeJobAnswer{}
	e.reconcile() // Measuring
	e.markCopying(true)
	e.reconcile()
	if got := e.fr("f1").Status.Phase; got != restorePhaseMeasuring {
		t.Fatalf("phase = %q: a Job that runs is not undone by a copy that began before the hold", got)
	}
}

func TestFileRestoreGuest_TheHoldEndsWithTheRestore(t *testing.T) {
	e := newGuestRestoreEnv(t)
	pvc, _ := e.existingClaim("work")
	e.guestRestore("f1", func(fr *v1alpha1.BackupFileRestore) {
		fr.Spec.Existing, fr.Spec.TargetName, fr.Spec.TargetUID = true, "work", pvc.UID
	})
	e.jobs.answer[filerestore.ModeFolder] = fakeJobAnswer{done: true, out: filerestore.Outcome{Folder: "restored-nightly-x"}}
	e.reconcile()
	e.reconcile()
	fr := e.fr("f1")
	if fr.Status.Phase != restorePhaseDone {
		t.Fatalf("status = %+v", fr.Status)
	}
	e.reconcile()
	if got := e.holdOf(); got != "" {
		t.Fatalf("hold = %q after the restore ended", got)
	}
}

func TestFileRestoreGuest_AFailedRestoreReleasesTheHold(t *testing.T) {
	e := newGuestRestoreEnv(t)
	pvc, _ := e.existingClaim("work")
	e.guestRestore("f1", func(fr *v1alpha1.BackupFileRestore) {
		fr.Spec.Existing, fr.Spec.TargetName, fr.Spec.TargetUID = true, "work", pvc.UID
	})
	e.jobs.answer[filerestore.ModeFolder] = refused("no room")
	for i := 0; i < 3; i++ {
		e.reconcile()
	}
	if e.fr("f1").Status.Phase != restorePhaseFailed {
		t.Fatalf("status = %+v", e.fr("f1").Status)
	}
	if got := e.holdOf(); got != "" {
		t.Fatalf("hold = %q", got)
	}
}

func TestFileRestoreGuest_NotAVMDiskFails(t *testing.T) {
	e := newFileRestoreEnv(t) // no VM
	e.jobs.guest = true
	e.guestRestore("f1", nil)
	e.reconcile()
	fr := e.fr("f1")
	if fr.Status.Phase != restorePhaseFailed || !strings.Contains(fr.Status.Message, "not a VM disk") {
		t.Fatalf("status = %+v", fr.Status)
	}
	if len(e.jobs.calls) != 0 || e.holdOf() != "" {
		t.Error("nothing may run or be held")
	}
}

func TestFileRestoreGuest_NotTurnedOnFailsInWords(t *testing.T) {
	e := newGuestRestoreEnv(t)
	e.jobs.guest = false
	e.guestRestore("f1", nil)
	e.reconcile()
	fr := e.fr("f1")
	if fr.Status.Phase != restorePhaseFailed || !strings.Contains(fr.Status.Message, "not turned on") {
		t.Fatalf("status = %+v", fr.Status)
	}
	if e.holdOf() != "" {
		t.Error("no hold for a restore that cannot start")
	}
}

func TestFileRestoreGuest_COSIsNotYetSupported(t *testing.T) {
	e := newGuestRestoreEnv(t)
	e.guestRestore("f1", func(fr *v1alpha1.BackupFileRestore) { fr.Spec.Source = v1alpha1.BackupSourceCOS })
	e.reconcile()
	fr := e.fr("f1")
	if fr.Status.Phase != restorePhaseFailed || !strings.Contains(fr.Status.Message, "COS") {
		t.Fatalf("status = %+v", fr.Status)
	}
}

func TestFileRestoreGuest_TheConsistencyAndTheMomentGoToTheJob(t *testing.T) {
	e := newGuestRestoreEnv(t)
	bp := e.bp("nightly")
	for i := range bp.Status.Volumes {
		bp.Status.Volumes[i].Consistency = "CrashConsistent"
	}
	if err := e.c.Status().Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	at := metav1.NewTime(e.now.Add(-24 * time.Hour))
	e.guestRestore("f1", func(fr *v1alpha1.BackupFileRestore) { fr.Spec.Moment = &at })
	e.jobs.answer[filerestore.ModeMeasure] = fakeJobAnswer{}
	e.reconcile()
	req := e.jobs.callsFor(filerestore.ModeMeasure)[0]
	if req.Guest == nil || req.Guest.Consistency != "CrashConsistent" || !req.At.Equal(at.Time) {
		t.Errorf("request = %+v", req)
	}
}
