package pool

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// restoreEnv is a backup environment with one backed-up volume, apps/db.
type restoreEnv struct {
	*backupEnv
	sd    *fakeShareData
	claim *v1alpha1.SubVolume // the SubVolume of the backed-up claim
}

func newRestoreEnv(t *testing.T) *restoreEnv {
	t.Helper()
	e := &restoreEnv{backupEnv: newBackupEnv(t)}
	e.sd = &fakeShareData{nfs: newFakeNFSOperations(), base: "/x"}
	e.bc.SetShareData(e.sd)
	e.claim = e.addClaim("apps", "db", "data-a", yes)
	e.claim.Spec.RequestedGB = 40
	e.claim.Spec.SubPath = "/pvcs/pvc-11111111-2222-3333-4444-555555555555"
	if err := e.c.Update(e.ctx, e.claim); err != nil {
		t.Fatal(err)
	}
	sc := "fast"
	pvc := &corev1.PersistentVolumeClaim{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db"}, pvc); err != nil {
		t.Fatal(err)
	}
	pvc.Spec.StorageClassName = &sc
	if err := e.c.Update(e.ctx, pvc); err != nil {
		t.Fatal(err)
	}
	e.policy("nightly", nil)
	e.reconcile() // the policy now lists the volume
	return e
}

func (e *restoreEnv) request(name string, mutate func(*v1alpha1.BackupRestore)) {
	e.t.Helper()
	br := &v1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.BackupRestoreSpec{Policy: "nightly", SourceNamespace: "apps", SourceName: "db",
			TargetNamespace: "apps", TargetName: "db-restored"},
	}
	if mutate != nil {
		mutate(br)
	}
	e.create(br)
}

func (e *restoreEnv) br(name string) *v1alpha1.BackupRestore {
	e.t.Helper()
	br := &v1alpha1.BackupRestore{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: name}, br); err != nil {
		e.t.Fatal(err)
	}
	return br
}

// bindTarget makes the target PVC bound, with the SubVolume the driver would make.
func (e *restoreEnv) bindTarget() *v1alpha1.SubVolume {
	e.t.Helper()
	uid, gid := int64(107), int64(107)
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-99999999-8888-7777-6666-555555555555"},
		Spec: v1alpha1.SubVolumeSpec{PoolName: "data-a", PVCNamespace: "apps", PVCName: "db-restored",
			SubPath: "/pvcs/pvc-99999999-8888-7777-6666-555555555555", ShareMountTargetIP: "10.240.1.1", ShareExportPath: "/export_a",
			UID: &uid, GID: &gid, Permissions: "0777"},
	}
	e.create(sv)
	pvc := &corev1.PersistentVolumeClaim{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-restored"}, pvc); err != nil {
		e.t.Fatal(err)
	}
	pvc.Spec.VolumeName = sv.Name
	if err := e.c.Update(e.ctx, pvc); err != nil {
		e.t.Fatal(err)
	}
	pvc.Status.Phase = corev1.ClaimBound
	if err := e.c.Status().Update(e.ctx, pvc); err != nil {
		e.t.Fatal(err)
	}
	return sv
}

func TestBackupRestore_RunsFromRequestToDone(t *testing.T) {
	e := newRestoreEnv(t)
	moment := metav1.NewTime(time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC))
	e.request("r1", func(br *v1alpha1.BackupRestore) { br.Spec.Moment = &moment })

	// Pass 1: the target PVC is made, like the backed-up one.
	e.reconcile()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-restored"}, pvc); err != nil {
		t.Fatalf("the target PVC was not made: %v", err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" {
		t.Errorf("storage class = %v, want fast (the class of the backed-up claim)", pvc.Spec.StorageClassName)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("40Gi")) != 0 {
		t.Errorf("size = %s, want 40Gi", got.String())
	}
	if len(pvc.Spec.AccessModes) != 1 || pvc.Spec.AccessModes[0] != corev1.ReadWriteMany {
		t.Errorf("access modes = %v, want ReadWriteMany", pvc.Spec.AccessModes)
	}
	if pvc.Annotations[backupRestoreAnnotation] != "r1" {
		t.Errorf("annotations = %v: the PVC must name the restore that made it", pvc.Annotations)
	}
	if got := e.br("r1").Status.Phase; got != "Provisioning" {
		t.Errorf("phase = %q, want Provisioning", got)
	}
	if len(e.sd.restores) != 0 {
		t.Fatal("the copy started before the PVC was bound")
	}

	// Pass 2: bound. The SubVolume is held back and the copy starts.
	target := e.bindTarget()
	e.sd.setInProgress(true)
	e.reconcile()
	if got := e.sv(target.Name).Labels[RestorePendingLabel]; got != "true" {
		t.Errorf("restore-pending label = %q: a pod must not mount the volume during the copy", got)
	}
	if len(e.sd.restores) == 0 {
		t.Fatal("no restore started")
	}
	call := e.sd.restores[0]
	wantVol := "/backup/nightly/pvcs/pvc-11111111-2222-3333-4444-555555555555"
	wantMoments := "/backup/nightly/.moments/pvcs/pvc-11111111-2222-3333-4444-555555555555"
	if call.key != "restore-r1" || call.volPath != wantVol || call.momentsPath != wantMoments ||
		call.dstPath != target.Spec.SubPath || !call.moment.Equal(moment.Time) {
		t.Errorf("restore call = %+v", call)
	}
	if got := e.br("r1").Status.Phase; got != "Restoring" {
		t.Errorf("phase = %q, want Restoring", got)
	}

	// Pass 3: the copy has ended.
	e.sd.setInProgress(false)
	e.sd.restoreOutcome = RestoreOutcome{Moment: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)}
	e.reconcile()
	br := e.br("r1")
	if br.Status.Phase != "Done" || br.Status.RestoredMoment == nil ||
		!br.Status.RestoredMoment.Equal(&metav1.Time{Time: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)}) || br.Status.Latest {
		t.Errorf("status = %+v, want Done at 09:00, not the latest", br.Status)
	}
	if got := e.sv(target.Name).Labels[RestorePendingLabel]; got != "" {
		t.Errorf("restore-pending label = %q after the copy, want it removed", got)
	}
	// Done stays done.
	n := len(e.sd.restores)
	e.reconcile()
	if len(e.sd.restores) != n {
		t.Error("a finished restore ran again")
	}
}

func TestBackupRestore_LatestWhenNoMoment(t *testing.T) {
	e := newRestoreEnv(t)
	e.request("r1", nil)
	e.reconcile()
	e.bindTarget()
	e.reconcile()
	if len(e.sd.restores) == 0 || !e.sd.restores[0].moment.IsZero() {
		t.Errorf("restores = %+v, want a restore of the latest (zero moment)", e.sd.restores)
	}
}

func TestBackupRestore_NeverWritesOverAnExistingVolume(t *testing.T) {
	e := newRestoreEnv(t)
	other := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "db-restored"}}
	e.create(other)
	e.request("r1", nil)
	e.reconcile()
	br := e.br("r1")
	if br.Status.Phase != "Failed" || !strings.Contains(br.Status.Message, "exists") {
		t.Errorf("status = %+v, want Failed because the PVC exists", br.Status)
	}
	if len(e.sd.restores) != 0 {
		t.Error("a restore ran into a volume that already existed")
	}
}

func TestBackupRestore_FailsWhenThereIsNoBackup(t *testing.T) {
	e := newRestoreEnv(t)
	e.request("nope", func(br *v1alpha1.BackupRestore) { br.Spec.SourceName = "other" })
	e.request("nopolicy", func(br *v1alpha1.BackupRestore) { br.Spec.Policy = "missing" })
	e.reconcile()
	for _, n := range []string{"nope", "nopolicy"} {
		br := e.br(n)
		if br.Status.Phase != "Failed" || br.Status.Message == "" {
			t.Errorf("%s: status = %+v, want Failed with a reason", n, br.Status)
		}
	}
	l := &corev1.PersistentVolumeClaimList{}
	if err := e.c.List(e.ctx, l); err != nil {
		t.Fatal(err)
	}
	for _, p := range l.Items {
		if p.Name == "db-restored" {
			t.Error("a PVC was made for a restore that cannot run")
		}
	}
}

func TestBackupRestore_RestoresAVolumeThatIsGone(t *testing.T) {
	e := newRestoreEnv(t)
	clock := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	e.bc.SetNowFunc(func() time.Time { return clock })
	e.markSynced(e.claim.Name, clock.Add(-time.Hour))
	e.reconcile()
	if err := e.c.Delete(e.ctx, e.claim); err != nil {
		t.Fatal(err)
	}
	if err := e.c.Delete(e.ctx, &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "db"}}); err != nil {
		t.Fatal(err)
	}
	e.reconcile()

	e.request("r1", nil)
	e.reconcile()
	e.bindTarget()
	e.reconcile()
	if len(e.sd.restores) == 0 || e.sd.restores[0].volPath != "/backup/nightly/pvcs/pvc-11111111-2222-3333-4444-555555555555" {
		t.Errorf("restores = %+v, want the backup of the deleted volume", e.sd.restores)
	}
}

func TestBackupRestore_FailedCopyKeepsTheVolumeHeldBack(t *testing.T) {
	e := newRestoreEnv(t)
	e.request("r1", nil)
	e.reconcile()
	target := e.bindTarget()
	e.sd.restoreErr = errPlain("no space")
	e.reconcile()
	br := e.br("r1")
	if br.Status.Phase != "Failed" || !strings.Contains(br.Status.Message, "no space") {
		t.Errorf("status = %+v, want Failed with the reason", br.Status)
	}
	if got := e.sv(target.Name).Labels[RestorePendingLabel]; got != "true" {
		t.Errorf("label = %q: a half-restored volume must stay held back", got)
	}
}

func TestBackupRestore_WaitsWhileTheSnapshotPoolIsNotReady(t *testing.T) {
	e := newRestoreEnv(t)
	p := &v1alpha1.FileSharePool{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: "backups"}, p); err != nil {
		t.Fatal(err)
	}
	p.Status.Shares[0].State = "Creating"
	if err := e.c.Update(e.ctx, p); err != nil {
		t.Fatal(err)
	}
	e.request("r1", nil)
	e.reconcile()
	br := e.br("r1")
	if br.Status.Phase == "Failed" {
		t.Errorf("status = %+v: a pool that is not ready now is no reason to fail", br.Status)
	}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-restored"}, &corev1.PersistentVolumeClaim{}); !apierrors.IsNotFound(err) {
		t.Error("a PVC was made while the backup cannot be read")
	}
}

type errPlain string

func (e errPlain) Error() string { return string(e) }
