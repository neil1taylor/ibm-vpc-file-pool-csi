package guestfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"k8s.io/klog/v2"
)

// Options set up a Guest.
type Options struct {
	// Drive is the disk as the tools name it, like
	// nbd+unix:///?socket=/tmp/nbd.sock. The appliance gets it read-only.
	Drive string
	// Runner runs the tools.
	Runner Runner
	// Crash says the copy was taken while the VM ran, so file systems are
	// mounted without replaying their journal from the start. Otherwise that
	// is tried only when a plain mount fails.
	Crash bool
	// Check, if set, runs before every call. An error ends the call: the
	// backup under the session changed, for example.
	Check func() error
	// Parallel is how many tools may run at once; zero means one, since each
	// holds an appliance of about 550 MiB.
	Parallel int
}

// A Guest shows the file systems of a VM disk as a browse.Tree. The top level
// folders are the file systems; below each is its content. All reads go through
// the libguestfs tools, one appliance each (virt-ls and virt-cat are the way
// the packaging reads NTFS, and the same way is used for every file system).
type Guest struct {
	ctx  context.Context
	opts Options
	sem  chan struct{}

	mu    sync.Mutex
	fss   []*fsState
	dirs  map[string][]browse.Entry
	order []string // cache keys, oldest first
}

type fsState struct {
	FS
	// opt is the mount option the file system is read with; empty is none.
	opt string
	// withoutJournal is true when opt skips the journal replay, which a copy
	// taken during writes may need.
	withoutJournal bool
}

const maxCachedDirs = 64

// New makes a Guest. ctx ends all its tools.
func New(ctx context.Context, opts Options) *Guest {
	n := opts.Parallel
	if n <= 0 {
		n = 1
	}
	return &Guest{ctx: ctx, opts: opts, sem: make(chan struct{}, n), dirs: map[string][]browse.Entry{}}
}

func internal(msg string) error { return &browse.Error{Code: browse.CodeInternal, Msg: msg} }

// slot takes a place to run a tool.
func (g *Guest) slot(ctx context.Context) (func(), error) {
	select {
	case g.sem <- struct{}{}:
		return func() { <-g.sem }, nil
	case <-ctx.Done():
		return nil, internal("the reader is busy; try again")
	case <-g.ctx.Done():
		return nil, internal("the session has ended")
	}
}

func (g *Guest) check() error {
	if g.opts.Check != nil {
		if err := g.opts.Check(); err != nil {
			return internal("the backup changed while the session was open; open it again (" + err.Error() + ")")
		}
	}
	return nil
}

func (g *Guest) driveArgs(tool string, rest ...string) []string {
	return append([]string{tool, "--format=raw", "-a", g.opts.Drive}, rest...)
}

// Filesystems finds the file systems of the disk. It starts the appliance, so it
// is also the check that the disk can be read at all.
func (g *Guest) Filesystems(ctx context.Context) ([]FS, error) {
	release, err := g.slot(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	out, err := g.opts.Runner.Output(ctx, g.driveArgs("virt-filesystems", "--filesystems", "--extra", "--long", "--csv"))
	if err != nil {
		return nil, fmt.Errorf("the reader could not list the file systems of the disk: %w", err)
	}
	fss, err := ParseFilesystems(out)
	if err != nil {
		return nil, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	g.fss = g.fss[:0]
	for _, f := range fss {
		st := &fsState{FS: f}
		if g.opts.Crash && f.Readable {
			st.opt, st.withoutJournal = recoveryOption(f.VFS), recoveryOption(f.VFS) != ""
		}
		g.fss = append(g.fss, st)
	}
	return fss, nil
}

// recoveryOption is the mount option that reads a file system without replaying
// its journal, for the types that have one. The appliance's drive is read-only,
// so a journal can never be replayed; a copy taken while the VM ran may need it.
func recoveryOption(vfs string) string {
	switch vfs {
	case "xfs":
		return "norecovery"
	case "ext3", "ext4":
		return "noload"
	}
	return ""
}

// Notices are sentences about what the reader had to do, for the person.
func (g *Guest) Notices() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var out []string
	for _, f := range g.fss {
		if f.withoutJournal {
			out = append(out, fmt.Sprintf("%s was read without replaying its journal, because the copy was taken while the VM was running. The newest changes may be missing.", f.ID))
		}
	}
	return out
}

func (g *Guest) lookup(id []byte) (*fsState, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, f := range g.fss {
		if f.ID == string(id) {
			return f, nil
		}
	}
	return nil, &browse.Error{Code: browse.CodeNotFound, Msg: "there is no such file system in this disk"}
}

func (g *Guest) mountArg(f *fsState) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	if f.opt == "" {
		return f.Device
	}
	return f.Device + ":/:" + f.opt
}

// guestPath makes the path inside a file system from names that were checked.
func guestPath(names [][]byte) string {
	if len(names) == 0 {
		return "/"
	}
	var b strings.Builder
	for _, n := range names {
		b.WriteByte('/')
		b.Write(n)
	}
	return b.String()
}

func isMountError(err error) bool {
	var ee *ExitError
	return errors.As(err, &ee) && strings.Contains(strings.ToLower(ee.Stderr), "mount")
}

// run runs a tool against one file system. If the mount fails and the file
// system has an option that skips its journal, it tries once more with it and
// remembers it.
func (g *Guest) run(ctx context.Context, f *fsState, args func(mount string) []string, stream bool) ([]byte, io.ReadCloser, func(), error) {
	for attempt := 0; ; attempt++ {
		release, err := g.slot(ctx)
		if err != nil {
			return nil, nil, nil, err
		}
		a := args(g.mountArg(f))
		var (
			out []byte
			rc  io.ReadCloser
		)
		if stream {
			rc, err = g.opts.Runner.Stream(ctx, a)
		} else {
			out, err = g.opts.Runner.Output(ctx, a)
		}
		if err == nil {
			return out, rc, release, nil
		}
		release()
		opt := recoveryOption(f.VFS)
		g.mu.Lock()
		again := attempt == 0 && opt != "" && f.opt == "" && isMountError(err)
		if again {
			f.opt, f.withoutJournal = opt, true
		}
		g.mu.Unlock()
		if !again {
			return nil, nil, nil, err
		}
		klog.V(2).InfoS("A guest file system did not mount; trying without its journal", "fs", f.ID)
	}
}

func toBrowseError(err error) error {
	var be *browse.Error
	if errors.As(err, &be) {
		return err
	}
	var ee *ExitError
	if errors.As(err, &ee) {
		low := strings.ToLower(ee.Stderr)
		switch {
		case strings.Contains(low, "no such file"):
			return &browse.Error{Code: browse.CodeNotFound, Msg: "no such file or folder"}
		case strings.Contains(low, "not a directory"):
			return &browse.Error{Code: browse.CodeNotDir, Msg: "this is not a folder"}
		}
		klog.V(2).InfoS("A guest tool failed", "exit", ee.Err)
	}
	return internal("the reader could not read the disk; try again")
}

func (g *Guest) readable(id []byte) (*fsState, error) {
	f, err := g.lookup(id)
	if err != nil {
		return nil, err
	}
	if !f.Readable {
		return nil, &browse.Error{Code: browse.CodeNotReadable, Msg: f.Reason}
	}
	return f, nil
}

// dir returns the entries of a folder inside a file system, in order of name.
func (g *Guest) dir(ctx context.Context, f *fsState, names [][]byte) ([]browse.Entry, error) {
	key := f.ID + "\x00" + guestPath(names)
	g.mu.Lock()
	if es, ok := g.dirs[key]; ok {
		g.mu.Unlock()
		return es, nil
	}
	g.mu.Unlock()
	path := guestPath(names)
	out, _, release, err := g.run(ctx, f, func(mount string) []string {
		return g.driveArgs("virt-ls", "-m", mount, "-l", "--csv", "--", path)
	}, false)
	if err != nil {
		return nil, toBrowseError(err)
	}
	release()
	es, err := ParseListing(out)
	if err != nil {
		return nil, err
	}
	sort.Slice(es, func(i, j int) bool { return bytes.Compare(es[i].Name, es[j].Name) < 0 })
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.order) >= maxCachedDirs {
		delete(g.dirs, g.order[0])
		g.order = g.order[1:]
	}
	g.dirs[key] = es
	g.order = append(g.order, key)
	return es, nil
}

func (g *Guest) fsEntry(f *fsState) browse.Entry {
	note := ""
	switch {
	case !f.Readable:
		note = f.Reason
	case f.withoutJournal:
		note = "Read without replaying the journal: the newest changes may be missing."
	default:
		note = strings.TrimSpace(f.VFS + " " + f.Label)
	}
	return browse.Entry{Name: []byte(f.ID), Type: browse.TypeDir, Size: f.Size, Mode: 0o555, Readable: f.Readable, Note: note}
}

// List lists the file systems at the top, and a folder below one.
func (g *Guest) List(path [][]byte, cursor []byte, limit int) (browse.ListPage, error) {
	if err := browse.ValidatePath(path); err != nil {
		return browse.ListPage{}, err
	}
	if err := g.check(); err != nil {
		return browse.ListPage{}, err
	}
	var entries []browse.Entry
	if len(path) == 0 {
		g.mu.Lock()
		for _, f := range g.fss {
			entries = append(entries, g.fsEntry(f))
		}
		g.mu.Unlock()
		sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].Name, entries[j].Name) < 0 })
	} else {
		f, err := g.readable(path[0])
		if err != nil {
			return browse.ListPage{}, err
		}
		if len(path) > 1 {
			top, serr := g.Stat(path)
			if serr != nil {
				return browse.ListPage{}, serr
			}
			if top.Type != browse.TypeDir {
				return browse.ListPage{}, &browse.Error{Code: browse.CodeNotDir, Msg: "this is not a folder"}
			}
		}
		if entries, err = g.dir(g.ctx, f, path[1:]); err != nil {
			return browse.ListPage{}, err
		}
	}
	return page(entries, cursor, limit), nil
}

func page(entries []browse.Entry, cursor []byte, limit int) browse.ListPage {
	if limit <= 0 {
		limit = 1000
	}
	start := 0
	if len(cursor) > 0 {
		start = sort.Search(len(entries), func(i int) bool { return bytes.Compare(entries[i].Name, cursor) > 0 })
	}
	end := min(start+limit, len(entries))
	p := browse.ListPage{Entries: append([]browse.Entry(nil), entries[start:end]...)}
	if end < len(entries) {
		p.Next = append([]byte(nil), entries[end-1].Name...)
	}
	return p
}

// Stat describes one name.
func (g *Guest) Stat(path [][]byte) (browse.Entry, error) {
	if err := browse.ValidatePath(path); err != nil {
		return browse.Entry{}, err
	}
	if err := g.check(); err != nil {
		return browse.Entry{}, err
	}
	switch len(path) {
	case 0:
		return browse.Entry{Type: browse.TypeDir, Mode: 0o555, Readable: true}, nil
	case 1:
		f, err := g.lookup(path[0])
		if err != nil {
			return browse.Entry{}, err
		}
		return g.fsEntry(f), nil
	}
	f, err := g.readable(path[0])
	if err != nil {
		return browse.Entry{}, err
	}
	entries, err := g.dir(g.ctx, f, path[1:len(path)-1])
	if err != nil {
		return browse.Entry{}, err
	}
	want := path[len(path)-1]
	i := sort.Search(len(entries), func(i int) bool { return bytes.Compare(entries[i].Name, want) >= 0 })
	if i < len(entries) && bytes.Equal(entries[i].Name, want) {
		return entries[i], nil
	}
	return browse.Entry{}, &browse.Error{Code: browse.CodeNotFound, Msg: "no such file or folder"}
}

// Open opens a regular file for reading.
func (g *Guest) Open(path [][]byte) (browse.File, error) {
	e, err := g.Stat(path)
	if err != nil {
		return nil, err
	}
	if len(path) < 2 {
		return nil, &browse.Error{Code: browse.CodeNotFile, Msg: "this is not a file"}
	}
	switch e.Type {
	case browse.TypeFile:
	case browse.TypeSymlink:
		return nil, &browse.Error{Code: browse.CodeLink, Msg: "this is a symbolic link; links are not downloaded"}
	case browse.TypeDir:
		return nil, &browse.Error{Code: browse.CodeNotFile, Msg: "this is a folder"}
	default:
		return nil, &browse.Error{Code: browse.CodeSpecial, Msg: "this is not a regular file"}
	}
	f, err := g.readable(path[0])
	if err != nil {
		return nil, err
	}
	return &guestFile{g: g, fs: f, path: guestPath(path[1:]), size: e.Size}, nil
}

// Tar writes a file or folder as a tar stream, the way any Tree does.
func (g *Guest) Tar(w io.Writer, path [][]byte) (browse.TarReport, error) {
	return browse.WriteTar(g, w, path)
}

var _ browse.Tree = (*Guest)(nil)

// guestFile reads a file by running virt-cat. The tool can only go forward, so a
// read at the place where the last one ended goes on with the same tool, and any
// other place starts it again and skips ahead.
type guestFile struct {
	g    *Guest
	fs   *fsState
	path string
	size int64

	mu      sync.Mutex
	rc      io.ReadCloser
	release func()
	pos     int64
}

func (f *guestFile) Size() int64 { return f.size }

func (f *guestFile) stop() {
	if f.rc != nil {
		_ = f.rc.Close()
		f.release()
		f.rc, f.release = nil, nil
	}
}

func (f *guestFile) ReadAt(p []byte, off int64) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= f.size {
		return 0, io.EOF
	}
	if err := f.g.check(); err != nil {
		return 0, err
	}
	if f.rc == nil || off != f.pos {
		f.stop()
		_, rc, release, err := f.g.run(f.g.ctx, f.fs, func(mount string) []string {
			return f.g.driveArgs("virt-cat", "-m", mount, "--", f.path)
		}, true)
		if err != nil {
			return 0, toBrowseError(err)
		}
		f.rc, f.release, f.pos = rc, release, 0
		if _, err = io.CopyN(io.Discard, f.rc, off); err != nil {
			f.stop()
			return 0, internal("the file got shorter while it was read")
		}
		f.pos = off
	}
	want := int64(len(p))
	if off+want > f.size {
		want = f.size - off
	}
	n, err := io.ReadFull(f.rc, p[:want])
	f.pos += int64(n)
	if err != nil {
		f.stop()
		return n, internal("the file got shorter while it was read")
	}
	if want < int64(len(p)) {
		return n, io.EOF
	}
	return n, nil
}

func (f *guestFile) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.stop()
	return nil
}
