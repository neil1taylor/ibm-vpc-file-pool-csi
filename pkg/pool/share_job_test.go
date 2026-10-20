package pool

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

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
