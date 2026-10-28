package cosbrowse

import (
	"bytes"
	"context"
	"errors"
	"io"
	"sort"
	"sync"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// ReadError is an error from the repository whose text is safe to show to a
// user: it names no key and no secret. Anything else that comes out of a Repo
// is replaced by a general sentence.
type ReadError struct{ Msg string }

func (e *ReadError) Error() string { return e.Msg }

// userText is what a user may be told about an error from the repository.
func userText(err error) string {
	var re *ReadError
	switch {
	case errors.As(err, &re):
		return re.Msg
	case errors.Is(err, context.Canceled):
		return "the session was closed"
	case errors.Is(err, context.DeadlineExceeded):
		return "COS did not answer in time"
	}
	return "an unexpected error while reading the backup in COS"
}

func readFailed(err error) error {
	return &browse.Error{Code: browse.CodeInternal, Msg: "the backup in COS could not be read: " + userText(err)}
}

// Root is the top folder of what a Tree shows, and who owns it.
type Root struct {
	OID  string
	Meta browse.Entry // mode, owner and time of the folder; the name and type are set here
}

// TreeOptions tunes a Tree. A field that is zero gets its default.
type TreeOptions struct {
	// LinkFetchers is how many symbolic link targets are read at once. Each one
	// is a run of the Kopia program.
	LinkFetchers int
	// CacheEntries is the most folder entries kept in memory for folders already
	// read.
	CacheEntries int
}

const (
	defaultLinkFetchers = 4
	defaultCacheEntries = 400_000
	maxListLimit        = 10_000
	defaultListLimit    = 1000
	maxLinkCache        = 20_000
)

// Tree is a read-only view of one folder of a Kopia snapshot. It implements
// browse.Tree. A path is walked by object IDs read from the repository, one
// folder at a time. There is no file system underneath, so a link cannot lead
// anywhere, and no name can reach outside the root: a name is only ever compared
// with the names in the folder where it is looked up.
type Tree struct {
	repo Repo
	root Root
	opts TreeOptions

	ctx    context.Context
	cancel context.CancelFunc

	mu        sync.Mutex
	cache     map[string]*manifest
	cacheOrd  []string
	cacheSize int
	links     map[string][]byte
	linkSem   chan struct{}
}

var _ browse.Tree = (*Tree)(nil)
var _ browse.Walker = (*Tree)(nil)

// NewTree makes a Tree that reads the folder with object ID root.OID.
func NewTree(repo Repo, root Root, opts TreeOptions) *Tree {
	if opts.LinkFetchers <= 0 {
		opts.LinkFetchers = defaultLinkFetchers
	}
	if opts.CacheEntries <= 0 {
		opts.CacheEntries = defaultCacheEntries
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &Tree{repo: repo, root: root, opts: opts, ctx: ctx, cancel: cancel,
		cache: map[string]*manifest{}, links: map[string][]byte{}, linkSem: make(chan struct{}, opts.LinkFetchers)}
}

// Close stops every read that is running.
func (t *Tree) Close() error {
	t.cancel()
	return nil
}

func (t *Tree) manifestOf(oid string) (*manifest, error) {
	if !ValidOID(oid) {
		return nil, &browse.Error{Code: browse.CodeNotReadable, Msg: "the backup holds a damaged entry here"}
	}
	t.mu.Lock()
	m, ok := t.cache[oid]
	t.mu.Unlock()
	if ok {
		return m, nil
	}
	rc, err := t.repo.Object(t.ctx, oid)
	if err != nil {
		return nil, readFailed(err)
	}
	m, perr := parseManifest(rc)
	cerr := rc.Close()
	if perr != nil {
		var be *browse.Error
		if errors.As(perr, &be) {
			return nil, perr
		}
		return nil, readFailed(perr)
	}
	if cerr != nil {
		return nil, readFailed(cerr)
	}
	t.remember(oid, m)
	return m, nil
}

// remember keeps a folder in the cache. When the cache holds more entries than
// its limit, the oldest folders go first.
func (t *Tree) remember(oid string, m *manifest) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.cache[oid]; ok {
		return
	}
	t.cache[oid] = m
	t.cacheOrd = append(t.cacheOrd, oid)
	t.cacheSize += len(m.entries) + 1
	for t.cacheSize > t.opts.CacheEntries && len(t.cacheOrd) > 1 {
		old := t.cacheOrd[0]
		t.cacheOrd = t.cacheOrd[1:]
		t.cacheSize -= len(t.cache[old].entries) + 1
		delete(t.cache, old)
	}
}

// folderAt returns the folder at path, starting at the root. Every name on the
// way must be a folder: a link is refused, and so is a file.
func (t *Tree) folderAt(path [][]byte) (*manifest, error) {
	oid := t.root.OID
	for _, name := range path {
		m, err := t.manifestOf(oid)
		if err != nil {
			return nil, err
		}
		e, ok := m.find(name)
		if !ok {
			return nil, &browse.Error{Code: browse.CodeNotFound, Msg: "no such file or folder in this backup"}
		}
		switch e.typ {
		case "d":
			if e.badOID {
				return nil, &browse.Error{Code: browse.CodeNotReadable, Msg: "the backup holds a damaged entry here"}
			}
			oid = e.obj
		case "s":
			return nil, &browse.Error{Code: browse.CodeLink, Msg: "a symbolic link is in the path; links are never followed"}
		default:
			return nil, &browse.Error{Code: browse.CodeNotDir, Msg: "a name in the path is not a folder"}
		}
	}
	return t.manifestOf(oid)
}

// entryAt returns the entry at a non-empty path.
func (t *Tree) entryAt(path [][]byte) (mEntry, error) {
	parent, err := t.folderAt(path[:len(path)-1])
	if err != nil {
		return mEntry{}, err
	}
	e, ok := parent.find(path[len(path)-1])
	if !ok {
		return mEntry{}, &browse.Error{Code: browse.CodeNotFound, Msg: "no such file or folder in this backup"}
	}
	return e, nil
}

func entryType(typ string) string {
	switch typ {
	case "d":
		return browse.TypeDir
	case "f":
		return browse.TypeFile
	case "s":
		return browse.TypeSymlink
	}
	return browse.TypeOther
}

func toEntry(e mEntry) browse.Entry {
	out := browse.Entry{
		Name: append([]byte(nil), e.name...), Type: entryType(e.typ), Mode: e.mode,
		UID: e.uid, GID: e.gid, MTime: e.mtime, Readable: !e.badOID,
	}
	if out.Type == browse.TypeFile {
		out.Size = e.size
	}
	if out.Type == browse.TypeOther {
		out.Readable = true
	}
	return out
}

// fillLinks reads the target of every symbolic link in entries, a few at a time.
// A target that is too long or cannot be read leaves the link unreadable; the
// listing does not fail.
func (t *Tree) fillLinks(raw []mEntry, out []browse.Entry) {
	var wg sync.WaitGroup
	for i := range out {
		if out[i].Type != browse.TypeSymlink || !out[i].Readable {
			continue
		}
		if raw[i].size > maxLinkTarget {
			out[i].Readable = false
			continue
		}
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			target, ok := t.linkTarget(raw[i].obj)
			if !ok {
				out[i].Readable = false
				return
			}
			out[i].Link = target
		}(i)
	}
	wg.Wait()
}

func (t *Tree) linkTarget(oid string) ([]byte, bool) {
	t.mu.Lock()
	b, ok := t.links[oid]
	t.mu.Unlock()
	if ok {
		return b, true
	}
	select {
	case t.linkSem <- struct{}{}:
		defer func() { <-t.linkSem }()
	case <-t.ctx.Done():
		return nil, false
	}
	rc, err := t.repo.Object(t.ctx, oid)
	if err != nil {
		return nil, false
	}
	b, err = io.ReadAll(io.LimitReader(rc, maxLinkTarget+1))
	_ = rc.Close()
	if err != nil || len(b) > maxLinkTarget {
		return nil, false
	}
	t.mu.Lock()
	if len(t.links) >= maxLinkCache {
		t.links = map[string][]byte{}
	}
	t.links[oid] = b
	t.mu.Unlock()
	return b, true
}

// List returns up to limit entries of the folder at path whose names sort after
// cursor.
func (t *Tree) List(path [][]byte, cursor []byte, limit int) (browse.ListPage, error) {
	if err := browse.ValidatePath(path); err != nil {
		return browse.ListPage{}, err
	}
	if limit <= 0 || limit > maxListLimit {
		limit = defaultListLimit
	}
	m, err := t.folderAt(path)
	if err != nil {
		return browse.ListPage{}, err
	}
	start := 0
	if len(cursor) > 0 {
		start = sort.Search(len(m.entries), func(i int) bool { return bytes.Compare(m.entries[i].name, cursor) > 0 })
	}
	end := min(start+limit, len(m.entries))
	raw := m.entries[start:end]
	page := browse.ListPage{Entries: make([]browse.Entry, 0, len(raw))}
	for _, e := range raw {
		page.Entries = append(page.Entries, toEntry(e))
	}
	t.fillLinks(raw, page.Entries)
	if end < len(m.entries) && len(raw) > 0 {
		page.Next = append([]byte(nil), raw[len(raw)-1].name...)
	}
	return page, nil
}

// Walk calls fn for every entry of the folder at path.
func (t *Tree) Walk(path [][]byte, fn func(browse.Entry) error) error {
	if err := browse.ValidatePath(path); err != nil {
		return err
	}
	m, err := t.folderAt(path)
	if err != nil {
		return err
	}
	const batch = 500
	for start := 0; start < len(m.entries); start += batch {
		raw := m.entries[start:min(start+batch, len(m.entries))]
		entries := make([]browse.Entry, 0, len(raw))
		for _, e := range raw {
			entries = append(entries, toEntry(e))
		}
		t.fillLinks(raw, entries)
		for _, e := range entries {
			if err := fn(e); err != nil {
				return err
			}
		}
	}
	return nil
}

// Stat describes the name at path. A link is described, not followed.
func (t *Tree) Stat(path [][]byte) (browse.Entry, error) {
	if err := browse.ValidatePath(path); err != nil {
		return browse.Entry{}, err
	}
	if len(path) == 0 {
		e := t.root.Meta
		e.Name, e.Type, e.Readable = nil, browse.TypeDir, true
		if e.Mode == 0 {
			e.Mode = 0o755
		}
		return e, nil
	}
	e, err := t.entryAt(path)
	if err != nil {
		return browse.Entry{}, err
	}
	out := toEntry(e)
	if out.Type == browse.TypeSymlink && out.Readable {
		if target, ok := t.linkTarget(e.obj); ok {
			out.Link = target
		} else {
			out.Readable = false
		}
	}
	return out, nil
}

// Open opens the regular file at path. A link, a folder, and a device, pipe or
// socket are refused.
func (t *Tree) Open(path [][]byte) (browse.File, error) {
	if err := browse.ValidatePath(path); err != nil {
		return nil, err
	}
	if len(path) == 0 {
		return nil, &browse.Error{Code: browse.CodeNotFile, Msg: "this is a folder, not a file"}
	}
	e, err := t.entryAt(path)
	if err != nil {
		return nil, err
	}
	switch e.typ {
	case "f":
	case "d":
		return nil, &browse.Error{Code: browse.CodeNotFile, Msg: "this is a folder, not a file"}
	case "s":
		return nil, &browse.Error{Code: browse.CodeLink, Msg: "this is a symbolic link; links are not downloaded"}
	default:
		return nil, &browse.Error{Code: browse.CodeSpecial, Msg: "this is not a regular file"}
	}
	if e.badOID {
		return nil, &browse.Error{Code: browse.CodeNotReadable, Msg: "the backup holds a damaged entry here"}
	}
	return newObjectFile(t.ctx, t.repo, e.obj, e.size), nil
}

// Tar writes the file or folder at path as a tar stream.
func (t *Tree) Tar(w io.Writer, path [][]byte) (browse.TarReport, error) {
	return browse.WriteTar(t, w, path)
}
