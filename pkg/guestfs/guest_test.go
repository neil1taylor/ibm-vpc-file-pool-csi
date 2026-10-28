package guestfs

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

const drive = "nbd+unix:///?socket=/tmp/nbd.sock"

// fakeRunner answers by the exact argument list.
type fakeRunner struct {
	mu      sync.Mutex
	calls   [][]string
	outputs map[string][]byte
	errs    map[string][]error // errors to return, one per call, before the output
	streams map[string]string
}

func newFake() *fakeRunner {
	return &fakeRunner{outputs: map[string][]byte{}, errs: map[string][]error{}, streams: map[string]string{}}
}

func key(args []string) string { return strings.Join(args, "\x1f") }

func (f *fakeRunner) Output(_ context.Context, args []string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	k := key(args)
	if es := f.errs[k]; len(es) > 0 {
		f.errs[k] = es[1:]
		return nil, es[0]
	}
	out, ok := f.outputs[k]
	if !ok {
		return nil, &ExitError{Stderr: "no such output: " + strings.Join(args, " ")}
	}
	return out, nil
}

func (f *fakeRunner) Stream(_ context.Context, args []string) (io.ReadCloser, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	s, ok := f.streams[key(args)]
	if !ok {
		return nil, &ExitError{Stderr: "no such stream"}
	}
	return io.NopCloser(strings.NewReader(s)), nil
}

func (f *fakeRunner) count(tool string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c[0] == tool {
			n++
		}
	}
	return n
}

func lsArgs(device, path string) []string {
	return []string{"virt-ls", "--format=raw", "-a", drive, "-m", device, "-l", "--csv", "--", path}
}

func catArgs(device, path string) []string {
	return []string{"virt-cat", "--format=raw", "-a", drive, "-m", device, "--", path}
}

func fsArgs() []string {
	return []string{"virt-filesystems", "--format=raw", "-a", drive, "--filesystems", "--extra", "--long", "--csv"}
}

const rootListing = "d,0755,4096,etc\nd,0700,4096,root\nf,0644,12,hello.txt\nl,0777,9,link,/etc/hosts\n"

func newGuest(t *testing.T, f *fakeRunner, mutate func(*Options)) *Guest {
	t.Helper()
	f.outputs[key(fsArgs())] = []byte(filesystemsCSV)
	o := Options{Drive: drive, Runner: f}
	if mutate != nil {
		mutate(&o)
	}
	g := New(context.Background(), o)
	if _, err := g.Filesystems(context.Background()); err != nil {
		t.Fatal(err)
	}
	return g
}

func names(es []browse.Entry) []string {
	var out []string
	for _, e := range es {
		out = append(out, string(e.Name))
	}
	return out
}

func TestRootListsTheFileSystems(t *testing.T) {
	g := newGuest(t, newFake(), nil)
	page, err := g.List(nil, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]browse.Entry{}
	for _, e := range page.Entries {
		got[string(e.Name)] = e
		if e.Type != browse.TypeDir {
			t.Errorf("%s is a %s; file systems are folders", e.Name, e.Type)
		}
	}
	if !got["sda1"].Readable || got["sdc1"].Readable || !strings.Contains(got["sdc1"].Note, "LUKS") {
		t.Errorf("entries = %+v", page.Entries)
	}
	if !got["rhel-root"].Readable || got["rhel-root"].Size != 40000000000 {
		t.Errorf("rhel-root = %+v", got["rhel-root"])
	}
	if got := names(page.Entries); !sortedBytes(got) {
		t.Errorf("not in order: %v", got)
	}
}

func sortedBytes(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i-1] >= s[i] {
			return false
		}
	}
	return true
}

func TestListAFolderInAFileSystem(t *testing.T) {
	f := newFake()
	f.outputs[key(lsArgs("/dev/sda1", "/"))] = []byte(rootListing)
	g := newGuest(t, f, nil)
	page, err := g.List([][]byte{[]byte("sda1")}, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(names(page.Entries), ","); got != "etc,hello.txt,link,root" {
		t.Fatalf("names = %s", got)
	}
	byName := map[string]browse.Entry{}
	for _, e := range page.Entries {
		byName[string(e.Name)] = e
	}
	if e := byName["hello.txt"]; e.Type != browse.TypeFile || e.Size != 12 || e.Mode != 0o644 || !e.Readable {
		t.Errorf("hello.txt = %+v", e)
	}
	if e := byName["link"]; e.Type != browse.TypeSymlink || string(e.Link) != "/etc/hosts" {
		t.Errorf("link = %+v", e)
	}
	if e := byName["etc"]; e.Type != browse.TypeDir {
		t.Errorf("etc = %+v", e)
	}
	// A second listing of the same folder does not start another appliance.
	if _, err = g.List([][]byte{[]byte("sda1")}, nil, 100); err != nil {
		t.Fatal(err)
	}
	if n := f.count("virt-ls"); n != 1 {
		t.Errorf("virt-ls ran %d times", n)
	}
}

func TestListPages(t *testing.T) {
	f := newFake()
	f.outputs[key(lsArgs("/dev/sda1", "/"))] = []byte(rootListing)
	g := newGuest(t, f, nil)
	root := [][]byte{[]byte("sda1")}
	p1, err := g.List(root, nil, 3)
	if err != nil || len(p1.Entries) != 3 || len(p1.Next) == 0 {
		t.Fatalf("%v %+v", err, p1)
	}
	p2, err := g.List(root, p1.Next, 3)
	if err != nil || len(p2.Entries) != 1 || string(p2.Entries[0].Name) != "root" || len(p2.Next) != 0 {
		t.Fatalf("%v %+v", err, p2)
	}
}

func TestNamesGoToTheToolAsOneArgumentEach(t *testing.T) {
	f := newFake()
	hostile := "-rf\n$(touch x); `id` ;|\"'"
	f.outputs[key(lsArgs("/dev/sda1", "/"))] = []byte("d,0755,4096,\"" + strings.ReplaceAll(hostile, `"`, `""`) + "\"\n")
	f.outputs[key(lsArgs("/dev/sda1", "/"+hostile))] = []byte("f,0644,1,x\n")
	g := newGuest(t, f, nil)
	if _, err := g.List([][]byte{[]byte("sda1"), []byte(hostile)}, nil, 10); err != nil {
		t.Fatal(err)
	}
	last := f.calls[len(f.calls)-1]
	if last[len(last)-2] != "--" || last[len(last)-1] != "/"+hostile {
		t.Fatalf("args = %q", last)
	}
	for _, a := range last[:len(last)-2] {
		if strings.Contains(a, "touch") {
			t.Fatalf("a name leaked into the options: %q", last)
		}
	}
}

func TestUnreadableFileSystemRefusesInWords(t *testing.T) {
	g := newGuest(t, newFake(), nil)
	_, err := g.List([][]byte{[]byte("sdc1")}, nil, 10)
	var be *browse.Error
	if !errors.As(err, &be) || be.Code != browse.CodeNotReadable || !strings.Contains(be.Msg, "LUKS") {
		t.Fatalf("err = %v", err)
	}
	if _, err = g.Open([][]byte{[]byte("sdc1"), []byte("x")}); !errors.As(err, &be) || be.Code != browse.CodeNotReadable {
		t.Fatalf("open: %v", err)
	}
}

func TestUnknownFileSystem(t *testing.T) {
	g := newGuest(t, newFake(), nil)
	_, err := g.List([][]byte{[]byte("nope")}, nil, 10)
	var be *browse.Error
	if !errors.As(err, &be) || be.Code != browse.CodeNotFound {
		t.Fatalf("err = %v", err)
	}
	if _, err = g.Stat([][]byte{[]byte("nope"), []byte("x")}); !errors.As(err, &be) || be.Code != browse.CodeNotFound {
		t.Fatalf("stat: %v", err)
	}
}

func TestStat(t *testing.T) {
	f := newFake()
	f.outputs[key(lsArgs("/dev/sda1", "/"))] = []byte(rootListing)
	g := newGuest(t, f, nil)
	if e, err := g.Stat(nil); err != nil || e.Type != browse.TypeDir {
		t.Fatalf("root: %+v %v", e, err)
	}
	if e, err := g.Stat([][]byte{[]byte("sda1")}); err != nil || e.Type != browse.TypeDir || string(e.Name) != "sda1" {
		t.Fatalf("fs: %+v %v", e, err)
	}
	e, err := g.Stat([][]byte{[]byte("sda1"), []byte("hello.txt")})
	if err != nil || e.Size != 12 {
		t.Fatalf("file: %+v %v", e, err)
	}
	var be *browse.Error
	if _, err = g.Stat([][]byte{[]byte("sda1"), []byte("missing")}); !errors.As(err, &be) || be.Code != browse.CodeNotFound {
		t.Fatalf("missing: %v", err)
	}
	if e, err = g.Stat([][]byte{[]byte("sda1")}); err != nil || e.Name == nil {
		t.Fatal(err)
	}
}

func TestOpenAndRead(t *testing.T) {
	f := newFake()
	f.outputs[key(lsArgs("/dev/sda1", "/"))] = []byte("f,0644,12,hello.txt\nd,0755,0,etc\nl,0777,3,ln,x\n")
	f.streams[key(catArgs("/dev/sda1", "/hello.txt"))] = "hello world!"
	g := newGuest(t, f, nil)
	file, err := g.Open([][]byte{[]byte("sda1"), []byte("hello.txt")})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if file.Size() != 12 {
		t.Errorf("size = %d", file.Size())
	}
	buf := make([]byte, 5)
	if n, rerr := file.ReadAt(buf, 0); rerr != nil || string(buf[:n]) != "hello" {
		t.Fatalf("%q %v", buf[:n], rerr)
	}
	// Going on from where it stopped reuses the stream; going back starts again.
	if n, rerr := file.ReadAt(buf, 5); rerr != nil || string(buf[:n]) != " worl" {
		t.Fatalf("%q %v", buf[:n], rerr)
	}
	if n, rerr := file.ReadAt(buf, 1); rerr != nil || string(buf[:n]) != "ello " {
		t.Fatalf("%q %v", buf[:n], rerr)
	}
	// A read that reaches the end says so.
	n, err := file.ReadAt(buf, 10)
	if string(buf[:n]) != "d!" || !errors.Is(err, io.EOF) {
		t.Fatalf("%q %v", buf[:n], err)
	}
	var be *browse.Error
	if _, err = g.Open([][]byte{[]byte("sda1"), []byte("etc")}); !errors.As(err, &be) || be.Code != browse.CodeNotFile {
		t.Errorf("folder: %v", err)
	}
	if _, err = g.Open([][]byte{[]byte("sda1"), []byte("ln")}); !errors.As(err, &be) || be.Code != browse.CodeLink {
		t.Errorf("link: %v", err)
	}
}

func TestTarOfAFolder(t *testing.T) {
	f := newFake()
	f.outputs[key(lsArgs("/dev/sda1", "/"))] = []byte("d,0755,0,dir\n")
	f.outputs[key(lsArgs("/dev/sda1", "/dir"))] = []byte("f,0644,3,a\nl,0777,1,ln,a\n")
	f.streams[key(catArgs("/dev/sda1", "/dir/a"))] = "abc"
	g := newGuest(t, f, nil)
	var buf bytes.Buffer
	rep, err := g.Tar(&buf, [][]byte{[]byte("sda1"), []byte("dir")})
	if err != nil {
		t.Fatal(err)
	}
	if rep.Files != 1 || rep.Links != 1 {
		t.Errorf("report = %+v", rep)
	}
}

func TestMountFailureRetriesWithoutReplayingTheJournal(t *testing.T) {
	f := newFake()
	k := key(lsArgs("/dev/sda1", "/"))
	f.errs[k] = []error{&ExitError{Stderr: "libguestfs: error: mount_options: mount(2) system call failed: Structure needs cleaning"}}
	f.outputs[key([]string{"virt-ls", "--format=raw", "-a", drive, "-m", "/dev/sda1:/:norecovery", "-l", "--csv", "--", "/"})] = []byte(rootListing)
	g := newGuest(t, f, nil)
	page, err := g.List([][]byte{[]byte("sda1")}, nil, 10)
	if err != nil || len(page.Entries) != 4 {
		t.Fatalf("%v %+v", err, page)
	}
	root, _ := g.List(nil, nil, 100)
	var note string
	for _, e := range root.Entries {
		if string(e.Name) == "sda1" {
			note = e.Note
		}
	}
	if !strings.Contains(note, "journal") {
		t.Errorf("the file system says nothing about the journal: %q", note)
	}
	if len(g.Notices()) != 1 {
		t.Errorf("notices = %v", g.Notices())
	}
	// Later calls go straight to the working options.
	f.outputs[key([]string{"virt-ls", "--format=raw", "-a", drive, "-m", "/dev/sda1:/:norecovery", "-l", "--csv", "--", "/etc"})] = []byte("f,0644,1,x\n")
	before := f.count("virt-ls")
	if _, err = g.List([][]byte{[]byte("sda1"), []byte("etc")}, nil, 10); err != nil {
		t.Fatal(err)
	}
	if f.count("virt-ls") != before+1 {
		t.Error("a second failing call was made")
	}
}

func TestMountFailureOfExt4UsesNoload(t *testing.T) {
	f := newFake()
	f.outputs[key(fsArgs())] = []byte("Name,Type,VFS,Label,MBR,Size,Parent\n/dev/sdb1,filesystem,ext4,,,1,\n")
	f.errs[key(lsArgs("/dev/sdb1", "/"))] = []error{&ExitError{Stderr: "mount: recovery required on readonly filesystem"}}
	f.outputs[key([]string{"virt-ls", "--format=raw", "-a", drive, "-m", "/dev/sdb1:/:noload", "-l", "--csv", "--", "/"})] = []byte("f,0644,1,x\n")
	g := New(context.Background(), Options{Drive: drive, Runner: f})
	if _, err := g.Filesystems(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := g.List([][]byte{[]byte("sdb1")}, nil, 10); err != nil {
		t.Fatal(err)
	}
}

func TestNTFSIsNotRetried(t *testing.T) {
	f := newFake()
	k := key(lsArgs("/dev/sdb1", "/"))
	f.errs[k] = []error{&ExitError{Stderr: "mount: unsupported"}}
	g := newGuest(t, f, nil)
	if _, err := g.List([][]byte{[]byte("sdb1")}, nil, 10); err == nil {
		t.Fatal("expected an error")
	}
	if n := f.count("virt-ls"); n != 1 {
		t.Errorf("virt-ls ran %d times", n)
	}
}

func TestCrashCopyStartsWithoutTheJournal(t *testing.T) {
	f := newFake()
	f.outputs[key([]string{"virt-ls", "--format=raw", "-a", drive, "-m", "/dev/sda1:/:norecovery", "-l", "--csv", "--", "/"})] = []byte(rootListing)
	g := newGuest(t, f, func(o *Options) { o.Crash = true })
	if _, err := g.List([][]byte{[]byte("sda1")}, nil, 10); err != nil {
		t.Fatal(err)
	}
	if n := f.count("virt-ls"); n != 1 {
		t.Errorf("virt-ls ran %d times", n)
	}
}

func TestACheckThatFailsStopsEveryCall(t *testing.T) {
	f := newFake()
	g := newGuest(t, f, func(o *Options) { o.Check = func() error { return errors.New("the backup disk changed") } })
	_, err := g.List(nil, nil, 10)
	var be *browse.Error
	if !errors.As(err, &be) || be.Code != browse.CodeInternal || !strings.Contains(be.Msg, "changed") {
		t.Fatalf("err = %v", err)
	}
	if _, err = g.Open([][]byte{[]byte("sda1"), []byte("x")}); err == nil {
		t.Fatal("open must fail too")
	}
}

func TestToolFailureIsInternalAndLeaksNothing(t *testing.T) {
	f := newFake()
	f.errs[key(lsArgs("/dev/sda1", "/"))] = []error{&ExitError{Stderr: "secret path /var/lib/x"}}
	g := newGuest(t, f, nil)
	_, err := g.List([][]byte{[]byte("sda1")}, nil, 10)
	var be *browse.Error
	if !errors.As(err, &be) || be.Code != browse.CodeInternal || strings.Contains(be.Msg, "secret") {
		t.Fatalf("err = %v", err)
	}
}

func TestPathsAreValidated(t *testing.T) {
	g := newGuest(t, newFake(), nil)
	for _, p := range [][][]byte{{[]byte("sda1"), []byte("..")}, {[]byte("sda1"), []byte("a/b")}, {[]byte("sda1"), {}}} {
		if _, err := g.List(p, nil, 10); err == nil {
			t.Errorf("%q must be refused", p)
		}
	}
}
