package filelist

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

var t0 = time.Date(2026, 11, 2, 12, 12, 36, 0, time.UTC)

func dir(name string) browse.Entry {
	return browse.Entry{Name: []byte(name), Type: browse.TypeDir, Mode: 0o755, Readable: true, MTime: t0}
}

func file(name string, size int64) browse.Entry {
	return browse.Entry{Name: []byte(name), Type: browse.TypeFile, Size: size, Mode: 0o644, UID: 1000, GID: 1000, Readable: true, MTime: t0}
}

func sample(t *testing.T) *List {
	t.Helper()
	l := NewList(t0)
	l.AddFilesystem(Filesystem{ID: "sda1", VFS: "xfs", Label: "root", Size: 10 << 30, Readable: true})
	l.AddFilesystem(Filesystem{ID: "sda2", VFS: "crypto_LUKS", Readable: false, Reason: "This volume is encrypted inside the VM."})
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(l.AddFolder("sda1", nil, []browse.Entry{dir("etc"), dir("data"), file("README", 12), {Name: []byte("lnk"), Type: browse.TypeSymlink, Link: []byte("etc/hosts"), Readable: true}}))
	must(l.AddFolder("sda1", [][]byte{[]byte("etc")}, []browse.Entry{file("hosts", 99), file("passwd", 1500)}))
	must(l.AddFolder("sda1", [][]byte{[]byte("data")}, []browse.Entry{file("f3", 3), file("f1", 1), file("f2", 2)}))
	return l
}

func roundTrip(t *testing.T, l *List) *List {
	t.Helper()
	var buf bytes.Buffer
	if err := l.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	got, err := Read(&buf, Limits{})
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	return got
}

func TestRoundTripKeepsTheMomentAndTheFolders(t *testing.T) {
	got := roundTrip(t, sample(t))
	if !got.Moment().Equal(t0) {
		t.Fatalf("moment = %v", got.Moment())
	}
	tree := NewTree(got, nil)
	page, err := tree.List([][]byte{[]byte("sda1"), []byte("etc")}, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 || string(page.Entries[0].Name) != "hosts" || page.Entries[1].Size != 1500 {
		t.Fatalf("etc = %+v", page.Entries)
	}
}

func TestTopLevelListsTheFilesystemsWithTheirNotes(t *testing.T) {
	tree := NewTree(roundTrip(t, sample(t)), nil)
	page, err := tree.List(nil, nil, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 {
		t.Fatalf("top = %+v", page.Entries)
	}
	a, b := page.Entries[0], page.Entries[1]
	if string(a.Name) != "sda1" || !a.Readable || a.Type != browse.TypeDir || !strings.Contains(a.Note, "xfs") {
		t.Fatalf("sda1 = %+v", a)
	}
	if string(b.Name) != "sda2" || b.Readable || b.Note != "This volume is encrypted inside the VM." {
		t.Fatalf("sda2 = %+v", b)
	}
}

func TestEntriesComeInByteOrderAndPage(t *testing.T) {
	tree := NewTree(roundTrip(t, sample(t)), nil)
	p := [][]byte{[]byte("sda1"), []byte("data")}
	page, err := tree.List(p, nil, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 2 || string(page.Entries[0].Name) != "f1" || string(page.Next) != "f2" {
		t.Fatalf("page 1 = %+v next=%q", page.Entries, page.Next)
	}
	page, err = tree.List(p, page.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || string(page.Entries[0].Name) != "f3" || len(page.Next) != 0 {
		t.Fatalf("page 2 = %+v next=%q", page.Entries, page.Next)
	}
}

func TestNamesWithControlBytesSurviveAsBytes(t *testing.T) {
	l := NewList(t0)
	l.AddFilesystem(Filesystem{ID: "sda1", VFS: "xfs", Readable: true})
	weird := []byte("a\nb\x1b[31m\xff\xfe")
	if err := l.AddFolder("sda1", nil, []browse.Entry{{Name: weird, Type: browse.TypeFile, Readable: true}}); err != nil {
		t.Fatal(err)
	}
	page, err := NewTree(roundTrip(t, l), nil).List([][]byte{[]byte("sda1")}, nil, 10)
	if err != nil || len(page.Entries) != 1 || !bytes.Equal(page.Entries[0].Name, weird) {
		t.Fatalf("got %+v, %v", page.Entries, err)
	}
}

func TestListErrors(t *testing.T) {
	tree := NewTree(roundTrip(t, sample(t)), nil)
	cases := []struct {
		name string
		path []string
		code string
	}{
		{"unknown file system", []string{"sdz9"}, browse.CodeNotFound},
		{"unknown folder", []string{"sda1", "nope"}, browse.CodeNotFound},
		{"a file is not a folder", []string{"sda1", "README"}, browse.CodeNotDir},
		{"a folder below a file", []string{"sda1", "README", "x"}, browse.CodeNotDir},
		{"a link is not followed", []string{"sda1", "lnk", "hosts"}, browse.CodeNotDir},
		{"unreadable volume", []string{"sda2"}, browse.CodeNotReadable},
		{"dotdot", []string{"sda1", ".."}, browse.CodeBadPath},
	}
	for _, c := range cases {
		var p [][]byte
		for _, s := range c.path {
			p = append(p, []byte(s))
		}
		_, err := tree.List(p, nil, 10)
		var be *browse.Error
		if !errors.As(err, &be) || be.Code != c.code {
			t.Errorf("%s: err = %v; want code %s", c.name, err, c.code)
		}
	}
}

func TestStat(t *testing.T) {
	tree := NewTree(roundTrip(t, sample(t)), nil)
	e, err := tree.Stat([][]byte{[]byte("sda1"), []byte("etc"), []byte("passwd")})
	if err != nil || e.Size != 1500 || e.Type != browse.TypeFile {
		t.Fatalf("passwd = %+v, %v", e, err)
	}
	if e, err = tree.Stat(nil); err != nil || e.Type != browse.TypeDir {
		t.Fatalf("root = %+v, %v", e, err)
	}
	if e, err = tree.Stat([][]byte{[]byte("sda1")}); err != nil || string(e.Name) != "sda1" {
		t.Fatalf("fs = %+v, %v", e, err)
	}
	if _, err = tree.Stat([][]byte{[]byte("sda1"), []byte("etc"), []byte("shadow")}); err == nil {
		t.Fatal("want not found")
	}
}

// diskTree records what a download asks the disk for.
type diskTree struct {
	browse.Tree
	opened [][]byte
}

func (d *diskTree) Open(path [][]byte) (browse.File, error) {
	d.opened = path
	return nil, &browse.Error{Code: browse.CodeNotFound, Msg: "not on the disk"}
}

func TestDownloadsGoToTheDiskNotTheList(t *testing.T) {
	disk := &diskTree{}
	tree := NewTree(roundTrip(t, sample(t)), disk)
	_, err := tree.Open([][]byte{[]byte("sda1"), []byte("etc"), []byte("hosts")})
	var be *browse.Error
	if !errors.As(err, &be) || be.Code != browse.CodeNotFound {
		t.Fatalf("err = %v; a path that is only in the list must be refused by the disk", err)
	}
	if len(disk.opened) != 3 {
		t.Fatalf("the disk was not asked: %v", disk.opened)
	}
}

func TestWithoutADiskDownloadsSayTheDiskIsNotOpen(t *testing.T) {
	tree := NewTree(roundTrip(t, sample(t)), nil)
	if _, err := tree.Open([][]byte{[]byte("sda1"), []byte("README")}); err == nil {
		t.Fatal("want an error")
	}
	if _, err := tree.Tar(io.Discard, [][]byte{[]byte("sda1")}); err == nil {
		t.Fatal("want an error")
	}
}

// ---- A list from a hostile disk is untrusted ----

type rawFolder struct {
	FS      string         `json:"fs"`
	Path    [][]byte       `json:"path"`
	Entries []browse.Entry `json:"entries"`
}

func gz(t *testing.T, lines ...any) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

type rawHeader struct {
	V      int       `json:"v"`
	Moment time.Time `json:"moment"`
	FS     []struct {
		ID       string `json:"id"`
		VFS      string `json:"vfs"`
		Readable bool   `json:"readable"`
	} `json:"fs"`
}

func hdr(ids ...string) rawHeader {
	h := rawHeader{V: 1, Moment: t0}
	for _, id := range ids {
		h.FS = append(h.FS, struct {
			ID       string `json:"id"`
			VFS      string `json:"vfs"`
			Readable bool   `json:"readable"`
		}{ID: id, VFS: "xfs", Readable: true})
	}
	return h
}

func TestReadRefusesCraftedLists(t *testing.T) {
	longName := bytes.Repeat([]byte("n"), browse.MaxNameLen+1)
	cases := []struct {
		name string
		data []byte
	}{
		{"not gzip", []byte("hello")},
		{"empty", gz(t)},
		{"wrong version", gz(t, rawHeader{V: 2, Moment: t0})},
		{"folder of an unknown file system", gz(t, hdr("sda1"), rawFolder{FS: "evil", Entries: []browse.Entry{file("x", 1)}})},
		{"name dotdot", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Entries: []browse.Entry{file("..", 1)}})},
		{"name with slash", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Entries: []browse.Entry{file("a/b", 1)}})},
		{"empty name", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Entries: []browse.Entry{file("", 1)}})},
		{"NUL in name", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Entries: []browse.Entry{file("a\x00b", 1)}})},
		{"name too long", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Entries: []browse.Entry{{Name: longName, Type: browse.TypeFile}}})},
		{"duplicate name", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Entries: []browse.Entry{file("a", 1), file("a", 2)}})},
		{"negative size", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Entries: []browse.Entry{file("a", -1)}})},
		{"unknown type", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Entries: []browse.Entry{{Name: []byte("a"), Type: "wormhole"}}})},
		{"link text too long", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Entries: []browse.Entry{{Name: []byte("a"), Type: browse.TypeSymlink, Link: bytes.Repeat([]byte("l"), maxLinkLen+1)}}})},
		{"folder not listed in its parent", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Entries: []browse.Entry{dir("a")}}, rawFolder{FS: "sda1", Path: [][]byte{[]byte("ghost")}, Entries: []browse.Entry{file("x", 1)}})},
		{"folder below a file", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Entries: []browse.Entry{file("a", 1)}}, rawFolder{FS: "sda1", Path: [][]byte{[]byte("a")}, Entries: []browse.Entry{file("x", 1)}})},
		{"the same folder twice", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Entries: []browse.Entry{dir("a")}}, rawFolder{FS: "sda1", Path: [][]byte{[]byte("a")}}, rawFolder{FS: "sda1", Path: [][]byte{[]byte("a")}})},
		{"path with dotdot", gz(t, hdr("sda1"), rawFolder{FS: "sda1", Path: [][]byte{[]byte("..")}})},
		{"two file systems with one id", gz(t, hdr("sda1", "sda1"))},
	}
	for _, c := range cases {
		if _, err := Read(bytes.NewReader(c.data), Limits{}); err == nil {
			t.Errorf("%s: want a refusal", c.name)
		}
	}
}

func TestReadStopsAtTheLimits(t *testing.T) {
	l := NewList(t0)
	l.AddFilesystem(Filesystem{ID: "sda1", VFS: "xfs", Readable: true})
	var entries []browse.Entry
	for i := 0; i < 500; i++ {
		entries = append(entries, file(fmt.Sprintf("file-%04d", i), int64(i)))
	}
	if err := l.AddFolder("sda1", nil, entries); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := l.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	data := buf.Bytes()
	if _, err := Read(bytes.NewReader(data), Limits{MaxEntries: 100}); err == nil {
		t.Error("entry limit: want a refusal")
	}
	if _, err := Read(bytes.NewReader(data), Limits{MaxBytes: 1000}); err == nil {
		t.Error("size limit: want a refusal; a small file can unpack to a huge one")
	}
	if _, err := Read(bytes.NewReader(data), Limits{MaxFilesystems: 0}); err != nil {
		t.Errorf("default limits: %v", err)
	}
}

func TestReadRefusesAListThatUnpacksToTooMuch(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_ = json.NewEncoder(zw).Encode(hdr("sda1"))
	_, _ = zw.Write([]byte(`{"fs":"sda1","entries":[{"name":"`))
	_, _ = zw.Write(bytes.Repeat([]byte("QUJD"), 1<<18)) // 1 MiB of base64
	_ = zw.Close()
	if _, err := Read(&buf, Limits{MaxBytes: 64 << 10}); err == nil {
		t.Fatal("want a refusal")
	}
}

func TestAddFolderRefusesWhatReadRefuses(t *testing.T) {
	l := NewList(t0)
	l.AddFilesystem(Filesystem{ID: "sda1", VFS: "xfs", Readable: true})
	if err := l.AddFolder("sda1", nil, []browse.Entry{file("a/b", 1)}); err == nil {
		t.Fatal("want a refusal")
	}
	if err := l.AddFolder("nope", nil, nil); err == nil {
		t.Fatal("want a refusal for an unknown file system")
	}
}

func FuzzRead(f *testing.F) {
	var buf bytes.Buffer
	l := NewList(t0)
	l.AddFilesystem(Filesystem{ID: "sda1", VFS: "xfs", Readable: true})
	_ = l.AddFolder("sda1", nil, []browse.Entry{dir("etc"), file("a", 1)})
	_ = l.Encode(&buf)
	f.Add(buf.Bytes())
	f.Add([]byte("garbage"))
	f.Fuzz(func(t *testing.T, data []byte) {
		got, err := Read(bytes.NewReader(data), Limits{MaxBytes: 1 << 20, MaxEntries: 10000})
		if err != nil {
			return
		}
		// Whatever was accepted must be listable without a panic.
		_, _ = NewTree(got, nil).List(nil, nil, 100)
	})
}

// countReader counts the bytes taken from it.
type countReader struct {
	r io.Reader
	n int
}

func (c *countReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func TestReadStopsEarlyWhenTheEntryLimitIsPassed(t *testing.T) {
	l := NewList(t0)
	l.AddFilesystem(Filesystem{ID: "sda1", VFS: "xfs", Readable: true})
	var entries []browse.Entry
	for i := 0; i < 200_000; i++ {
		entries = append(entries, file(fmt.Sprintf("file-%07d", i), int64(i)))
	}
	if err := l.AddFolder("sda1", nil, entries); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := l.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	total := buf.Len()
	cr := &countReader{r: &buf}
	if _, err := Read(cr, Limits{MaxEntries: 100}); err == nil {
		t.Fatal("want a refusal")
	}
	if cr.n > total/10 {
		t.Fatalf("read %d of %d compressed bytes before refusing; the entry count must be enforced while reading, not after a whole folder is in memory", cr.n, total)
	}
}

func TestReadStopsEarlyWhenTheFilesystemLimitIsPassed(t *testing.T) {
	h := rawHeader{V: 1, Moment: t0}
	for i := 0; i < 100_000; i++ {
		h.FS = append(h.FS, struct {
			ID       string `json:"id"`
			VFS      string `json:"vfs"`
			Readable bool   `json:"readable"`
		}{ID: fmt.Sprintf("fs%d", i), VFS: "xfs", Readable: true})
	}
	data := gz(t, h)
	cr := &countReader{r: bytes.NewReader(data)}
	if _, err := Read(cr, Limits{MaxFilesystems: 5}); err == nil {
		t.Fatal("want a refusal")
	}
	if cr.n > len(data)/10 {
		t.Fatalf("read %d of %d bytes before refusing", cr.n, len(data))
	}
}

func TestReadRefusesDataAfterTheLastFolder(t *testing.T) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	_ = json.NewEncoder(zw).Encode(hdr("sda1"))
	_, _ = zw.Write([]byte("}\n"))
	_ = zw.Close()
	if _, err := Read(&buf, Limits{}); err == nil {
		t.Fatal("want a refusal for a stray closing brace")
	}
}
