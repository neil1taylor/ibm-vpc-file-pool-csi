package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	corev1 "k8s.io/api/core/v1"
)

func guestFileRestoreRequest(mode string) FileRestoreRequest {
	req := fileRestoreRequest(mode)
	req.MomentsPath = ""
	req.Guest = &FileRestoreGuest{Consistency: "CrashConsistent"}
	return req
}

func TestRunFileRestore_GuestNeedsItsImage(t *testing.T) {
	r, _ := newApplyRunner()
	if _, _, err := r.RunFileRestore(context.Background(), guestFileRestoreRequest(filerestore.ModeFolder)); err == nil || !strings.Contains(err.Error(), "guest") {
		t.Fatalf("err = %v", err)
	}
}

func TestRunFileRestore_GuestJobShape(t *testing.T) {
	r, c := newApplyRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{KVM: true})
	if _, _, err := r.RunFileRestore(context.Background(), guestFileRestoreRequest(filerestore.ModeFolder)); err != nil {
		t.Fatal(err)
	}
	job := fileRestoreJob(t, c)
	pod := job.Spec.Template.Spec
	ctr := pod.Containers[0]
	if ctr.Image != testGuestImage {
		t.Errorf("image = %q: the driver image has no libguestfs", ctr.Image)
	}
	if ctr.Command[0] != "/usr/local/bin/browse" || ctr.Command[1] != "--mode=restore-files" {
		t.Errorf("command %v", ctr.Command)
	}
	for _, a := range ctr.Command {
		if strings.Contains(a, "alice") || strings.Contains(a, "index.php") {
			t.Errorf("a path reached the command line: %q", a)
		}
	}
	spec := jobSpecOf(t, job)
	if spec.Guest == nil || spec.Guest.Socket != "/tmp/disk.sock" || spec.Guest.Appliance != "/opt/guestfs-appliance" || spec.Guest.Consistency != "CrashConsistent" {
		t.Errorf("guest = %+v", spec.Guest)
	}
	if spec.Latest != "/src" || spec.Moments != "" || spec.COS != nil {
		t.Errorf("spec = %+v", spec)
	}
	mounts := map[string]corev1.VolumeMount{}
	for _, m := range ctr.VolumeMounts {
		mounts[m.MountPath] = m
	}
	if !mounts["/src"].ReadOnly || mounts["/src"].SubPath == "" {
		t.Errorf("/src = %+v: the backup must be read-only and below a sub path", mounts["/src"])
	}
	if mounts["/dst"].ReadOnly || mounts["/tmp"].Name == "" || mounts["/var/tmp"].Name == "" {
		t.Errorf("mounts = %+v", mounts)
	}
	sc := ctr.SecurityContext
	if sc == nil || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem ||
		len(sc.Capabilities.Add) != 2 || sc.Capabilities.Add[0] != "CHOWN" || sc.Capabilities.Add[1] != "FOWNER" {
		t.Errorf("security context %+v", sc)
	}
	if _, ok := ctr.Resources.Limits[kvmResource]; !ok {
		t.Errorf("limits = %v: the KVM device was configured", ctr.Resources.Limits)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the pod parses a hostile guest disk: it must not get a service account token")
	}
	if pod.HostNetwork || pod.HostPID || pod.HostIPC {
		t.Error("no host namespaces")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64((2*time.Hour).Seconds()) {
		t.Errorf("deadline %v, want 2 hours: the disk is held for the length of the Job", job.Spec.ActiveDeadlineSeconds)
	}
}

func TestRunFileRestore_GuestMeasureJobOnlyReads(t *testing.T) {
	r, c := newApplyRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	if _, _, err := r.RunFileRestore(context.Background(), guestFileRestoreRequest(filerestore.ModeMeasure)); err != nil {
		t.Fatal(err)
	}
	ctr := fileRestoreJob(t, c).Spec.Template.Spec.Containers[0]
	for _, m := range ctr.VolumeMounts {
		if m.MountPath == "/dst" {
			t.Error("a measuring Job must not mount the target")
		}
	}
	if sc := ctr.SecurityContext; len(sc.Capabilities.Add) != 0 {
		t.Errorf("capabilities %v", sc.Capabilities.Add)
	}
	if spec := jobSpecOf(t, fileRestoreJob(t, c)); spec.Guest == nil {
		t.Error("the measuring Job must read the guest too")
	}
}

func TestRunFileRestore_GuestRefusesWhatItCannotCheck(t *testing.T) {
	cases := map[string]func(*FileRestoreRequest){
		"moments folder": func(r *FileRestoreRequest) { r.MomentsPath = testBackupMoments },
		"COS": func(r *FileRestoreRequest) {
			r.COS = &FileRestoreCOS{Repo: testExportRepo(), Policy: "p", Volume: testExportVolume}
		},
		"bad consistency": func(r *FileRestoreRequest) { r.Guest.Consistency = "Maybe; echo" },
	}
	for name, mutate := range cases {
		r, _ := newApplyRunner()
		r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
		req := guestFileRestoreRequest(filerestore.ModeFolder)
		mutate(&req)
		if _, _, err := r.RunFileRestore(context.Background(), req); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRunFileRestore_GuestDeadlineIsCappedByWhatIsLeft(t *testing.T) {
	for _, mode := range []string{filerestore.ModeMeasure, filerestore.ModeFolder} {
		r, c := newApplyRunner()
		r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
		req := guestFileRestoreRequest(mode)
		req.MaxRun = 30 * time.Minute
		if _, _, err := r.RunFileRestore(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		d := fileRestoreJob(t, c).Spec.ActiveDeadlineSeconds
		if d == nil || *d != int64((30*time.Minute).Seconds()) {
			t.Errorf("%s: deadline %v, want 30 minutes", mode, d)
		}
	}
}
