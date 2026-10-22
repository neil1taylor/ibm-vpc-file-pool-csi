//go:build linux

package browse

import (
	"archive/tar"
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// hostile builds a backup tree that an attacker could have made, next to a
// folder with secrets that must never be reached.
type hostile struct {
	root    string // the backup root
	outside string // a sibling folder that holds secrets
}

const secretText = "TOP SECRET OUTSIDE THE BACKUP"

func mustWrite(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func mustLink(t *testing.T, target, name string) {
	t.Helper()
	if err := os.Symlink(target, name); err != nil {
		t.Fatal(err)
	}
}

func newHostile(t *testing.T) hostile {
	t.Helper()
	base := t.TempDir()
	h := hostile{root: filepath.Join(base, "volume"), outside: filepath.Join(base, "other-volume")}
	for _, d := range []string{h.root, h.outside, filepath.Join(h.root, "docs", "deep"), filepath.Join(h.root, "empty")} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	mustWrite(t, filepath.Join(h.outside, "secret.txt"), secretText, 0o644)
	mustWrite(t, filepath.Join(h.root, "hello.txt"), "hello world\n", 0o644)
	mustWrite(t, filepath.Join(h.root, "docs", "a.txt"), "doc a", 0o644)
	mustWrite(t, filepath.Join(h.root, "docs", "deep", "b.txt"), "deep b", 0o600)
	mustWrite(t, filepath.Join(h.root, "name with space é.txt"), "spaces", 0o644)
	mustWrite(t, filepath.Join(h.root, "line\nbreak"), "newline in the name", 0o644)
	mustWrite(t, filepath.Join(h.root, "bad\xff\xfeutf8"), "invalid utf-8", 0o644)
	mustWrite(t, filepath.Join(h.root, strings.Repeat("n", 255)), "longest name", 0o644)
	// links an attacker plants
	mustLink(t, "/etc", filepath.Join(h.root, "to-etc"))
	mustLink(t, "/etc/passwd", filepath.Join(h.root, "to-passwd"))
	mustLink(t, "../other-volume", filepath.Join(h.root, "to-other"))
	mustLink(t, "../other-volume/secret.txt", filepath.Join(h.root, "to-secret"))
	mustLink(t, filepath.Join(h.outside, "secret.txt"), filepath.Join(h.root, "to-secret-abs"))
	mustLink(t, "loop", filepath.Join(h.root, "loop"))
	mustLink(t, "../../../../other-volume", filepath.Join(h.root, "docs", "deep", "up"))
	mustLink(t, "hello.txt", filepath.Join(h.root, "inside-link"))
	mustLink(t, "/proc/self/root", filepath.Join(h.root, "to-proc-root"))
	if err := syscall.Mkfifo(filepath.Join(h.root, "pipe"), 0o644); err != nil {
		t.Fatal(err)
	}
	return h
}

func p(parts ...string) [][]byte {
	var out [][]byte
	for _, s := range parts {
		out = append(out, []byte(s))
	}
	return out
}

func wantCode(t *testing.T, err error, code string) {
	t.Helper()
	var e *Error
	if !errors.As(err, &e) || e.Code != code {
		t.Fatalf("error = %v, want code %q", err, code)
	}
}

// treeModes runs a test with openat2 and with the folder-by-folder fallback.
func treeModes(t *testing.T, root string, fn func(t *testing.T, tr *DirTree)) {
	t.Helper()
	for _, walk := range []bool{false, true} {
		name := "openat2"
		if walk {
			name = "walk"
		}
		t.Run(name, func(t *testing.T) {
			tr, err := newTree(root, walk)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tr.Close() }()
			fn(t, tr)
		})
	}
}

func listAll(t *testing.T, tr *DirTree, path [][]byte) []Entry {
	t.Helper()
	var all []Entry
	var cursor []byte
	for {
		page, err := tr.List(path, cursor, 3)
		if err != nil {
			t.Fatal(err)
		}
		all = append(all, page.Entries...)
		if len(page.Next) == 0 {
			return all
		}
		cursor = page.Next
	}
}

func byName(es []Entry) map[string]Entry {
	m := map[string]Entry{}
	for _, e := range es {
		m[string(e.Name)] = e
	}
	return m
}

func TestListShowsLinksAsLinksAndKeepsRawNames(t *testing.T) {
	h := newHostile(t)
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		all := listAll(t, tr, nil) // page size 3 exercises the cursor
		m := byName(all)
		if !sort.SliceIsSorted(all, func(i, j int) bool { return bytes.Compare(all[i].Name, all[j].Name) < 0 }) {
			t.Error("entries are not in byte order")
		}
		if len(m) != len(all) {
			t.Errorf("a name came twice across pages: %d entries, %d names", len(all), len(m))
		}
		for _, want := range []string{"hello.txt", "docs", "line\nbreak", "bad\xff\xfeutf8", "name with space é.txt", strings.Repeat("n", 255)} {
			if _, ok := m[want]; !ok {
				t.Errorf("missing %q", want)
			}
		}
		if e := m["to-etc"]; e.Type != TypeSymlink || string(e.Link) != "/etc" {
			t.Errorf("to-etc = %+v, want a symlink to /etc", e)
		}
		if e := m["to-other"]; e.Type != TypeSymlink || string(e.Link) != "../other-volume" {
			t.Errorf("to-other = %+v", e)
		}
		if e := m["loop"]; e.Type != TypeSymlink {
			t.Errorf("loop = %+v", e)
		}
		if e := m["pipe"]; e.Type != TypeOther {
			t.Errorf("pipe = %+v, want other", e)
		}
		if e := m["docs"]; e.Type != TypeDir {
			t.Errorf("docs = %+v", e)
		}
		e := m["hello.txt"]
		if e.Type != TypeFile || e.Size != 12 || e.Mode&0o777 != 0o644 || !e.Readable || e.MTime.IsZero() {
			t.Errorf("hello.txt = %+v", e)
		}
		if e.UID != uint32(os.Getuid()) {
			t.Errorf("uid = %d, want %d", e.UID, os.Getuid())
		}
	})
}

func TestListLimit(t *testing.T) {
	h := newHostile(t)
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		page, err := tr.List(nil, nil, 2)
		if err != nil || len(page.Entries) != 2 || len(page.Next) == 0 {
			t.Fatalf("page = %d entries, next %q, err %v", len(page.Entries), page.Next, err)
		}
		page, err = tr.List(p("empty"), nil, 10)
		if err != nil || len(page.Entries) != 0 || len(page.Next) != 0 {
			t.Fatalf("empty folder = %+v, %v", page, err)
		}
	})
}

func TestLinksAreNeverFollowed(t *testing.T) {
	h := newHostile(t)
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		// A link as the folder to list.
		for _, name := range []string{"to-etc", "to-other", "loop", "inside-link", "to-proc-root"} {
			_, err := tr.List(p(name), nil, 10)
			wantCode(t, err, CodeLink)
		}
		// A link in the middle of a path.
		for _, path := range [][][]byte{p("to-other", "secret.txt"), p("to-etc", "passwd"), p("to-proc-root", "etc", "passwd"), p("docs", "deep", "up", "secret.txt")} {
			if f, err := tr.Open(path); err == nil {
				_ = f.Close()
				t.Fatalf("Open(%q) followed a link", JoinPath(path))
			} else {
				wantCode(t, err, CodeLink)
			}
			if _, err := tr.Stat(path); err == nil {
				t.Fatalf("Stat(%q) followed a link", JoinPath(path))
			}
		}
		// A link as the file to read.
		for _, name := range []string{"to-passwd", "to-secret", "to-secret-abs", "inside-link"} {
			f, err := tr.Open(p(name))
			if err == nil {
				_ = f.Close()
				t.Fatalf("Open(%s) read through a link", name)
			}
			wantCode(t, err, CodeLink)
		}
		// Stat shows the link itself.
		e, err := tr.Stat(p("to-secret"))
		if err != nil || e.Type != TypeSymlink || string(e.Link) != "../other-volume/secret.txt" {
			t.Fatalf("Stat(to-secret) = %+v, %v", e, err)
		}
	})
}

func TestOpenReadsRegularFilesOnly(t *testing.T) {
	h := newHostile(t)
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		f, err := tr.Open(p("docs", "a.txt"))
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = f.Close() }()
		buf := make([]byte, 10)
		n, _ := f.ReadAt(buf, 1)
		if f.Size() != 5 || string(buf[:n]) != "oc a" {
			t.Errorf("size %d, read %q", f.Size(), buf[:n])
		}
		for _, name := range []string{"line\nbreak", "bad\xff\xfeutf8", strings.Repeat("n", 255)} {
			f2, err := tr.Open(p(name))
			if err != nil {
				t.Fatalf("Open(%q): %v", name, err)
			}
			_ = f2.Close()
		}
		_, err = tr.Open(p("docs"))
		wantCode(t, err, CodeNotFile)
		_, err = tr.Open(p("nope"))
		wantCode(t, err, CodeNotFound)
		_, err = tr.Open(p("hello.txt", "x"))
		wantCode(t, err, CodeNotDir)
		_, err = tr.List(p("hello.txt"), nil, 10)
		wantCode(t, err, CodeNotDir)
		_, err = tr.List(p("nope"), nil, 10)
		wantCode(t, err, CodeNotFound)
		_, err = tr.Open(p(".."))
		wantCode(t, err, CodeBadPath)
		_, err = tr.List(p("docs", ".."), nil, 10)
		wantCode(t, err, CodeBadPath)
	})
}

func TestPipeIsNotOpened(t *testing.T) {
	h := newHostile(t)
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		done := make(chan error, 1)
		go func() {
			f, err := tr.Open(p("pipe"))
			if err == nil {
				_ = f.Close()
			}
			done <- err
		}()
		select {
		case err := <-done:
			wantCode(t, err, CodeSpecial)
		case <-time.After(5 * time.Second):
			t.Fatal("Open of a pipe blocked")
		}
	})
}

func TestUnreadableFile(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	h := newHostile(t)
	mustWrite(t, filepath.Join(h.root, "private"), "x", 0o000)
	if err := os.Mkdir(filepath.Join(h.root, "closed"), 0o000); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(h.root, "closed"), 0o755) })
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		m := byName(listAll(t, tr, nil))
		if m["private"].Readable || m["closed"].Readable {
			t.Errorf("unreadable entries are marked readable: %+v %+v", m["private"], m["closed"])
		}
		if !m["hello.txt"].Readable {
			t.Error("a readable file is marked unreadable")
		}
		_, err := tr.Open(p("private"))
		wantCode(t, err, CodeNotReadable)
		_, err = tr.List(p("closed"), nil, 10)
		wantCode(t, err, CodeNotReadable)
	})
}

func TestRootIsAFolderBehindALink(t *testing.T) {
	h := newHostile(t)
	// The root itself may be reached through a link: a mount point is trusted
	// configuration, only names below it are not.
	alias := filepath.Join(filepath.Dir(h.root), "alias")
	mustLink(t, h.root, alias)
	tr, err := NewTree(alias)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tr.Close() }()
	if _, err := tr.Stat(p("hello.txt")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewTree(filepath.Join(h.root, "hello.txt")); err == nil {
		t.Error("a file is not a root")
	}
	if _, err := NewTree(filepath.Join(h.root, "missing")); err == nil {
		t.Error("a missing folder is not a root")
	}
}

func TestStatRoot(t *testing.T) {
	h := newHostile(t)
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		e, err := tr.Stat(nil)
		if err != nil || e.Type != TypeDir {
			t.Fatalf("Stat(root) = %+v, %v", e, err)
		}
	})
}

func readTar(t *testing.T, b []byte) map[string]tarFile {
	t.Helper()
	out := map[string]tarFile{}
	r := tar.NewReader(bytes.NewReader(b))
	for {
		hdr, err := r.Next()
		if err == io.EOF {
			return out
		}
		if err != nil {
			t.Fatalf("reading the tar: %v", err)
		}
		data, _ := io.ReadAll(r)
		out[hdr.Name] = tarFile{hdr: *hdr, data: string(data)}
	}
}

func TestTarOfFolderKeepsLinksAsLinks(t *testing.T) {
	h := newHostile(t)
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		var buf bytes.Buffer
		rep, err := tr.Tar(&buf, nil)
		if err != nil {
			t.Fatal(err)
		}
		got := readTar(t, buf.Bytes())
		if got["hello.txt"].data != "hello world\n" {
			t.Errorf("hello.txt = %q", got["hello.txt"].data)
		}
		if got["docs/deep/b.txt"].data != "deep b" {
			t.Errorf("docs/deep/b.txt = %q", got["docs/deep/b.txt"].data)
		}
		if l := got["to-etc"]; l.hdr.Typeflag != tar.TypeSymlink || l.hdr.Linkname != "/etc" || l.data != "" {
			t.Errorf("to-etc = %+v", l.hdr)
		}
		if l := got["to-secret"]; l.hdr.Typeflag != tar.TypeSymlink {
			t.Errorf("to-secret = %+v", l.hdr)
		}
		if got["line\nbreak"].data != "newline in the name" {
			t.Errorf("a name with a newline: %+v", got["line\nbreak"].hdr)
		}
		if got["bad\xff\xfeutf8"].data != "invalid utf-8" {
			var names []string
			for n := range got {
				names = append(names, fmt.Sprintf("%q", n))
			}
			t.Errorf("a name with invalid UTF-8 is missing from %v", names)
		}
		if _, ok := got["pipe"]; ok {
			t.Error("a pipe was put in the tar")
		}
		for name, f := range got {
			if strings.Contains(f.data, secretText) || strings.Contains(f.data, "root:") {
				t.Errorf("%q holds data from outside the backup", name)
			}
		}
		if _, ok := got["to-etc/passwd"]; ok {
			t.Error("the tar followed a link")
		}
		if f := got["docs/deep/b.txt"]; f.hdr.Mode&0o777 != 0o600 || f.hdr.Uid != os.Getuid() {
			t.Errorf("mode/owner not kept: %+v", f.hdr)
		}
		if rep.Skipped != 1 || rep.Links < 8 || rep.Files < 6 {
			t.Errorf("report = %+v", rep)
		}
	})
}

func TestTarOfOneSubfolderAndOfALink(t *testing.T) {
	h := newHostile(t)
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		var buf bytes.Buffer
		if _, err := tr.Tar(&buf, p("docs")); err != nil {
			t.Fatal(err)
		}
		got := readTar(t, buf.Bytes())
		if got["docs/a.txt"].data != "doc a" || got["docs/deep/b.txt"].data != "deep b" {
			t.Errorf("tar of docs has %d entries", len(got))
		}
		if _, err := tr.Tar(io.Discard, p("to-etc")); err == nil {
			t.Error("a tar of a link must be refused")
		} else {
			wantCode(t, err, CodeLink)
		}
		buf.Reset()
		if _, err := tr.Tar(&buf, p("hello.txt")); err != nil {
			t.Fatal(err)
		}
		if got := readTar(t, buf.Bytes()); got["hello.txt"].data != "hello world\n" {
			t.Errorf("tar of one file = %v", got)
		}
	})
}

func TestTarSkipsUnreadableFiles(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	h := newHostile(t)
	mustWrite(t, filepath.Join(h.root, "docs", "private"), "x", 0o000)
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		var buf bytes.Buffer
		rep, err := tr.Tar(&buf, p("docs"))
		if err != nil || rep.Unreadable != 1 {
			t.Fatalf("report %+v, err %v", rep, err)
		}
		if _, ok := readTar(t, buf.Bytes())["docs/private"]; ok {
			t.Error("an unreadable file is in the tar")
		}
	})
}

func TestManyReadersAtOnce(t *testing.T) {
	h := newHostile(t)
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		var wg sync.WaitGroup
		for i := 0; i < 16; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for j := 0; j < 20; j++ {
					if _, err := tr.List(p("docs"), nil, 10); err != nil {
						t.Error(err)
						return
					}
					f, err := tr.Open(p("docs", "a.txt"))
					if err != nil {
						t.Error(err)
						return
					}
					_ = f.Close()
					if _, err := tr.Tar(io.Discard, p("docs")); err != nil {
						t.Error(err)
						return
					}
				}
			}()
		}
		wg.Wait()
	})
}

func TestUnchangedTreeAfterBrowsing(t *testing.T) {
	h := newHostile(t)
	before := snapshot(t, h.root)
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		_ = listAll(t, tr, nil)
		_, _ = tr.Tar(io.Discard, nil)
	})
	if after := snapshot(t, h.root); after != before {
		t.Errorf("browsing changed the tree:\n%s\nvs\n%s", before, after)
	}
}

// snapshot describes every name below dir, with mode, size and mtime, without
// following links. Access times are not included: reading changes them.
func snapshot(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		lines = append(lines, fmt.Sprintf("%q %v %d %d", path, info.Mode(), info.Size(), info.ModTime().UnixNano()))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return strings.Join(lines, "\n")
}

func TestTarOpensEachFolderOnce(t *testing.T) {
	h := newHostile(t) // folders: root, docs, docs/deep, empty
	treeModes(t, h.root, func(t *testing.T, tr *DirTree) {
		before := tr.dirReads.Load()
		if _, err := tr.Tar(io.Discard, nil); err != nil {
			t.Fatal(err)
		}
		if n := tr.dirReads.Load() - before; n != 4 {
			t.Errorf("folders read %d times, want 4 (once each)", n)
		}
	})
}

func TestListLongNamesStaysBounded(t *testing.T) {
	dir := t.TempDir()
	long := strings.Repeat("y", 200)
	for i := 0; i < 3000; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s%05d", long, i)), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	treeModes(t, dir, func(t *testing.T, tr *DirTree) {
		page, err := tr.List(nil, nil, 5)
		if err != nil || len(page.Entries) != 5 || len(page.Next) == 0 {
			t.Fatalf("page %d next %q err %v", len(page.Entries), page.Next, err)
		}
		if got := string(page.Entries[0].Name); got != fmt.Sprintf("%s%05d", long, 0) {
			t.Errorf("first = %q", got)
		}
		if tr.lastMaxHeld.Load() > 6 {
			t.Errorf("held %d names at once, want at most 6", tr.lastMaxHeld.Load())
		}
	})
}
