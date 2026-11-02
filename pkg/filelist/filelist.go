// Package filelist is the file list that an export of a VM disk to COS saves
// with it (docs/specs/2026-10-29-vm-disks-in-cos.md, section 1a). It holds the
// file systems of the guest and, for every folder, its entries: name, type,
// size, mode, owner, times and link target. No file contents.
//
// The list is made from a disk that a tenant controls, so a reader must treat it
// as untrusted: Read refuses names that could escape a folder, too many entries,
// a list that unpacks to far more than it should, and folders that no parent
// lists. A wrong list can only show wrong names. A download or a restore reads
// the disk itself (Tree.Open and Tree.Tar go to the disk), so it refuses a path
// that is only in the list.
//
// Format, version 1: a gzip stream of JSON values. The first is the header
// {"v":1,"moment":...,"fs":[{"id","vfs","label","size","readable","reason"}]}.
// Each later value is one folder {"fs":ID,"path":[name,...],"entries":[Entry,...]},
// with names as base64 (they are bytes, not text).
package filelist

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

const (
	version = 1

	defaultMaxBytes       = 256 << 20
	defaultMaxEntries     = 5_000_000
	defaultMaxFilesystems = 64

	maxLinkLen = 4096
	maxNoteLen = 1024
)

// Limits bound what Read accepts. Zero means the default.
type Limits struct {
	// MaxBytes is the most the list may unpack to.
	MaxBytes int64
	// MaxEntries is the most entries in all folders.
	MaxEntries int
	// MaxFilesystems is the most file systems.
	MaxFilesystems int
}

func (l Limits) bytes() int64 {
	if l.MaxBytes > 0 {
		return l.MaxBytes
	}
	return defaultMaxBytes
}

func (l Limits) entries() int {
	if l.MaxEntries > 0 {
		return l.MaxEntries
	}
	return defaultMaxEntries
}

func (l Limits) filesystems() int {
	if l.MaxFilesystems > 0 {
		return l.MaxFilesystems
	}
	return defaultMaxFilesystems
}

// Filesystem is a file system of the guest: a top level folder.
type Filesystem struct {
	// ID is the name shown as the top level folder: sda1, rhel-root.
	ID    string `json:"id"`
	VFS   string `json:"vfs,omitempty"`
	Label string `json:"label,omitempty"`
	Size  int64  `json:"size,omitempty"`
	// Readable is false for a volume that cannot be listed; Reason says why.
	Readable bool   `json:"readable"`
	Reason   string `json:"reason,omitempty"`
}

type header struct {
	V      int          `json:"v"`
	Moment time.Time    `json:"moment"`
	FS     []Filesystem `json:"fs"`
}

type folderLine struct {
	FS      string         `json:"fs"`
	Path    [][]byte       `json:"path"`
	Entries []browse.Entry `json:"entries"`
}

// List is the file list of one moment of one disk.
type List struct {
	moment  time.Time
	fss     []Filesystem
	byID    map[string]int
	folders map[string][]browse.Entry // key: file system, NUL, names joined by NUL
	entries int
	max     int   // most entries; zero means the default
	err     error // the first bad file system added with AddFilesystem
}

// NewList makes an empty list for the moment.
func NewList(moment time.Time) *List {
	return &List{moment: moment.UTC(), byID: map[string]int{}, folders: map[string][]browse.Entry{}}
}

// Moment is the moment of the disk that the list describes.
func (l *List) Moment() time.Time { return l.moment }

// AddFilesystem adds a file system. A repeated or unsafe ID is an error that
// AddFolder and Encode report.
func (l *List) AddFilesystem(f Filesystem) {
	if err := l.addFS(f); err != nil && l.err == nil {
		l.err = err
	}
}

func (l *List) addFS(f Filesystem) error {
	if err := browse.ValidatePath([][]byte{[]byte(f.ID)}); err != nil {
		return fmt.Errorf("filelist: file system id: %w", err)
	}
	if _, dup := l.byID[f.ID]; dup {
		return fmt.Errorf("filelist: the file system %q is listed twice", f.ID)
	}
	if f.Size < 0 || len(f.Reason) > maxNoteLen || len(f.VFS) > maxNoteLen || len(f.Label) > maxNoteLen {
		return fmt.Errorf("filelist: the file system %q has a bad size or text", f.ID)
	}
	l.byID[f.ID] = len(l.fss)
	l.fss = append(l.fss, f)
	return nil
}

func key(fs string, path [][]byte) string {
	var b strings.Builder
	b.WriteString(fs)
	for _, n := range path {
		b.WriteByte(0)
		b.Write(n)
	}
	return b.String()
}

// AddFolder adds the entries of one folder. path is below the file system;
// empty is its top. Everything is checked as Read checks it.
func (l *List) AddFolder(fsID string, path [][]byte, entries []browse.Entry) error {
	if l.err != nil {
		return l.err
	}
	if _, ok := l.byID[fsID]; !ok {
		return fmt.Errorf("filelist: a folder of the unknown file system %q", fsID)
	}
	if err := browse.ValidatePath(path); err != nil {
		return fmt.Errorf("filelist: %w", err)
	}
	k := key(fsID, path)
	if _, dup := l.folders[k]; dup {
		return fmt.Errorf("filelist: the folder %s of %q is listed twice", browse.JoinPath(path), fsID)
	}
	limit := l.max
	if limit <= 0 {
		limit = defaultMaxEntries
	}
	if l.entries+len(entries) > limit {
		return fmt.Errorf("filelist: more than %d entries", limit)
	}
	clean := make([]browse.Entry, len(entries))
	for i, e := range entries {
		if err := checkEntry(e); err != nil {
			return fmt.Errorf("filelist: in %s of %q: %w", browse.JoinPath(path), fsID, err)
		}
		clean[i] = e
	}
	sort.Slice(clean, func(i, j int) bool { return bytes.Compare(clean[i].Name, clean[j].Name) < 0 })
	for i := 1; i < len(clean); i++ {
		if bytes.Equal(clean[i].Name, clean[i-1].Name) {
			return fmt.Errorf("filelist: the name %q is listed twice in %s of %q", browse.Escape(clean[i].Name), browse.JoinPath(path), fsID)
		}
	}
	l.entries += len(clean)
	l.folders[k] = clean
	return nil
}

func checkEntry(e browse.Entry) error {
	if err := browse.ValidatePath([][]byte{e.Name}); err != nil {
		return err
	}
	switch e.Type {
	case browse.TypeFile, browse.TypeDir, browse.TypeSymlink, browse.TypeOther:
	default:
		return fmt.Errorf("the entry %q has an unknown type", browse.Escape(e.Name))
	}
	switch {
	case e.Size < 0:
		return fmt.Errorf("the entry %q has a negative size", browse.Escape(e.Name))
	case e.Mode > 0o7777:
		return fmt.Errorf("the entry %q has a bad mode", browse.Escape(e.Name))
	case len(e.Link) > maxLinkLen, e.Type != browse.TypeSymlink && len(e.Link) > 0:
		return fmt.Errorf("the entry %q has a bad link target", browse.Escape(e.Name))
	case len(e.Note) > maxNoteLen:
		return fmt.Errorf("the entry %q has too long a note", browse.Escape(e.Name))
	}
	return nil
}

// check makes sure every folder is reachable: its parent lists it as a folder.
func (l *List) check() error {
	for k := range l.folders {
		parts := strings.Split(k, "\x00")
		if len(parts) == 1 {
			continue
		}
		parentKey := strings.Join(parts[:len(parts)-1], "\x00")
		parent, ok := l.folders[parentKey]
		name := []byte(parts[len(parts)-1])
		if !ok || !hasDir(parent, name) {
			return fmt.Errorf("filelist: the folder %q is not listed as a folder in its parent", browse.Escape(name))
		}
	}
	return nil
}

func hasDir(sorted []browse.Entry, name []byte) bool {
	e, ok := find(sorted, name)
	return ok && e.Type == browse.TypeDir
}

func find(sorted []browse.Entry, name []byte) (browse.Entry, bool) {
	i := sort.Search(len(sorted), func(i int) bool { return bytes.Compare(sorted[i].Name, name) >= 0 })
	if i < len(sorted) && bytes.Equal(sorted[i].Name, name) {
		return sorted[i], true
	}
	return browse.Entry{}, false
}

// Encode writes the list, gzip compressed. A repeated file system id or an
// unreachable folder is an error.
func (l *List) Encode(w io.Writer) error {
	if l.err != nil {
		return l.err
	}
	if err := l.check(); err != nil {
		return err
	}
	zw := gzip.NewWriter(w)
	enc := json.NewEncoder(zw)
	if err := enc.Encode(header{V: version, Moment: l.moment, FS: l.fss}); err != nil {
		return err
	}
	keys := make([]string, 0, len(l.folders))
	for k := range l.folders {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		parts := strings.Split(k, "\x00")
		line := folderLine{FS: parts[0], Entries: l.folders[k]}
		for _, p := range parts[1:] {
			line.Path = append(line.Path, []byte(p))
		}
		if err := enc.Encode(line); err != nil {
			return err
		}
	}
	return zw.Close()
}

var errTooLarge = errors.New("filelist: the list unpacks to more than the limit")

type limitedReader struct {
	r    io.Reader
	left int64
}

func (l *limitedReader) Read(p []byte) (int, error) {
	if l.left <= 0 {
		return 0, errTooLarge
	}
	if int64(len(p)) > l.left {
		p = p[:l.left]
	}
	n, err := l.r.Read(p)
	l.left -= int64(n)
	return n, err
}

// Read reads and checks a list. It never returns a list that Encode would
// refuse.
func Read(r io.Reader, lim Limits) (*List, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("filelist: not a gzip stream: %w", err)
	}
	defer zr.Close()
	dec := json.NewDecoder(&limitedReader{r: zr, left: lim.bytes()})
	dec.DisallowUnknownFields()
	var h header
	if err = dec.Decode(&h); err != nil {
		return nil, fmt.Errorf("filelist: reading the header: %w", err)
	}
	if h.V != version {
		return nil, fmt.Errorf("filelist: version %d is not supported", h.V)
	}
	if len(h.FS) > lim.filesystems() {
		return nil, fmt.Errorf("filelist: more than %d file systems", lim.filesystems())
	}
	l := NewList(h.Moment)
	l.max = lim.entries()
	for _, f := range h.FS {
		if err = l.addFS(f); err != nil {
			return nil, err
		}
	}
	for {
		var fl folderLine
		if err = dec.Decode(&fl); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("filelist: reading a folder: %w", err)
		}
		if err = l.AddFolder(fl.FS, fl.Path, fl.Entries); err != nil {
			return nil, err
		}
	}
	if err = l.check(); err != nil {
		return nil, err
	}
	return l, nil
}
