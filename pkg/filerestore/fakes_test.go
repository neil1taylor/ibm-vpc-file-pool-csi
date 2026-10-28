package filerestore

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// fakeTree is a backup tree in memory.
type fakeTree struct {
	nodes map[string]*fnode
}

type fnode struct {
	e          browse.Entry
	data       []byte
	unreadable bool
}

var testTime = time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC)

func newFakeTree() *fakeTree {
	t := &fakeTree{nodes: map[string]*fnode{}}
	t.nodes[""] = &fnode{e: browse.Entry{Type: browse.TypeDir, Mode: 0o755, MTime: testTime, Readable: true}}
	return t
}

func key(path [][]byte) string {
	parts := make([]string, len(path))
	for i, p := range path {
		parts[i] = string(p)
	}
	return strings.Join(parts, "/")
}

// add puts a node at a slash path; parents must exist.
func (t *fakeTree) add(p string, n *fnode) {
	n.e.Name = []byte(p[strings.LastIndex(p, "/")+1:])
	if n.e.MTime.IsZero() {
		n.e.MTime = testTime
	}
	n.e.Readable = !n.unreadable
	t.nodes[strings.TrimPrefix(p, "/")] = n
}

func (t *fakeTree) dir(p string, mode, uid, gid uint32) {
	t.add(p, &fnode{e: browse.Entry{Type: browse.TypeDir, Mode: mode, UID: uid, GID: gid}})
}

func (t *fakeTree) file(p, content string, mode, uid, gid uint32) {
	t.add(p, &fnode{e: browse.Entry{Type: browse.TypeFile, Mode: mode, UID: uid, GID: gid, Size: int64(len(content))}, data: []byte(content)})
}

func (t *fakeTree) link(p, target string) {
	t.add(p, &fnode{e: browse.Entry{Type: browse.TypeSymlink, Mode: 0o777, UID: 1000, GID: 1000, Link: []byte(target)}})
}

func (t *fakeTree) get(path [][]byte) (*fnode, error) {
	n, ok := t.nodes[key(path)]
	if !ok {
		return nil, &browse.Error{Code: browse.CodeNotFound, Msg: "no such file"}
	}
	return n, nil
}

func (t *fakeTree) Stat(path [][]byte) (browse.Entry, error) {
	n, err := t.get(path)
	if err != nil {
		return browse.Entry{}, err
	}
	return n.e, nil
}

func (t *fakeTree) List(path [][]byte, _ []byte, _ int) (browse.ListPage, error) {
	n, err := t.get(path)
	if err != nil {
		return browse.ListPage{}, err
	}
	if n.e.Type != browse.TypeDir {
		return browse.ListPage{}, &browse.Error{Code: browse.CodeNotDir, Msg: "not a folder"}
	}
	if n.unreadable {
		return browse.ListPage{}, &browse.Error{Code: browse.CodeNotReadable, Msg: "no"}
	}
	prefix := key(path)
	if prefix != "" {
		prefix += "/"
	}
	var names []string
	for k := range t.nodes {
		if k == "" || !strings.HasPrefix(k, prefix) {
			continue
		}
		if rest := k[len(prefix):]; !strings.Contains(rest, "/") {
			names = append(names, rest)
		}
	}
	sort.Strings(names)
	page := browse.ListPage{Entries: []browse.Entry{}}
	for _, nm := range names {
		page.Entries = append(page.Entries, t.nodes[prefix+nm].e)
	}
	return page, nil
}

type memFile struct{ *bytes.Reader }

func (memFile) Close() error  { return nil }
func (m memFile) Size() int64 { return m.Reader.Size() }

func (t *fakeTree) Open(path [][]byte) (browse.File, error) {
	n, err := t.get(path)
	if err != nil {
		return nil, err
	}
	switch {
	case n.unreadable:
		return nil, &browse.Error{Code: browse.CodeNotReadable, Msg: "no"}
	case n.e.Type == browse.TypeSymlink:
		return nil, &browse.Error{Code: browse.CodeLink, Msg: "link"}
	case n.e.Type != browse.TypeFile:
		return nil, &browse.Error{Code: browse.CodeSpecial, Msg: "not a regular file"}
	}
	return memFile{bytes.NewReader(n.data)}, nil
}

func (t *fakeTree) Tar(io.Writer, [][]byte) (browse.TarReport, error) {
	return browse.TarReport{}, errors.New("not used")
}

// recDest records what the engine does, and refuses what a real destination
// would refuse: a write into a folder that is finished, and a name that exists.
type recDest struct {
	ops       []string
	dirs      map[string]*rdir
	files     map[string]*rfile
	links     map[string][]byte
	linkMeta  map[string]Meta
	failOwner bool // Finish fails when an owner is set
	free      int64
	aborted   bool
}

type rdir struct {
	finished bool
	meta     Meta
}

type rfile struct {
	data bytes.Buffer
	meta Meta
	done bool
}

func newRecDest() *recDest {
	d := &recDest{dirs: map[string]*rdir{"": {}}, files: map[string]*rfile{}, links: map[string][]byte{},
		linkMeta: map[string]Meta{}, free: 1 << 40}
	return d
}

func (d *recDest) op(format string, a ...any) { d.ops = append(d.ops, fmt.Sprintf(format, a...)) }

func parent(path [][]byte) string { return key(path[:len(path)-1]) }

func (d *recDest) checkParent(path [][]byte) error {
	if len(path) == 0 {
		return errors.New("empty path")
	}
	p, ok := d.dirs[parent(path)]
	if !ok {
		return fmt.Errorf("no parent folder for %q", key(path))
	}
	if p.finished {
		return fmt.Errorf("write into the finished folder %q", parent(path))
	}
	k := key(path)
	if _, ok := d.dirs[k]; ok {
		return fmt.Errorf("%q exists", k)
	}
	if _, ok := d.files[k]; ok {
		return fmt.Errorf("%q exists", k)
	}
	if _, ok := d.links[k]; ok {
		return fmt.Errorf("%q exists", k)
	}
	return nil
}

func (d *recDest) Mkdir(path [][]byte) error {
	if err := d.checkParent(path); err != nil {
		return err
	}
	d.dirs[key(path)] = &rdir{}
	d.op("mkdir /%s", key(path))
	return nil
}

func (d *recDest) FinishDir(path [][]byte, m Meta) error {
	dir, ok := d.dirs[key(path)]
	if !ok {
		return fmt.Errorf("no folder %q", key(path))
	}
	if d.failOwner && (m.UID != nil || m.GID != nil) {
		return errors.New("chown: operation not permitted")
	}
	dir.finished, dir.meta = true, m
	d.op("finishdir /%s", key(path))
	return nil
}

type recFile struct {
	d    *recDest
	k    string
	file *rfile
}

func (f *recFile) Write(p []byte) (int, error) { return f.file.data.Write(p) }
func (f *recFile) Close() error                { return nil }
func (f *recFile) Finish(m Meta) error {
	if f.d.failOwner && (m.UID != nil || m.GID != nil) {
		return errors.New("chown: operation not permitted")
	}
	f.file.meta, f.file.done = m, true
	f.d.op("finishfile /%s", f.k)
	return nil
}

func (d *recDest) Create(path [][]byte) (DestFile, error) {
	if err := d.checkParent(path); err != nil {
		return nil, err
	}
	f := &rfile{}
	d.files[key(path)] = f
	d.op("create /%s", key(path))
	return &recFile{d: d, k: key(path), file: f}, nil
}

func (d *recDest) Symlink(path [][]byte, target []byte, m Meta) error {
	if err := d.checkParent(path); err != nil {
		return err
	}
	if d.failOwner && (m.UID != nil || m.GID != nil) {
		return errors.New("chown: operation not permitted")
	}
	d.links[key(path)], d.linkMeta[key(path)] = target, m
	d.op("symlink /%s", key(path))
	return nil
}

func (d *recDest) Free() (int64, error) { return d.free, nil }

func (d *recDest) Abort() error {
	d.aborted = true
	d.op("abort")
	return nil
}

func (d *recDest) opIndex(op string) int {
	for i, o := range d.ops {
		if o == op {
			return i
		}
	}
	return -1
}

func p(s string) [][]byte {
	var out [][]byte
	for _, part := range strings.Split(strings.Trim(s, "/"), "/") {
		if part != "" {
			out = append(out, []byte(part))
		}
	}
	return out
}

func paths(ss ...string) [][][]byte {
	var out [][][]byte
	for _, s := range ss {
		out = append(out, p(s))
	}
	return out
}

func i64(v int64) *int64 { return &v }
