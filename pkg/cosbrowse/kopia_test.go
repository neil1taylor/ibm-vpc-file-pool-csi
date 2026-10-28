package cosbrowse

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const (
	testAccess   = "TESTACCESSKEYID"
	testSecret   = "testsecretaccesskey/value"
	testPassword = "test-backup-key-readable"
	sentinelName = "BROWSE_TEST_SENTINEL_APIKEY"
	sentinelVal  = "sentinel-api-key-must-not-reach-kopia"
)

// fakeKopia is a shell script that stands in for the Kopia program. It writes
// every argument list and the whole environment it was started with to files,
// so a test can see what a child is given.
type fakeKopia struct {
	dir string
	bin string
}

func newFakeKopia(t *testing.T) *fakeKopia {
	t.Helper()
	dir := t.TempDir()
	fk := &fakeKopia{dir: dir, bin: filepath.Join(dir, "kopia")}
	script := `#!/bin/sh
echo "$*" >> '` + dir + `/argv'
env >> '` + dir + `/env'
if [ -f '` + dir + `/stderr' ]; then cat '` + dir + `/stderr' >&2; fi
if [ -f '` + dir + `/exit' ]; then exit "$(cat '` + dir + `/exit')"; fi
case "$1" in
  repository) exit 0 ;;
  snapshot) cat '` + dir + `/snapshots.json' ;;
  show) cat "` + dir + `/obj-$2" ;;
esac
`
	if err := os.WriteFile(fk.bin, []byte(script), 0o700); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	return fk
}

func (fk *fakeKopia) set(t *testing.T, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(fk.dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (fk *fakeKopia) read(t *testing.T, name string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(fk.dir, name))
	return string(b)
}

func testSource() Source {
	return Source{
		Endpoint: "https://s3.eu-de.cloud-object-storage.appdomain.cloud", Bucket: "my-bucket", Prefix: "backups/pol/",
		Policy: "pol", Volume: "pvc-11111111-2222-3333-4444-555555555555",
		KeyProtect: &KeyProtect{Region: "eu-de", InstanceID: "inst-1", RootKeyID: "root-1"},
	}
}

func newTestRepo(t *testing.T, fk *fakeKopia) *KopiaRepo {
	t.Helper()
	return NewKopia(KopiaConfig{
		Binary: fk.bin, StateDir: filepath.Join(t.TempDir(), "state"), Source: testSource(),
		AccessKey: testAccess, SecretKey: testSecret, Password: testPassword,
	})
}

func TestKopia_ConnectIsReadOnlyAndKeepsSecretsOffTheCommandLine(t *testing.T) {
	fk := newFakeKopia(t)
	t.Setenv(sentinelName, sentinelVal)
	k := newTestRepo(t, fk)
	if err := k.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	argv := fk.read(t, "argv")
	for _, want := range []string{"repository connect s3", "--bucket=my-bucket", "--endpoint=s3.eu-de.cloud-object-storage.appdomain.cloud",
		"--prefix=backups/pol/", "--readonly", "--no-persist-credentials"} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv lacks %q: %s", want, argv)
		}
	}
	if strings.Contains(argv, "disable-tls") || strings.Contains(argv, "https://") {
		t.Errorf("a https endpoint must not turn TLS off or keep the scheme: %s", argv)
	}
	for _, secret := range []string{testAccess, testSecret, testPassword, sentinelVal} {
		if strings.Contains(argv, secret) {
			t.Errorf("a secret is on the command line: %s", argv)
		}
	}
	env := fk.read(t, "env")
	for _, want := range []string{"KOPIA_PASSWORD=" + testPassword, "AWS_ACCESS_KEY_ID=" + testAccess, "AWS_SECRET_ACCESS_KEY=" + testSecret} {
		if !strings.Contains(env, want) {
			t.Errorf("the child lacks %s", strings.SplitN(want, "=", 2)[0])
		}
	}
	if strings.Contains(env, sentinelName) || strings.Contains(env, sentinelVal) {
		t.Error("the helper's own environment reached the Kopia child")
	}
}

func TestKopia_HTTPEndpointTurnsTLSOffForTests(t *testing.T) {
	fk := newFakeKopia(t)
	k := newTestRepo(t, fk)
	k.cfg.Source.Endpoint = "http://minio.test:9000"
	if err := k.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	if argv := fk.read(t, "argv"); !strings.Contains(argv, "--endpoint=minio.test:9000") || !strings.Contains(argv, "--disable-tls") {
		t.Errorf("argv = %s", argv)
	}
}

func TestKopia_FailuresAreWordedForPeopleAndLeakNoRawText(t *testing.T) {
	for _, c := range []struct {
		stderr, want string
	}{
		{"ERROR unable to open repository: invalid repository password", "backup key does not open"},
		{"operation error S3: AccessDenied, key AKIA77777", "COS refused the key"},
		{"NoSuchBucket: The specified bucket does not exist", "bucket does not exist"},
		{"dial tcp 10.1.2.3:443: i/o timeout", "COS cannot be reached"},
		{"repository not initialized in the provided storage", "no backup repository"},
		{"something nobody expected AKIA77777", "COS could not be read"},
	} {
		fk := newFakeKopia(t)
		fk.set(t, "stderr", c.stderr)
		fk.set(t, "exit", "1")
		err := newTestRepo(t, fk).Connect(context.Background())
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%q: err = %v, want text %q", c.stderr, err, c.want)
			continue
		}
		for _, raw := range []string{"AKIA77777", "10.1.2.3"} {
			if strings.Contains(err.Error(), raw) {
				t.Errorf("%q: the raw text %q reached the error: %v", c.stderr, raw, err)
			}
		}
	}
}

func TestKopia_SnapshotsAreParsed(t *testing.T) {
	fk := newFakeKopia(t)
	fk.set(t, "snapshots.json", `[
 {"id":"aaa","source":{"host":"pool-csi","userName":"driver","path":"/share0/backup/pol/pvcs/pvc-11111111-2222-3333-4444-555555555555"},
  "startTime":"2026-10-22T09:16:00.5Z","rootEntry":{"name":"x","type":"d","mode":"0755","mtime":"2026-10-22T09:00:00Z","uid":107,"gid":107,"obj":"kabc123"},
  "tags":{"tag:moment":"2026-10-22T09:15:00Z","tag:volume":"pvc-11111111-2222-3333-4444-555555555555"}},
 {"id":"bbb","source":{"path":"/other"},"startTime":"2026-10-22T09:16:00Z","rootEntry":{"obj":"kdef"},"tags":{}}
]`)
	snaps, err := newTestRepo(t, fk).Snapshots(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snaps) != 2 {
		t.Fatalf("snapshots = %d", len(snaps))
	}
	s := snaps[0]
	if s.ID != "aaa" || s.Root != "kabc123" || s.RootMode != 0o755 || s.RootUID != 107 || s.RootGID != 107 ||
		s.Volume != "pvc-11111111-2222-3333-4444-555555555555" || !s.Moment.Equal(time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC)) ||
		!strings.HasSuffix(s.SourcePath, "pvc-11111111-2222-3333-4444-555555555555") {
		t.Errorf("snapshot = %+v", s)
	}
	if !snaps[1].Moment.IsZero() || snaps[1].Volume != "" {
		t.Errorf("a snapshot without tags = %+v", snaps[1])
	}
}

func TestKopia_ObjectStreamsAndReportsFailures(t *testing.T) {
	fk := newFakeKopia(t)
	fk.set(t, "obj-kabc123", "file content")
	k := newTestRepo(t, fk)
	rc, err := k.Object(context.Background(), "kabc123")
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(b) != "file content" {
		t.Fatalf("read = %q, %v", b, err)
	}
	if argv := fk.read(t, "argv"); !strings.Contains(argv, "show kabc123") {
		t.Errorf("argv = %s", argv)
	}

	// A child that fails after it wrote part of the file must not look like a
	// whole file.
	fk.set(t, "stderr", "AccessDenied")
	fk.set(t, "exit", "1")
	rc, err = k.Object(context.Background(), "kabc123")
	if err != nil {
		t.Fatal(err)
	}
	_, err = io.ReadAll(rc)
	_ = rc.Close()
	if err == nil || !strings.Contains(err.Error(), "COS refused the key") {
		t.Errorf("a failed child gave %v", err)
	}
}

func TestKopia_ObjectRefusesABadObjectIDWithoutRunningKopia(t *testing.T) {
	fk := newFakeKopia(t)
	k := newTestRepo(t, fk)
	for _, oid := range []string{"", "--help", "k1 k2", "../x", "k1;ls", "a/b", strings.Repeat("a", 600)} {
		if _, err := k.Object(context.Background(), oid); err == nil {
			t.Errorf("Object(%q) worked", oid)
		}
	}
	if argv := fk.read(t, "argv"); argv != "" {
		t.Errorf("Kopia was run: %s", argv)
	}
}

func TestKopia_AtMostProcsRunAtOnce(t *testing.T) {
	fk := newFakeKopia(t)
	fk.set(t, "obj-k1", "x")
	k := NewKopia(KopiaConfig{Binary: fk.bin, StateDir: t.TempDir(), Source: testSource(), Procs: 1})
	first, err := k.Object(context.Background(), "k1")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if _, err = k.Object(ctx, "k1"); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("the second child started while the first was open: %v", err)
	}
	_ = first.Close()
	rc, err := k.Object(context.Background(), "k1")
	if err != nil {
		t.Fatalf("after the first closed: %v", err)
	}
	_ = rc.Close()
}

// The tests below run the real Kopia program against a repository in a folder.
// They skip without it (CI does not install it), and check that the shapes this
// package reads are the shapes Kopia 0.23 writes.
func realKopia(t *testing.T) string {
	t.Helper()
	bin, err := exec.LookPath("kopia")
	if err != nil {
		t.Skip("kopia is not on PATH; install it to run the Kopia-backed tests")
	}
	return bin
}

type realRepo struct {
	t     *testing.T
	bin   string
	state string
	repo  string
	env   []string
}

func newRealRepo(t *testing.T) *realRepo {
	t.Helper()
	bin := realKopia(t)
	state := t.TempDir()
	r := &realRepo{t: t, bin: bin, state: state, repo: filepath.Join(t.TempDir(), "repo")}
	r.env = []string{"PATH=" + os.Getenv("PATH"), "HOME=" + state, "KOPIA_CONFIG_PATH=" + filepath.Join(state, "w.config"),
		"KOPIA_PASSWORD=" + testPassword, "KOPIA_CHECK_FOR_UPDATES=false", "KOPIA_LOG_DIR=" + filepath.Join(state, "wlogs"),
		"KOPIA_CACHE_DIRECTORY=" + filepath.Join(state, "wcache")}
	r.run("repository", "create", "filesystem", "--path="+r.repo, "--no-persist-credentials",
		"--override-username=driver", "--override-hostname=pool-csi")
	return r
}

func (r *realRepo) run(args ...string) {
	r.t.Helper()
	cmd := exec.Command(r.bin, args...) //nolint:gosec // a test
	cmd.Env = r.env
	if out, err := cmd.CombinedOutput(); err != nil {
		r.t.Fatalf("kopia %v: %v\n%s", args, err, out)
	}
}

func (r *realRepo) snapshot(dir, volume string, moment time.Time) {
	r.t.Helper()
	r.run("snapshot", "create", "--no-progress", "--override-source="+dir, "--tags=moment:"+moment.UTC().Format(time.RFC3339),
		"--tags=volume:"+volume, dir)
}

func (r *realRepo) kopia() *KopiaRepo {
	k := NewKopia(KopiaConfig{
		Binary: r.bin, StateDir: filepath.Join(r.t.TempDir(), "reader"), Source: testSource(),
		Password: testPassword, backend: []string{"filesystem", "--path=" + r.repo},
	})
	if err := k.Connect(context.Background()); err != nil {
		r.t.Fatalf("connect: %v", err)
	}
	return k
}

func TestRealKopia_ReadsTheShapesItWrites(t *testing.T) {
	r := newRealRepo(t)
	src := filepath.Join(t.TempDir(), "pvcs", "pvc-11111111-2222-3333-4444-555555555555")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, content string, mode os.FileMode) {
		if err := os.WriteFile(filepath.Join(src, name), []byte(content), mode); err != nil {
			t.Fatal(err)
		}
	}
	write("a.txt", "alpha", 0o600)
	write("sp ace é.txt", "accent", 0o644)
	write("new\nline", "newline", 0o644)
	write("sub/z", "zed", 0o644)
	if err := os.Symlink("/etc", filepath.Join(src, "to-etc")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../escape", filepath.Join(src, "sub", "rel")); err != nil {
		t.Fatal(err)
	}
	r.snapshot(src, "pvc-11111111-2222-3333-4444-555555555555", time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC))

	k := r.kopia()
	snaps, err := k.Snapshots(context.Background())
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots = %v, %v", snaps, err)
	}
	s := snaps[0]
	if s.Volume != "pvc-11111111-2222-3333-4444-555555555555" || s.Moment.IsZero() || !ValidOID(s.Root) || s.SourcePath != src {
		t.Fatalf("snapshot = %+v", s)
	}
	tr := NewTree(k, Root{OID: s.Root}, TreeOptions{})
	defer func() { _ = tr.Close() }()
	page, err := tr.List(nil, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(page), "|"); got != "a.txt|new\nline|sp ace é.txt|sub|to-etc" {
		t.Fatalf("names = %q", got)
	}
	for _, e := range page.Entries {
		if string(e.Name) == "to-etc" && string(e.Link) != "/etc" {
			t.Errorf("link target = %q", e.Link)
		}
		if string(e.Name) == "a.txt" && (e.Mode != 0o600 || e.Size != 5) {
			t.Errorf("a.txt = %+v", e)
		}
	}
	f, err := tr.Open(p("sub/z"))
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 3)
	if n, _ := f.ReadAt(buf, 0); string(buf[:n]) != "zed" {
		t.Errorf("sub/z = %q", buf[:n])
	}
	_ = f.Close()
	if _, err = tr.Open(p("to-etc/passwd")); code(err) != "link_refused" {
		t.Errorf("a path through a link: %v", err)
	}
	if _, err = tr.Open(p("sub/rel")); code(err) != "link_refused" {
		t.Errorf("a relative link: %v", err)
	}
	// A wrong backup key fails with words, and the key is not in them.
	bad := NewKopia(KopiaConfig{Binary: r.bin, StateDir: t.TempDir(), Source: testSource(), Password: "wrong-key-value",
		backend: []string{"filesystem", "--path=" + r.repo}})
	err = bad.Connect(context.Background())
	if err == nil || !strings.Contains(err.Error(), "backup key does not open") || strings.Contains(err.Error(), "wrong-key-value") {
		t.Errorf("a wrong key: %v", err)
	}
}
