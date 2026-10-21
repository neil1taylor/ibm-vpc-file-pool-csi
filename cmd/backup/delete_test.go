package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	delVolume  = "pvc-11111111-2222-3333-4444-555555555555"
	delBackup  = "/backup/nightly/pvcs/" + delVolume
	delMoments = "/backup/nightly/.moments/pvcs/" + delVolume
)

var delShare = pool.ShareMount{MountTargetIP: "10.240.0.9", ExportPath: "/share_bk"}

// fakeRunner stands in for the Jobs on the snapshot pool's share.
type fakeRunner struct {
	bytes      int64
	measureErr error
	removeErr  error
	measured   []string
	removed    []string
	share      pool.ShareMount
	pending    int // calls that answer "not done yet" before the Job is done
}

func (f *fakeRunner) MeasureBackup(_ context.Context, _ string, share pool.ShareMount, backupPath, momentsPath string) (bool, int64, error) {
	f.measured = append(f.measured, backupPath, momentsPath)
	f.share = share
	if f.pending > 0 {
		f.pending--
		return false, 0, nil
	}
	return f.measureErr == nil, f.bytes, f.measureErr
}

func (f *fakeRunner) RemoveBackup(_ context.Context, _ string, share pool.ShareMount, backupPath, momentsPath string) (bool, error) {
	f.removed = append(f.removed, backupPath, momentsPath)
	f.share = share
	return f.removeErr == nil, f.removeErr
}

func delPolicy(retained bool) *v1alpha1.BackupPolicy {
	return &v1alpha1.BackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "nightly"},
		Spec:       v1alpha1.BackupPolicySpec{SnapshotPool: "backups"},
		Status: v1alpha1.BackupPolicyStatus{Volumes: []v1alpha1.BackupVolumeStatus{
			{PVCNamespace: "vms", PVCName: "web-root", SubVolumeName: delVolume, SubPath: "/pvcs/" + delVolume,
				Retained: retained, LastBackupTime: &last, RestorePoints: 3},
			{PVCNamespace: "vms", PVCName: "other", SubVolumeName: "pvc-x", SubPath: "/pvcs/pvc-66666666-7777-8888-9999-000000000000", Retained: true},
		}},
	}
}

func delPool() *v1alpha1.FileSharePool {
	return &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "backups"},
		Status: v1alpha1.FileSharePoolStatus{Shares: []v1alpha1.PoolShareStatus{
			{ShareID: "r1", MountTargetIP: delShare.MountTargetIP, ExportPath: delShare.ExportPath},
		}},
	}
}

func delRestore(name, phase, src string) *v1alpha1.BackupRestore {
	return &v1alpha1.BackupRestore{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec:       v1alpha1.BackupRestoreSpec{Policy: "nightly", SourceNamespace: "vms", SourceName: src, TargetNamespace: "vms", TargetName: "new"},
		Status:     v1alpha1.BackupRestoreStatus{Phase: phase},
	}
}

func baseFlags() map[string]string {
	return map[string]string{"policy": "nightly", "pvc": "vms/web-root", "yes": "true", "poll": "1ms"}
}

type delRun struct {
	out    bytes.Buffer
	runner *fakeRunner
	err    error
}

func doDelete(t *testing.T, flags map[string]string, input string, interactive bool, runner *fakeRunner, objs ...client.Object) *delRun {
	t.Helper()
	r := &delRun{runner: runner}
	r.err = runDelete(context.Background(), newClient(objs...), &r.out, strings.NewReader(input), interactive, runner, flags)
	return r
}

func TestDelete_RemovesTheTwoFoldersAfterYes(t *testing.T) {
	r := doDelete(t, baseFlags(), "", false, &fakeRunner{bytes: 5 << 30}, delPolicy(true), delPool())
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := strings.Join(r.runner.removed, " "); got != delBackup+" "+delMoments {
		t.Errorf("removed %q, want %q %q", r.runner.removed, delBackup, delMoments)
	}
	if r.runner.share != delShare {
		t.Errorf("Job ran on %+v, want the snapshot pool's share %+v", r.runner.share, delShare)
	}
	for _, want := range []string{"vms/web-root", delBackup, delMoments, "5.0 GiB", "Removed"} {
		if !strings.Contains(r.out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, r.out.String())
		}
	}
}

func TestDelete_ShowsWhatWillGoBeforeItAsks(t *testing.T) {
	r := doDelete(t, map[string]string{"policy": "nightly", "pvc": "vms/web-root", "poll": "1ms"}, "vms/web-root\n", true,
		&fakeRunner{bytes: 1 << 20}, delPolicy(true), delPool())
	if r.err != nil {
		t.Fatal(r.err)
	}
	out := r.out.String()
	size, prompt := strings.Index(out, "1.0 MiB"), strings.Index(out, "Type vms/web-root to confirm")
	if size < 0 || prompt < 0 || size > prompt {
		t.Errorf("the size must be shown before the prompt:\n%s", out)
	}
}

func TestDelete_TypedConfirmationMustMatch(t *testing.T) {
	flags := map[string]string{"policy": "nightly", "pvc": "vms/web-root", "poll": "1ms"}
	for _, input := range []string{"", "\n", "yes\n", "web-root\n", "vms/other\n"} {
		r := doDelete(t, flags, input, true, &fakeRunner{bytes: 10}, delPolicy(true), delPool())
		if r.err == nil || !strings.Contains(r.err.Error(), "not confirmed") {
			t.Errorf("input %q: err = %v, want a not-confirmed error", input, r.err)
		}
		if len(r.runner.removed) != 0 {
			t.Errorf("input %q: removed %v without confirmation", input, r.runner.removed)
		}
	}
}

func TestDelete_RefusesWithoutYesInANonInteractiveShell(t *testing.T) {
	flags := map[string]string{"policy": "nightly", "pvc": "vms/web-root", "poll": "1ms"}
	r := doDelete(t, flags, "vms/web-root\n", false, &fakeRunner{}, delPolicy(true), delPool())
	if r.err == nil || !strings.Contains(r.err.Error(), "--yes") {
		t.Fatalf("err = %v, want a refusal that names --yes", r.err)
	}
	if len(r.runner.measured)+len(r.runner.removed) != 0 {
		t.Errorf("a Job was started: %v %v", r.runner.measured, r.runner.removed)
	}
}

func TestDelete_RefusesWhileThePolicyStillBacksUpTheVolume(t *testing.T) {
	r := doDelete(t, baseFlags(), "", false, &fakeRunner{}, delPolicy(false), delPool())
	if r.err == nil {
		t.Fatal("want a refusal")
	}
	for _, want := range []string{"still backs up", "remove the label", "delete the policy"} {
		if !strings.Contains(r.err.Error(), want) {
			t.Errorf("error lacks %q: %v", want, r.err)
		}
	}
	if len(r.runner.measured)+len(r.runner.removed) != 0 {
		t.Errorf("a Job was started")
	}
}

func TestDelete_RefusesWhileARestoreOfTheVolumeRuns(t *testing.T) {
	for _, phase := range []string{"", "Pending", "Provisioning", "Restoring"} {
		r := doDelete(t, baseFlags(), "", false, &fakeRunner{}, delPolicy(true), delPool(), delRestore("r1", phase, "web-root"))
		if r.err == nil || !strings.Contains(r.err.Error(), "restore") {
			t.Errorf("phase %q: err = %v, want a refusal that names the restore", phase, r.err)
		}
		if len(r.runner.removed) != 0 {
			t.Errorf("phase %q: removed %v", phase, r.runner.removed)
		}
	}
}

func TestDelete_IgnoresFinishedRestoresAndRestoresOfOtherVolumes(t *testing.T) {
	r := doDelete(t, baseFlags(), "", false, &fakeRunner{bytes: 10}, delPolicy(true), delPool(),
		delRestore("done", "Done", "web-root"), delRestore("failed", "Failed", "web-root"), delRestore("other", "Restoring", "other"))
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.runner.removed) != 2 {
		t.Errorf("removed %v, want both folders", r.runner.removed)
	}
}

func TestDelete_NothingOnTheShareMeansNothingToRemove(t *testing.T) {
	r := doDelete(t, baseFlags(), "", false, &fakeRunner{bytes: 0}, delPolicy(true), delPool())
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.runner.removed) != 0 {
		t.Errorf("removed %v from folders that are empty or missing", r.runner.removed)
	}
	if !strings.Contains(r.out.String(), "Nothing to remove") {
		t.Errorf("output: %s", r.out.String())
	}
}

func TestDelete_RefusesBadRequests(t *testing.T) {
	cases := map[string]struct {
		flags   map[string]string
		objs    []client.Object
		wantErr string
	}{
		"no policy":     {map[string]string{"pvc": "vms/web-root", "yes": "true"}, nil, "--policy is required"},
		"no pvc":        {map[string]string{"policy": "nightly", "yes": "true"}, nil, "--pvc is required"},
		"bad pvc":       {map[string]string{"policy": "nightly", "pvc": "web-root", "yes": "true"}, nil, "NAMESPACE/NAME"},
		"unknown pvc":   {map[string]string{"policy": "nightly", "pvc": "vms/nope", "yes": "true"}, []client.Object{delPolicy(true), delPool()}, "no backup of vms/nope"},
		"missing pool":  {baseFlags(), []client.Object{delPolicy(true)}, `"backups"`},
		"pool no share": {baseFlags(), []client.Object{delPolicy(true), &v1alpha1.FileSharePool{ObjectMeta: metav1.ObjectMeta{Name: "backups"}}}, "share"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := doDelete(t, tc.flags, "", false, &fakeRunner{}, tc.objs...)
			if r.err == nil || !strings.Contains(r.err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to contain %q", r.err, tc.wantErr)
			}
			if len(r.runner.measured)+len(r.runner.removed) != 0 {
				t.Errorf("a Job was started")
			}
		})
	}
}

func TestDelete_PolicyGoneNeedsPoolAndVolume(t *testing.T) {
	flags := baseFlags()
	r := doDelete(t, flags, "", false, &fakeRunner{}, delPool())
	if r.err == nil || !strings.Contains(r.err.Error(), "--pool and --volume") {
		t.Fatalf("err = %v, want it to ask for --pool and --volume", r.err)
	}

	flags["pool"], flags["volume"] = "backups", delVolume
	r = doDelete(t, flags, "", false, &fakeRunner{bytes: 10}, delPool())
	if r.err != nil {
		t.Fatal(r.err)
	}
	if got := strings.Join(r.runner.removed, " "); got != delBackup+" "+delMoments {
		t.Errorf("removed %q", r.runner.removed)
	}
}

func TestDelete_PolicyGoneStillRefusesWhileARestoreRuns(t *testing.T) {
	flags := baseFlags()
	flags["pool"], flags["volume"] = "backups", delVolume
	r := doDelete(t, flags, "", false, &fakeRunner{bytes: 10}, delPool(), delRestore("r1", "Restoring", "web-root"))
	if r.err == nil || len(r.runner.removed) != 0 {
		t.Fatalf("err = %v, removed %v; want a refusal", r.err, r.runner.removed)
	}
}

func TestDelete_VolumeFlagIsCheckedAgainstTheBackupPathPattern(t *testing.T) {
	flags := baseFlags()
	flags["pool"], flags["volume"] = "backups", "../../etc"
	r := doDelete(t, flags, "", false, &fakeRunner{bytes: 10}, delPool())
	if r.err == nil || len(r.runner.removed) != 0 {
		t.Fatalf("err = %v, removed %v; want a refusal", r.err, r.runner.removed)
	}
}

func TestDelete_APolicyNameThatIsNotAFolderNameIsRefused(t *testing.T) {
	flags := baseFlags()
	flags["policy"], flags["pool"], flags["volume"] = "../x", "backups", delVolume
	r := doDelete(t, flags, "", false, &fakeRunner{bytes: 10}, delPool())
	if r.err == nil || len(r.runner.removed) != 0 {
		t.Fatalf("err = %v, removed %v; want a refusal", r.err, r.runner.removed)
	}
}

func TestDelete_WaitsForTheMeasuringJob(t *testing.T) {
	r := doDelete(t, baseFlags(), "", false, &fakeRunner{bytes: 1 << 30, pending: 2}, delPolicy(true), delPool())
	if r.err != nil {
		t.Fatal(r.err)
	}
	if len(r.runner.removed) != 2 {
		t.Errorf("removed %v", r.runner.removed)
	}
}

func TestDelete_JobFailuresAreReported(t *testing.T) {
	r := doDelete(t, baseFlags(), "", false, &fakeRunner{measureErr: errors.New("boom")}, delPolicy(true), delPool())
	if r.err == nil || !strings.Contains(r.err.Error(), "boom") {
		t.Errorf("measure: err = %v", r.err)
	}
	r = doDelete(t, baseFlags(), "", false, &fakeRunner{bytes: 10, removeErr: errors.New("bang")}, delPolicy(true), delPool())
	if r.err == nil || !strings.Contains(r.err.Error(), "bang") {
		t.Errorf("remove: err = %v", r.err)
	}
}

func TestDelete_TimesOut(t *testing.T) {
	flags := baseFlags()
	flags["timeout"] = "5ms"
	r := doDelete(t, flags, "", false, &fakeRunner{bytes: 10, pending: 1 << 30}, delPolicy(true), delPool())
	if r.err == nil || !strings.Contains(r.err.Error(), "timed out") {
		t.Errorf("err = %v", r.err)
	}
}

func TestParseFlags_YesTakesNoValue(t *testing.T) {
	flags := parseFlags([]string{"--yes", "extra", "--pvc", "vms/web-root"})
	if flags["yes"] != "true" || flags["pvc"] != "vms/web-root" {
		t.Errorf("flags = %v", flags)
	}
}

func TestRun_DoesNotKnowDeleteSoItCannotRunWithoutARunner(t *testing.T) {
	err := run(context.Background(), newClient(), &bytes.Buffer{}, []string{"delete"}, time.Now)
	if err == nil {
		t.Fatal("run must not handle delete: it has no runner")
	}
}
