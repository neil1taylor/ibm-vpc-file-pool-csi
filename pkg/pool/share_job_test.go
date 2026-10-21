package pool

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	testDriverNS = "kube-system"
	testVolPath  = "/pvcs/pvc-11111111-2222-3333-4444-555555555555"
	testVolPath2 = "/pvcs/pvc-66666666-7777-8888-9999-000000000000"
	testSnapPath = "/pvcs/.snapshots/snap-snapshot-abc"
)

var (
	testShareA = ShareMount{MountTargetIP: "10.240.0.1", ExportPath: "/share_a"}
	testShareB = ShareMount{MountTargetIP: "10.240.0.2", ExportPath: "/share_b"}
)

func newShareJobTestRunner() (*ShareJobRunner, client.Client) {
	c := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).WithStatusSubresource(&batchv1.Job{}).Build()
	return NewShareJobRunner(c, testDriverNS, DefaultCloneImage), c
}

func getShareJob(t *testing.T, c client.Client, key string) *batchv1.Job {
	t.Helper()
	job := &batchv1.Job{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testDriverNS, Name: shareJobName(key)}, job); err != nil {
		t.Fatalf("get share Job: %v", err)
	}
	return job
}

func jobScript(job *batchv1.Job) string {
	return strings.Join(job.Spec.Template.Spec.Containers[0].Command, " ")
}

func setJobStatus(t *testing.T, c client.Client, key string, succeeded, failed int32) {
	t.Helper()
	job := getShareJob(t, c, key)
	job.Status.Succeeded = succeeded
	job.Status.Failed = failed
	if err := c.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("update Job status: %v", err)
	}
}

func countPVs(t *testing.T, c client.Client) int {
	t.Helper()
	pvs := &corev1.PersistentVolumeList{}
	if err := c.List(context.Background(), pvs); err != nil {
		t.Fatalf("list PVs: %v", err)
	}
	return len(pvs.Items)
}

func TestShareJobRunner_RemoveDir_FirstCallStartsJob(t *testing.T) {
	r, c := newShareJobTestRunner()

	done, err := r.RemoveDir(context.Background(), "rm-pvc-1", testShareA, testVolPath)
	if err != nil || done {
		t.Fatalf("RemoveDir() = %v, %v; want false, nil", done, err)
	}

	job := getShareJob(t, c, "rm-pvc-1")
	if !strings.Contains(jobScript(job), `rm -r -f -- "/share0`+testVolPath+`"`) {
		t.Errorf("script does not remove the volume folder:\n%s", jobScript(job))
	}
	spec := job.Spec.Template.Spec
	if spec.SecurityContext == nil || spec.SecurityContext.RunAsUser == nil || *spec.SecurityContext.RunAsUser != 0 {
		t.Errorf("share Job must run as root")
	}
	if len(spec.Volumes) != 1 || spec.Volumes[0].PersistentVolumeClaim == nil {
		t.Fatalf("want one claim volume, got %+v", spec.Volumes)
	}

	pvc := &corev1.PersistentVolumeClaim{}
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testDriverNS, Name: spec.Volumes[0].PersistentVolumeClaim.ClaimName}, pvc); err != nil {
		t.Fatalf("share claim not created: %v", err)
	}
	pv := &corev1.PersistentVolume{}
	if err := c.Get(context.Background(), types.NamespacedName{Name: pvc.Spec.VolumeName}, pv); err != nil {
		t.Fatalf("share PV not created: %v", err)
	}
	if pv.Spec.NFS == nil || pv.Spec.NFS.Server != testShareA.MountTargetIP || pv.Spec.NFS.Path != testShareA.ExportPath {
		t.Errorf("PV mounts %+v, want %+v", pv.Spec.NFS, testShareA)
	}
	if !slices.Contains(pv.Spec.MountOptions, "sec=sys") || !slices.Contains(pv.Spec.MountOptions, "soft") {
		t.Errorf("PV mount options %v must include sec=sys and soft", pv.Spec.MountOptions)
	}
	if pv.Spec.PersistentVolumeReclaimPolicy != corev1.PersistentVolumeReclaimRetain {
		t.Errorf("share PV reclaim policy = %s, want Retain (it must never delete the share)", pv.Spec.PersistentVolumeReclaimPolicy)
	}
}

func TestShareJobRunner_RemoveDir_RunningJobIsNotDone(t *testing.T) {
	r, c := newShareJobTestRunner()
	ctx := context.Background()
	_, _ = r.RemoveDir(ctx, "rm-pvc-1", testShareA, testVolPath)

	done, err := r.RemoveDir(ctx, "rm-pvc-1", testShareA, testVolPath)
	if err != nil || done {
		t.Fatalf("RemoveDir() = %v, %v; want false, nil", done, err)
	}
	getShareJob(t, c, "rm-pvc-1")
}

func TestShareJobRunner_RemoveDir_SucceededJobIsDoneAndCleanedUp(t *testing.T) {
	r, c := newShareJobTestRunner()
	ctx := context.Background()
	_, _ = r.RemoveDir(ctx, "rm-pvc-1", testShareA, testVolPath)
	setJobStatus(t, c, "rm-pvc-1", 1, 0)

	done, err := r.RemoveDir(ctx, "rm-pvc-1", testShareA, testVolPath)
	if err != nil || !done {
		t.Fatalf("RemoveDir() = %v, %v; want true, nil", done, err)
	}
	err = c.Get(ctx, types.NamespacedName{Namespace: testDriverNS, Name: shareJobName("rm-pvc-1")}, &batchv1.Job{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Job not removed after success: %v", err)
	}
	if n := countPVs(t, c); n != 0 {
		t.Errorf("%d share PVs left after success", n)
	}
}

func TestShareJobRunner_RemoveDir_FailedJobReportsErrorAndCleansUp(t *testing.T) {
	r, c := newShareJobTestRunner()
	ctx := context.Background()
	_, _ = r.RemoveDir(ctx, "rm-pvc-1", testShareA, testVolPath)
	setJobStatus(t, c, "rm-pvc-1", 0, 4)

	done, err := r.RemoveDir(ctx, "rm-pvc-1", testShareA, testVolPath)
	if done || !errors.Is(err, ErrShareJobFailed) {
		t.Fatalf("RemoveDir() = %v, %v; want false, ErrShareJobFailed", done, err)
	}
	if n := countPVs(t, c); n != 0 {
		t.Errorf("%d share PVs left after failure", n)
	}

	// The next call starts a fresh Job.
	done, err = r.RemoveDir(ctx, "rm-pvc-1", testShareA, testVolPath)
	if err != nil || done {
		t.Fatalf("retry RemoveDir() = %v, %v; want false, nil", done, err)
	}
	if job := getShareJob(t, c, "rm-pvc-1"); job.Status.Failed != 0 {
		t.Errorf("retry reused the failed Job")
	}
}

func TestShareJobRunner_RejectsUnsafePaths(t *testing.T) {
	r, c := newShareJobTestRunner()
	ctx := context.Background()
	for _, p := range []string{"/", "/pvcs", "/pvcs/../etc", "/pvcs/pvc-x; rm -r -f /", ""} {
		if _, err := r.RemoveDir(ctx, "rm-bad", testShareA, p); err == nil {
			t.Errorf("RemoveDir(%q) accepted an unsafe path", p)
		}
		if _, err := r.CopyDir(ctx, "cp-bad", testShareA, p, testShareA, testVolPath, nil); err == nil {
			t.Errorf("CopyDir(src %q) accepted an unsafe path", p)
		}
		if _, err := r.CopyDir(ctx, "cp-bad", testShareA, testVolPath, testShareA, p, nil); err == nil {
			t.Errorf("CopyDir(dst %q) accepted an unsafe path", p)
		}
	}
	jobs := &batchv1.JobList{}
	_ = c.List(ctx, jobs)
	if len(jobs.Items) != 0 {
		t.Errorf("unsafe paths created %d Jobs", len(jobs.Items))
	}
}

func TestShareJobRunner_CopyDir_SameShareMountsOnce(t *testing.T) {
	r, c := newShareJobTestRunner()

	done, err := r.CopyDir(context.Background(), "snap-1", testShareA, testVolPath, testShareA, testSnapPath, nil)
	if err != nil || done {
		t.Fatalf("CopyDir() = %v, %v; want false, nil", done, err)
	}
	job := getShareJob(t, c, "snap-1")
	script := jobScript(job)
	for _, want := range []string{
		`rm -r -f -- "/share0` + testSnapPath + `"`,
		`mkdir -p "/share0` + testSnapPath + `"`,
		`cp -a "/share0` + testVolPath + `/." "/share0` + testSnapPath + `/"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if !strings.Contains(script, `trap 'rm -r -f -- "/share0`+testSnapPath+`"' EXIT`) {
		t.Errorf("a failed copy must remove its partial folder:\n%s", script)
	}
	if !strings.Contains(script, "trap - EXIT") {
		t.Errorf("a finished copy must keep its folder:\n%s", script)
	}
	if strings.Contains(script, "chown") {
		t.Errorf("a copy without an owner must keep the copied owners:\n%s", script)
	}
	if n := countPVs(t, c); n != 1 {
		t.Errorf("same share mounted through %d PVs, want 1", n)
	}
}

func TestShareJobRunner_CopyDir_OtherShareWithOwner(t *testing.T) {
	r, c := newShareJobTestRunner()
	uid, gid := int64(107), int64(107)

	_, err := r.CopyDir(context.Background(), "restore-1", testShareA, testSnapPath, testShareB, testVolPath2,
		&Ownership{UID: &uid, GID: &gid, Permissions: "0777"})
	if err != nil {
		t.Fatalf("CopyDir() error: %v", err)
	}
	job := getShareJob(t, c, "restore-1")
	script := jobScript(job)
	for _, want := range []string{
		`cp -a "/share0` + testSnapPath + `/." "/share1` + testVolPath2 + `/"`,
		`chown 107:107 "/share1` + testVolPath2 + `"`,
		`chmod 0777 "/share1` + testVolPath2 + `"`,
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if n := countPVs(t, c); n != 2 {
		t.Errorf("two shares mounted through %d PVs, want 2", n)
	}
}

func TestShareJobRunner_RejectsBadPermissions(t *testing.T) {
	r, _ := newShareJobTestRunner()
	_, err := r.CopyDir(context.Background(), "restore-1", testShareA, testSnapPath, testShareA, testVolPath,
		&Ownership{Permissions: "0777; reboot"})
	if err == nil {
		t.Fatal("CopyDir accepted bad permissions")
	}
}

func TestShareJobName_FitsKubernetesLimits(t *testing.T) {
	long := "rmsnap-snapshot-0123456789abcdef0123456789abcdef0123"
	name := shareJobName(long)
	if len(name) > 56 {
		t.Errorf("shareJobName length %d > 56: %s", len(name), name)
	}
	if name == shareJobName(long+"x") {
		t.Errorf("different keys gave the same Job name")
	}
	if got := shareJobName("rm-pvc-1"); got != "share-data-rm-pvc-1" {
		t.Errorf("short key name = %q", got)
	}
}

func TestShareJobRunner_Stop(t *testing.T) {
	r, c := newShareJobTestRunner()
	ctx := context.Background()

	done, err := r.Stop(ctx, "snap-1")
	if err != nil || !done {
		t.Fatalf("Stop() with no Job = %v, %v; want true, nil", done, err)
	}

	_, _ = r.CopyDir(ctx, "snap-1", testShareA, testVolPath, testShareA, testSnapPath, nil)
	done, err = r.Stop(ctx, "snap-1")
	if err != nil || done {
		t.Fatalf("Stop() on a running Job = %v, %v; want false, nil", done, err)
	}
	err = c.Get(ctx, types.NamespacedName{Namespace: testDriverNS, Name: shareJobName("snap-1")}, &batchv1.Job{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("Stop() did not delete the Job: %v", err)
	}

	// The Job is gone, but its pod is still shutting down.
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "share-data-snap-1-abcde", Namespace: testDriverNS,
		Labels: map[string]string{shareJobLabel: shareJobName("snap-1")},
	}}
	if createErr := c.Create(ctx, pod); createErr != nil {
		t.Fatalf("create pod: %v", createErr)
	}
	done, err = r.Stop(ctx, "snap-1")
	if err != nil || done {
		t.Fatalf("Stop() while the pod remains = %v, %v; want false, nil", done, err)
	}
	if n := countPVs(t, c); n != 1 {
		t.Errorf("share PV removed while the pod still uses it (%d PVs)", n)
	}

	_ = c.Delete(ctx, pod)
	done, err = r.Stop(ctx, "snap-1")
	if err != nil || !done {
		t.Fatalf("Stop() after the pod is gone = %v, %v; want true, nil", done, err)
	}
	if n := countPVs(t, c); n != 0 {
		t.Errorf("%d share PVs left after Stop", n)
	}
}

func newApplyRunner() (*ShareJobRunner, client.Client) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("de.icr.io/test/driver:v1")
	return r, c
}

func TestShareJobRunner_ApplyVMDisk_FirstCallStartsJob(t *testing.T) {
	r, c := newApplyRunner()
	moment := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	done, err := r.ApplyVMDisk(context.Background(), "snap-a", testShareA, testVolPath, ".repl-staging-abc", testSnapPath, moment)
	if err != nil || done {
		t.Fatalf("ApplyVMDisk() = %v, %v; want false, nil", done, err)
	}
	job := getShareJob(t, c, "snap-a")
	spec := job.Spec.Template.Spec
	if len(spec.InitContainers) != 1 || len(spec.Containers) != 1 {
		t.Fatalf("want 1 init container and 1 container, got %d and %d", len(spec.InitContainers), len(spec.Containers))
	}
	initC, mainC := spec.InitContainers[0], spec.Containers[0]
	if *initC.SecurityContext.RunAsUser != 0 {
		t.Errorf("init container runs as %d, want root", *initC.SecurityContext.RunAsUser)
	}
	if got := strings.Join(initC.Command, " "); !strings.Contains(got, `mkdir -p "/share0`+testSnapPath+`"`) {
		t.Errorf("init script does not make the snapshot folder: %s", got)
	}
	// The Job is safe to run twice: a second run, after the staging file was
	// applied and removed, leaves the finished snapshot alone.
	if got := strings.Join(initC.Command, " "); !strings.Contains(got, `if [ -e "/share0`+testVolPath+`/.repl-staging-abc" ]`) {
		t.Errorf("init script must only clear the folder while the staging file exists: %s", got)
	}
	if got := strings.Join(mainC.Command, " "); !strings.Contains(got, "already applied") {
		t.Errorf("the command must skip an apply that has been done: %s", got)
	}
	// A folder from an earlier try or a discarded file copy is replaced.
	if got := strings.Join(initC.Command, " "); !strings.Contains(got, `rm -r -f -- "/share0`+testSnapPath+`"`) {
		t.Errorf("init script does not clear an old snapshot folder: %s", got)
	}
	if got := *spec.SecurityContext.RunAsGroup; got != qemuUID {
		t.Errorf("pod group = %d, want %d", got, qemuUID)
	}
	if *spec.SecurityContext.RunAsUser != qemuUID {
		t.Errorf("pod runs as %d, want %d", *spec.SecurityContext.RunAsUser, qemuUID)
	}
	if mainC.Image != "de.icr.io/test/driver:v1" {
		t.Errorf("image = %q, want the driver image", mainC.Image)
	}
	cmd := strings.Join(mainC.Command, " ")
	for _, want := range []string{
		"--mode=apply-vm-disk",
		`--staging-path="/share0` + testVolPath + `/.repl-staging-abc"`,
		`--disk-path="/share0` + testSnapPath + `/disk.img"`,
		"--to=2026-10-21T09:30:00Z",
		"--full",
	} {
		if !strings.Contains(cmd, want) {
			t.Errorf("command lacks %q: %s", want, cmd)
		}
	}
	if countPVs(t, c) != 1 {
		t.Errorf("want one share PV")
	}
}

func TestShareJobRunner_ApplyVMDisk_SucceededIsDoneAndCleanedUp(t *testing.T) {
	r, c := newApplyRunner()
	ctx := context.Background()
	moment := time.Now()
	if _, err := r.ApplyVMDisk(ctx, "snap-a", testShareA, testVolPath, ".repl-staging-abc", testSnapPath, moment); err != nil {
		t.Fatal(err)
	}
	setJobStatus(t, c, "snap-a", 1, 0)
	done, err := r.ApplyVMDisk(ctx, "snap-a", testShareA, testVolPath, ".repl-staging-abc", testSnapPath, moment)
	if err != nil || !done {
		t.Fatalf("ApplyVMDisk() = %v, %v; want true, nil", done, err)
	}
	if countPVs(t, c) != 0 {
		t.Errorf("share PV not removed")
	}
}

func TestShareJobRunner_ApplyVMDisk_FailedJobReportsError(t *testing.T) {
	r, c := newApplyRunner()
	ctx := context.Background()
	moment := time.Now()
	if _, err := r.ApplyVMDisk(ctx, "snap-a", testShareA, testVolPath, ".repl-staging-abc", testSnapPath, moment); err != nil {
		t.Fatal(err)
	}
	setJobStatus(t, c, "snap-a", 0, shareJobBackoffLimit+1)
	_, err := r.ApplyVMDisk(ctx, "snap-a", testShareA, testVolPath, ".repl-staging-abc", testSnapPath, moment)
	if !errors.Is(err, ErrShareJobFailed) {
		t.Fatalf("err = %v, want ErrShareJobFailed", err)
	}
}

func TestShareJobRunner_ApplyVMDisk_RejectsBadInput(t *testing.T) {
	r, _ := newApplyRunner()
	ctx := context.Background()
	moment := time.Now()
	cases := []struct{ name, vol, staging, dst string }{
		{"volume path", "/etc", ".repl-staging-abc", testSnapPath},
		{"snapshot path", testVolPath, ".repl-staging-abc", "/etc"},
		{"staging name", testVolPath, "disk.img", testSnapPath},
		{"staging with slash", testVolPath, ".repl-staging-../x", testSnapPath},
	}
	for _, tc := range cases {
		if _, err := r.ApplyVMDisk(ctx, "snap-bad", testShareA, tc.vol, tc.staging, tc.dst, moment); err == nil {
			t.Errorf("%s: want an error", tc.name)
		}
	}
	noImage, _ := newShareJobTestRunner()
	if _, err := noImage.ApplyVMDisk(ctx, "snap-bad", testShareA, testVolPath, ".repl-staging-abc", testSnapPath, moment); err == nil {
		t.Errorf("no driver image: want an error")
	}
}

func TestShareJobRunner_ScriptJobsKeepTheirSecurityContext(t *testing.T) {
	r, c := newShareJobTestRunner()
	if _, err := r.RemoveDir(context.Background(), "rm-a", testShareA, testVolPath); err != nil {
		t.Fatal(err)
	}
	sc := getShareJob(t, c, "rm-a").Spec.Template.Spec.SecurityContext
	if sc.RunAsUser == nil || *sc.RunAsUser != 0 || sc.RunAsGroup != nil {
		t.Errorf("a script Job must run as root and set no group, as before: %+v", sc)
	}
}

func TestShareJobRunner_RunChainScript(t *testing.T) {
	r, c := newApplyRunner()
	ctx := context.Background()
	folders := []ShareFolder{{Share: testShareA, Path: testVolPath}, {Share: testShareA, Path: testSnapPath}}
	if _, err := r.RunChainScript(ctx, "chain-a", folders, "set -eu\nqemu-img info x\n", "mkdir -p /vol1/x\n"); err == nil {
		t.Fatal("without the launcher image the Job must be refused")
	}
	r.SetLauncherImage("registry.redhat.io/cnv/virt-launcher@sha256:abc")
	done, err := r.RunChainScript(ctx, "chain-a", folders, "set -eu\nqemu-img info x\n", "mkdir -p /vol1/x\n")
	if err != nil || done {
		t.Fatalf("RunChainScript() = %v, %v; want false, nil", done, err)
	}
	spec := getShareJob(t, c, "chain-a").Spec.Template.Spec
	mainC := spec.Containers[0]
	if mainC.Image != "registry.redhat.io/cnv/virt-launcher@sha256:abc" || !strings.Contains(strings.Join(mainC.Command, " "), "qemu-img info x") {
		t.Errorf("main container = %s %v", mainC.Image, mainC.Command)
	}
	if *spec.SecurityContext.RunAsUser != qemuUID || *spec.SecurityContext.RunAsGroup != qemuUID {
		t.Errorf("the Job must run as the QEMU user")
	}
	if len(spec.InitContainers) != 1 || *spec.InitContainers[0].SecurityContext.RunAsUser != 0 ||
		!strings.Contains(strings.Join(spec.InitContainers[0].Command, " "), "mkdir -p /vol1/x") {
		t.Errorf("init containers = %+v", spec.InitContainers)
	}
	// Each folder is mounted alone, from the one share volume.
	for _, c := range append(spec.InitContainers, spec.Containers...) {
		got := fmt.Sprint(c.VolumeMounts)
		want := fmt.Sprint([]corev1.VolumeMount{{Name: "share0", MountPath: "/vol0", SubPath: strings.TrimPrefix(testVolPath, "/")},
			{Name: "share0", MountPath: "/vol1", SubPath: strings.TrimPrefix(testSnapPath, "/")}})
		if got != want {
			t.Errorf("%s mounts %s, want %s", c.Name, got, want)
		}
	}
	if len(spec.Volumes) != 1 {
		t.Errorf("one share needs one volume, got %d", len(spec.Volumes))
	}
	if _, badErr := r.RunChainScript(ctx, "chain-b", []ShareFolder{{Share: testShareA, Path: "/pvcs/../etc"}}, "x", ""); badErr == nil {
		t.Error("a bad folder path must be refused")
	}
	setJobStatus(t, c, "chain-a", 1, 0)
	if done, err = r.RunChainScript(ctx, "chain-a", folders, "x", ""); err != nil || !done {
		t.Fatalf("RunChainScript() = %v, %v; want true, nil", done, err)
	}
}
