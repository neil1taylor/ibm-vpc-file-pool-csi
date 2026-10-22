package pool

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const testTargetVolume = "pvc-99999999-8888-7777-6666-555555555555"

func testCOSRestoreRequest() COSRestoreRequest {
	return COSRestoreRequest{
		SourceVolume: testExportVolume, TargetVolume: testTargetVolume,
		Scratch: COSScratchName("restore-1"), Moment: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC),
	}
}

func testRestoreRepo() ExportRepo {
	r := testExportRepo()
	r.LockMode, r.KeepFor = ExportLockNone, 0
	return r
}

func TestFetchFromCOS_JobMountsTheTargetShareForWriting(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	done, _, err := r.FetchFromCOS(context.Background(), "cosfetch-1", testShareA, testRestoreRepo(), testCOSRestoreRequest())
	if err != nil || done {
		t.Fatalf("FetchFromCOS() = %v, %v; want false, nil", done, err)
	}
	spec := getShareJob(t, c, "cosfetch-1").Spec.Template.Spec
	if spec.Containers[0].Image != "registry.example/driver:test" {
		t.Errorf("image = %q; want the driver image (it holds kopia)", spec.Containers[0].Image)
	}
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim != nil && v.PersistentVolumeClaim.ReadOnly {
			t.Errorf("volume %s is read-only; the scratch copy must be written there", v.Name)
		}
	}
	script := jobScript(getShareJob(t, c, "cosfetch-1"))
	for _, want := range []string{
		"kopia repository connect s3", "--readonly",
		"kopia snapshot restore",
		"/share0/backup/" + COSScratchName("restore-1"),
		testExportVolume, testTargetVolume,
		"2026-10-21T09:00:00Z",
		"--bucket='pool-backups'", "--prefix='policy-nightly/'",
		"unwrap-backup-key",
		"/dev/termination-log",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "repository create") {
		t.Error("a restore must never create a repository")
	}
	if strings.Contains(script, "set -x") || strings.Contains(script, "--password") {
		t.Error("the script traces commands or passes a password argument")
	}
}

func TestFetchFromCOS_EmergencyOnlyNeverTouchesKeyProtect(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	repo := testRestoreRepo()
	repo.EmergencyOnly, repo.EmergencyKeySecret = true, "emergency"
	repo.BackupKeySecret, repo.KeyProtect = "", testExportRepo().KeyProtect
	repo.KeyProtect.APIKeySecret = ""
	if _, _, err := r.FetchFromCOS(context.Background(), "cosfetch-1", testShareA, repo, testCOSRestoreRequest()); err != nil {
		t.Fatal(err)
	}
	job := getShareJob(t, c, "cosfetch-1")
	script := jobScript(job)
	if strings.Contains(script, "unwrap-backup-key") {
		t.Errorf("emergency-only must not call Key Protect:\n%s", script)
	}
	if !strings.Contains(script, emergencyKeyDir+"/"+ExportEmergencyKeyKey) {
		t.Errorf("the script does not read the emergency key file:\n%s", script)
	}
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Secret != nil && v.Secret.SecretName != "emergency" {
			t.Errorf("volume %s mounts Secret %q; only the emergency key belongs here", v.Name, v.Secret.SecretName)
		}
	}
}

func TestFetchFromCOS_Refuses(t *testing.T) {
	cases := map[string]func(*COSRestoreRequest, *ExportRepo){
		"source volume is not a volume": func(q *COSRestoreRequest, _ *ExportRepo) { q.SourceVolume = "../x" },
		"target volume is not a volume": func(q *COSRestoreRequest, _ *ExportRepo) { q.TargetVolume = "x; rm -rf /" },
		"scratch outside the prefix":    func(q *COSRestoreRequest, _ *ExportRepo) { q.Scratch = "nightly" },
		"scratch with a slash":          func(q *COSRestoreRequest, _ *ExportRepo) { q.Scratch = "cosrestore-a/../b" },
		"bad bucket":                    func(_ *COSRestoreRequest, r *ExportRepo) { r.Bucket = "A_B" },
		"emergency only with no secret": func(_ *COSRestoreRequest, r *ExportRepo) { r.EmergencyOnly, r.EmergencyKeySecret = true, "" },
	}
	for name, mutate := range cases {
		r, _ := newShareJobTestRunner()
		r.SetDriverImage("registry.example/driver:test")
		req, repo := testCOSRestoreRequest(), testRestoreRepo()
		mutate(&req, &repo)
		if _, _, err := r.FetchFromCOS(context.Background(), "cosfetch-1", testShareA, repo, req); err == nil {
			t.Errorf("%s: FetchFromCOS accepted it", name)
		}
	}
	r, _ := newShareJobTestRunner()
	if _, _, err := r.FetchFromCOS(context.Background(), "cosfetch-1", testShareA, testRestoreRepo(), testCOSRestoreRequest()); err == nil {
		t.Error("a runner with no driver image must refuse: only that image holds kopia")
	}
}

func TestFetchFromCOS_ReportsTheMomentFetched(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	ctx := context.Background()
	_, _, _ = r.FetchFromCOS(ctx, "cosfetch-1", testShareA, testRestoreRepo(), testCOSRestoreRequest())
	setJobStatus(t, c, "cosfetch-1", 1, 0)
	addTerminationMessage(t, c, "cosfetch-1", `{"moment":"2026-10-21T09:30:00Z"}`)
	done, out, err := r.FetchFromCOS(ctx, "cosfetch-1", testShareA, testRestoreRepo(), testCOSRestoreRequest())
	if err != nil || !done || !out.Moment.Equal(testExportMoment) {
		t.Fatalf("FetchFromCOS() = %v, %+v, %v; want done at %s", done, out, err, testExportMoment)
	}
}

func TestFetchFromCOS_FailedJobIsReported(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	ctx := context.Background()
	_, _, _ = r.FetchFromCOS(ctx, "cosfetch-1", testShareA, testRestoreRepo(), testCOSRestoreRequest())
	setJobStatus(t, c, "cosfetch-1", 0, 5)
	_, _, err := r.FetchFromCOS(ctx, "cosfetch-1", testShareA, testRestoreRepo(), testCOSRestoreRequest())
	if !errors.Is(err, ErrShareJobFailed) {
		t.Fatalf("err = %v, want ErrShareJobFailed", err)
	}
}

func TestRemoveCOSScratch(t *testing.T) {
	r, c := newShareJobTestRunner()
	scratch := COSScratchName("restore-1")
	if done, err := r.RemoveCOSScratch(context.Background(), "coscleanup-1", testShareA, scratch); err != nil || done {
		t.Fatalf("RemoveCOSScratch() = %v, %v; want false, nil", done, err)
	}
	if script := jobScript(getShareJob(t, c, "coscleanup-1")); !strings.Contains(script, "/share0/backup/"+scratch) {
		t.Errorf("script does not remove the scratch folder:\n%s", script)
	}
	for _, bad := range []string{"nightly", "cosrestore-", "cosrestore-a/b", "cosrestore-..", "", "COSRESTORE-x"} {
		if _, err := r.RemoveCOSScratch(context.Background(), "coscleanup-2", testShareA, bad); err == nil {
			t.Errorf("RemoveCOSScratch accepted %q: only a scratch folder may be removed", bad)
		}
	}
}

func TestCOSScratchName_FitsTheBackupFolderRules(t *testing.T) {
	long := COSScratchName(strings.Repeat("a", 200))
	if len(long) > 63 {
		t.Errorf("scratch name has %d characters, want at most 63", len(long))
	}
	b, m := COSScratchPaths(COSScratchName("restore-1"), testTargetVolume)
	if err := validateScratchPaths(b, m); err != nil {
		t.Errorf("scratch paths do not pass the backup path rules: %v", err)
	}
}

func TestListCOS_JobNeedsNoShareAndParsesTheListing(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	ctx := context.Background()
	if done, _, err := r.ListCOS(ctx, "coslist-1", testRestoreRepo(), ""); err != nil || done {
		t.Fatalf("ListCOS() = %v, %v; want false, nil", done, err)
	}
	job := getShareJob(t, c, "coslist-1")
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.PersistentVolumeClaim != nil {
			t.Errorf("a listing needs no share, but the Job mounts %s", v.Name)
		}
	}
	if script := jobScript(job); !strings.Contains(script, "--readonly") || strings.Contains(script, "repository create") {
		t.Errorf("a listing must connect read-only and never create:\n%s", script)
	}
	setJobStatus(t, c, "coslist-1", 1, 0)
	addTerminationMessage(t, c, "coslist-1", `{"volumes":[{"volume":"`+testExportVolume+`","moments":["2026-10-21T09:30:00Z","2026-10-21T10:30:00Z"]}],"truncated":true}`)
	done, l, err := r.ListCOS(ctx, "coslist-1", testRestoreRepo(), "")
	if err != nil || !done {
		t.Fatalf("ListCOS() = %v, %v", done, err)
	}
	if len(l.Volumes) != 1 || l.Volumes[0].Name != testExportVolume || len(l.Volumes[0].Moments) != 2 || !l.Truncated {
		t.Errorf("listing = %+v", l)
	}
}

func TestListCOS_RefusesABadVolumeFilter(t *testing.T) {
	r, _ := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	if _, _, err := r.ListCOS(context.Background(), "coslist-1", testRestoreRepo(), "x'; rm -rf /"); err == nil {
		t.Error("ListCOS accepted a volume filter that is not a volume name")
	}
}

// addTerminationMessage gives the pod of a finished Job its termination message.
func addTerminationMessage(t *testing.T, c client.Client, key, msg string) {
	t.Helper()
	job := getShareJob(t, c, key)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: key + "-pod", Namespace: testDriverNS, Labels: job.Spec.Template.Labels},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: msg}},
		}}},
	}
	if err := c.Create(context.Background(), pod); err != nil {
		t.Fatal(err)
	}
}

// ---- script-level tests against a Kopia repository in a folder ----

func TestCOSRestoreScript_PicksTheExportAndWritesTheScratchFolder(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	diskPath := filepath.Join(l.root, testBackupPath, "disk.img")
	first := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	second := first.Add(time.Hour)

	l.params.moment = first
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("first export: %v\n%s", err, out)
	}
	if err := os.WriteFile(diskPath, []byte(strings.Repeat("b", 200000)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.root, testMomentsPath, "20261021T093000Z"), 0o755); err != nil {
		t.Fatal(err)
	}
	l.params.moment = second
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("second export: %v\n%s", err, out)
	}

	cases := []struct {
		name       string
		want       time.Time
		wantMoment time.Time
		wantFirst  byte
	}{
		{"no moment gives the newest", time.Time{}, second, 'b'},
		{"a time before all exports gives the oldest one that covers it", first.Add(-time.Hour), first, 'a'},
		{"a time between exports gives the next one", first.Add(15 * time.Minute), second, 'b'},
		{"a time after all exports gives the newest", second.Add(5 * time.Hour), second, 'b'},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := COSRestoreRequest{SourceVolume: testExportVolume, TargetVolume: testTargetVolume, Scratch: COSScratchName("t"), Moment: tc.want}
			p := cosRestoreParams{exportParams: l.params, req: req}
			p.stateDir = t.TempDir()
			msg, out, err := runScript(t, cosRestoreScript(p), l.termLog, l.env)
			if err != nil {
				t.Fatalf("restore script failed: %v\n%s", err, out)
			}
			got := parseCOSRestoreOutcome(msg)
			if !got.Moment.Equal(tc.wantMoment) {
				t.Errorf("moment = %s from %q, want %s", got.Moment, msg, tc.wantMoment)
			}
			b, m := COSScratchPaths(req.Scratch, testTargetVolume)
			data, err := os.ReadFile(filepath.Join(l.root, b, "disk.img"))
			if err != nil {
				t.Fatalf("the scratch copy has no disk: %v", err)
			}
			if len(data) != 200000 || data[0] != tc.wantFirst {
				t.Errorf("scratch disk has %d bytes starting with %q, want 200000 starting with %q", len(data), data[0], tc.wantFirst)
			}
			if _, err := os.Stat(filepath.Join(l.root, m, "20261020T090000Z", "undo")); err != nil {
				t.Errorf("the undo files were not restored: %v", err)
			}
			if strings.Contains(out, exportSecretSentinel) || strings.Contains(msg, exportSecretSentinel) {
				t.Error("a key reached the output or the termination message")
			}
		})
	}
}

func TestCOSRestoreScript_UnknownVolumeFailsAndLeavesNothing(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	req := COSRestoreRequest{SourceVolume: "pvc-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", TargetVolume: testTargetVolume, Scratch: COSScratchName("t")}
	p := cosRestoreParams{exportParams: l.params, req: req}
	p.stateDir = t.TempDir()
	_, out, err := runScript(t, cosRestoreScript(p), l.termLog, l.env)
	if err == nil {
		t.Fatal("restoring a volume that is not in the repository must fail")
	}
	if !strings.Contains(out, "no backup of") {
		t.Errorf("output does not say what is missing:\n%s", out)
	}
	if _, statErr := os.Stat(filepath.Join(l.root, "backup", req.Scratch)); !os.IsNotExist(statErr) {
		t.Error("a failed restore left the scratch folder behind")
	}
}

func TestCOSListScript_ListsVolumesAndMoments(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	l.params.moment = testExportMoment
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	l.params.moment = testExportMoment.Add(time.Hour)
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	p := l.params
	p.stateDir = t.TempDir()
	msg, out, err := runScript(t, cosListScript(p, ""), l.termLog, l.env)
	if err != nil {
		t.Fatalf("list script failed: %v\n%s", err, out)
	}
	got := parseCOSListing(msg)
	if len(got.Volumes) != 1 || got.Volumes[0].Name != testExportVolume || len(got.Volumes[0].Moments) != 2 {
		t.Fatalf("listing = %+v from %q, want one volume with two moments", got, msg)
	}
	if !got.Volumes[0].Moments[1].Equal(testExportMoment.Add(time.Hour)) || got.Truncated {
		t.Errorf("moments = %v truncated = %v", got.Volumes[0].Moments, got.Truncated)
	}

	msg, _, err = runScript(t, cosListScript(p, "pvc-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"), l.termLog, l.env)
	if err != nil {
		t.Fatal(err)
	}
	if got = parseCOSListing(msg); len(got.Volumes) != 0 {
		t.Errorf("a filter for another volume still listed %+v", got)
	}
}
