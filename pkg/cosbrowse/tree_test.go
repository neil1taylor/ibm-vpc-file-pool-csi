package cosbrowse

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// sampleTree builds:
//
//	hello.txt  "hello world\n"
//	to-etc     -> /etc
//	docs/      a.md, deep/b.md
func sampleTree(f *fakeRepo) *Tree {
	deep := f.dir(f.fileEnt("b.md", "bee"))
	docs := f.dir(f.fileEnt("a.md", "ay"), dirEnt("deep", deep))
	root := f.dir(f.fileEnt("hello.txt", "hello world\n"), f.linkEnt("to-etc", "/etc"), dirEnt("docs", docs))
	return NewTree(f, Root{OID: root}, TreeOptions{})
}

func code(err error) string {
	var e *browse.Error
	if errors.As(err, &e) {
		return e.Code
	}
	return ""
}

func names(page browse.ListPage) []string {
	var out []string
	for _, e := range page.Entries {
		out = append(out, string(e.Name))
	}
	return out
}

func TestTree_ListIsSortedAndTyped(t *testing.T) {
	tr := sampleTree(newFakeRepo())
	page, err := tr.List(nil, nil, 100)
	mustT(t, err)
	if got := strings.Join(names(page), ","); got != "docs,hello.txt,to-etc" {
		t.Fatalf("names = %s", got)
	}
	byName := map[string]browse.Entry{}
	for _, e := range page.Entries {
		byName[string(e.Name)] = e
	}
	if e := byName["hello.txt"]; e.Type != browse.TypeFile || e.Size != 12 || e.Mode != 0o644 || !e.Readable {
		t.Errorf("hello.txt = %+v", e)
	}
	if e := byName["to-etc"]; e.Type != browse.TypeSymlink || string(e.Link) != "/etc" {
		t.Errorf("to-etc = %+v", e)
	}
	if e := byName["docs"]; e.Type != browse.TypeDir {
		t.Errorf("docs = %+v", e)
	}
	sub, err := tr.List(p("docs"), nil, 100)
	mustT(t, err)
	if got := strings.Join(names(sub), ","); got != "a.md,deep" {
		t.Errorf("docs = %s", got)
	}
}

func TestTree_ListPages(t *testing.T) {
	f := newFakeRepo()
	var ents []ent
	for i := 0; i < 25; i++ {
		ents = append(ents, f.fileEnt(fmt.Sprintf("f%02d", i), "x"))
	}
	tr := NewTree(f, Root{OID: f.dir(ents...)}, TreeOptions{})
	var got []string
	var cursor []byte
	pages := 0
	for {
		page, err := tr.List(nil, cursor, 10)
		mustT(t, err)
		pages++
		got = append(got, names(page)...)
		if len(page.Next) == 0 {
			break
		}
		cursor = page.Next
	}
	if pages != 3 || len(got) != 25 || !sort.StringsAreSorted(got) {
		t.Fatalf("pages = %d, names = %d, sorted = %v", pages, len(got), sort.StringsAreSorted(got))
	}
}

func TestTree_Stat(t *testing.T) {
	tr := sampleTree(newFakeRepo())
	root, err := tr.Stat(nil)
	mustT(t, err)
	if root.Type != browse.TypeDir || !root.Readable {
		t.Errorf("root = %+v", root)
	}
	e, err := tr.Stat(p("docs/deep/b.md"))
	mustT(t, err)
	if e.Type != browse.TypeFile || e.Size != 3 {
		t.Errorf("b.md = %+v", e)
	}
	l, err := tr.Stat(p("to-etc"))
	mustT(t, err)
	if l.Type != browse.TypeSymlink || string(l.Link) != "/etc" {
		t.Errorf("link = %+v", l)
	}
	if _, err := tr.Stat(p("nope")); code(err) != browse.CodeNotFound {
		t.Errorf("missing: %v", err)
	}
}

func TestTree_OpenReadsAndSeeksBackwards(t *testing.T) {
	tr := sampleTree(newFakeRepo())
	file, err := tr.Open(p("hello.txt"))
	mustT(t, err)
	defer func() { _ = file.Close() }()
	if file.Size() != 12 {
		t.Fatalf("size = %d", file.Size())
	}
	buf := make([]byte, 5)
	for _, c := range []struct {
		off  int64
		want string
	}{{0, "hello"}, {5, " worl"}, {1, "ello "}, {7, "orld\n"}} {
		n, err := file.ReadAt(buf, c.off)
		if err != nil && !errors.Is(err, io.EOF) {
			t.Fatalf("ReadAt(%d): %v", c.off, err)
		}
		if string(buf[:n]) != c.want {
			t.Errorf("ReadAt(%d) = %q, want %q", c.off, buf[:n], c.want)
		}
	}
	if n, err := file.ReadAt(buf, 12); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end = %d, %v", n, err)
	}
}

func TestTree_OpenRefusesLinksFoldersAndPathsThroughLinks(t *testing.T) {
	f := newFakeRepo()
	tr := sampleTree(f)
	for _, c := range []struct {
		path, code string
	}{
		{"to-etc", browse.CodeLink},
		{"docs", browse.CodeNotFile},
		{"to-etc/passwd", browse.CodeLink},
		{"hello.txt/x", browse.CodeNotDir},
		{"docs/missing", browse.CodeNotFound},
	} {
		_, err := tr.Open(p(c.path))
		if code(err) != c.code {
			t.Errorf("Open(%s) = %v, want code %s", c.path, err, c.code)
		}
	}
	// The link's target is text only. Nothing may have asked for /etc.
	for _, c := range f.calls {
		if strings.Contains(c, "etc") {
			t.Errorf("a link target was followed: %s", c)
		}
	}
}

func TestTree_BadPathsAreRefusedBeforeAnyRead(t *testing.T) {
	f := newFakeRepo()
	tr := sampleTree(f)
	before := f.totalCalls()
	for _, path := range [][][]byte{{[]byte("..")}, {[]byte("docs"), []byte("..")}, {[]byte(".")}, {[]byte("a/b")}, {[]byte("")}, {[]byte("a\x00b")}} {
		if _, err := tr.List(path, nil, 10); code(err) != browse.CodeBadPath {
			t.Errorf("List(%q) = %v", path, err)
		}
		if _, err := tr.Open(path); code(err) != browse.CodeBadPath {
			t.Errorf("Open(%q) = %v", path, err)
		}
		if _, err := tr.Stat(path); code(err) != browse.CodeBadPath {
			t.Errorf("Stat(%q) = %v", path, err)
		}
	}
	if f.totalCalls() != before {
		t.Errorf("the repository was read for a bad path")
	}
}

func TestTree_HostileNamesAreNotReachable(t *testing.T) {
	f := newFakeRepo()
	secret := f.dir(f.fileEnt("secret", "other volume"))
	root := f.dir(
		f.fileEnt("ok", "fine"),
		f.fileEnt("..", "dots"),
		f.fileEnt(".", "dot"),
		f.fileEnt("a/b", "slash"),
		f.fileEnt("nul\x00name", "nul"),
		dirEnt("", secret),
		f.fileEnt("new\nline", "newline is a normal name"),
	)
	tr := NewTree(f, Root{OID: root}, TreeOptions{})
	page, err := tr.List(nil, nil, 100)
	mustT(t, err)
	if got := strings.Join(names(page), "|"); got != "new\nline|ok" {
		t.Fatalf("names = %q", got)
	}
	for _, bad := range []string{"..", ".", "a/b", ""} {
		if _, err := tr.Open([][]byte{[]byte(bad)}); err == nil {
			t.Errorf("Open(%q) worked", bad)
		}
	}
	if f.callCount(secret) != 0 {
		t.Error("a folder behind a hostile name was read")
	}
}

func TestTree_DuplicateNamesMeanADamagedFolder(t *testing.T) {
	f := newFakeRepo()
	root := f.dir(f.fileEnt("same", "one"), f.fileEnt("same", "two"))
	tr := NewTree(f, Root{OID: root}, TreeOptions{})
	if _, err := tr.List(nil, nil, 10); code(err) != browse.CodeInternal || !strings.Contains(err.Error(), "twice") {
		t.Errorf("List = %v", err)
	}
}

func TestTree_ObjectIDsAreCheckedBeforeKopiaSeesThem(t *testing.T) {
	f := newFakeRepo()
	root := f.dir(
		ent{Name: "dash", Type: "f", Mode: "0644", Size: 1, MTime: epoch, Obj: "--password=x"},
		ent{Name: "space", Type: "d", Mode: "0755", MTime: epoch, Obj: "k1 --json"},
		ent{Name: "empty", Type: "f", Mode: "0644", Size: 1, MTime: epoch, Obj: ""},
		f.fileEnt("fine", "x"),
	)
	tr := NewTree(f, Root{OID: root}, TreeOptions{})
	page, err := tr.List(nil, nil, 10)
	mustT(t, err)
	for _, e := range page.Entries {
		if string(e.Name) != "fine" && e.Readable {
			t.Errorf("%s is listed as readable with a bad object ID", e.Name)
		}
	}
	for _, name := range []string{"dash", "space", "empty"} {
		if _, err := tr.Open(p(name)); err == nil {
			t.Errorf("Open(%s) worked", name)
		}
		if _, err := tr.List(p(name), nil, 10); err == nil {
			t.Errorf("List(%s) worked", name)
		}
	}
}

func TestTree_ManifestsAreCached(t *testing.T) {
	f := newFakeRepo()
	deep := f.dir(f.fileEnt("b.md", "bee"))
	docs := f.dir(dirEnt("deep", deep))
	root := f.dir(dirEnt("docs", docs))
	tr := NewTree(f, Root{OID: root}, TreeOptions{})
	for i := 0; i < 3; i++ {
		if _, err := tr.List(p("docs/deep"), nil, 10); err != nil {
			t.Fatal(err)
		}
	}
	for _, oid := range []string{root, docs, deep} {
		if n := f.callCount(oid); n != 1 {
			t.Errorf("folder %s was read %d times", oid, n)
		}
	}
}

func TestTree_LinkTargetsAreFetchedWithABoundOnProcesses(t *testing.T) {
	f := newFakeRepo()
	var ents []ent
	for i := 0; i < 200; i++ {
		ents = append(ents, f.linkEnt(fmt.Sprintf("l%03d", i), fmt.Sprintf("/t/%d", i)))
	}
	tr := NewTree(f, Root{OID: f.dir(ents...)}, TreeOptions{LinkFetchers: 4})
	page, err := tr.List(nil, nil, 200)
	mustT(t, err)
	if len(page.Entries) != 200 {
		t.Fatalf("entries = %d", len(page.Entries))
	}
	for i, e := range page.Entries {
		if want := fmt.Sprintf("/t/%d", i); string(e.Link) != want {
			t.Fatalf("link %d = %q, want %q", i, e.Link, want)
		}
	}
	if m := f.maxRunning.Load(); m > 5 {
		t.Errorf("%d objects were open at once; the bound is 4 links and the folder", m)
	}
}

func TestTree_ALongLinkTargetIsNotShown(t *testing.T) {
	f := newFakeRepo()
	long := strings.Repeat("a", 5000)
	root := f.dir(f.linkEnt("long", long))
	tr := NewTree(f, Root{OID: root}, TreeOptions{})
	page, err := tr.List(nil, nil, 10)
	mustT(t, err)
	if e := page.Entries[0]; len(e.Link) != 0 || e.Readable {
		t.Errorf("a 5000 byte link target was shown: %+v", e)
	}
}

func TestTree_RepositoryErrorsAreWordedForPeople(t *testing.T) {
	f := newFakeRepo()
	root := f.dir(f.fileEnt("a", "x"))
	f.objErr[root] = errors.New("raw failure: access denied for key AKIA123")
	tr := NewTree(f, Root{OID: root}, TreeOptions{})
	_, err := tr.List(nil, nil, 10)
	if code(err) != browse.CodeInternal {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "AKIA123") {
		t.Errorf("the raw repository error reached the user: %v", err)
	}
}

func TestTree_TarWritesFilesAndLinksAndNeverFollows(t *testing.T) {
	f := newFakeRepo()
	tr := sampleTree(f)
	var buf bytes.Buffer
	rep, err := tr.Tar(&buf, nil)
	mustT(t, err)
	got := map[string]string{}
	links := map[string]string{}
	r := tar.NewReader(&buf)
	for {
		h, err := r.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		mustT(t, err)
		switch h.Typeflag {
		case tar.TypeReg:
			b, _ := io.ReadAll(r)
			got[h.Name] = string(b)
		case tar.TypeSymlink:
			links[h.Name] = h.Linkname
		}
	}
	if got["hello.txt"] != "hello world\n" || got["docs/deep/b.md"] != "bee" || links["to-etc"] != "/etc" {
		t.Errorf("tar = %v, %v", got, links)
	}
	if rep.Files != 3 || rep.Links != 1 {
		t.Errorf("report = %+v", rep)
	}
	if _, err := tr.Tar(io.Discard, p("to-etc")); err == nil {
		t.Error("a link was downloaded")
	}
}

func TestTree_ConcurrentUseIsSafe(t *testing.T) {
	tr := sampleTree(newFakeRepo())
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := tr.List(p("docs"), nil, 10); err != nil {
				t.Error(err)
			}
			file, err := tr.Open(p("hello.txt"))
			if err != nil {
				t.Error(err)
				return
			}
			defer func() { _ = file.Close() }()
			b := make([]byte, 5)
			if _, err := file.ReadAt(b, 0); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}
