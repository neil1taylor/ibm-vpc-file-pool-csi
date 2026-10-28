package pool

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func newFileRestoreCOSEnv(t *testing.T) *fileRestoreEnv {
	t.Helper()
	e := newFileRestoreEnv(t)
	bp := e.bp("nightly")
	bp.Spec.Export = testBackupExport()
	if err := e.c.Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"cos-credentials", "cos-backup-key", "kp-apikey"} {
		s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testDriverNS, Name: name}}
		if err := e.c.Create(e.ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	return e
}

func TestFileRestoreCOS_IntoAnExistingClaimReadsTheBackupFromTheBucket(t *testing.T) {
	e := newFileRestoreCOSEnv(t)
	pvc, sv := e.existingClaim("work")
	moment := metav1.NewTime(time.Date(2026, 10, 20, 9, 30, 0, 0, time.UTC))
	e.request("f1", func(fr *v1alpha1.BackupFileRestore) {
		fr.Spec.Source, fr.Spec.Existing, fr.Spec.TargetName, fr.Spec.Moment, fr.Spec.TargetUID =
			v1alpha1.BackupSourceCOS, true, "work", &moment, pvc.UID
	})
	e.jobs.answer[filerestore.ModeFolder] = fakeJobAnswer{}
	e.reconcile()

	calls := e.jobs.callsFor(filerestore.ModeFolder)
	if len(calls) != 1 {
		t.Fatalf("calls %+v", e.jobs.calls)
	}
	c := calls[0]
	if c.COS == nil || c.COS.Repo.Bucket != "pool-backups" || c.COS.Repo.Prefix != "lab/nightly/" || c.COS.Policy != "nightly" ||
		c.COS.Volume != "pvc-11111111-2222-3333-4444-555555555555" {
		t.Fatalf("COS = %+v", c.COS)
	}
	if c.LatestPath != "" || c.MomentsPath != "" || c.Backup != (ShareMount{}) {
		t.Errorf("a restore from COS names a folder on the snapshot pool: %+v", c)
	}
	if !c.At.Equal(moment.Time) || c.Key != "filerestore-uid-f1" || c.Policy != "nightly" {
		t.Errorf("request = %+v", c)
	}
	// The target is checked and written the same way as for a local backup.
	if c.Dest != (ShareMount{MountTargetIP: "10.240.1.1", ExportPath: "/export_a"}) || c.Volume != sv.Spec.SubPath ||
		c.UID == nil || *c.UID != 107 || c.Permissions != "0777" {
		t.Errorf("target = %+v", c)
	}
}

func TestFileRestoreCOS_IntoANewClaimMeasuresThenCopies(t *testing.T) {
	e := newFileRestoreCOSEnv(t)
	e.request("f1", func(fr *v1alpha1.BackupFileRestore) { fr.Spec.Source = v1alpha1.BackupSourceCOS })
	e.jobs.answer[filerestore.ModeMeasure] = fakeJobAnswer{done: true, out: filerestore.Outcome{Result: filerestore.Result{Bytes: 3 << 30}}}
	e.reconcile()
	if m := e.jobs.callsFor(filerestore.ModeMeasure); len(m) != 1 || m[0].COS == nil {
		t.Fatalf("measure calls %+v", e.jobs.calls)
	}
	fr := e.fr("f1")
	if fr.Status.SizeGB != 4 || fr.Status.Phase != "Provisioning" {
		t.Errorf("status = %+v (3 GiB and 20 percent is 4 GiB)", fr.Status)
	}
	// The class of the backed-up claim comes from the policy's record of it.
	pvc := &corev1.PersistentVolumeClaim{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Namespace: "apps", Name: "db-files"}, pvc); err != nil {
		t.Fatal(err)
	}
	if pvc.Spec.StorageClassName == nil || *pvc.Spec.StorageClassName != "fast" {
		t.Errorf("storage class = %v", pvc.Spec.StorageClassName)
	}
}

func TestFileRestoreCOS_ABucketNamedDirectlyNeedsAClassForANewClaim(t *testing.T) {
	e := newFileRestoreCOSEnv(t)
	e.request("f1", func(fr *v1alpha1.BackupFileRestore) {
		fr.Spec.Source, fr.Spec.Policy, fr.Spec.SourceNamespace, fr.Spec.SourceName = v1alpha1.BackupSourceCOS, "lost-policy", v1alpha1.NoNamespace, "pvc-99999999-8888-7777-6666-555555555555"
		fr.Spec.FromCOS = fromCOSBlock()
		fr.Spec.SizeGB = 5
	})
	e.reconcile()
	fr := e.fr("f1")
	if fr.Status.Phase != "Failed" || !strings.Contains(fr.Status.Message, "storage class") {
		t.Errorf("status = %+v", fr.Status)
	}
	// With a class it goes on.
	e2 := newFileRestoreCOSEnv(t)
	e2.request("f2", func(fr *v1alpha1.BackupFileRestore) {
		fr.Spec.Source, fr.Spec.Policy, fr.Spec.SourceNamespace, fr.Spec.SourceName = v1alpha1.BackupSourceCOS, "lost-policy", v1alpha1.NoNamespace, "pvc-99999999-8888-7777-6666-555555555555"
		fr.Spec.FromCOS = fromCOSBlock()
		fr.Spec.SizeGB, fr.Spec.StorageClassName = 5, "fast"
	})
	e2.reconcile()
	if st := e2.fr("f2").Status; st.Phase != "Provisioning" {
		t.Errorf("status = %+v", st)
	}
}

func TestFileRestoreCOS_FailsInWordsForTheThingsAUserCanFix(t *testing.T) {
	cases := map[string]struct {
		mutate func(*fileRestoreEnv, *v1alpha1.BackupFileRestore)
		want   string
	}{
		"no export block": {func(e *fileRestoreEnv, _ *v1alpha1.BackupFileRestore) {
			bp := e.bp("nightly")
			bp.Spec.Export = nil
			if err := e.c.Update(e.ctx, bp); err != nil {
				t.Fatal(err)
			}
		}, "has no export block"},
		"a missing Secret": {func(e *fileRestoreEnv, _ *v1alpha1.BackupFileRestore) {
			s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: testDriverNS, Name: "cos-credentials"}}
			if err := e.c.Delete(e.ctx, s); err != nil {
				t.Fatal(err)
			}
		}, `Secret "cos-credentials"`},
		"an unknown volume": {func(_ *fileRestoreEnv, fr *v1alpha1.BackupFileRestore) { fr.Spec.SourceName = "nope" }, "has no backup of apps/nope"},
		"a hostile bucket block": {func(_ *fileRestoreEnv, fr *v1alpha1.BackupFileRestore) {
			fr.Spec.FromCOS = fromCOSBlock()
			fr.Spec.FromCOS.Endpoint = "evil.example"
		}, "fromCOS.endpoint"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			e := newFileRestoreCOSEnv(t)
			e.request("f1", func(r *v1alpha1.BackupFileRestore) {
				r.Spec.Source = v1alpha1.BackupSourceCOS
				c.mutate(e, r)
			})
			e.reconcile()
			st := e.fr("f1").Status
			if st.Phase != "Failed" || !strings.Contains(st.Message, c.want) {
				t.Errorf("status = %+v, want a message with %q", st, c.want)
			}
			if len(e.jobs.calls) != 0 {
				t.Errorf("a Job was started: %+v", e.jobs.calls)
			}
		})
	}
}
