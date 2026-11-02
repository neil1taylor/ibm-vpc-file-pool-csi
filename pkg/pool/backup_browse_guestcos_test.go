package pool

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// vmCOSEnv is a COS browse environment where the volume is a VM disk that was
// exported a day ago and is kept for 30 days.
func newVMCOSEnv(t *testing.T) *browseCOSEnv {
	t.Helper()
	e := newBrowseCOSEnv(t)
	e.jobs.guest = true
	e.addVM("apps", "web", "db")
	e.reconcile() // the policy learns that the claim is a VM disk
	e.setExported(e.now.Add(-24 * time.Hour))
	return e
}

// setExported sets the moment of the newest export of every volume.
func (e *browseCOSEnv) setExported(at time.Time) {
	e.t.Helper()
	bp := e.bp("nightly")
	m := metav1.NewTime(at)
	for i := range bp.Status.Volumes {
		bp.Status.Volumes[i].ExportedTime = &m
	}
	if err := e.c.Status().Update(e.ctx, bp); err != nil {
		e.t.Fatal(err)
	}
}

func (e *browseCOSEnv) openGuestCOS(name string) {
	e.openCOS(name, func(b *v1alpha1.BackupBrowse) { b.Spec.Guest = true })
}

func TestBrowseGuestCOS_StartsTheDiskServerFirst(t *testing.T) {
	e := newVMCOSEnv(t)
	e.openGuestCOS("bb1")
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Starting" || !strings.Contains(bb.Status.Message, "disk server") {
		t.Fatalf("status = %+v", bb.Status)
	}
	req, ok := e.jobs.disks["uid-bb1"]
	if !ok || !req.Guest || req.COS == nil || req.DiskSecretName != "browsedisk-uid-bb1" {
		t.Fatalf("disk server request = %+v", req)
	}
	if _, started := e.jobs.started["uid-bb1"]; started {
		t.Error("the reader started before the disk server was ready")
	}
	full, err := e.secretIn(testDriverNS, "browsedisk-uid-bb1")
	if err != nil || len(full.Data["tls.key"]) == 0 || len(full.Data["token"]) == 0 {
		t.Fatalf("driver namespace Secret: %v %v", err, full)
	}
	cp, err := e.secretIn(testBrowseNS, "browsedisk-uid-bb1")
	if err != nil {
		t.Fatal(err)
	}
	if _, has := cp.Data["tls.key"]; has {
		t.Error("the copy for the reader holds the private key of the disk server")
	}
	if string(cp.Data["token"]) != string(full.Data["token"]) || string(cp.Data["tls.crt"]) != string(full.Data["tls.crt"]) {
		t.Error("the copy does not match")
	}
	if e.holdOf() != "" {
		t.Error("a backup in COS needs no hold on the local disk")
	}
}

func TestBrowseGuestCOS_StartsTheReaderWhenTheDiskServerIsReady(t *testing.T) {
	e := newVMCOSEnv(t)
	e.openGuestCOS("bb1")
	e.reconcile()
	e.jobs.diskState["uid-bb1"] = BrowseJobState{Exists: true, Ready: true, PodIP: "10.5.5.5"}
	e.reconcile()
	req, ok := e.jobs.started["uid-bb1"]
	if !ok {
		t.Fatal("the reader was not started")
	}
	if req.COS != nil || req.Disk == nil || req.Disk.IP != "10.5.5.5" || req.Disk.SecretName != "browsedisk-uid-bb1" || !req.Guest {
		t.Errorf("reader request = %+v", req)
	}
	if bb := e.bb("bb1"); bb.Status.Phase != "Starting" || !strings.Contains(bb.Status.Message, "reader") {
		t.Errorf("status = %+v", bb.Status)
	}
	// The reader comes up: the session is ready on the reader's address.
	e.jobs.state["uid-bb1"] = BrowseJobState{Exists: true, Ready: true, PodIP: "10.6.6.6"}
	e.reconcile()
	if bb := e.bb("bb1"); bb.Status.Phase != "Ready" || bb.Status.PodIP != "10.6.6.6" {
		t.Errorf("status = %+v", bb.Status)
	}
}

func TestBrowseGuestCOS_ADiskServerThatFailsEndsTheSessionInWords(t *testing.T) {
	e := newVMCOSEnv(t)
	e.openGuestCOS("bb1")
	e.reconcile()
	e.jobs.diskState["uid-bb1"] = BrowseJobState{Exists: true, Finished: true, Reason: "the key was refused"}
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "key was refused") {
		t.Errorf("status = %+v", bb.Status)
	}
}

func TestBrowseGuestCOS_ADiskServerThatEndsByItselfClosesTheSession(t *testing.T) {
	e := newVMCOSEnv(t)
	e.openGuestCOS("bb1")
	e.reconcile()
	e.jobs.diskState["uid-bb1"] = BrowseJobState{Exists: true, Finished: true, Succeeded: true, Reason: "idle"}
	e.reconcile()
	if bb := e.bb("bb1"); bb.Status.Phase != "Closed" || bb.Status.EndReason != "idle" {
		t.Errorf("status = %+v", bb.Status)
	}
}

func TestBrowseGuestCOS_ALostDiskServerIsStartedAgainWhileStarting(t *testing.T) {
	e := newVMCOSEnv(t)
	e.openGuestCOS("bb1")
	e.reconcile()
	delete(e.jobs.diskState, "uid-bb1")
	delete(e.jobs.disks, "uid-bb1")
	e.reconcile()
	if _, ok := e.jobs.disks["uid-bb1"]; !ok {
		t.Error("the disk server was not started again")
	}
}

func TestBrowseGuestCOS_RefusesAnExportThatWillBeDeletedDuringTheSession(t *testing.T) {
	e := newVMCOSEnv(t)
	e.setExported(e.now.Add(-29*24*time.Hour - 22*time.Hour)) // kept 30 days: 2 hours left, less than a session plus an hour
	e.openGuestCOS("bb1")
	e.reconcile()
	bb := e.bb("bb1")
	if bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "deleted from COS") {
		t.Errorf("status = %+v", bb.Status)
	}
	if len(e.jobs.disks) != 0 {
		t.Error("a disk server was started for a backup that is about to be deleted")
	}
}

func TestBrowseGuestCOS_AcceptsAnExportWithEnoughTimeLeft(t *testing.T) {
	e := newVMCOSEnv(t)
	e.setExported(e.now.Add(-29*24*time.Hour + 4*time.Hour)) // about 3 days left
	e.openGuestCOS("bb1")
	e.reconcile()
	if bb := e.bb("bb1"); bb.Status.Phase != "Starting" {
		t.Errorf("status = %+v", bb.Status)
	}
}

func TestBrowseGuestCOS_RefusesWhatIsNotAVMDiskOrNotSetUp(t *testing.T) {
	e := newBrowseCOSEnv(t) // no VM uses the claim
	e.jobs.guest = true
	e.reconcile()
	e.setExported(e.now.Add(-24 * time.Hour))
	e.openGuestCOS("bb1")
	e.reconcile()
	if bb := e.bb("bb1"); bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "not a VM disk") {
		t.Errorf("not a VM disk: %+v", bb.Status)
	}

	e2 := newVMCOSEnv(t)
	e2.jobs.guest = false
	e2.openGuestCOS("bb2")
	e2.reconcile()
	if bb := e2.bb("bb2"); bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "not turned on") {
		t.Errorf("guest off: %+v", bb.Status)
	}

	e3 := newVMCOSEnv(t)
	e3.bc.SetBrowse(struct{ BrowseJobs }{e3.jobs}, e3.cfg)
	e3.openGuestCOS("bb3")
	e3.reconcile()
	if bb := e3.bb("bb3"); bb.Status.Phase != "Failed" || !strings.Contains(bb.Status.Message, "not set up") {
		t.Errorf("no disk runner: %+v", bb.Status)
	}
}

func TestBrowseGuestCOS_EndingTheSessionRemovesBothSecrets(t *testing.T) {
	e := newVMCOSEnv(t)
	e.openGuestCOS("bb1")
	e.reconcile()
	bb := e.bb("bb1")
	bb.Spec.Close = true
	if err := e.c.Update(e.ctx, bb); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	e.reconcile()
	for _, ns := range []string{testDriverNS, testBrowseNS} {
		if _, err := e.secretIn(ns, "browsedisk-uid-bb1"); err == nil {
			t.Errorf("the disk Secret is still in %s", ns)
		}
	}
	if e.jobs.stopped["uid-bb1"] == 0 {
		t.Error("the session's Jobs were not stopped")
	}
}
