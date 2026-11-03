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

// NewListWithLimits is NewList with the entry limit of lim, so that a list that
// is built holds no more than a list that is read.
func NewListWithLimits(moment time.Time, lim Limits) *List {
	l := NewList(moment)
	l.max = lim.entries()
	return l
}

// EntryLimit is the most entries a list may hold.
func (l Limits) EntryLimit() int { return l.entries() }

// ByteLimit is the most the list may unpack to. The builder uses it as the
// budget for what it holds in memory.
func (l Limits) ByteLimit() int64 { return l.bytes() }

// MaxLinkLen is the longest link target an entry may have.
const MaxLinkLen = maxLinkLen

// FilesystemLimit is the most file systems a list may hold.
func (l Limits) FilesystemLimit() int { return l.filesystems() }

// Entries is how many entries the list holds, in all folders.
func (l *List) Entries() int { return l.entries }

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

// token reads one JSON token and checks it is the delimiter want.
func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if d, ok := tok.(json.Delim); !ok || d != want {
		return fmt.Errorf("expected %q, found %v", rune(want), tok)
	}
	return nil
}

// readArray calls item for each element of the JSON array that comes next. A
// null is an empty array. The elements are decoded one at a time, so a limit
// that item enforces stops the read before the rest is unpacked.
func readArray(dec *json.Decoder, item func() error) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok == nil {
		return nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return fmt.Errorf("expected a list, found %v", tok)
	}
	for dec.More() {
		if err = item(); err != nil {
			return err
		}
	}
	return expectDelim(dec, ']')
}

// readObject calls field for each key of the JSON object that comes next. field
// must read the value; it returns an error for a key it does not know.
func readObject(dec *json.Decoder, field func(key string) error) error {
	if err := expectDelim(dec, '{'); err != nil {
		return err
	}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		key, ok := tok.(string)
		if !ok {
			return fmt.Errorf("expected a field name, found %v", tok)
		}
		if err = field(key); err != nil {
			return err
		}
	}
	return expectDelim(dec, '}')
}

// readHeader reads the header and adds its file systems one at a time.
func (l *List) readHeader(dec *json.Decoder, lim Limits) error {
	seenV := false
	err := readObject(dec, func(key string) error {
		switch key {
		case "v":
			var v int
			if err := dec.Decode(&v); err != nil {
				return err
			}
			if v != version {
				return fmt.Errorf("version %d is not supported", v)
			}
			seenV = true
		case "moment":
			var m time.Time
			if err := dec.Decode(&m); err != nil {
				return err
			}
			l.moment = m.UTC()
		case "fs":
			return readArray(dec, func() error {
				if len(l.fss) >= lim.filesystems() {
					return fmt.Errorf("more than %d file systems", lim.filesystems())
				}
				var f Filesystem
				if err := dec.Decode(&f); err != nil {
					return err
				}
				return l.addFS(f)
			})
		default:
			return fmt.Errorf("unknown field %q in the header", key)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("filelist: reading the header: %w", err)
	}
	if !seenV {
		return errors.New("filelist: the header has no version")
	}
	return nil
}

// readFolder reads one folder line. The entries are checked and counted as they
// are decoded, against the entries already read.
func (l *List) readFolder(dec *json.Decoder) (fs string, path [][]byte, entries []browse.Entry, err error) {
	limit := l.max
	if limit <= 0 {
		limit = defaultMaxEntries
	}
	err = readObject(dec, func(key string) error {
		switch key {
		case "fs":
			return dec.Decode(&fs)
		case "path":
			return readArray(dec, func() error {
				if len(path) >= browse.MaxPathDepth {
					return fmt.Errorf("a path deeper than %d folders", browse.MaxPathDepth)
				}
				var n []byte
				if derr := dec.Decode(&n); derr != nil {
					return derr
				}
				path = append(path, n)
				return nil
			})
		case "entries":
			return readArray(dec, func() error {
				if l.entries+len(entries) >= limit {
					return fmt.Errorf("more than %d entries", limit)
				}
				var e browse.Entry
				if derr := dec.Decode(&e); derr != nil {
					return derr
				}
				if cerr := checkEntry(e); cerr != nil {
					return cerr
				}
				entries = append(entries, e)
				return nil
			})
		}
		return fmt.Errorf("unknown field %q in a folder", key)
	})
	return fs, path, entries, err
}

// Read reads and checks a list. It never returns a list that Encode would
// refuse. The file systems and the entries are counted as they are read, so a
// list that passes a limit is refused before the rest of it is unpacked; one
// value (a name) can be as large as the unpacked size limit allows.
func Read(r io.Reader, lim Limits) (*List, error) {
	zr, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("filelist: not a gzip stream: %w", err)
	}
	defer func() { _ = zr.Close() }()
	dec := json.NewDecoder(&limitedReader{r: zr, left: lim.bytes()})
	l := NewList(time.Time{})
	l.max = lim.entries()
	if err = l.readHeader(dec, lim); err != nil {
		return nil, err
	}
	for dec.More() {
		fs, path, entries, ferr := l.readFolder(dec)
		if ferr != nil {
			return nil, fmt.Errorf("filelist: reading a folder: %w", ferr)
		}
		if err = l.AddFolder(fs, path, entries); err != nil {
			return nil, err
		}
	}
	if _, terr := dec.Token(); !errors.Is(terr, io.EOF) {
		return nil, fmt.Errorf("filelist: unexpected data after the last folder: %v", terr)
	}
	if err = l.check(); err != nil {
		return nil, err
	}
	return l, nil
}
