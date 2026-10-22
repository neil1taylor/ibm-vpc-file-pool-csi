package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func watchClient(t *testing.T, c client.Client) client.WithWatch {
	t.Helper()
	w, ok := c.(client.WithWatch)
	if !ok {
		t.Fatal("the fake client cannot watch")
	}
	return w
}

// These tests cover the findings of the review of the backup code.

// A restore that cannot save "Done" must not open the volume to pods.
func TestBackupRestore_VolumeStaysHeldUntilDoneIsSaved(t *testing.T) {
	e := newRestoreEnv(t)
	e.request("r1", nil)
	e.reconcile()
	target := e.bindTarget()
	e.sd.setInProgress(true)
	e.reconcile() // the copy starts and the volume is held back
	e.sd.setInProgress(false)

	failSave := true
	e.bc.client = interceptor.NewClient(watchClient(t, e.c), interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, c client.Client, sub string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
			if _, ok := obj.(*v1alpha1.BackupRestore); ok && failSave {
				return errors.New("conflict")
			}
			return c.SubResource(sub).Update(ctx, obj, opts...)
		},
	})
	e.reconcile()
	if got := e.sv(target.Name).Labels[RestorePendingLabel]; got != "true" {
		t.Fatalf("restore-pending label = %q: the volume was opened before Done was saved", got)
	}
	if got := e.br("r1").Status.Phase; got == "Done" {
		t.Fatal("phase is Done although the save failed")
	}

	failSave = false
	e.reconcile()
	if got := e.br("r1").Status.Phase; got != "Done" {
		t.Fatalf("phase = %q, want Done", got)
	}
	if got := e.sv(target.Name).Labels[RestorePendingLabel]; got != "" {
		t.Errorf("restore-pending label = %q after Done, want it removed", got)
	}
}

// If the controller stops between saving Done and removing the label, the
// next pass removes the label.
func TestBackupRestore_RemovesAGateLeftBehindByADoneRestore(t *testing.T) {
	e := newRestoreEnv(t)
	e.request("r1", nil)
	e.reconcile()
	target := e.bindTarget()
	e.reconcile()
	e.reconcile()
	if e.br("r1").Status.Phase != "Done" {
		t.Fatal("restore not done")
	}
	sv := e.sv(target.Name)
	e.bc.setSubVolumeRestoreLabel(e.ctx, sv, true) // as if the removal was lost
	n := len(e.sd.restores)
	e.reconcile()
	if got := e.sv(target.Name).Labels[RestorePendingLabel]; got != "" {
		t.Errorf("restore-pending label = %q: a Done restore must not hold a volume", got)
	}
	if len(e.sd.restores) != n {
		t.Error("a finished restore ran again")
	}
}

// A running restore keeps its gate while the sweep runs.
func TestBackupRestore_SweepKeepsTheGateOfARunningRestore(t *testing.T) {
	e := newRestoreEnv(t)
	e.request("r1", nil)
	e.reconcile()
	target := e.bindTarget()
	e.sd.setInProgress(true)
	e.reconcile()
	e.reconcile()
	if got := e.sv(target.Name).Labels[RestorePendingLabel]; got != "true" {
		t.Errorf("restore-pending label = %q while the copy runs, want true", got)
	}
}

func TestBackupReplicationName_DoesNotCollide(t *testing.T) {
	if a, b := backupReplicationName("a-b", "c", "s1", "s1"), backupReplicationName("a", "b-c", "s1", "s1"); a == b {
		t.Errorf("both give %q", a)
	}
	first := backupReplicationName("nightly", "data-a", "s1", "s1")
	if again := backupReplicationName("nightly", "data-a", "s1", "s1"); first != again {
		t.Error("the name must be stable")
	}
}

func TestBackupController_DoesNotRewriteAnUnchangedStatus(t *testing.T) {
	e := newBackupEnv(t)
	clock := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	e.bc.SetNowFunc(func() time.Time { return clock })
	sv := e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()
	e.markSynced(sv.Name, clock.Add(-time.Hour))
	e.reconcile()
	e.reconcile()
	bpRV := e.bp("nightly").ResourceVersion
	rpRV := e.repls()[0].ResourceVersion
	e.reconcile()
	if got := e.bp("nightly").ResourceVersion; got != bpRV {
		t.Errorf("the policy status was written again (%s -> %s)", bpRV, got)
	}
	if got := e.repls()[0].ResourceVersion; got != rpRV {
		t.Errorf("the replication policy was written again (%s -> %s)", rpRV, got)
	}
}

func TestBackupController_KeepsEarlierTimesWhenTheReplicationPolicyHasNoReport(t *testing.T) {
	e := newBackupEnv(t)
	clock := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	e.bc.SetNowFunc(func() time.Time { return clock })
	sv := e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()
	e.markSynced(sv.Name, clock.Add(-time.Hour))
	e.reconcile()
	if e.bp("nightly").Status.Volumes[0].LastBackupTime == nil {
		t.Fatal("setup: no backup time")
	}

	rp := e.repls()[0]
	rp.Status.SubVolumeStatuses = nil
	if err := e.c.Status().Update(e.ctx, &rp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if e.bp("nightly").Status.Volumes[0].LastBackupTime == nil {
		t.Error("the last backup time was lost; that raises a false stale-backup alert")
	}
}

func TestBackupController_ARoundedKeepTimeIsTheSameEverywhere(t *testing.T) {
	e := newBackupEnv(t)
	clock := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	e.bc.SetNowFunc(func() time.Time { return clock })
	sv := e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", func(p *v1alpha1.BackupPolicy) { p.Spec.KeepFor = "90m" })
	e.reconcile()
	if got := *e.repls()[0].Spec.RestorePointKeepHours; got != 2 {
		t.Errorf("keep hours = %d, want 90m rounded up to 2", got)
	}
	e.markSynced(sv.Name, clock.Add(-110*time.Minute))
	e.reconcile()
	if err := e.c.Delete(e.ctx, e.sv(sv.Name)); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := len(e.bp("nightly").Status.Volumes); got != 1 {
		t.Errorf("volumes = %d: the backup is 110m old and the keep time is 2h, so the entry stays", got)
	}
}

func TestBackupController_ASlowListDoesNotTidyALivePolicy(t *testing.T) {
	e := newBackupEnv(t)
	sv := e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()

	// The list of policies comes back empty (a cache that has not filled).
	e.bc.client = interceptor.NewClient(watchClient(t, e.c), interceptor.Funcs{
		List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
			if _, ok := list.(*v1alpha1.BackupPolicyList); ok {
				return nil
			}
			return c.List(ctx, list, opts...)
		},
	})
	e.reconcile()
	if got := e.sv(sv.Name).Labels[backupPolicyLabel]; got != "nightly" {
		t.Errorf("label = %q: a live policy lost its volume", got)
	}
	if got := len(e.repls()); got != 1 {
		t.Errorf("replication policies = %d, want 1", got)
	}
}

func TestMomentsScript_HasNoPipeThatCanFailTheJob(t *testing.T) {
	keep := int32(24)
	script := momentsScript("/backup/p", "/pvcs/pvc-x", &keep)
	if strings.Contains(script, "| head") {
		t.Errorf("a pipe into head fails under pipefail when the producer writes more:\n%s", script)
	}
}

func TestRestoreScript_RunsInBashAndSurvivesARetry(t *testing.T) {
	r, c := newApplyRunner()
	if _, _, err := r.RestoreBackup(context.Background(), "restore-a", testShareB, testBackupVol, testBackupMoments,
		testShareA, testVolPath, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	job := getShareJob(t, c, "restore-a")
	if got := job.Spec.Template.Spec.Containers[0].Command[0]; got != "/bin/bash" {
		t.Errorf("shell = %q: the script uses bash only syntax", got)
	}
	script := jobScript(job)
	disk := strings.Index(script, "--mode=restore-vm-disk")
	rm := strings.Index(script, `rm -f "$dst/disk.img"`)
	if rm < 0 || rm > disk {
		t.Errorf("a retry finds disk.img from the first try; the script must remove it before the disk restore:\n%s", script)
	}
	mk := strings.Index(script, `mkdir -p "$dst"`)
	if mk < 0 || mk > disk {
		t.Errorf("the new volume has no folder yet; the script must make it before the disk restore opens a file in it:\n%s", script)
	}
	if strings.Contains(script, "| tail") {
		t.Errorf("no pipes under set -e:\n%s", script)
	}
	if !strings.Contains(script, `pick="$src"`) || !strings.Contains(script, "no moment folder yet") {
		t.Errorf("a restore at a time needs the latest copy when no moment folder exists:\n%s", script)
	}
}
