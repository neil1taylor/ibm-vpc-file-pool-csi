package pool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

const testExportVolume = "pvc-11111111-2222-3333-4444-555555555555"

var testExportMoment = time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)

func testExportRepo() ExportRepo {
	return ExportRepo{
		Endpoint:            "s3.eu-de.cloud-object-storage.appdomain.cloud",
		Bucket:              "pool-backups",
		Prefix:              "policy-nightly/",
		CredentialsSecret:   "cos-credentials",
		BackupKeySecret:     "cos-backup-key",
		LockMode:            ExportLockCompliance,
		KeepFor:             25 * time.Hour,
		MaintenanceInterval: time.Hour,
	}
}

func TestExportToCOS_JobMountsTheShareReadOnly(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	done, _, err := r.ExportToCOS(context.Background(), "exp-1", testShareA, testExportRepo(), testBackupPath, testMomentsPath, testExportMoment)
	if err != nil || done {
		t.Fatalf("ExportToCOS() = %v, %v; want false, nil", done, err)
	}
	job := getShareJob(t, c, "exp-1")
	spec := job.Spec.Template.Spec
	if spec.Containers[0].Image != "registry.example/driver:test" {
		t.Errorf("image = %q; want the driver image (it holds kopia)", spec.Containers[0].Image)
	}
	for _, v := range spec.Volumes {
		if v.PersistentVolumeClaim == nil || !v.PersistentVolumeClaim.ReadOnly {
			t.Errorf("volume %s is not a read-only claim", v.Name)
		}
	}
	for _, m := range spec.Containers[0].VolumeMounts {
		if !m.ReadOnly {
			t.Errorf("mount %s is not read-only", m.MountPath)
		}
	}
	pvs := &corev1.PersistentVolumeList{}
	if err := c.List(context.Background(), pvs); err != nil {
		t.Fatal(err)
	}
	if len(pvs.Items) != 1 || !pvs.Items[0].Spec.NFS.ReadOnly {
		t.Errorf("the share volume must be an NFS read-only export: %+v", pvs.Items)
	}
}

func TestExportToCOS_ScriptSnapshotsBothFoldersWithTags(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	_, _, _ = r.ExportToCOS(context.Background(), "exp-1", testShareA, testExportRepo(), testBackupPath, testMomentsPath, testExportMoment)
	script := jobScript(getShareJob(t, c, "exp-1"))
	for _, want := range []string{
		"kopia repository connect s3",
		"kopia repository create s3",
		"--retention-mode=COMPLIANCE",
		"--retention-period='25h0m0s'",
		"'/share0" + testBackupPath + "'",
		"'/share0" + testMomentsPath + "'",
		"--tags='moment:2026-10-21T09:30:00Z'",
		"--tags='volume:" + testExportVolume + "'",
		"--bucket='pool-backups'",
		"--endpoint='s3.eu-de.cloud-object-storage.appdomain.cloud'",
		"--prefix='policy-nightly/'",
		"/dev/termination-log",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %s:\n%s", want, script)
		}
	}
	if strings.Contains(script, "set -x") || strings.Contains(script, "xtrace") {
		t.Error("the script must not trace commands")
	}
}

func TestExportToCOS_LockNoneSetsNoRetention(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	repo := testExportRepo()
	repo.LockMode = ExportLockNone
	repo.KeepFor = 0
	if _, _, err := r.ExportToCOS(context.Background(), "exp-1", testShareA, repo, testBackupPath, testMomentsPath, testExportMoment); err != nil {
		t.Fatal(err)
	}
	if script := jobScript(getShareJob(t, c, "exp-1")); strings.Contains(script, "retention") {
		t.Errorf("lock None must not set a retention:\n%s", script)
	}
}

func TestExportToCOS_SecretsComeFromSecretReferencesNotTheScript(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	_, _, _ = r.ExportToCOS(context.Background(), "exp-1", testShareA, testExportRepo(), testBackupPath, testMomentsPath, testExportMoment)
	ctr := getShareJob(t, c, "exp-1").Spec.Template.Spec.Containers[0]
	want := map[string][2]string{
		"AWS_ACCESS_KEY_ID":     {"cos-credentials", "access_key_id"},
		"AWS_SECRET_ACCESS_KEY": {"cos-credentials", "secret_access_key"},
		"KOPIA_PASSWORD":        {"cos-backup-key", "backupKey"},
	}
	for _, e := range ctr.Env {
		w, ok := want[e.Name]
		if !ok {
			continue
		}
		if e.Value != "" || e.ValueFrom == nil || e.ValueFrom.SecretKeyRef == nil ||
			e.ValueFrom.SecretKeyRef.Name != w[0] || e.ValueFrom.SecretKeyRef.Key != w[1] {
			t.Errorf("%s = %+v; want a reference to secret %s key %s", e.Name, e, w[0], w[1])
		}
		delete(want, e.Name)
	}
	if len(want) != 0 {
		t.Errorf("missing environment variables: %v", want)
	}
	for _, a := range ctr.Command {
		if strings.Contains(a, "--secret-access-key") || strings.Contains(a, "--access-key") || strings.Contains(a, "--password") {
			t.Errorf("a key must not be passed as an argument:\n%s", a)
		}
	}
}

func TestExportToCOS_RefusesUnsafeInputsBeforeAnyJob(t *testing.T) {
	cases := map[string]func(*ExportRepo, *string, *string){
		"endpoint with a quote":    func(r *ExportRepo, _, _ *string) { r.Endpoint = "s3.example'; rm -rf /; '" },
		"endpoint with a space":    func(r *ExportRepo, _, _ *string) { r.Endpoint = "s3.example com" },
		"empty endpoint":           func(r *ExportRepo, _, _ *string) { r.Endpoint = "" },
		"bucket in capitals":       func(r *ExportRepo, _, _ *string) { r.Bucket = "Bad_Bucket" },
		"bucket with a quote":      func(r *ExportRepo, _, _ *string) { r.Bucket = "b'; id; '" },
		"prefix with traversal":    func(r *ExportRepo, _, _ *string) { r.Prefix = "a/../b/" },
		"prefix with a quote":      func(r *ExportRepo, _, _ *string) { r.Prefix = "a'b/" },
		"credentials secret name":  func(r *ExportRepo, _, _ *string) { r.CredentialsSecret = "Bad Name" },
		"empty backup key secret":  func(r *ExportRepo, _, _ *string) { r.BackupKeySecret = "" },
		"unknown lock mode":        func(r *ExportRepo, _, _ *string) { r.LockMode = "Governance" },
		"keep time too short":      func(r *ExportRepo, _, _ *string) { r.KeepFor = 24 * time.Hour },
		"maintenance too frequent": func(r *ExportRepo, _, _ *string) { r.MaintenanceInterval = time.Minute },
		"keep below interval+24h":  func(r *ExportRepo, _, _ *string) { r.MaintenanceInterval = 2 * time.Hour },
		"backup path traversal":    func(_ *ExportRepo, b, _ *string) { *b = "/backup/nightly/pvcs/../../etc" },
		"a data volume as backup":  func(_ *ExportRepo, b, _ *string) { *b = "/pvcs/pvc-11111111-2222-3333-4444-555555555555" },
		"moments of other volume": func(_ *ExportRepo, _, m *string) {
			*m = "/backup/nightly/.moments/pvcs/pvc-66666666-7777-8888-9999-000000000000"
		},
		"shell quote in path": func(_ *ExportRepo, b, _ *string) {
			*b = `/backup/nightly/pvcs/pvc-11111111-2222-3333-4444-55555555555"; rm -rf /`
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			r, c := newShareJobTestRunner()
			r.SetDriverImage("registry.example/driver:test")
			repo, backup, moments := testExportRepo(), testBackupPath, testMomentsPath
			mutate(&repo, &backup, &moments)
			if _, _, err := r.ExportToCOS(context.Background(), "exp-1", testShareA, repo, backup, moments, testExportMoment); err == nil {
				t.Fatal("want an error")
			}
			if n := countPVs(t, c); n != 0 {
				t.Errorf("%d share volume(s) made for an unsafe input", n)
			}
		})
	}
}

func TestExportToCOS_NeedsTheDriverImage(t *testing.T) {
	r, _ := newShareJobTestRunner()
	if _, _, err := r.ExportToCOS(context.Background(), "exp-1", testShareA, testExportRepo(), testBackupPath, testMomentsPath, testExportMoment); err == nil {
		t.Fatal("a runner with no driver image must refuse: only that image holds kopia")
	}
}

func TestExportToCOS_ReadsBytesAndMomentFromTheTerminationMessage(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	ctx := context.Background()
	_, _, _ = r.ExportToCOS(ctx, "exp-1", testShareA, testExportRepo(), testBackupPath, testMomentsPath, testExportMoment)
	setJobStatus(t, c, "exp-1", 1, 0)
	job := getShareJob(t, c, "exp-1")
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "exp-1-pod", Namespace: testDriverNS, Labels: job.Spec.Template.Labels},
		Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: `{"moment":"2026-10-21T09:30:00Z","bytes":4096}`}},
		}}},
	}
	if err := c.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
	done, out, err := r.ExportToCOS(ctx, "exp-1", testShareA, testExportRepo(), testBackupPath, testMomentsPath, testExportMoment)
	if err != nil || !done {
		t.Fatalf("ExportToCOS() = %v, %v; want done", done, err)
	}
	if out.Bytes != 4096 || !out.Moment.Equal(testExportMoment) {
		t.Errorf("outcome = %+v; want 4096 bytes at %s", out, testExportMoment)
	}
}

func TestParseExportOutcome_AnUnreadableMessageIsNotKnown(t *testing.T) {
	for _, msg := range []string{"", "garbage", `{"bytes":-5}`} {
		if out := parseExportOutcome(msg); out != (ExportOutcome{}) {
			t.Errorf("parseExportOutcome(%q) = %+v; want zero", msg, out)
		}
	}
}

func TestMaintainCOSRepo_JobMountsNoShareAndRunsMaintenance(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	done, err := r.MaintainCOSRepo(context.Background(), "mnt-1", testExportRepo())
	if err != nil || done {
		t.Fatalf("MaintainCOSRepo() = %v, %v; want false, nil", done, err)
	}
	if n := countPVs(t, c); n != 0 {
		t.Errorf("a maintenance Job needs no share, got %d volume(s)", n)
	}
	script := jobScript(getShareJob(t, c, "mnt-1"))
	for _, want := range []string{
		"kopia repository connect s3",
		"kopia maintenance set",
		"--extend-object-locks=true",
		"--full-interval='1h0m0s'",
		"kopia maintenance run --full",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %s:\n%s", want, script)
		}
	}
	if strings.Contains(script, "repository create") {
		t.Error("maintenance must never create a repository")
	}
}

func TestMaintainCOSRepo_LockNoneDoesNotExtendLocks(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	repo := testExportRepo()
	repo.LockMode, repo.KeepFor = ExportLockNone, 0
	if _, err := r.MaintainCOSRepo(context.Background(), "mnt-1", repo); err != nil {
		t.Fatal(err)
	}
	script := jobScript(getShareJob(t, c, "mnt-1"))
	if strings.Contains(script, "extend-object-locks") || !strings.Contains(script, "kopia maintenance run --full") {
		t.Errorf("lock None must run maintenance without extending locks:\n%s", script)
	}
}

// The tests below run the generated script itself against a Kopia repository
// in a folder. They need the kopia program, and skip without it.

const exportSecretSentinel = "SENTINEL-KEY-MUST-NOT-APPEAR"

func requireKopia(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("kopia"); err != nil {
		t.Skip("kopia is not on PATH; install it to run the script-level export tests")
	}
}

// runScript runs script with /bin/sh and returns the termination message and
// everything the script printed.
func runScript(t *testing.T, script, termLog string, env []string) (message, output string, err error) {
	t.Helper()
	cmd := exec.Command("/bin/sh", "-c", script)
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	b, _ := os.ReadFile(termLog)
	return string(b), string(out), err
}

type localExport struct {
	root, repoDir, termLog string
	params                 exportParams
	env                    []string
}

func newLocalExport(t *testing.T) *localExport {
	t.Helper()
	dir := t.TempDir()
	l := &localExport{root: filepath.Join(dir, "share0"), repoDir: filepath.Join(dir, "repo"), termLog: filepath.Join(dir, "termination-log")}
	if err := os.MkdirAll(filepath.Join(l.root, testBackupPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(l.root, testMomentsPath, "20261020T090000Z"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(p, content string) {
		if err := os.WriteFile(filepath.Join(l.root, p), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(testBackupPath+"/disk.img", strings.Repeat("a", 200000))
	write(testMomentsPath+"/20261020T090000Z/undo", "older moment")
	l.params = exportParams{
		root:        l.root,
		termLog:     l.termLog,
		backend:     "filesystem --path=" + l.repoDir,
		backupPath:  testBackupPath,
		momentsPath: testMomentsPath,
		volume:      testExportVolume,
		moment:      testExportMoment,
		stateDir:    filepath.Join(dir, "state"),
		repo:        ExportRepo{LockMode: ExportLockNone, MaintenanceInterval: time.Hour},
	}
	l.env = []string{"KOPIA_PASSWORD=" + exportSecretSentinel, "AWS_SECRET_ACCESS_KEY=" + exportSecretSentinel}
	return l
}

func TestExportScript_LocalRepository_ExportsAndSecondRunSendsLittle(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	script := exportScript(l.params)

	msg, out, err := runScript(t, script, l.termLog, l.env)
	if err != nil {
		t.Fatalf("first export failed: %v\n%s", err, out)
	}
	first := parseExportOutcome(msg)
	if first.Bytes < 1000 || !first.Moment.Equal(testExportMoment) {
		t.Fatalf("first outcome = %+v from %q; want some bytes and the moment", first, msg)
	}

	msg, out, err = runScript(t, script, l.termLog, l.env)
	if err != nil {
		t.Fatalf("second export failed: %v\n%s", err, out)
	}
	second := parseExportOutcome(msg)
	if second.Moment.IsZero() || second.Bytes >= first.Bytes/2 {
		t.Errorf("second export sent %d bytes after %d; unchanged data must not be sent again", second.Bytes, first.Bytes)
	}
}

func TestExportScript_LocalRepository_SnapshotsAreTaggedAndComeBack(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("export failed: %v\n%s", err, out)
	}
	list := exec.Command("kopia", "snapshot", "list", "--all", "--json")
	list.Env = append(os.Environ(), l.env...)
	list.Env = append(list.Env, "KOPIA_CONFIG_PATH="+filepath.Join(l.params.stateDir, "repository.config"),
		"KOPIA_CACHE_DIRECTORY="+filepath.Join(l.params.stateDir, "cache"), "KOPIA_LOG_DIR="+filepath.Join(l.params.stateDir, "logs"))
	out, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("kopia snapshot list: %v\n%s", err, out)
	}
	s := string(out)
	for _, want := range []string{"tag:moment", "2026-10-21T09:30:00Z", "tag:volume", testExportVolume, testBackupPath, testMomentsPath} {
		if !strings.Contains(s, want) {
			t.Errorf("snapshot list lacks %q:\n%s", want, s)
		}
	}
}

func TestExportScript_NeverWritesToTheShare(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	before := dirListing(t, l.root)
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("export failed: %v\n%s", err, out)
	}
	if after := dirListing(t, l.root); after != before {
		t.Errorf("the export changed the share:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func dirListing(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		b.WriteString(p + " " + info.Mode().String() + " " + info.ModTime().String() + "\n")
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestExportScript_LocalRepository_NoKeyInOutputOrMessage(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	script := exportScript(l.params)
	msg, out, err := runScript(t, script, l.termLog, l.env)
	if err != nil {
		t.Fatalf("export failed: %v\n%s", err, out)
	}
	for name, text := range map[string]string{"script": script, "termination message": msg, "output": out} {
		if strings.Contains(text, exportSecretSentinel) {
			t.Errorf("the %s contains the key:\n%s", name, text)
		}
	}
}

func TestExportScript_MissingMomentsFolderIsSkipped(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	if err := os.RemoveAll(filepath.Join(l.root, testMomentsPath)); err != nil {
		t.Fatal(err)
	}
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("a volume with no earlier moments must still export: %v\n%s", err, out)
	}
}

func TestExportScript_MissingBackupFolderFails(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	if err := os.RemoveAll(filepath.Join(l.root, testBackupPath)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := runScript(t, exportScript(l.params), l.termLog, l.env); err == nil {
		t.Fatal("an export with no backup folder must fail, not report success")
	}
}

func TestMaintenanceScript_LocalRepository_Runs(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("export failed: %v\n%s", err, out)
	}
	_, out, err := runScript(t, maintenanceScript(l.params), l.termLog, l.env)
	if err != nil {
		t.Fatalf("maintenance failed: %v\n%s", err, out)
	}
	if !strings.Contains(out, "Finished full maintenance") {
		t.Errorf("maintenance did not finish:\n%s", out)
	}
	if strings.Contains(out, exportSecretSentinel) {
		t.Error("maintenance output contains the key")
	}
}

// MinIO test: the S3 path. Set MINIO_ENDPOINT (host:port, plain HTTP),
// MINIO_ACCESS_KEY, MINIO_SECRET_KEY and MINIO_BUCKET (a bucket that exists).
func TestExportScript_MinIO(t *testing.T) {
	requireKopia(t)
	endpoint := os.Getenv("MINIO_ENDPOINT")
	if endpoint == "" {
		t.Skip("MINIO_ENDPOINT is not set; skipping the S3 export test")
	}
	l := newLocalExport(t)
	l.params.backend = "" // use the S3 backend, built from repo
	l.params.repo = ExportRepo{
		Endpoint: "http://" + endpoint,
		Bucket:   os.Getenv("MINIO_BUCKET"),
		Prefix:   "test-" + strings.ToLower(strings.ReplaceAll(t.Name(), "/", "-")) + "-" + time.Now().UTC().Format("20060102t150405") + "/",
		// No object lock on a plain MinIO bucket.
		LockMode: ExportLockNone, MaintenanceInterval: time.Hour,
	}
	l.env = []string{
		"AWS_ACCESS_KEY_ID=" + os.Getenv("MINIO_ACCESS_KEY"),
		"AWS_SECRET_ACCESS_KEY=" + os.Getenv("MINIO_SECRET_KEY"),
		"KOPIA_PASSWORD=" + exportSecretSentinel,
	}
	script := exportScript(l.params)
	msg, out, err := runScript(t, script, l.termLog, l.env)
	if err != nil {
		t.Fatalf("export to MinIO failed: %v\n%s", err, out)
	}
	if first := parseExportOutcome(msg); first.Bytes < 1000 {
		t.Errorf("outcome %+v from %q; want bytes sent", first, msg)
	}
	msg, out, err = runScript(t, script, l.termLog, l.env)
	if err != nil {
		t.Fatalf("second export to MinIO failed: %v\n%s", err, out)
	}
	if second := parseExportOutcome(msg); second.Bytes > 50000 {
		t.Errorf("second export sent %d bytes; unchanged data must not be sent again", second.Bytes)
	}
}
