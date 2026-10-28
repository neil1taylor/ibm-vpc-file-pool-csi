package pool

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const hostilePath = `/home/alice/$(touch /x); 'echo pwned' --flag`

func fileRestoreRequest(mode string) FileRestoreRequest {
	uid, gid := int64(107), int64(107)
	req := FileRestoreRequest{
		Key:    "filerestore-uid-1",
		Owner:  metav1.OwnerReference{APIVersion: "storage.ibmcloud.io/v1alpha1", Kind: "BackupFileRestore", Name: "f1", UID: "uid-1"},
		Mode:   mode,
		Backup: testShareB, LatestPath: testBackupVol, MomentsPath: testBackupMoments,
		At:     time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC),
		Paths:  []string{"/var/www/index.php", hostilePath},
		Policy: "nightly", MaxBytes: 1 << 30,
	}
	if mode != filerestore.ModeMeasure {
		req.Dest, req.Volume, req.CapacityBytes = testShareA, testVolPath, 5<<30
		req.UID, req.GID, req.Permissions = &uid, &gid, "0777"
	}
	return req
}

func fileRestoreJob(t *testing.T, c client.Client) *batchv1.Job {
	t.Helper()
	return getShareJob(t, c, "filerestore-uid-1")
}

func jobSpecOf(t *testing.T, job *batchv1.Job) filerestore.JobSpec {
	t.Helper()
	var spec filerestore.JobSpec
	for _, a := range job.Spec.Template.Spec.Containers[0].Command {
		if v, ok := strings.CutPrefix(a, "--restore-files-spec="); ok {
			if err := json.Unmarshal([]byte(v), &spec); err != nil {
				t.Fatal(err)
			}
			return spec
		}
	}
	t.Fatal("no --restore-files-spec argument")
	return spec
}

func TestRunFileRestore_FolderJobShape(t *testing.T) {
	r, c := newApplyRunner()
	ctx := context.Background()
	done, _, err := r.RunFileRestore(ctx, fileRestoreRequest(filerestore.ModeFolder))
	if err != nil || done {
		t.Fatalf("RunFileRestore() = %v, %v; want false, nil", done, err)
	}
	job := fileRestoreJob(t, c)
	pod := job.Spec.Template.Spec
	ctr := pod.Containers[0]
	cmd := ctr.Command
	if cmd[0] != "/usr/local/bin/vpc-file-pool-csi" || cmd[1] != "--mode=restore-files" {
		t.Errorf("command %v", cmd)
	}
	// The command runs the program itself: no shell, and no path in it.
	for _, a := range cmd {
		if a == "/bin/sh" || a == "/bin/bash" || a == "-c" {
			t.Errorf("the command uses a shell: %v", cmd)
		}
		if strings.Contains(a, "alice") || strings.Contains(a, "index.php") {
			t.Errorf("a path reached the command line: %q", a)
		}
	}
	spec := jobSpecOf(t, job)
	want := filerestore.JobSpec{Mode: filerestore.ModeFolder, Latest: "/src", Moments: "/moments", At: "2026-10-21T09:30:00Z",
		Dest: "/dst", Volume: "pvcs/pvc-11111111-2222-3333-4444-555555555555", Policy: "nightly",
		CapacityBytes: 5 << 30, MaxBytes: 1 << 30, Permissions: "0777"}
	if spec.Staging != filerestore.StagingName("uid-1") {
		t.Errorf("staging %q: it must be named by the UID of the restore", spec.Staging)
	}
	want.Staging = spec.Staging
	want.UID, want.GID = spec.UID, spec.GID
	if spec != want {
		t.Errorf("spec\n got %+v\nwant %+v", spec, want)
	}
	if spec.UID == nil || *spec.UID != 107 || *spec.GID != 107 {
		t.Errorf("owner %v %v", spec.UID, spec.GID)
	}

	// The paths are in a ConfigMap that the restore owns.
	cm := &corev1.ConfigMap{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: testDriverNS, Name: shareJobName("filerestore-uid-1") + "-paths"}, cm); err != nil {
		t.Fatalf("ConfigMap: %v", err)
	}
	if got := cm.Data["paths"]; got != "/var/www/index.php\n"+hostilePath+"\n" {
		t.Errorf("paths %q", got)
	}
	if len(cm.OwnerReferences) != 1 || cm.OwnerReferences[0].UID != "uid-1" || cm.OwnerReferences[0].BlockOwnerDeletion != nil {
		t.Errorf("owner references %+v", cm.OwnerReferences)
	}
	if len(job.OwnerReferences) != 1 || job.OwnerReferences[0].UID != "uid-1" {
		t.Errorf("Job owner references %+v", job.OwnerReferences)
	}

	// Mounts: the backup read-only, the share with the claim writable, the paths read-only.
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range ctr.VolumeMounts {
		mounts[m.MountPath] = m
	}
	for _, p := range []string{"/src", "/moments", "/etc/filerestore"} {
		if m, ok := mounts[p]; !ok || !m.ReadOnly {
			t.Errorf("mount %s = %+v, want read-only", p, m)
		}
	}
	if m, ok := mounts["/dst"]; !ok || m.ReadOnly || m.SubPath != "" {
		t.Errorf("/dst = %+v, want the share, writable", m)
	}
	if mounts["/src"].SubPath != strings.TrimPrefix(testBackupVol, "/") {
		t.Errorf("/src subPath %q", mounts["/src"].SubPath)
	}
	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			t.Errorf("host path volume %s", v.Name)
		}
	}

	// Hardening and limits.
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the pod must not get a service account token")
	}
	sc := ctr.SecurityContext
	if sc == nil || sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem ||
		sc.Capabilities == nil || len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" ||
		len(sc.Capabilities.Add) != 2 || sc.Capabilities.Add[0] != "CHOWN" || sc.Capabilities.Add[1] != "FOWNER" {
		t.Errorf("security context %+v: it needs CHOWN and FOWNER to set owner and mode, and nothing else", sc)
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64((4*time.Hour).Seconds()) {
		t.Errorf("deadline %v, want 4 hours", job.Spec.ActiveDeadlineSeconds)
	}
	if pod.RestartPolicy != corev1.RestartPolicyOnFailure || job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != shareJobBackoffLimit {
		t.Errorf("restart %v backoff %v", pod.RestartPolicy, job.Spec.BackoffLimit)
	}
}

func TestRunFileRestore_MeasureJobOnlyReads(t *testing.T) {
	r, c := newApplyRunner()
	if _, _, err := r.RunFileRestore(context.Background(), fileRestoreRequest(filerestore.ModeMeasure)); err != nil {
		t.Fatal(err)
	}
	job := fileRestoreJob(t, c)
	ctr := job.Spec.Template.Spec.Containers[0]
	for _, m := range ctr.VolumeMounts {
		if !m.ReadOnly {
			t.Errorf("mount %s is writable", m.MountPath)
		}
		if m.MountPath == "/dst" {
			t.Error("a measure Job mounted the target")
		}
	}
	if sc := ctr.SecurityContext; sc == nil || len(sc.Capabilities.Add) != 0 {
		t.Errorf("security context %+v: a measure Job needs no capability", sc)
	}
	if spec := jobSpecOf(t, job); spec.Mode != filerestore.ModeMeasure || spec.Dest != "" {
		t.Errorf("spec %+v", spec)
	}
}

func TestRunFileRestore_LatestAloneAndNoMomentsFolder(t *testing.T) {
	r, c := newApplyRunner()
	req := fileRestoreRequest(filerestore.ModeNewClaim)
	req.MomentsPath, req.At = "", time.Time{}
	if _, _, err := r.RunFileRestore(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	spec := jobSpecOf(t, fileRestoreJob(t, c))
	if spec.Moments != "" || spec.At != "" || spec.Mode != filerestore.ModeNewClaim || spec.Staging != "" {
		t.Errorf("spec %+v", spec)
	}
	for _, m := range fileRestoreJob(t, c).Spec.Template.Spec.Containers[0].VolumeMounts {
		if m.MountPath == "/moments" {
			t.Error("a moments folder that is not there was mounted")
		}
	}
}

func TestRunFileRestore_SecondCallDoesNotMakeAnotherJob(t *testing.T) {
	r, c := newApplyRunner()
	for i := 0; i < 2; i++ {
		if done, _, err := r.RunFileRestore(context.Background(), fileRestoreRequest(filerestore.ModeFolder)); err != nil || done {
			t.Fatalf("call %d = %v, %v", i, done, err)
		}
	}
	jobs := &batchv1.JobList{}
	if err := c.List(context.Background(), jobs); err != nil || len(jobs.Items) != 1 {
		t.Fatalf("jobs %d %v", len(jobs.Items), err)
	}
}

func TestRunFileRestore_SuccessReturnsTheOutcome(t *testing.T) {
	r, c := newApplyRunner()
	ctx := context.Background()
	req := fileRestoreRequest(filerestore.ModeFolder)
	if _, _, err := r.RunFileRestore(ctx, req); err != nil {
		t.Fatal(err)
	}
	setJobStatus(t, c, "filerestore-uid-1", 1, 0)
	addTerminationMessage(t, c, "filerestore-uid-1", filerestore.EncodeOutcome(filerestore.Outcome{
		Result: filerestore.Result{Files: 3, Bytes: 99}, Folder: "restored-nightly-x", Moment: "2026-10-21T09:00:00Z"}))
	done, out, err := r.RunFileRestore(ctx, req)
	if err != nil || !done || out.Files != 3 || out.Folder != "restored-nightly-x" || out.Moment != "2026-10-21T09:00:00Z" {
		t.Fatalf("= %v %+v %v", done, out, err)
	}
	if countPVs(t, c) != 0 {
		t.Error("the share PVs were not removed")
	}
}

// A Job that decided to refuse exits with code 0 and the reason in its message.
// The runner hands the outcome on; the controller reads the reason from it.
func TestRunFileRestore_ARefusalIsADoneJobWithAReason(t *testing.T) {
	r, c := newApplyRunner()
	ctx := context.Background()
	req := fileRestoreRequest(filerestore.ModeFolder)
	if _, _, err := r.RunFileRestore(ctx, req); err != nil {
		t.Fatal(err)
	}
	setJobStatus(t, c, "filerestore-uid-1", 1, 0)
	addTerminationMessage(t, c, "filerestore-uid-1", filerestore.EncodeOutcome(filerestore.Outcome{Error: "this claim holds a VM disk"}))
	done, out, err := r.RunFileRestore(ctx, req)
	if err != nil || !done || out.Error != "this claim holds a VM disk" {
		t.Fatalf("= %v %+v %v", done, out, err)
	}
}

func TestRunFileRestore_FailureCarriesTheReasonTheJobWrote(t *testing.T) {
	r, c := newApplyRunner()
	ctx := context.Background()
	req := fileRestoreRequest(filerestore.ModeFolder)
	if _, _, err := r.RunFileRestore(ctx, req); err != nil {
		t.Fatal(err)
	}
	setJobStatus(t, c, "filerestore-uid-1", 0, shareJobBackoffLimit+1)
	addTerminationMessage(t, c, "filerestore-uid-1", filerestore.EncodeOutcome(filerestore.Outcome{Error: "not enough space"}))
	_, _, err := r.RunFileRestore(ctx, req)
	if !errors.Is(err, ErrShareJobFailed) || fileRestoreFailureReason(err) != "not enough space" {
		t.Fatalf("err %v reason %q", err, fileRestoreFailureReason(err))
	}
}

func TestRunFileRestore_RejectsBadInput(t *testing.T) {
	r, _ := newApplyRunner()
	cases := map[string]func(*FileRestoreRequest){
		"no key":            func(q *FileRestoreRequest) { q.Key = "" },
		"unknown mode":      func(q *FileRestoreRequest) { q.Mode = "wipe" },
		"no paths":          func(q *FileRestoreRequest) { q.Paths = nil },
		"dot dot path":      func(q *FileRestoreRequest) { q.Paths = []string{"/a/../../etc"} },
		"bad backup folder": func(q *FileRestoreRequest) { q.LatestPath = "/pvcs/pvc-11111111-2222-3333-4444-555555555555" },
		"moments of another": func(q *FileRestoreRequest) {
			q.MomentsPath = "/backup/nightly/.moments/pvcs/pvc-66666666-7777-8888-9999-000000000000"
		},
		"bad volume":            func(q *FileRestoreRequest) { q.Volume = "/pvcs/../.." },
		"the whole share":       func(q *FileRestoreRequest) { q.Volume = "/" },
		"bad permissions":       func(q *FileRestoreRequest) { q.Permissions = "9999" },
		"command in the policy": func(q *FileRestoreRequest) { q.Policy = "x; echo pwned" },
	}
	for name, mutate := range cases {
		req := fileRestoreRequest(filerestore.ModeFolder)
		mutate(&req)
		if _, _, err := r.RunFileRestore(context.Background(), req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	noImage, _ := newShareJobTestRunner()
	if _, _, err := noImage.RunFileRestore(context.Background(), fileRestoreRequest(filerestore.ModeFolder)); err == nil {
		t.Error("accepted without a driver image")
	}
}
