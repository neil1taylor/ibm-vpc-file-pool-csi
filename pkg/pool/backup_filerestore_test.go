package pool

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// fakeFileRestoreJobs records what the controller asks for and answers from a
// script.
type fakeFileRestoreJobs struct {
	calls []FileRestoreRequest
	// answer is what the Job does for a mode.
	answer map[string]fakeJobAnswer
}

type fakeJobAnswer struct {
	done bool
	out  filerestore.Outcome
	err  error
}

func (f *fakeFileRestoreJobs) RunFileRestore(_ context.Context, req FileRestoreRequest) (bool, filerestore.Outcome, error) {
	f.calls = append(f.calls, req)
	a := f.answer[req.Mode]
	return a.done, a.out, a.err
}
func (f *fakeFileRestoreJobs) Namespace() string { return testDriverNS }

func (f *fakeFileRestoreJobs) callsFor(mode string) []FileRestoreRequest {
	var out []FileRestoreRequest
	for _, c := range f.calls {
		if c.Mode == mode {
			out = append(out, c)
		}
	}
	return out
}

// refused is what a Job answers when it decided the restore cannot go on: it
// exited with code 0 and wrote the reason (a Job that is started again after a
// failure would lose the message).
func refused(reason string) fakeJobAnswer {
	return fakeJobAnswer{done: true, out: filerestore.Outcome{Error: reason}}
}

type fileRestoreEnv struct {
	*restoreEnv
	jobs *fakeFileRestoreJobs
	now  time.Time
}

func newFileRestoreEnv(t *testing.T) *fileRestoreEnv {
	t.Helper()
	e := &fileRestoreEnv{restoreEnv: newRestoreEnv(t), jobs: &fakeFileRestoreJobs{answer: map[string]fakeJobAnswer{}},
		now: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	e.bc.SetFileRestore(e.jobs, FileRestoreConfig{})
	e.bc.SetNowFunc(func() time.Time { return e.now })
	return e
}

func (e *fileRestoreEnv) request(name string, mutate func(*v1alpha1.BackupFileRestore)) *v1alpha1.BackupFileRestore {
	e.t.Helper()
	fr := &v1alpha1.BackupFileRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name)},
		Spec: v1alpha1.BackupFileRestoreSpec{Policy: "nightly", SourceNamespace: "apps", SourceName: "db",
			Paths: []string{"/var/www/html/index.php", "/etc/app.conf"}, TargetNamespace: "apps", TargetName: "db-files"},
	}
	if mutate != nil {
		mutate(fr)
	}
	e.create(fr)
	return fr
}

func (e *fileRestoreEnv) fr(name string) *v1alpha1.BackupFileRestore {
	e.t.Helper()
	fr := &v1alpha1.BackupFileRestore{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: name}, fr); err != nil {
		e.t.Fatal(err)
	}
	return fr
}

// existingClaim makes a bound claim apps/work with its SubVolume, as the driver would.
func (e *fileRestoreEnv) existingClaim(name string) (*corev1.PersistentVolumeClaim, *v1alpha1.SubVolume) {
	e.t.Helper()
	uid, gid := int64(107), int64(107)
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-" + name},
		Spec: v1alpha1.SubVolumeSpec{PoolName: "data-a", PVCNamespace: "apps", PVCName: name,
			SubPath: "/pvcs/pvc-aaaaaaaa-bbbb-cccc-dddd-" + fmt.Sprintf("%012d", len(name)), ShareMountTargetIP: "10.240.1.1", ShareExportPath: "/export_a",
			UID: &uid, GID: &gid, Permissions: "0777"},
	}
	e.create(sv)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: name, UID: types.UID("pvc-uid-" + name)},
		Spec: corev1.PersistentVolumeClaimSpec{VolumeName: sv.Name,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("10Gi")}}},
		Status: corev1.PersistentVolumeClaimStatus{Phase: corev1.ClaimBound},
	}
	e.create(pvc)
	return pvc, sv
}

func (e *fileRestoreEnv) existing(name, target string) *v1alpha1.BackupFileRestore {
	return e.request(name, func(fr *v1alpha1.BackupFileRestore) {
		fr.Spec.Existing, fr.Spec.TargetName = true, target
	})
}

func TestFileRestore_IntoAnExistingClaimRunsToDone(t *testing.T) {
	e := newFileRestoreEnv(t)
	pvc, sv := e.existingClaim("work")
	moment := metav1.NewTime(time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC))
	bp := e.bp("nightly")
	for i := range bp.Status.Volumes {
		bp.Status.Volumes[i].RestorePoints = 3
	}
	if err := e.c.Status().Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.request("f1", func(fr *v1alpha1.BackupFileRestore) {
		fr.Spec.Existing, fr.Spec.TargetName, fr.Spec.Moment = true, "work", &moment
		fr.Spec.TargetUID = pvc.UID
	})
	e.jobs.answer[filerestore.ModeFolder] = fakeJobAnswer{}
	e.reconcile()

	calls := e.jobs.callsFor(filerestore.ModeFolder)
	if len(calls) != 1 {
		t.Fatalf("calls %+v", e.jobs.calls)
	}
	c := calls[0]
	if c.Key != "filerestore-uid-f1" || c.Owner.UID != "uid-f1" || c.Owner.Kind != "BackupFileRestore" {
		t.Errorf("key %q owner %+v: they must name the UID of the restore", c.Key, c.Owner)
	}
	if c.Backup != (ShareMount{MountTargetIP: "10.240.9.9", ExportPath: "/export_backups"}) {
		t.Errorf("backup share %+v", c.Backup)
	}
	if c.LatestPath != "/backup/nightly/pvcs/pvc-11111111-2222-3333-4444-555555555555" ||
		c.MomentsPath != "/backup/nightly/.moments/pvcs/pvc-11111111-2222-3333-4444-555555555555" || !c.At.Equal(moment.Time) {
		t.Errorf("paths %q %q at %v", c.LatestPath, c.MomentsPath, c.At)
	}
	if c.Dest != (ShareMount{MountTargetIP: "10.240.1.1", ExportPath: "/export_a"}) || c.Volume != sv.Spec.SubPath {
		t.Errorf("dest %+v volume %q", c.Dest, c.Volume)
	}
	if c.CapacityBytes != 10<<30 || c.UID == nil || *c.UID != 107 || *c.GID != 107 || c.Permissions != "0777" {
		t.Errorf("capacity %d owner %v/%v mode %q: the claim's own owner must be used", c.CapacityBytes, c.UID, c.GID, c.Permissions)
	}
	if c.Policy != "nightly" || len(c.Paths) != 2 || c.Paths[0] != "/var/www/html/index.php" {
		t.Errorf("policy %q paths %v", c.Policy, c.Paths)
	}
	if c.MaxBytes != filerestore.DefaultMaxBytes {
		t.Errorf("max bytes %d", c.MaxBytes)
	}
	if got := e.fr("f1"); got.Status.Phase != "Restoring" || got.Status.TargetUID != pvc.UID {
		t.Errorf("status %+v", got.Status)
	}
	if e.sv(sv.Name).Labels[RestorePendingLabel] != "" {
		t.Error("an existing claim must not be held back from its pods")
	}

	e.jobs.answer[filerestore.ModeFolder] = fakeJobAnswer{done: true, out: filerestore.Outcome{
		Result: filerestore.Result{Files: 2, Dirs: 4, Links: 1, Skipped: 1, Unreadable: 2, Bytes: 4096},
		Folder: "restored-nightly-20261021T093000Z", Moment: "2026-10-21T09:30:00Z"}}
	e.now = e.now.Add(time.Minute)
	e.reconcile()
	st := e.fr("f1").Status
	if st.Phase != "Done" || st.Folder != "restored-nightly-20261021T093000Z" || st.Files != 2 || st.Dirs != 4 || st.Links != 1 ||
		st.Skipped != 1 || st.Unreadable != 2 || st.Bytes != 4096 {
		t.Errorf("status %+v", st)
	}
	if st.RestoredMoment == nil || !st.RestoredMoment.Equal(&moment) {
		t.Errorf("restored moment %v", st.RestoredMoment)
	}
	if st.EndedTime == nil || !st.EndedTime.Time.Equal(e.now) {
		t.Errorf("ended %v", st.EndedTime)
	}
	if !strings.Contains(st.Message, "2 files") || !strings.Contains(st.Message, "2 could not be read") {
		t.Errorf("message %q must say what was left out", st.Message)
	}
}

func TestFileRestore_AnExistingClaimThatWasMadeAgainIsRefused(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.existingClaim("work")
	e.request("f1", func(fr *v1alpha1.BackupFileRestore) {
		fr.Spec.Existing, fr.Spec.TargetName, fr.Spec.TargetUID = true, "work", "an-old-claim"
	})
	e.reconcile()
	st := e.fr("f1").Status
	if st.Phase != "Failed" || !strings.Contains(st.Message, "made again") || len(e.jobs.calls) != 0 {
		t.Fatalf("status %+v calls %v", st, e.jobs.calls)
	}
}

func TestFileRestore_TheClaimIsPinnedOnFirstSight(t *testing.T) {
	e := newFileRestoreEnv(t)
	pvc, _ := e.existingClaim("work")
	e.existing("f1", "work")
	e.reconcile()
	if got := e.fr("f1").Status.TargetUID; got != pvc.UID {
		t.Fatalf("pinned %q", got)
	}
	// The claim is deleted and made again while the restore runs.
	if err := e.c.Delete(e.ctx, pvc); err != nil {
		t.Fatal(err)
	}
	again := pvc.DeepCopy()
	again.ResourceVersion, again.UID = "", "a-new-uid"
	e.create(again)
	e.reconcile()
	if st := e.fr("f1").Status; st.Phase != "Failed" {
		t.Fatalf("status %+v", st)
	}
}

func TestFileRestore_AnExistingClaimThatIsGoneFails(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.existing("f1", "nothing")
	e.reconcile()
	st := e.fr("f1").Status
	if st.Phase != "Failed" || !strings.Contains(st.Message, "does not exist") {
		t.Fatalf("status %+v", st)
	}
}

// A claim can name any volume in spec.volumeName. Only the binding by the
// cluster (status.phase Bound) is trusted, and the SubVolume must say it belongs
// to this claim.
func TestFileRestore_ASubVolumeOfAnotherClaimIsRefused(t *testing.T) {
	e := newFileRestoreEnv(t)
	_, sv := e.existingClaim("work")
	sv.Spec.PVCName = "someone-elses"
	if err := e.c.Update(e.ctx, sv); err != nil {
		t.Fatal(err)
	}
	e.existing("f1", "work")
	e.reconcile()
	st := e.fr("f1").Status
	if st.Phase != "Failed" || !strings.Contains(st.Message, "does not belong") || len(e.jobs.calls) != 0 {
		t.Fatalf("status %+v calls %v", st, e.jobs.calls)
	}
}

func TestFileRestore_WaitsForAClaimThatIsBeingRestored(t *testing.T) {
	e := newFileRestoreEnv(t)
	_, sv := e.existingClaim("work")
	e.bc.setSubVolumeRestoreLabel(e.ctx, sv, true)
	e.existing("f1", "work")
	e.reconcile()
	st := e.fr("f1").Status
	if st.Phase != "Pending" || !strings.Contains(st.Message, "being restored") || len(e.jobs.calls) != 0 {
		t.Fatalf("status %+v calls %v", st, e.jobs.calls)
	}
}

func TestFileRestore_OneRestoreAtATimeIntoAClaim(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.existingClaim("work")
	e.existing("f1", "work")
	e.reconcile()
	e.now = e.now.Add(time.Second)
	e.existing("f2", "work")
	e.reconcile()
	if st := e.fr("f2").Status; st.Phase != "Pending" || !strings.Contains(st.Message, "another restore") {
		t.Fatalf("second restore status %+v", st)
	}
	for _, c := range e.jobs.calls {
		if c.Key == "filerestore-uid-f2" {
			t.Fatal("the second restore started its Job")
		}
	}
	// The first ends; the second goes on.
	e.jobs.answer[filerestore.ModeFolder] = fakeJobAnswer{done: true, out: filerestore.Outcome{Folder: "x"}}
	e.reconcile()
	e.reconcile()
	if st := e.fr("f2").Status; st.Phase != "Done" {
		t.Fatalf("second restore status %+v", st)
	}
}

func TestFileRestore_FailedJobShowsTheReasonAndEnds(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.existingClaim("work")
	e.existing("f1", "work")
	e.jobs.answer[filerestore.ModeFolder] = refused("not enough space: the chosen files need 10 bytes and 5 bytes can be written")
	e.reconcile()
	st := e.fr("f1").Status
	if st.Phase != "Failed" || !strings.Contains(st.Message, "not enough space") || st.EndedTime == nil {
		t.Fatalf("status %+v", st)
	}
	if st.Files != 0 || st.Folder != "" {
		t.Fatalf("a refused restore must not report a result: %+v", st)
	}
}

// A BackupFileRestore has no namespace, so the API server accepts its Events
// only in "default". An Event in any other namespace is refused, and the
// restore's result would never be recorded.
func TestFileRestore_EventsAreWrittenWhereTheAPIServerAcceptsThem(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.existingClaim("work")
	e.existing("f1", "work")
	e.jobs.answer[filerestore.ModeFolder] = refused("not enough space")
	e.reconcile()
	evs := &corev1.EventList{}
	if err := e.c.List(e.ctx, evs); err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, ev := range evs.Items {
		if ev.Reason == "FileRestoreFailed" {
			found = true
			if ev.Namespace != metav1.NamespaceDefault {
				t.Errorf("the Event is in namespace %q, want %q", ev.Namespace, metav1.NamespaceDefault)
			}
		}
	}
	if !found {
		t.Fatal("no FileRestoreFailed Event")
	}
}

func TestFileRestore_AJobThatFailedWithoutAReasonStillEnds(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.existingClaim("work")
	e.existing("f1", "work")
	e.jobs.answer[filerestore.ModeFolder] = fakeJobAnswer{err: &shareJobFailure{job: "j"}}
	e.reconcile()
	st := e.fr("f1").Status
	if st.Phase != "Failed" || !strings.Contains(st.Message, "failed or ran out of time") {
		t.Fatalf("status %+v", st)
	}
}

func TestFileRestore_AJobErrorThatIsNotAFailureIsTriedAgain(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.existingClaim("work")
	e.existing("f1", "work")
	e.jobs.answer[filerestore.ModeFolder] = fakeJobAnswer{err: errors.New("the API is down")}
	e.reconcile()
	if st := e.fr("f1").Status; st.Phase != "Restoring" {
		t.Fatalf("status %+v", st)
	}
}

func TestFileRestore_NewClaimIsSizedByAMeasureJobThenFilled(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.request("f1", nil)

	// Measuring: the Job runs, the claim waits.
	e.reconcile()
	if st := e.fr("f1").Status; st.Phase != "Measuring" {
		t.Fatalf("status %+v", st)
	}
	if got := len(e.jobs.callsFor(filerestore.ModeMeasure)); got != 1 {
		t.Fatalf("measure calls %d", got)
	}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-files"}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatal("the claim was made before the size was known")
	}

	// 1.5 GiB of files: plus 20% is 1.8 GiB, which rounds up to 2 GiB.
	e.jobs.answer[filerestore.ModeMeasure] = fakeJobAnswer{done: true, out: filerestore.Outcome{Result: filerestore.Result{Bytes: 3 << 29}}}
	e.reconcile()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-files"}, pvc); err != nil {
		t.Fatalf("the claim was not made: %v", err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("2Gi")) != 0 {
		t.Errorf("size %s, want 2Gi", got.String())
	}
	if pvc.Annotations[backupFileRestoreAnnotation] != "f1" || pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" {
		t.Errorf("claim %+v", pvc.ObjectMeta)
	}
	if st := e.fr("f1").Status; st.Phase != "Provisioning" || st.SizeGB != 2 {
		t.Fatalf("status %+v", st)
	}

	// Bound: held back, and the copy runs into the new claim.
	uid, gid := int64(107), int64(107)
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-newclaim"},
		Spec: v1alpha1.SubVolumeSpec{PoolName: "data-a", PVCNamespace: "apps", PVCName: "db-files",
			SubPath: "/pvcs/pvc-99999999-8888-7777-6666-555555555555", ShareMountTargetIP: "10.240.1.1", ShareExportPath: "/export_a",
			UID: &uid, GID: &gid, Permissions: "0777"},
	}
	e.create(sv)
	pvc.Spec.VolumeName = sv.Name
	if err := e.c.Update(e.ctx, pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := e.c.Status().Update(e.ctx, pvc); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if e.sv(sv.Name).Labels[RestorePendingLabel] != "true" {
		t.Error("a new claim must be held back from pods during the copy")
	}
	calls := e.jobs.callsFor(filerestore.ModeNewClaim)
	if len(calls) != 1 || calls[0].Volume != sv.Spec.SubPath || calls[0].CapacityBytes != 2<<30 || *calls[0].UID != 107 {
		t.Fatalf("new-claim calls %+v", calls)
	}

	e.jobs.answer[filerestore.ModeNewClaim] = fakeJobAnswer{done: true, out: filerestore.Outcome{Result: filerestore.Result{Files: 2, Bytes: 3 << 29}, Latest: true}}
	e.reconcile()
	if st := e.fr("f1").Status; st.Phase != "Done" || !st.Latest || st.Files != 2 {
		t.Fatalf("status %+v", st)
	}
	if e.sv(sv.Name).Labels[RestorePendingLabel] != "" {
		t.Error("the claim is still held back after Done")
	}
}

func TestFileRestore_NewClaimSizeFromTheRequestSkipsTheMeasureJob(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.request("f1", func(fr *v1alpha1.BackupFileRestore) { fr.Spec.SizeGB = 7; fr.Spec.StorageClassName = "gold" })
	e.reconcile()
	if len(e.jobs.callsFor(filerestore.ModeMeasure)) != 0 {
		t.Fatal("measured although the size was given")
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-files"}, pvc); err != nil {
		t.Fatal(err)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("7Gi")) != 0 || *pvc.Spec.StorageClassName != "gold" {
		t.Errorf("claim size %s class %v", got.String(), pvc.Spec.StorageClassName)
	}
}

func TestFileRestore_NewClaimIsAtLeastOneGiB(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.request("f1", nil)
	e.jobs.answer[filerestore.ModeMeasure] = fakeJobAnswer{done: true, out: filerestore.Outcome{Result: filerestore.Result{Bytes: 10}}}
	e.reconcile()
	if st := e.fr("f1").Status; st.SizeGB != 1 {
		t.Fatalf("size %d", st.SizeGB)
	}
}

func TestFileRestore_NeverWritesOverAClaimThatExists(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.existingClaim("db-files")
	e.request("f1", nil)
	e.jobs.answer[filerestore.ModeMeasure] = fakeJobAnswer{done: true, out: filerestore.Outcome{Result: filerestore.Result{Bytes: 10}}}
	e.reconcile()
	st := e.fr("f1").Status
	if st.Phase != "Failed" || !strings.Contains(st.Message, "never writes over") {
		t.Fatalf("status %+v", st)
	}
	if len(e.jobs.callsFor(filerestore.ModeNewClaim)) != 0 {
		t.Fatal("a copy started into a claim that exists")
	}
}

func TestFileRestore_FilesThatAreTooBigFailBeforeAClaimIsMade(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.request("f1", nil)
	e.jobs.answer[filerestore.ModeMeasure] = fakeJobAnswer{done: true, out: filerestore.Outcome{Result: filerestore.Result{Bytes: filerestore.DefaultMaxBytes + 1}}}
	e.reconcile()
	st := e.fr("f1").Status
	if st.Phase != "Failed" || !strings.Contains(st.Message, "limit") {
		t.Fatalf("status %+v", st)
	}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-files"}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatal("a claim was made")
	}
}

func TestFileRestore_AFailedCopyKeepsTheNewClaimHeldBack(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.request("f1", func(fr *v1alpha1.BackupFileRestore) { fr.Spec.SizeGB = 3 })
	e.reconcile()
	pvc := &corev1.PersistentVolumeClaim{}
	_ = e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-files"}, pvc)
	sv := &v1alpha1.SubVolume{ObjectMeta: metav1.ObjectMeta{Name: "pvc-newclaim"},
		Spec: v1alpha1.SubVolumeSpec{PoolName: "data-a", PVCNamespace: "apps", PVCName: "db-files", SubPath: "/pvcs/pvc-99999999-8888-7777-6666-555555555555",
			ShareMountTargetIP: "10.240.1.1", ShareExportPath: "/export_a"}}
	e.create(sv)
	pvc.Spec.VolumeName = sv.Name
	_ = e.c.Update(e.ctx, pvc)
	pvc.Status.Phase = corev1.ClaimBound
	_ = e.c.Status().Update(e.ctx, pvc)
	e.jobs.answer[filerestore.ModeNewClaim] = refused("set the owner: operation not permitted")
	e.reconcile()
	st := e.fr("f1").Status
	if st.Phase != "Failed" || !strings.Contains(st.Message, "operation not permitted") || !strings.Contains(st.Message, "held back") {
		t.Fatalf("status %+v", st)
	}
	if e.sv(sv.Name).Labels[RestorePendingLabel] != "true" {
		t.Fatal("a half-filled claim was opened to pods")
	}
}

func TestFileRestore_AMeasureJobThatRefusesFailsBeforeAClaimIsMade(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.request("f1", nil)
	e.jobs.answer[filerestore.ModeMeasure] = refused("a path is not in the backup")
	e.reconcile()
	if st := e.fr("f1").Status; st.Phase != "Failed" || !strings.Contains(st.Message, "not in the backup") {
		t.Fatalf("status %+v", st)
	}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-files"}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Fatal("a claim was made")
	}
}

func TestFileRestore_FailsWhenThereIsNoBackup(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.request("f1", func(fr *v1alpha1.BackupFileRestore) { fr.Spec.SourceName = "other" })
	e.request("f2", func(fr *v1alpha1.BackupFileRestore) { fr.Spec.Policy = "ghost" })
	e.reconcile()
	for _, n := range []string{"f1", "f2"} {
		if st := e.fr(n).Status; st.Phase != "Failed" {
			t.Errorf("%s status %+v", n, st)
		}
	}
}

func TestFileRestore_AnEndedRestoreIsRemovedAfterTheKeepTime(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.existingClaim("work")
	e.existing("f1", "work")
	e.jobs.answer[filerestore.ModeFolder] = fakeJobAnswer{done: true, out: filerestore.Outcome{Folder: "x"}}
	e.reconcile()
	if e.fr("f1").Status.Phase != "Done" {
		t.Fatal("not done")
	}
	e.now = e.now.Add(23 * time.Hour)
	e.reconcile()
	_ = e.fr("f1") // still there
	e.now = e.now.Add(2 * time.Hour)
	e.reconcile()
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: "f1"}, &v1alpha1.BackupFileRestore{}); !apierrors.IsNotFound(err) {
		t.Fatalf("the ended restore is still there: %v", err)
	}
}

func TestFileRestore_OpensAClaimWhoseRestoreIsDone(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.request("f1", func(fr *v1alpha1.BackupFileRestore) { fr.Spec.SizeGB = 3 })
	e.reconcile()
	pvc := &corev1.PersistentVolumeClaim{}
	_ = e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-files"}, pvc)
	sv := &v1alpha1.SubVolume{ObjectMeta: metav1.ObjectMeta{Name: "pvc-newclaim", Labels: map[string]string{RestorePendingLabel: "true"}},
		Spec: v1alpha1.SubVolumeSpec{PVCNamespace: "apps", PVCName: "db-files"}}
	e.create(sv)
	pvc.Spec.VolumeName = sv.Name
	_ = e.c.Update(e.ctx, pvc)
	fr := e.fr("f1")
	fr.Status.Phase = "Done"
	if err := e.c.Status().Update(e.ctx, fr); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if e.sv(sv.Name).Labels[RestorePendingLabel] != "" {
		t.Fatal("a claim whose restore is Done stays held back")
	}
}

func TestFileRestore_OnlyAFewRunAtOnce(t *testing.T) {
	e := newFileRestoreEnv(t)
	e.bc.SetFileRestore(e.jobs, FileRestoreConfig{MaxRunning: 2})
	for _, n := range []string{"w1", "w2", "w3"} {
		e.existingClaim(n)
	}
	for i, n := range []string{"w1", "w2", "w3"} {
		e.now = e.now.Add(time.Second)
		e.existing("f"+string(rune('1'+i)), n)
	}
	e.reconcile()
	for _, n := range []string{"f1", "f2"} {
		if st := e.fr(n).Status; st.Phase != "Restoring" {
			t.Errorf("%s status %+v", n, st)
		}
	}
	st := e.fr("f3").Status
	if st.Phase != "Pending" || !strings.Contains(st.Message, "limit is 2") {
		t.Fatalf("third restore status %+v", st)
	}
	for _, c := range e.jobs.calls {
		if c.Key == "filerestore-uid-f3" {
			t.Fatal("the third restore started a Job over the limit")
		}
	}
	// One ends; the waiting one goes on.
	e.jobs.answer[filerestore.ModeFolder] = fakeJobAnswer{done: true, out: filerestore.Outcome{Folder: "x"}}
	e.reconcile()
	e.reconcile()
	if st := e.fr("f3").Status; st.Phase != "Done" {
		t.Fatalf("third restore status %+v", st)
	}
}

func TestFileRestore_DoesNothingWithoutJobs(t *testing.T) {
	e := newRestoreEnv(t)
	fr := &v1alpha1.BackupFileRestore{ObjectMeta: metav1.ObjectMeta{Name: "f1", UID: "u"},
		Spec: v1alpha1.BackupFileRestoreSpec{Policy: "nightly", SourceNamespace: "apps", SourceName: "db", Paths: []string{"/a"}, TargetNamespace: "apps", TargetName: "x"}}
	e.create(fr)
	e.reconcile()
	got := &v1alpha1.BackupFileRestore{}
	_ = e.c.Get(e.ctx, types.NamespacedName{Name: "f1"}, got)
	if got.Status.Phase != "" {
		t.Fatalf("status %+v", got.Status)
	}
}
