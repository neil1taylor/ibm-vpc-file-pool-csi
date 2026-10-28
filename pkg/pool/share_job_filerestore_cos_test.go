package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	corev1 "k8s.io/api/core/v1"
)

func cosFileRestoreRequest(mode string) FileRestoreRequest {
	req := fileRestoreRequest(mode)
	req.Backup, req.LatestPath, req.MomentsPath = ShareMount{}, "", ""
	req.COS = &FileRestoreCOS{Repo: testExportRepo(), Policy: "policy-nightly", Volume: testExportVolume}
	return req
}

func TestRunFileRestoreCOS_WritingJobMountsOnlyTheTargetShare(t *testing.T) {
	r, c := newApplyRunner()
	if done, _, err := r.RunFileRestore(context.Background(), cosFileRestoreRequest(filerestore.ModeFolder)); err != nil || done {
		t.Fatalf("RunFileRestore() = %v, %v", done, err)
	}
	job := fileRestoreJob(t, c)
	pod := job.Spec.Template.Spec
	ctr := pod.Containers[0]
	if ctr.Image != "de.icr.io/test/driver:v1" {
		t.Errorf("image = %q: only the driver image holds the restore program and Kopia", ctr.Image)
	}
	spec := jobSpecOf(t, job)
	if spec.COS == nil || spec.COS.Volume != testExportVolume || spec.COS.Policy != "policy-nightly" || spec.Latest != "" || spec.Moments != "" ||
		spec.Dest != "/dst" || spec.Mode != filerestore.ModeFolder || spec.At != "2026-10-21T09:30:00Z" {
		t.Errorf("spec = %+v", spec)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range ctr.VolumeMounts {
		mounts[m.MountPath] = m
	}
	if m, ok := mounts["/dst"]; !ok || m.ReadOnly {
		t.Errorf("/dst = %+v, want the target share, writable", m)
	}
	for _, p := range []string{"/src", "/moments"} {
		if _, ok := mounts[p]; ok {
			t.Errorf("%s is mounted though the backup is in COS", p)
		}
	}
	for _, p := range []string{cosbrowse.CredentialsDir, cosbrowse.WrappedKeyDir, cosbrowse.APIKeyDir, "/etc/filerestore"} {
		if m, ok := mounts[p]; !ok || !m.ReadOnly {
			t.Errorf("%s must be mounted read-only: %+v", p, m)
		}
	}
	if m := mounts[cosbrowse.DefaultStateDir]; m.Name != cosStateVolume {
		t.Errorf("Kopia's folder = %+v", m)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("a Job that holds the backup key must not get a service account token")
	}
	sc := ctr.SecurityContext
	if sc == nil || sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem || len(sc.Capabilities.Add) != 2 {
		t.Errorf("security context = %+v", sc)
	}
	if len(ctr.Env) != 0 {
		t.Errorf("environment = %v: the keys are files", ctr.Env)
	}
	cmd := strings.Join(ctr.Command, " ")
	for _, secret := range []string{"cos-credentials", "cos-backup-key", "kp-apikey", "alice"} {
		if strings.Contains(cmd, secret) {
			t.Errorf("%q is in the command", secret)
		}
	}
	for _, v := range pod.Volumes {
		if v.HostPath != nil {
			t.Errorf("host path volume %s", v.Name)
		}
	}
}

func TestRunFileRestoreCOS_MeasureJobMountsNoShareAtAll(t *testing.T) {
	r, c := newApplyRunner()
	if _, _, err := r.RunFileRestore(context.Background(), cosFileRestoreRequest(filerestore.ModeMeasure)); err != nil {
		t.Fatal(err)
	}
	pod := fileRestoreJob(t, c).Spec.Template.Spec
	ctr := pod.Containers[0]
	for _, v := range pod.Volumes {
		if v.PersistentVolumeClaim != nil || v.HostPath != nil {
			t.Errorf("volume %s mounts storage", v.Name)
		}
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("no service account token")
	}
	if pod.SecurityContext == nil || pod.SecurityContext.RunAsUser == nil || *pod.SecurityContext.RunAsUser == 0 {
		t.Errorf("a measure Job that reads COS needs no root: %+v", pod.SecurityContext)
	}
	if sc := ctr.SecurityContext; sc == nil || len(sc.Capabilities.Add) != 0 {
		t.Errorf("security context = %+v", sc)
	}
	pvs := &corev1.PersistentVolumeList{}
	_ = c.List(context.Background(), pvs)
	if len(pvs.Items) != 0 {
		t.Errorf("%d share PVs were made", len(pvs.Items))
	}
}

func TestRunFileRestoreCOS_RefusesWhatItCannotCheck(t *testing.T) {
	cases := map[string]func(*FileRestoreRequest){
		"a backup folder with a COS source": func(r *FileRestoreRequest) { r.LatestPath = testBackupVol },
		"a bad endpoint":                    func(r *FileRestoreRequest) { r.COS.Repo.Endpoint = "s3.example; ls" },
		"a bad volume":                      func(r *FileRestoreRequest) { r.COS.Volume = "../x" },
		"a bad Secret name":                 func(r *FileRestoreRequest) { r.COS.Repo.CredentialsSecret = "A B" },
		"a traversing prefix":               func(r *FileRestoreRequest) { r.COS.Repo.Prefix = "a/../b/" },
	}
	for name, mutate := range cases {
		r, c := newApplyRunner()
		req := cosFileRestoreRequest(filerestore.ModeFolder)
		req.At = time.Time{}
		mutate(&req)
		if _, _, err := r.RunFileRestore(context.Background(), req); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if n := batchJobs(t, c); n != 0 {
			t.Errorf("%s: %d Jobs were made", name, n)
		}
	}
}
