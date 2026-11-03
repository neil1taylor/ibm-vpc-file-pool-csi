package guestfs

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filelist"
)

var moment = time.Date(2026, 11, 2, 10, 0, 0, 0, time.UTC)

// roundTrip encodes the list and reads it back, as the export and the session do.
func roundTrip(t *testing.T, l *filelist.List) *filelist.List {
	t.Helper()
	var buf bytes.Buffer
	if err := l.Encode(&buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	back, err := filelist.Read(&buf, filelist.Limits{})
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	return back
}

func listNames(t *testing.T, tr browse.Tree, path ...string) string {
	t.Helper()
	var p [][]byte
	for _, s := range path {
		p = append(p, []byte(s))
	}
	page, err := tr.List(p, nil, 1000)
	if err != nil {
		t.Fatalf("list %v: %v", path, err)
	}
	return strings.Join(names(page.Entries), ",")
}

func TestBuildListReadsEachFileSystemWithOneTool(t *testing.T) {
	f := newFake()
	f.streams[key(lsArgs("/dev/sda1", "/"))] = rootListing
	g := newGuest(t, f, nil)
	l, notes, err := g.BuildList(context.Background(), moment, filelist.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	if n := f.count("virt-ls"); n < 1 {
		t.Fatalf("virt-ls ran %d times", n)
	}
	for _, c := range f.calls {
		if c[0] == "virt-ls" && c[len(c)-1] != "/" {
			t.Errorf("virt-ls was asked for %q; the whole file system is read once, from /", c[len(c)-1])
		}
	}
	tr := filelist.NewTree(roundTrip(t, l), nil)
	if got := listNames(t, tr, "sda1"); got != "etc,hello.txt,link,root" {
		t.Errorf("top = %s", got)
	}
	if got := listNames(t, tr, "sda1", "etc"); got != "hosts" {
		t.Errorf("etc = %s", got)
	}
	if got := listNames(t, tr, "sda1", "root"); got != "" {
		t.Errorf("an empty folder lists %q", got)
	}
	e, err := tr.Stat([][]byte{[]byte("sda1"), []byte("link")})
	if err != nil || e.Type != browse.TypeSymlink || string(e.Link) != "/etc/hosts" {
		t.Errorf("link = %+v, %v", e, err)
	}
	_ = notes
}

func TestBuildListMarksAFileSystemThatCannotBeListed(t *testing.T) {
	f := newFake()
	f.streams[key(lsArgs("/dev/sda1", "/"))] = rootListing
	// the other readable file systems have no stream: their tool fails
	g := newGuest(t, f, nil)
	l, notes, err := g.BuildList(context.Background(), moment, filelist.Limits{})
	if err != nil {
		t.Fatalf("one bad file system must not fail the list: %v", err)
	}
	tr := filelist.NewTree(roundTrip(t, l), nil)
	page, err := tr.List(nil, nil, 100)
	if err != nil {
		t.Fatal(err)
	}
	var bad int
	for _, e := range page.Entries {
		if string(e.Name) == "rhel-root" {
			if e.Readable || e.Note == "" {
				t.Errorf("rhel-root = %+v; want unreadable with a reason", e)
			}
			bad++
		}
	}
	if bad != 1 || len(notes) == 0 {
		t.Errorf("bad=%d notes=%v", bad, notes)
	}
	for _, n := range notes {
		if strings.Contains(n, "/dev/") {
			t.Errorf("note leaks a device name: %q", n)
		}
	}
}

func TestBuildListDropsAFolderWhoseParentIsMissing(t *testing.T) {
	f := newFake()
	// /a/b is listed but /a is not: the list must still be valid
	f.streams[key(lsArgs("/dev/sda1", "/"))] = "d,0755,0,0,0,0,1700000000,0,/\nf,0644,1,0,0,0,1700000000,0,/x\nd,0755,0,0,0,0,1700000000,0,/a/b\nf,0644,1,0,0,0,1700000000,0,/a/b/y\n"
	g := newGuest(t, f, nil)
	l, _, err := g.BuildList(context.Background(), moment, filelist.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	tr := filelist.NewTree(roundTrip(t, l), nil)
	if got := listNames(t, tr, "sda1"); got != "x" {
		t.Errorf("top = %s", got)
	}
}

func TestBuildListSkipsUnsafeNames(t *testing.T) {
	f := newFake()
	f.streams[key(lsArgs("/dev/sda1", "/"))] = "d,0755,0,0,0,0,1700000000,0,/\nd,0755,0,0,0,0,1700000000,0,/.\nf,0644,1,0,0,0,1700000000,0,/ok\nf,0644,1,0,0,0,1700000000,0,/../x\n"
	g := newGuest(t, f, nil)
	l, _, err := g.BuildList(context.Background(), moment, filelist.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	tr := filelist.NewTree(roundTrip(t, l), nil)
	if got := listNames(t, tr, "sda1"); got != "ok" {
		t.Errorf("top = %s", got)
	}
}

func TestBuildListStopsAtTheEntryLimit(t *testing.T) {
	f := newFake()
	var b strings.Builder
	b.WriteString("d,0755,0,0,0,0,1700000000,0,/\n")
	for i := 0; i < 50; i++ {
		b.WriteString("f,0644,1,0,0,0,1700000000,0,/f" + strings.Repeat("x", i) + "\n")
	}
	f.streams[key(lsArgs("/dev/sda1", "/"))] = b.String()
	g := newGuest(t, f, nil)
	if _, _, err := g.BuildList(context.Background(), moment, filelist.Limits{MaxEntries: 10}); err == nil {
		t.Fatal("want an error when the disk holds more entries than the limit")
	}
}

func TestBuildListCheckStopsIt(t *testing.T) {
	f := newFake()
	f.streams[key(lsArgs("/dev/sda1", "/"))] = rootListing
	g := newGuest(t, f, func(o *Options) { o.Check = func() error { return context.Canceled } })
	if _, _, err := g.BuildList(context.Background(), moment, filelist.Limits{}); err == nil {
		t.Fatal("a failed check must stop the build")
	}
}

func TestBuildListCutsALongLinkTargetInsteadOfRefusingTheList(t *testing.T) {
	f := newFake()
	f.streams[key(lsArgs("/dev/sda1", "/"))] = "d,0755,0,0,0,0,1700000000,0,/\nl,0777,9,0,0,0,1700000000,0,/ln," + strings.Repeat("a", 5000) + "\nf,0644,1,0,0,0,1700000000,0,/ok\n"
	g := newGuest(t, f, nil)
	l, _, err := g.BuildList(context.Background(), moment, filelist.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	tr := filelist.NewTree(roundTrip(t, l), nil)
	e, err := tr.Stat([][]byte{[]byte("sda1"), []byte("ln")})
	if err != nil || len(e.Link) != filelist.MaxLinkLen {
		t.Fatalf("link = %d bytes, %v; want it cut to %d", len(e.Link), err, filelist.MaxLinkLen)
	}
}

func TestBuildListMarksAFileSystemWithAnEndlessLine(t *testing.T) {
	f := newFake()
	f.streams[key(lsArgs("/dev/sda1", "/"))] = "d,0755,0,0,0,0,1700000000,0,/\nf,0644,1,0,0,0,1700000000,0,/" + strings.Repeat("x", maxLineLen+10) + "\n"
	g := newGuest(t, f, nil)
	l, notes, err := g.BuildList(context.Background(), moment, filelist.Limits{})
	if err != nil {
		t.Fatalf("a hostile line must not fail the whole list: %v", err)
	}
	tr := filelist.NewTree(roundTrip(t, l), nil)
	page, _ := tr.List(nil, nil, 100)
	for _, e := range page.Entries {
		if string(e.Name) == "sda1" && e.Readable {
			t.Errorf("sda1 should be unreadable: %+v", e)
		}
	}
	if len(notes) == 0 {
		t.Error("want a note")
	}
}

func TestBuildListStopsAtTheByteBudget(t *testing.T) {
	f := newFake()
	var b strings.Builder
	b.WriteString("d,0755,0,0,0,0,1700000000,0,/\n")
	for i := 0; i < 100; i++ {
		b.WriteString("l,0777,1,0,0,0,1700000000,0,/l" + strings.Repeat("n", i) + "," + strings.Repeat("t", 3000) + "\n")
	}
	f.streams[key(lsArgs("/dev/sda1", "/"))] = b.String()
	g := newGuest(t, f, nil)
	if _, _, err := g.BuildList(context.Background(), moment, filelist.Limits{MaxBytes: 100_000}); err == nil {
		t.Fatal("want an error: the links alone are 300 KB and the budget is 100 KB")
	}
}
