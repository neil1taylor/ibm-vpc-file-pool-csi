package pool

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
)

// newGuestCOSRestoreEnv is a VM disk that was exported to COS.
func newGuestCOSRestoreEnv(t *testing.T) *fileRestoreEnv {
	t.Helper()
	e := newFileRestoreCOSEnv(t)
	e.jobs.guest = true
	e.addVM("apps", "web", "db")
	e.reconcile() // the policy now knows that the volume is a VM disk
	bp := e.bp("nightly")
	exported := metav1.NewTime(e.now.Add(-time.Hour))
	for i := range bp.Status.Volumes {
		bp.Status.Volumes[i].ExportedTime = &exported
	}
	if err := e.c.Status().Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *fileRestoreEnv) guestCOSRestore(name string, mutate func(*v1alpha1.BackupFileRestore)) {
	e.t.Helper()
	e.guestRestore(name, func(fr *v1alpha1.BackupFileRestore) {
		fr.Spec.Source = v1alpha1.BackupSourceCOS
		if mutate != nil {
			mutate(fr)
		}
	})
}

func (e *fileRestoreEnv) diskSecret(name string) *corev1.Secret {
	e.t.Helper()
	s := &corev1.Secret{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: testDriverNS, Name: name}, s); err != nil {
		return nil
	}
	return s
}

func TestFileRestoreGuestCOS_StartsTheDiskServerAndWaitsForIt(t *testing.T) {
	e := newGuestCOSRestoreEnv(t)
	e.guestCOSRestore("f1", nil)
	e.reconcile()

	if len(e.jobs.diskStarts) == 0 {
		t.Fatal("the disk server was not started")
	}
	d := e.jobs.diskStarts[0]
	if !d.Restore || d.ID != "uid-f1" || d.COS == nil || d.COS.Policy != "nightly" || d.DiskSecretName == "" ||
		d.Owner.Kind != "BackupFileRestore" || d.MaxLife < guestFileRestoreDeadline {
		t.Errorf("disk server request = %+v", d)
	}
	s := e.diskSecret(d.DiskSecretName)
	if s == nil || len(s.Data["token"]) == 0 || len(s.Data["tls.crt"]) == 0 || len(s.Data["tls.key"]) == 0 {
		t.Fatalf("the disk server Secret = %+v", s)
	}
	if s.Labels[restoreDiskLabel] != "uid-f1" || s.Labels[browseLabel] != "" {
		t.Errorf("labels %v: the browse sweeps must not see the Secret of a restore", s.Labels)
	}
	if len(e.jobs.calls) != 0 {
		t.Errorf("a restore Job was started before the disk server was ready: %+v", e.jobs.calls)
	}
	if st := e.fr("f1").Status; st.Phase != restorePhasePending || !strings.Contains(st.Message, "disk server") {
		t.Errorf("status = %+v", st)
	}
}

func TestFileRestoreGuestCOS_ReadsTheDiskFromTheDiskServerThenStopsIt(t *testing.T) {
	e := newGuestCOSRestoreEnv(t)
	e.guestCOSRestore("f1", nil)
	e.reconcile()
	e.jobs.diskState = BrowseJobState{Exists: true, Ready: true, PodIP: "10.9.8.7"}
	e.jobs.answer[filerestore.ModeMeasure] = fakeJobAnswer{done: true, out: filerestore.Outcome{Result: filerestore.Result{Bytes: 1 << 20}}}
	e.reconcile()

	m := e.jobs.callsFor(filerestore.ModeMeasure)
	if len(m) == 0 {
		t.Fatalf("no measure call: %+v", e.jobs.calls)
	}
	c := m[0]
	if c.COS == nil || c.Guest == nil || c.Guest.Disk == nil || c.Guest.Disk.IP != "10.9.8.7" || c.Guest.Disk.SecretName == "" {
		t.Fatalf("request = %+v", c)
	}
	if c.LatestPath != "" || c.MomentsPath != "" || c.Backup != (ShareMount{}) {
		t.Errorf("a restore from COS names a folder: %+v", c)
	}
}

func TestFileRestoreGuestCOS_TheDiskServerIsStoppedWhenTheRestoreEnds(t *testing.T) {
	e := newGuestCOSRestoreEnv(t)
	e.guestCOSRestore("f1", nil)
	e.reconcile()
	e.jobs.diskState = BrowseJobState{Exists: true, Ready: true, PodIP: "10.9.8.7"}
	e.jobs.answer[filerestore.ModeMeasure] = refused("no room")
	e.reconcile()
	if st := e.fr("f1").Status; st.Phase != restorePhaseFailed {
		t.Fatalf("status = %+v", st)
	}
	if len(e.jobs.diskStopped) == 0 || e.jobs.diskStopped[0] != "uid-f1" {
		t.Errorf("stopped = %v: a restore that ended must not leave its disk server running", e.jobs.diskStopped)
	}
	if e.diskSecret(restoreDiskSecretName("uid-f1")) != nil {
		t.Error("the Secret of the disk server was left")
	}
}

func TestFileRestoreGuestCOS_ADiskServerThatEndedFailsTheRestore(t *testing.T) {
	e := newGuestCOSRestoreEnv(t)
	e.guestCOSRestore("f1", nil)
	e.reconcile()
	e.jobs.diskState = BrowseJobState{Exists: true, Finished: true, Reason: "the backup has no readable disk"}
	e.reconcile()
	st := e.fr("f1").Status
	if st.Phase != restorePhaseFailed || !strings.Contains(st.Message, "no readable disk") {
		t.Errorf("status = %+v", st)
	}
}

func TestFileRestoreGuestCOS_RefusesInWordsWhatItCannotDo(t *testing.T) {
	cases := map[string]struct {
		mutate func(*fileRestoreEnv, *v1alpha1.BackupFileRestore)
		want   string
	}{
		"not turned on": {func(e *fileRestoreEnv, _ *v1alpha1.BackupFileRestore) { e.jobs.guest = false }, "not turned on"},
		"a plain volume": {func(e *fileRestoreEnv, fr *v1alpha1.BackupFileRestore) {
			// The VM is gone, so the next pass records the volume as a plain one.
			vm := &unstructured.Unstructured{}
			vm.SetGroupVersionKind(schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"})
			vm.SetNamespace("apps")
			vm.SetName("web")
			if err := e.c.Delete(e.ctx, vm); err != nil {
				t.Fatal(err)
			}
		}, "not a VM disk"},
		"a bucket named directly": {func(_ *fileRestoreEnv, fr *v1alpha1.BackupFileRestore) {
			fr.Spec.Policy, fr.Spec.SourceNamespace, fr.Spec.SourceName = "lost-policy", v1alpha1.NoNamespace, "pvc-99999999-8888-7777-6666-555555555555"
			fr.Spec.FromCOS = fromCOSBlock()
		}, "named directly"},
		"an export that is about to be deleted": {func(e *fileRestoreEnv, _ *v1alpha1.BackupFileRestore) {
			bp := e.bp("nightly")
			old := metav1.NewTime(e.now.Add(-29*24*time.Hour - 23*time.Hour))
			for i := range bp.Status.Volumes {
				bp.Status.Volumes[i].ExportedTime = &old
			}
			if err := e.c.Status().Update(e.ctx, bp); err != nil {
				t.Fatal(err)
			}
		}, "deleted from COS"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newGuestCOSRestoreEnv(t)
			e.guestCOSRestore("f1", func(fr *v1alpha1.BackupFileRestore) { c.mutate(e, fr) })
			e.reconcile()
			st := e.fr("f1").Status
			if st.Phase != restorePhaseFailed || !strings.Contains(st.Message, c.want) {
				t.Errorf("status = %+v, want a message with %q", st, c.want)
			}
			if len(e.jobs.diskStarts) != 0 || len(e.jobs.calls) != 0 {
				t.Errorf("work was started: disk servers %d, Jobs %d", len(e.jobs.diskStarts), len(e.jobs.calls))
			}
		})
	}
}

func TestFileRestoreGuestCOS_ChecksTheKeepTimeFromTheRestorePoint(t *testing.T) {
	e := newGuestCOSRestoreEnv(t) // the newest export is an hour old
	at := metav1.NewTime(e.now.Add(-29*24*time.Hour - 23*time.Hour))
	e.guestCOSRestore("f1", func(fr *v1alpha1.BackupFileRestore) { fr.Spec.RestorePoint = &at })
	e.reconcile()
	st := e.fr("f1").Status
	if st.Phase != restorePhaseFailed || !strings.Contains(st.Message, "deleted from COS") {
		t.Errorf("status = %+v", st)
	}
	if len(e.jobs.diskStarts) != 0 {
		t.Error("a disk server was started for a restore point that is about to be deleted")
	}
}

func TestFileRestoreGuestCOS_AnOlderRestorePointReachesTheDiskServer(t *testing.T) {
	e := newGuestCOSRestoreEnv(t)
	at := metav1.NewTime(e.now.Add(-10 * 24 * time.Hour))
	e.guestCOSRestore("f1", func(fr *v1alpha1.BackupFileRestore) { fr.Spec.RestorePoint = &at })
	e.reconcile()
	if len(e.jobs.diskStarts) == 0 {
		t.Fatalf("the disk server was not started: %+v", e.fr("f1").Status)
	}
	if got := e.jobs.diskStarts[0].At; !got.Equal(at.Time) {
		t.Errorf("disk server At = %s, want %s", got, at.Time)
	}
}
