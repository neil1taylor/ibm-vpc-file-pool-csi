package pool

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
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
	if mounts["/tmp"].Name == "" || mounts["/var/tmp"].Name == "" {
		t.Errorf("mounts = %+v", mounts)
	}
	// The reader parses the hostile disk: it cannot write the target and holds no capability.
	for _, m := range ctr.VolumeMounts {
		if m.MountPath == "/dst" || strings.HasPrefix(m.Name, "share1") {
			t.Errorf("the reader mounts the target: %+v", m)
		}
	}
	sc := ctr.SecurityContext
	if sc == nil || *sc.AllowPrivilegeEscalation || !*sc.ReadOnlyRootFilesystem || len(sc.Capabilities.Add) != 0 ||
		len(sc.Capabilities.Drop) != 1 || sc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("reader security context %+v", sc)
	}
	if _, ok := ctr.Resources.Limits[kvmResource]; !ok {
		t.Errorf("limits = %v: the KVM device was configured", ctr.Resources.Limits)
	}
	if pod.AutomountServiceAccountToken == nil || *pod.AutomountServiceAccountToken {
		t.Error("the pod parses a hostile guest disk: it must not get a service account token")
	}
	if spec.Writer == "" || spec.Dest != "" || spec.Volume != "" || spec.Staging != "" || spec.Policy != "" {
		t.Errorf("the reader's spec names the writer and no target: %+v", spec)
	}
	if len(pod.Containers) != 2 {
		t.Fatalf("containers = %d, want the reader and the writer", len(pod.Containers))
	}
	w := pod.Containers[1]
	if w.Image != testGuestImage || w.Command[0] != "/usr/local/bin/browse" || w.Command[1] != "--mode=restore-writer" {
		t.Errorf("writer %s %v", w.Image, w.Command)
	}
	wm := map[string]corev1.VolumeMount{}
	for _, m := range w.VolumeMounts {
		wm[m.MountPath] = m
	}
	if wm["/dst"].ReadOnly || wm["/dst"].SubPath == "" || !strings.HasPrefix(wm["/dst"].SubPath, "pvcs/") {
		t.Errorf("the writer's target %+v: only the claim's own folder, never the whole share", wm["/dst"])
	}
	if _, ok := wm["/src"]; ok {
		t.Error("the writer must not see the backup")
	}
	if _, ok := wm["/tmp"]; ok {
		t.Error("the writer shares no scratch folder with the reader")
	}
	if wm[guestSocketDir].Name == "" || mounts[guestSocketDir].Name != wm[guestSocketDir].Name {
		t.Errorf("reader and writer must share the socket folder: %+v / %+v", mounts[guestSocketDir], wm[guestSocketDir])
	}
	wsc := w.SecurityContext
	if wsc == nil || *wsc.AllowPrivilegeEscalation || !*wsc.ReadOnlyRootFilesystem ||
		len(wsc.Capabilities.Add) != 2 || wsc.Capabilities.Add[0] != "CHOWN" || wsc.Capabilities.Add[1] != "FOWNER" ||
		len(wsc.Capabilities.Drop) != 1 || wsc.Capabilities.Drop[0] != "ALL" {
		t.Errorf("writer security context %+v", wsc)
	}
	if _, ok := w.Resources.Limits[kvmResource]; ok {
		t.Error("the writer needs no KVM device")
	}
	if pod.HostNetwork || pod.HostPID || pod.HostIPC {
		t.Error("no host namespaces")
	}
	if job.Spec.ActiveDeadlineSeconds == nil || *job.Spec.ActiveDeadlineSeconds != int64((2*time.Hour).Seconds()) {
		t.Errorf("deadline %v, want 2 hours: the disk is held for the length of the Job", job.Spec.ActiveDeadlineSeconds)
	}
}

func writerSpecOf(t *testing.T, ctr corev1.Container) filerestore.WriterSpec {
	t.Helper()
	var ws filerestore.WriterSpec
	for _, a := range ctr.Command {
		if v, ok := strings.CutPrefix(a, "--restore-writer-spec="); ok {
			if err := json.Unmarshal([]byte(v), &ws); err != nil {
				t.Fatal(err)
			}
			return ws
		}
	}
	t.Fatalf("no writer spec in %v", ctr.Command)
	return ws
}

func TestRunFileRestore_GuestWriterIsToldItsTargetAndOwner(t *testing.T) {
	r, c := newApplyRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	req := guestFileRestoreRequest(filerestore.ModeFolder)
	if _, _, err := r.RunFileRestore(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	ws := writerSpecOf(t, fileRestoreJob(t, c).Spec.Template.Spec.Containers[1])
	if ws.Mode != filerestore.ModeFolder || ws.Dest != "/dst" || ws.Volume != "" || ws.Policy != req.Policy || ws.Staging == "" || ws.Socket != guestSocketDir+"/writer.sock" {
		t.Errorf("writer spec %+v", ws)
	}
}

func TestRunFileRestore_GuestNewClaimWriterNeedsTheShareToMakeTheFolder(t *testing.T) {
	r, c := newApplyRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	if _, _, err := r.RunFileRestore(context.Background(), guestFileRestoreRequest(filerestore.ModeNewClaim)); err != nil {
		t.Fatal(err)
	}
	pod := fileRestoreJob(t, c).Spec.Template.Spec
	ws := writerSpecOf(t, pod.Containers[1])
	if ws.Mode != filerestore.ModeNewClaim || ws.Volume == "" {
		t.Errorf("writer spec %+v: the folder of a new claim may not exist yet, so the writer makes it", ws)
	}
	for _, m := range pod.Containers[0].VolumeMounts {
		if m.MountPath == "/dst" {
			t.Error("the reader mounts the target")
		}
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
	if n := len(fileRestoreJob(t, c).Spec.Template.Spec.Containers); n != 1 {
		t.Errorf("a measuring Job has %d containers: it has no writer", n)
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

func TestRunFileRestore_GuestWriterNeedsAnOwner(t *testing.T) {
	r, _ := newApplyRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	req := guestFileRestoreRequest(filerestore.ModeFolder)
	req.Mode = filerestore.ModeNewClaim
	req.UID, req.GID = nil, nil
	if _, _, err := r.RunFileRestore(context.Background(), req); err == nil || !strings.Contains(err.Error(), "the owner of the target claim is not known") {
		t.Fatalf("err = %v: the writer must not guess an owner", err)
	}
}

func TestRunFileRestore_GuestIntoAnExistingClaimWithoutAnOwnerUsesTheOwnerOfItsFolder(t *testing.T) {
	r, c := newApplyRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	req := guestFileRestoreRequest(filerestore.ModeFolder)
	req.UID, req.GID = nil, nil
	if _, _, err := r.RunFileRestore(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	job := fileRestoreJob(t, c)
	ws := writerSpecOf(t, job.Spec.Template.Spec.Containers[1])
	if !ws.OwnerFromFolder || ws.UID != nil || ws.GID != nil {
		t.Errorf("writer spec %+v: the writer must measure the owner of the folder", ws)
	}
	if spec := jobSpecOf(t, job); spec.UID != nil || spec.GID != nil {
		t.Errorf("the reader was given an owner: %+v", spec)
	}
}

// addContainerMessages adds the Job's pod with one termination message per
// container name.
func addContainerMessages(t *testing.T, c client.Client, key string, msgs map[string]string) {
	t.Helper()
	job := getShareJob(t, c, key)
	var statuses []corev1.ContainerStatus
	for name, msg := range msgs {
		statuses = append(statuses, corev1.ContainerStatus{Name: name,
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: msg}}})
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: key + "-pod", Namespace: testDriverNS, Labels: job.Spec.Template.Labels},
		Status:     corev1.PodStatus{ContainerStatuses: statuses},
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

func finishedGuestRestore(t *testing.T, mode string, msgs map[string]string) (bool, filerestore.Outcome, error) {
	t.Helper()
	r, c := newApplyRunner()
	r.SetBrowseGuest(testGuestImage, BrowseGuestConfig{})
	req := guestFileRestoreRequest(mode)
	ctx := context.Background()
	if _, _, err := r.RunFileRestore(ctx, req); err != nil {
		t.Fatal(err)
	}
	setJobStatus(t, c, "filerestore-uid-1", 1, 0)
	addContainerMessages(t, c, "filerestore-uid-1", msgs)
	return r.RunFileRestore(ctx, req)
}

// The reader parses the untrusted disk. What lands in the restore status (the
// counts and the folder) comes from the writer, which holds the target.
func TestRunFileRestore_GuestCountsAndFolderComeFromTheWriterNotTheReader(t *testing.T) {
	done, out, err := finishedGuestRestore(t, filerestore.ModeFolder, map[string]string{
		"share-data": filerestore.EncodeOutcome(filerestore.Outcome{
			Result: filerestore.Result{Files: 999999, Dirs: 5, Links: 7, Bytes: 1 << 40, Skipped: 2, Unreadable: 4},
			Folder: "forged-folder", Moment: "2026-10-21T09:00:00Z", Latest: true}),
		"writer": `{"files":3,"dirs":1,"links":1,"bytes":99,"folder":"restored-nightly-x"}`,
	})
	if err != nil || !done {
		t.Fatalf("= %v %v", done, err)
	}
	if out.Files != 3 || out.Dirs != 1 || out.Links != 1 || out.Bytes != 99 || out.Folder != "restored-nightly-x" {
		t.Errorf("outcome %+v: the counts and the folder must be the writer's", out)
	}
	if out.Skipped != 2 || out.Unreadable != 4 || out.Moment != "2026-10-21T09:00:00Z" || !out.Latest {
		t.Errorf("outcome %+v: what the writer cannot know comes from the reader", out)
	}
}

func TestRunFileRestore_GuestWithoutAWriterOutcomeIsAFailureNotTheReadersCounts(t *testing.T) {
	_, out, err := finishedGuestRestore(t, filerestore.ModeFolder, map[string]string{
		"share-data": filerestore.EncodeOutcome(filerestore.Outcome{Result: filerestore.Result{Files: 999999}, Folder: "forged-folder"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Error == "" || out.Files != 0 || out.Folder != "" {
		t.Errorf("outcome %+v: with no outcome from the writer nothing the reader says about the target may be used", out)
	}
}

func TestRunFileRestore_GuestKeepsTheReadersReasonWhenItRefused(t *testing.T) {
	_, out, err := finishedGuestRestore(t, filerestore.ModeFolder, map[string]string{
		"share-data": filerestore.EncodeOutcome(filerestore.Outcome{Error: "no file system could be opened"}),
		"writer":     `{}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if out.Error != "no file system could be opened" || out.Files != 0 {
		t.Errorf("outcome %+v", out)
	}
}
