package filelist

import (
	"bytes"
	"io"
	"sort"
	"strings"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

const (
	defaultListLimit = 1000
	maxListLimit     = 10_000
)

// Tree answers listings from a file list and sends everything that reads file
// contents to the disk. It satisfies browse.Tree.
type Tree struct {
	list *List
	disk browse.Tree
}

// NewTree shows list. disk answers Open and Tar; it may be nil while the disk
// is not open, and then they report that.
func NewTree(list *List, disk browse.Tree) *Tree { return &Tree{list: list, disk: disk} }

func notFound() error {
	return &browse.Error{Code: browse.CodeNotFound, Msg: "there is nothing at this path"}
}

func notDir() error {
	return &browse.Error{Code: browse.CodeNotDir, Msg: "this is not a folder"}
}

func (t *Tree) fsEntry(f Filesystem) browse.Entry {
	note := strings.TrimSpace(f.VFS + " " + f.Label)
	if !f.Readable {
		note = f.Reason
	}
	return browse.Entry{Name: []byte(f.ID), Type: browse.TypeDir, Size: f.Size, Mode: 0o555, Readable: f.Readable, Note: note}
}

func (t *Tree) filesystem(name []byte) (Filesystem, error) {
	i, ok := t.list.byID[string(name)]
	if !ok {
		return Filesystem{}, notFound()
	}
	return t.list.fss[i], nil
}

// folder returns the entries of the folder at path (below the file system), or
// an error if the path is not a folder in the list. A folder that the list
// names but has no line for is empty.
func (t *Tree) folder(fsID string, path [][]byte) ([]browse.Entry, error) {
	for i := range path {
		parent := t.list.folders[key(fsID, path[:i])]
		e, ok := find(parent, path[i])
		if !ok {
			return nil, notFound()
		}
		if e.Type != browse.TypeDir {
			return nil, notDir()
		}
	}
	return t.list.folders[key(fsID, path)], nil
}

// List lists the file systems at the top and a folder below one.
func (t *Tree) List(path [][]byte, cursor []byte, limit int) (browse.ListPage, error) {
	if err := browse.ValidatePath(path); err != nil {
		return browse.ListPage{}, err
	}
	if limit <= 0 || limit > maxListLimit {
		limit = defaultListLimit
	}
	var entries []browse.Entry
	if len(path) == 0 {
		for _, f := range t.list.fss {
			entries = append(entries, t.fsEntry(f))
		}
		sort.Slice(entries, func(i, j int) bool { return bytes.Compare(entries[i].Name, entries[j].Name) < 0 })
	} else {
		f, err := t.filesystem(path[0])
		if err != nil {
			return browse.ListPage{}, err
		}
		if !f.Readable {
			return browse.ListPage{}, &browse.Error{Code: browse.CodeNotReadable, Msg: f.Reason}
		}
		if entries, err = t.folder(f.ID, path[1:]); err != nil {
			return browse.ListPage{}, err
		}
	}
	start := 0
	if len(cursor) > 0 {
		start = sort.Search(len(entries), func(i int) bool { return bytes.Compare(entries[i].Name, cursor) > 0 })
	}
	end := min(start+limit, len(entries))
	page := browse.ListPage{Entries: append([]browse.Entry(nil), entries[start:end]...)}
	if end < len(entries) && len(page.Entries) > 0 {
		page.Next = append([]byte(nil), page.Entries[len(page.Entries)-1].Name...)
	}
	return page, nil
}

// Stat describes the entry at path.
func (t *Tree) Stat(path [][]byte) (browse.Entry, error) {
	if err := browse.ValidatePath(path); err != nil {
		return browse.Entry{}, err
	}
	if len(path) == 0 {
		return browse.Entry{Type: browse.TypeDir, Mode: 0o555, Readable: true}, nil
	}
	f, err := t.filesystem(path[0])
	if err != nil {
		return browse.Entry{}, err
	}
	if len(path) == 1 {
		return t.fsEntry(f), nil
	}
	if !f.Readable {
		return browse.Entry{}, &browse.Error{Code: browse.CodeNotReadable, Msg: f.Reason}
	}
	parent, err := t.folder(f.ID, path[1:len(path)-1])
	if err != nil {
		return browse.Entry{}, err
	}
	e, ok := find(parent, path[len(path)-1])
	if !ok {
		return browse.Entry{}, notFound()
	}
	return e, nil
}

func noDisk() error {
	return &browse.Error{Code: browse.CodeInternal, Msg: "the disk is not open yet; only listings are available"}
}

// Open reads a file from the disk itself, never from the list.
func (t *Tree) Open(path [][]byte) (browse.File, error) {
	if t.disk == nil {
		return nil, noDisk()
	}
	return t.disk.Open(path)
}

// Tar reads a file or folder from the disk itself, never from the list.
func (t *Tree) Tar(w io.Writer, path [][]byte) (browse.TarReport, error) {
	if t.disk == nil {
		return browse.TarReport{}, noDisk()
	}
	return t.disk.Tar(w, path)
}
