//go:build unix

package filerestore

import (
	"context"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// sockPath is a short socket path: a Unix socket path has a small length limit.
func sockPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "fr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "w.sock")
}

// startWriter runs a writer on a socket in a temporary folder and returns the socket.
func startWriter(t *testing.T, spec WriterSpec) (string, chan error) {
	t.Helper()
	sock := sockPath(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	spec.Socket = sock
	done := make(chan error, 1)
	go func() { _, err := RunWriter(context.Background(), ln, spec); done <- err }()
	return sock, done
}

func waitWriter(t *testing.T, done chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(20 * time.Second):
		t.Fatal("the writer did not end")
		return nil
	}
}

func TestRunJobThroughAWriterWritesTheSameAsLocally(t *testing.T) {
	share, claim := claimDir(t)
	u, g := int64(os.Getuid()), int64(os.Getgid())
	sock, done := startWriter(t, WriterSpec{Mode: ModeFolder, Dest: claim, Staging: StagingName("uid-1"), Policy: "nightly", UID: &u, GID: &g})
	_ = share
	spec := JobSpec{Mode: ModeFolder, Latest: "/src", Writer: sock, UID: &u, GID: &g, Permissions: "0755"}
	out, err := runJob(context.Background(), spec, writePaths(t, "/var/www/html/index.php"), fakeDeps(sampleTree(), "20261022T091500Z"))
	if err != nil {
		t.Fatal(err)
	}
	if err := waitWriter(t, done); err != nil {
		t.Fatal(err)
	}
	if out.Folder != "restored-nightly-20261022T091500Z" || out.Files != 1 || out.Bytes != 13 {
		t.Fatalf("outcome %+v", out)
	}
	if b, rerr := os.ReadFile(filepath.Join(claim, out.Folder, "var/www/html/index.php")); rerr != nil || string(b) != "<?php echo 1;" {
		t.Fatalf("file %q %v", b, rerr)
	}
	if entries, _ := os.ReadDir(claim); len(entries) != 1 {
		t.Fatalf("claim holds %v: the staging folder must be gone", entries)
	}
}

func TestRunJobThroughAWriterIntoANewClaim(t *testing.T) {
	share := t.TempDir()
	u, g := int64(os.Getuid()), int64(os.Getgid())
	vol := "pvcs/pvc-0a1b2c3d-0a1b-0a1b-0a1b-0a1b2c3d4e5f"
	sock, done := startWriter(t, WriterSpec{Mode: ModeNewClaim, Dest: share, Volume: vol, UID: &u, GID: &g})
	out, err := runJob(context.Background(), JobSpec{Mode: ModeNewClaim, Latest: "/src", Writer: sock, UID: &u, GID: &g, Permissions: "0777"},
		writePaths(t, "/var/www/html/secret.key"), fakeDeps(sampleTree(), ""))
	if err != nil {
		t.Fatal(err)
	}
	if err := waitWriter(t, done); err != nil {
		t.Fatal(err)
	}
	if b, rerr := os.ReadFile(filepath.Join(share, vol, "var/www/html/secret.key")); rerr != nil || string(b) != "s3cret" || out.Folder != "" {
		t.Fatalf("file %q %v outcome %+v", b, rerr, out)
	}
}

func TestWriterTellsTheReaderWhyItCannotUseTheTarget(t *testing.T) {
	_, claim := claimDir(t)
	mustWrite(t, filepath.Join(claim, "disk.img"), "x")
	u, g := int64(os.Getuid()), int64(os.Getgid())
	sock, done := startWriter(t, WriterSpec{Mode: ModeFolder, Dest: claim, Staging: StagingName("u"), Policy: "p", UID: &u, GID: &g})
	out, err := runJob(context.Background(), JobSpec{Mode: ModeFolder, Latest: "/src", Writer: sock, UID: &u, GID: &g},
		writePaths(t, "/var"), fakeDeps(sampleTree(), ""))
	if err == nil || !strings.Contains(out.Error, "VM disk") {
		t.Fatalf("err %v outcome %+v", err, out)
	}
	_ = waitWriter(t, done)
}

func TestWriterRemovesWhatWasWrittenWhenTheReaderFails(t *testing.T) {
	_, claim := claimDir(t)
	u, g := int64(os.Getuid()), int64(os.Getgid())
	sock, done := startWriter(t, WriterSpec{Mode: ModeFolder, Dest: claim, Staging: StagingName("u"), Policy: "p", UID: &u, GID: &g})
	out, err := runJob(context.Background(), JobSpec{Mode: ModeFolder, Latest: "/src", Writer: sock, UID: &u, GID: &g},
		writePaths(t, "/no/such/file"), fakeDeps(sampleTree(), ""))
	if err == nil || out.Error == "" {
		t.Fatalf("err %v outcome %+v", err, out)
	}
	if err := waitWriter(t, done); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(claim); len(entries) != 0 {
		t.Fatalf("left in the claim: %v", entries)
	}
}

func TestWriterEndsWhenTheReaderDropsOutAndLeavesNothing(t *testing.T) {
	_, claim := claimDir(t)
	u, g := int64(os.Getuid()), int64(os.Getgid())
	sock, done := startWriter(t, WriterSpec{Mode: ModeFolder, Dest: claim, Staging: StagingName("u"), Policy: "p", UID: &u, GID: &g})
	rd, err := DialWriter(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	if err := rd.Mkdir([][]byte{[]byte("a")}); err != nil {
		t.Fatal(err)
	}
	_ = rd.Close() // the reader dies without a commit
	if err := waitWriter(t, done); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(claim); len(entries) != 0 {
		t.Fatalf("left in the claim: %v", entries)
	}
}

func TestWriterGivesUpOnAReaderThatNeverComes(t *testing.T) {
	_, claim := claimDir(t)
	ln, err := net.Listen("unix", sockPath(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		u, g := int64(1), int64(1)
		_, err := RunWriter(ctx, ln, WriterSpec{Mode: ModeFolder, Socket: "/x", Dest: claim, Staging: StagingName("u"), Policy: "p", UID: &u, GID: &g})
		done <- err
	}()
	cancel()
	if err := waitWriter(t, done); err == nil {
		t.Fatal("no error for a reader that never came")
	}
}

// hostile is a reader that sends what a compromised reader would.
func hostile(t *testing.T, spec WriterSpec) (*RemoteDest, string, chan error) {
	t.Helper()
	if spec.UID == nil {
		u, g := int64(os.Getuid()), int64(os.Getgid())
		spec.UID, spec.GID = &u, &g
	}
	_, claim := claimDir(t)
	spec.Dest, spec.Mode, spec.Staging, spec.Policy = claim, ModeFolder, StagingName("u"), "p"
	sock, done := startWriter(t, spec)
	rd, err := DialWriter(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = rd.Close() })
	return rd, claim, done
}

func TestWriterChecksEveryPathAgain(t *testing.T) {
	rd, _, _ := hostile(t, WriterSpec{})
	for name, path := range map[string][][]byte{
		"dot dot":  {[]byte("..")},
		"slash":    {[]byte("a/b")},
		"empty":    {},
		"too deep": make([][]byte, 5000),
	} {
		if err := rd.Mkdir(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := rd.Create([][]byte{[]byte("..")}); err == nil {
		t.Error("a file named .. was accepted")
	}
}

func TestWriterStripsSetuidAndForcesTheOwnerOfItsPolicy(t *testing.T) {
	u, g := int64(os.Getuid()), int64(os.Getgid())
	rd, claim, _ := hostile(t, WriterSpec{UID: &u, GID: &g})
	other := int64(4242)
	f, err := rd.Create([][]byte{[]byte("evil")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.Write([]byte("x")); err != nil {
		t.Fatal(err)
	}
	if err = f.Finish(Meta{Mode: 0o6755, UID: &other, GID: &other}); err != nil {
		t.Fatalf("finish: %v (the owner of the policy must replace the reader's, so chown to another user is never tried)", err)
	}
	st, err := os.Stat(filepath.Join(claim, ".restoring-u", "evil"))
	if err != nil {
		entries, _ := os.ReadDir(claim)
		t.Fatalf("%v; claim holds %v", err, entries)
	}
	if st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		t.Errorf("mode %v keeps setuid or setgid", st.Mode())
	}
}

func TestWriterCountsTheBytesItselfAndStopsAtItsLimit(t *testing.T) {
	rd, _, _ := hostile(t, WriterSpec{MaxBytes: 10})
	f, err := rd.Create([][]byte{[]byte("big")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(strings.Repeat("x", 11))); err != nil {
		t.Fatal(err) // content is sent without waiting; the answer comes at the end
	}
	if err := f.Finish(Meta{Mode: 0o644}); err == nil {
		t.Fatal("11 bytes were accepted under a limit of 10")
	}
}

func TestWriterNamesTheFolderItself(t *testing.T) {
	rd, claim, _ := hostile(t, WriterSpec{})
	name, err := rd.CommitMoment("../../etc", time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if name != "restored-p-20261022T091500Z" {
		t.Fatalf("name %q: it comes from the writer's policy, not from the reader", name)
	}
	if _, err := os.Stat(filepath.Join(claim, name)); err != nil {
		t.Fatal(err)
	}
}

func TestWriterRefusesToCommitANewClaim(t *testing.T) {
	share := t.TempDir()
	u, g := int64(os.Getuid()), int64(os.Getgid())
	sock, _ := startWriter(t, WriterSpec{Mode: ModeNewClaim, Dest: share, Volume: "pvcs/pvc-0a1b2c3d-0a1b-0a1b-0a1b-0a1b2c3d4e5f", UID: &u, GID: &g})
	rd, err := DialWriter(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rd.Close() }()
	if _, err := rd.CommitMoment("p", time.Time{}); err == nil {
		t.Fatal("a new claim has no folder to commit")
	}
}

func TestFolderDestAtTheClaimFolderRefusesAVMDisk(t *testing.T) {
	_, claim := claimDir(t)
	mustWrite(t, filepath.Join(claim, "disk.img"), "x")
	if _, err := NewFolderDestAt(claim, StagingName("u"), 0); err != ErrVMDiskClaim {
		t.Fatalf("err = %v", err)
	}
}

func TestWriterSpecIsChecked(t *testing.T) {
	one := int64(1)
	for name, s := range map[string]WriterSpec{
		"no owner":                    {Mode: ModeFolder, Socket: "/s", Dest: "/d", Staging: StagingName("u"), Policy: "p"},
		"no gid":                      {Mode: ModeFolder, Socket: "/s", Dest: "/d", Staging: StagingName("u"), Policy: "p", UID: &one},
		"folder owner on a new claim": {Mode: ModeNewClaim, Socket: "/s", Dest: "/d", Volume: "pvcs/x", OwnerFromFolder: true},
		"folder owner and uid":        {Mode: ModeFolder, Socket: "/s", Dest: "/d", Staging: StagingName("u"), Policy: "p", OwnerFromFolder: true, UID: &one, GID: &one},
		"bad permissions":             {Mode: ModeFolder, Socket: "/s", Dest: "/d", Staging: StagingName("u"), Policy: "p", UID: &one, GID: &one, Permissions: "4777"},
		"relative":                    {Mode: ModeFolder, Socket: "s", Dest: "/d", Staging: StagingName("u"), Policy: "p"},
		"new claim, no vol":           {Mode: ModeNewClaim, Socket: "/s", Dest: "/d"},
		"folder, no policy":           {Mode: ModeFolder, Socket: "/s", Dest: "/d", Staging: StagingName("u")},
		"bad staging":                 {Mode: ModeFolder, Socket: "/s", Dest: "/d", Staging: "../x", Policy: "p"},
		"measure":                     {Mode: ModeMeasure, Socket: "/s", Dest: "/d"},
	} {
		if err := s.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestJobThatUsesAWriterNamesNoTarget(t *testing.T) {
	deps := fakeDeps(sampleTree(), "")
	for name, s := range map[string]JobSpec{
		"with dest":       {Mode: ModeFolder, Latest: "/src", Writer: "/w.sock", Dest: "/d"},
		"relative writer": {Mode: ModeFolder, Latest: "/src", Writer: "w.sock"},
	} {
		if _, err := runJob(context.Background(), s, writePaths(t, "/a"), deps); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// smallDest is a destination with a small free space.
type smallDest struct {
	WriterDest
	free int64
}

func (d smallDest) Free() (int64, error) { return d.free, nil }

func TestWriterLimitsTheSessionToTheFreeSpaceOfTheTarget(t *testing.T) {
	_, claim := claimDir(t)
	real, err := NewFolderDestAt(claim, StagingName("u"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = real.Close() }()
	a, b := net.Pipe()
	u, g := int64(os.Getuid()), int64(os.Getgid())
	// MaxBytes is not set: the default (200 GiB) must not win over 10 free bytes.
	go func() {
		_, _ = ServeWriter(b, smallDest{real, 10}, WriterPolicy{Mode: ModeFolder, Policy: "p", Owner: Owner{UID: &u, GID: &g}})
	}()
	rd, err := newRemoteDest(a)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rd.Close() }()
	f, err := rd.Create([][]byte{[]byte("big")})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte(strings.Repeat("x", 11))); err != nil {
		t.Fatal(err)
	}
	if err := f.Finish(Meta{Mode: 0o644}); err == nil {
		t.Fatal("11 bytes were accepted with 10 bytes free")
	}
}

func TestWriterRefusesASessionWithoutAnOwner(t *testing.T) {
	_, claim := claimDir(t)
	real, err := NewFolderDestAt(claim, StagingName("u"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = real.Close() }()
	a, b := net.Pipe()
	errc := make(chan error, 1)
	go func() { _, e := ServeWriter(b, real, WriterPolicy{Mode: ModeFolder, Policy: "p"}); errc <- e }()
	if _, err := newRemoteDest(a); err == nil {
		t.Fatal("the session opened without an owner")
	}
	if err := <-errc; err == nil {
		t.Fatal("no error for a policy without an owner")
	}
}

func TestWriterIgnoresTheOwnerAndTheModeBitsTheReaderAsksFor(t *testing.T) {
	u, g := int64(os.Getuid()), int64(os.Getgid())
	rd, claim, _ := hostile(t, WriterSpec{UID: &u, GID: &g, Permissions: "0750"})
	root, other := int64(0), int64(0)
	for name, mode := range map[string]uint32{"setuid": 0o4777, "sticky": 0o1777} {
		f, err := rd.Create([][]byte{[]byte(name)})
		if err != nil {
			t.Fatal(err)
		}
		if err = f.Finish(Meta{Mode: mode, UID: &root, GID: &other}); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		st, err := os.Stat(filepath.Join(claim, ".restoring-u", name))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 || st.Mode().Perm() != 0o777 {
			t.Errorf("%s: mode %v", name, st.Mode())
		}
		if sys, ok := st.Sys().(*syscall.Stat_t); ok && (int64(sys.Uid) != u || int64(sys.Gid) != g) {
			t.Errorf("%s: owner %d:%d, want the policy's", name, sys.Uid, sys.Gid)
		}
	}
	if merr := rd.Mkdir([][]byte{[]byte("d")}); merr != nil {
		t.Fatal(merr)
	}
	if err := rd.FinishDir([][]byte{[]byte("d")}, Meta{Mode: 0o1777, UID: &root}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(claim, ".restoring-u", "d")); st.Mode()&os.ModeSticky != 0 {
		t.Error("a folder kept the sticky bit")
	}
	// The root folder: the writer's own mode, whatever the reader says.
	if err := rd.FinishDir(nil, Meta{Mode: 0o4777, UID: &root}); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(claim, ".restoring-u")); st.Mode().Perm() != 0o750 || st.Mode()&(os.ModeSetuid|os.ModeSticky) != 0 {
		t.Errorf("root folder mode %v, want 0750", st.Mode())
	}
}

func TestWriterMeasuresTheOwnerOfTheClaimFolderAndKeepsItForTheSession(t *testing.T) {
	_, claim := claimDir(t)
	sock, done := startWriter(t, WriterSpec{Mode: ModeFolder, Dest: claim, Staging: StagingName("u"), Policy: "p", OwnerFromFolder: true})
	rd, err := DialWriter(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	root, other := int64(0), int64(4242)
	f, err := rd.Create([][]byte{[]byte("f")})
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Finish(Meta{Mode: 0o644, UID: &root, GID: &other}); err != nil {
		t.Fatalf("finish: %v: the reader's owner must not be used", err)
	}
	st, err := os.Stat(filepath.Join(claim, ".restoring-u", "f"))
	if err != nil {
		t.Fatal(err)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || int(sys.Uid) != os.Getuid() || int(sys.Gid) != os.Getgid() {
		t.Errorf("owner %+v, want the owner of the claim folder", st.Sys())
	}
	_ = rd.Close()
	_ = waitWriter(t, done)
}

func TestServeWriterWithoutAnOwnerFromAnywhereIsRefused(t *testing.T) {
	_, claim := claimDir(t)
	real, err := NewFolderDestAt(claim, StagingName("u"), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = real.Close() }()
	a, b := net.Pipe()
	go func() { _, _ = ServeWriter(b, real, WriterPolicy{Mode: ModeFolder, Policy: "p"}) }()
	if _, err := newRemoteDest(a); err == nil {
		t.Fatal("opened without an owner")
	}
}

func TestWriterReportsWhatItWroteAndTheFolderItCommitted(t *testing.T) {
	u, g := int64(os.Getuid()), int64(os.Getgid())
	_, claim := claimDir(t)
	sock := sockPath(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	type result struct {
		out WriterOutcome
		err error
	}
	done := make(chan result, 1)
	go func() {
		out, rerr := RunWriter(context.Background(), ln, WriterSpec{Mode: ModeFolder, Socket: sock, Dest: claim,
			Staging: StagingName("u"), Policy: "p", UID: &u, GID: &g})
		done <- result{out, rerr}
	}()
	rd, err := DialWriter(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	if merr := rd.Mkdir([][]byte{[]byte("d")}); merr != nil {
		t.Fatal(merr)
	}
	f, err := rd.Create([][]byte{[]byte("d"), []byte("f")})
	if err != nil {
		t.Fatal(err)
	}
	if _, werr := f.Write([]byte("hello")); werr != nil {
		t.Fatal(werr)
	}
	if ferr := f.Finish(Meta{Mode: 0o644}); ferr != nil {
		t.Fatal(ferr)
	}
	if lerr := rd.Symlink([][]byte{[]byte("l")}, []byte("d/f"), Meta{Mode: 0o777}); lerr != nil {
		t.Fatal(lerr)
	}
	name, err := rd.CommitMoment("ignored", time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	_ = rd.Close()
	r := <-done
	if r.err != nil {
		t.Fatal(r.err)
	}
	want := WriterOutcome{Result: Result{Files: 1, Dirs: 1, Links: 1, Bytes: 5}, Folder: name}
	if r.out != want {
		t.Fatalf("writer outcome = %+v, want %+v", r.out, want)
	}
}

func TestWriterDoesNotCountAFileThatFailed(t *testing.T) {
	u, g := int64(os.Getuid()), int64(os.Getgid())
	_, claim := claimDir(t)
	sock := sockPath(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan WriterOutcome, 1)
	go func() {
		out, _ := RunWriter(context.Background(), ln, WriterSpec{Mode: ModeFolder, Socket: sock, Dest: claim,
			Staging: StagingName("u"), Policy: "p", UID: &u, GID: &g, MaxBytes: 10})
		done <- out
	}()
	rd, err := DialWriter(context.Background(), sock)
	if err != nil {
		t.Fatal(err)
	}
	f, err := rd.Create([][]byte{[]byte("big")})
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.Write([]byte(strings.Repeat("x", 11)))
	if err := f.Finish(Meta{Mode: 0o644}); err == nil {
		t.Fatal("11 bytes were accepted under a limit of 10")
	}
	_ = rd.Close()
	if out := <-done; out != (WriterOutcome{}) {
		t.Fatalf("outcome %+v: nothing was written", out)
	}
}

func TestMainWriterWritesItsOutcomeToTheTerminationLog(t *testing.T) {
	u, g := int64(os.Getuid()), int64(os.Getgid())
	_, claim := claimDir(t)
	sock := sockPath(t)
	spec := WriterSpec{Mode: ModeFolder, Socket: sock, Dest: claim, Staging: StagingName("u"), Policy: "p", UID: &u, GID: &g}
	b, _ := json.Marshal(spec)
	termLog := filepath.Join(t.TempDir(), "term")
	code := make(chan int, 1)
	go func() { code <- MainWriter(string(b), termLog) }()
	var rd *RemoteDest
	var err error
	for i := 0; i < 100; i++ {
		if rd, err = DialWriter(context.Background(), sock); err == nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil {
		t.Fatal(err)
	}
	f, _ := rd.Create([][]byte{[]byte("f")})
	_, _ = f.Write([]byte("abc"))
	if ferr := f.Finish(Meta{Mode: 0o644}); ferr != nil {
		t.Fatal(ferr)
	}
	name, err := rd.CommitMoment("x", time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	_ = rd.Close()
	if c := <-code; c != 0 {
		t.Fatalf("exit code %d", c)
	}
	raw, err := os.ReadFile(termLog)
	if err != nil {
		t.Fatal(err)
	}
	var got WriterOutcome
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("%q: %v", raw, err)
	}
	if got.Files != 1 || got.Bytes != 3 || got.Folder != name {
		t.Fatalf("termination log %q", raw)
	}
}
