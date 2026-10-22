package pool

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const cosRestoreTargetVolume = "pvc-99999999-8888-7777-6666-555555555555"

// cosRestoreEnv is a cluster with no BackupPolicy at all: a new cluster that
// restores from a bucket.
type cosRestoreEnv struct {
	*backupEnv
	sd *fakeShareData
}

func newCOSRestoreEnv(t *testing.T) *cosRestoreEnv {
	t.Helper()
	e := &cosRestoreEnv{backupEnv: newBackupEnv(t)}
	e.sd = &fakeShareData{nfs: newFakeNFSOperations(), base: "/x"}
	e.bc.SetShareData(e.sd)
	return e
}

func (e *cosRestoreEnv) request(name string, mutate func(*v1alpha1.BackupRestore)) {
	e.t.Helper()
	br := &v1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.BackupRestoreSpec{
			Policy: "nightly", TargetNamespace: "apps", TargetName: "db-restored", StorageClassName: "fast",
			FromCOS: &v1alpha1.RestoreFromCOS{
				Bucket: "pool-backups", Endpoint: "s3.eu-de.cloud-object-storage.appdomain.cloud", Prefix: "team-a",
				CredentialsSecret: "cos-credentials", BackupKeySecret: "cos-backup-key",
				KeyProtect: &v1alpha1.KeyProtectRef{InstanceID: "11111111-2222-3333-4444-555555555555", Region: "eu-de",
					RootKeyID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", APIKeySecret: "kp-apikey"},
				Volume: "pvc-11111111-2222-3333-4444-555555555555", SizeGB: 40,
			},
		},
	}
	if mutate != nil {
		mutate(br)
	}
	e.create(br)
}

func (e *cosRestoreEnv) br(name string) *v1alpha1.BackupRestore {
	e.t.Helper()
	br := &v1alpha1.BackupRestore{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: name}, br); err != nil {
		e.t.Fatal(err)
	}
	return br
}

func (e *cosRestoreEnv) bindTarget() *v1alpha1.SubVolume {
	e.t.Helper()
	uid, gid := int64(107), int64(107)
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: cosRestoreTargetVolume},
		Spec: v1alpha1.SubVolumeSpec{PoolName: "data-a", PVCNamespace: "apps", PVCName: "db-restored",
			SubPath: "/pvcs/" + cosRestoreTargetVolume, ShareMountTargetIP: "10.240.1.1", ShareExportPath: "/export_a",
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

func TestBackupRestoreFromCOS_RunsWithoutAPolicyFromRequestToDone(t *testing.T) {
	e := newCOSRestoreEnv(t)
	moment := metav1.NewTime(time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC))
	e.request("c1", func(br *v1alpha1.BackupRestore) { br.Spec.Moment = &moment })

	// Pass 1: the PVC is made from the class and size in the request.
	e.reconcile()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-restored"}, pvc); err != nil {
		t.Fatalf("the target PVC was not made: %v", err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" {
		t.Errorf("storage class = %v, want fast", pvc.Spec.StorageClassName)
	}
	if got := pvc.Spec.Resources.Requests[corev1.ResourceStorage]; got.Cmp(resource.MustParse("40Gi")) != 0 {
		t.Errorf("size = %s, want 40Gi", got.String())
	}
	if got := e.br("c1").Status.Phase; got != "Provisioning" {
		t.Errorf("phase = %q, want Provisioning", got)
	}
	if len(e.sd.cosFetches()) != 0 {
		t.Fatal("the fetch started before the PVC was bound")
	}

	// Pass 2: bound. The volume is held back and the fetch starts.
	target := e.bindTarget()
	e.sd.mu.Lock()
	e.sd.cos.fetchRunning = true
	e.sd.mu.Unlock()
	e.reconcile()
	if got := e.sv(target.Name).Labels[RestorePendingLabel]; got != "true" {
		t.Errorf("restore-pending label = %q: a pod must not mount the volume during the restore", got)
	}
	fetches := e.sd.cosFetches()
	if len(fetches) != 1 {
		t.Fatalf("fetches = %d, want 1", len(fetches))
	}
	f := fetches[0]
	if f.key != "cosfetch-c1" || f.dst.MountTargetIP != "10.240.1.1" || f.dst.ExportPath != "/export_a" {
		t.Errorf("fetch = %+v; want key cosfetch-c1 on the share of the new volume", f)
	}
	if f.repo.Bucket != "pool-backups" || f.repo.Prefix != "team-a/nightly/" || f.repo.CredentialsSecret != "cos-credentials" ||
		f.repo.BackupKeySecret != "cos-backup-key" || f.repo.KeyProtect.RootKeyID == "" || f.repo.LockMode != ExportLockNone {
		t.Errorf("repo = %+v", f.repo)
	}
	if f.req.SourceVolume != "pvc-11111111-2222-3333-4444-555555555555" || f.req.TargetVolume != cosRestoreTargetVolume ||
		f.req.Scratch != COSScratchName("c1") || !f.req.Moment.Equal(moment.Time) {
		t.Errorf("request = %+v", f.req)
	}
	if got := e.br("c1").Status; got.Phase != "Restoring" || got.Fetched {
		t.Errorf("status = %+v, want Restoring and not fetched", got)
	}
	if len(e.sd.restores) != 0 {
		t.Fatal("the copy started before the fetch ended")
	}

	// Pass 3: the fetch ends. The copy starts from the scratch folder, on the
	// same share, and runs on.
	e.sd.mu.Lock()
	e.sd.cos.fetchRunning = false
	e.sd.inProgress = true
	e.sd.mu.Unlock()
	e.reconcile()
	if got := e.br("c1").Status; !got.Fetched || got.Copied || got.Phase != "Restoring" {
		t.Errorf("status = %+v, want fetched, not copied, Restoring", got)
	}
	if len(e.sd.restores) == 0 {
		t.Fatal("the copy did not start after the fetch")
	}
	call := e.sd.restores[0]
	scratchBackup, scratchMoments := COSScratchPaths(COSScratchName("c1"), cosRestoreTargetVolume)
	if call.key != "restore-c1" || call.volPath != scratchBackup || call.momentsPath != scratchMoments ||
		call.dstPath != target.Spec.SubPath || !call.moment.Equal(moment.Time) || call.srcIP != "10.240.1.1" {
		t.Errorf("restore call = %+v; want the scratch folder of the new volume", call)
	}

	// Pass 4: the copy ends. The scratch folder is removed, then it is Done.
	e.sd.mu.Lock()
	e.sd.inProgress = false
	e.sd.restoreOutcome = RestoreOutcome{Moment: moment.Time, Latest: false}
	e.sd.cos.cleanupRunning = true
	e.sd.mu.Unlock()
	fetchesBefore := len(e.sd.cosFetches())
	e.reconcile()
	if got := e.br("c1").Status; !got.Copied || got.Phase == "Done" {
		t.Errorf("status = %+v, want copied and not done until the scratch folder is gone", got)
	}
	if e.sv(target.Name).Labels[RestorePendingLabel] != "true" {
		t.Error("the volume opened before the restore was done")
	}
	if len(e.sd.cosFetches()) != fetchesBefore {
		t.Error("a fetch that had ended was started again")
	}

	// Pass 5: the scratch folder is gone.
	e.sd.mu.Lock()
	e.sd.cos.cleanupRunning = false
	e.sd.mu.Unlock()
	restoresBefore := len(e.sd.restores)
	e.reconcile()
	got := e.br("c1").Status
	if got.Phase != "Done" || got.RestoredMoment == nil || !got.RestoredMoment.Equal(&moment) {
		t.Errorf("status = %+v, want Done at %s", got, moment.Time)
	}
	if _, held := e.sv(target.Name).Labels[RestorePendingLabel]; held {
		t.Error("the volume is still held back after Done")
	}
	if len(e.sd.restores) != restoresBefore {
		t.Error("the copy ran again after it had ended")
	}
	cl := e.sd.cosCleanups()
	if len(cl) == 0 || cl[0].scratch != COSScratchName("c1") || cl[0].key != "coscleanup-c1" {
		t.Errorf("cleanup calls = %+v", cl)
	}
}

func TestBackupRestoreFromCOS_FailedFetchFailsTheRestoreAndHoldsTheVolume(t *testing.T) {
	e := newCOSRestoreEnv(t)
	e.request("c1", nil)
	e.reconcile()
	target := e.bindTarget()
	e.sd.mu.Lock()
	e.sd.cos.fetchFail = "no backup of the volume"
	e.sd.mu.Unlock()
	e.reconcile()
	st := e.br("c1").Status
	if st.Phase != "Failed" || !strings.Contains(st.Message, "db-restored") {
		t.Errorf("status = %+v, want Failed and a message that names the held-back PVC", st)
	}
	if e.sv(target.Name).Labels[RestorePendingLabel] != "true" {
		t.Error("a failed restore must leave the volume held back")
	}
}

func TestBackupRestoreFromCOS_FailedCleanupDoesNotFailTheRestore(t *testing.T) {
	e := newCOSRestoreEnv(t)
	e.request("c1", nil)
	e.reconcile()
	e.bindTarget()
	e.sd.mu.Lock()
	e.sd.cos.cleanupFail = "rm failed"
	e.sd.restoreOutcome = RestoreOutcome{Latest: true}
	e.sd.mu.Unlock()
	e.reconcile() // fetch, copy, cleanup in one pass: all Jobs report done
	if got := e.br("c1").Status.Phase; got != "Done" {
		t.Errorf("phase = %q, want Done: the data is in the volume, only the scratch folder is left", got)
	}
}

func TestBackupRestoreFromCOS_NeverWritesOverAnExistingPVC(t *testing.T) {
	e := newCOSRestoreEnv(t)
	e.create(&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "apps", Name: "db-restored"}})
	e.request("c1", nil)
	e.reconcile()
	if st := e.br("c1").Status; st.Phase != "Failed" || !strings.Contains(st.Message, "exists") {
		t.Errorf("status = %+v, want Failed because the PVC exists", st)
	}
	if len(e.sd.cosFetches()) != 0 {
		t.Error("a fetch started for a PVC that already existed")
	}
}

func TestBackupRestoreFromCOS_InvalidBlockFailsWithoutMakingAPVC(t *testing.T) {
	e := newCOSRestoreEnv(t)
	e.request("c1", func(br *v1alpha1.BackupRestore) { br.Spec.FromCOS.Volume = "../x" })
	e.reconcile()
	if st := e.br("c1").Status; st.Phase != "Failed" || !strings.Contains(st.Message, "fromCOS.volume") {
		t.Errorf("status = %+v, want Failed naming spec.fromCOS.volume", st)
	}
	pvc := &corev1.PersistentVolumeClaim{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-restored"}, pvc); err == nil {
		t.Error("a PVC was made for an invalid request")
	}
}

func TestBackupRestoreFromCOS_EmergencyOnlyReachesTheJob(t *testing.T) {
	e := newCOSRestoreEnv(t)
	e.request("c1", func(br *v1alpha1.BackupRestore) {
		br.Spec.FromCOS.KeyProtect, br.Spec.FromCOS.BackupKeySecret = nil, ""
		br.Spec.FromCOS.EmergencyKeyOnly, br.Spec.FromCOS.EmergencyKeySecret = true, "emergency"
	})
	e.reconcile()
	e.bindTarget()
	e.sd.mu.Lock()
	e.sd.cos.fetchRunning = true
	e.sd.mu.Unlock()
	e.reconcile()
	f := e.sd.cosFetches()
	if len(f) != 1 || !f[0].repo.EmergencyOnly || f[0].repo.EmergencyKeySecret != "emergency" {
		t.Fatalf("fetches = %+v, want one emergency-only fetch", f)
	}
}
