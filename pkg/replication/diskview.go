package replication

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// A DiskView shows a backed-up VM disk as it was at one moment, read-only, with
// no copy and no scratch space. It rebuilds the old contents on every read from
// the latest disk and the undo files: the same rule as Rewind, applied to the
// window that was asked for. The result for every moment is byte for byte what
// RestoreDisk writes. The view never writes: it does not roll back a copy that
// stopped half way (Recover does), it does not delete a .part file, and it does
// not reseal the disk. It is for the browse helper, which mounts the backup
// read-only. Design: docs/specs/2026-10-22-backup-file-restore.md, section 3.

// ErrDiskChanged means the backup disk or its undo files changed while a view
// was open, for example because a new copy started. The view then shows nothing
// more, since a half-applied copy would give a mix of two moments.
var ErrDiskChanged = errors.New("the backup disk changed while it was open; open it again")

// ErrUndoOrder means an undo file has records that are not in order, so a view
// cannot be indexed. The writer always writes records in order of offset.
var ErrUndoOrder = errors.New("an undo file has records out of order")

// checkEvery is how often a read looks for a change of the backup.
const checkEvery = 2 * time.Second

// viewRec is one record of an undo file: n bytes that belong at off, stored at
// pos in the file. The data stays in the file.
type viewRec struct {
	off int64
	n   int32
	pos int64
}

type viewLayer struct {
	f       *os.File
	recs    []viewRec // in order of offset, never overlapping
	oldSize int64     // the size of the disk before this undo file's copy
}

// DiskView is a read-only view of a backup disk at one moment. It is safe to
// read from many goroutines.
type DiskView struct {
	root     *os.Root
	disk     *os.File
	diskSize int64
	diskInfo os.FileInfo
	// layers are the undo files from the chosen restore point up to the newest.
	// Reading applies them newest first, like Rewind.
	layers []viewLayer
	size   int64

	moment      time.Time
	momentKnown bool
	latest      bool

	undoNames string // the names in the undo folder when the view was opened

	now       func() time.Time
	lastCheck atomic.Int64 // unix nanoseconds
	changed   atomic.Bool
}

// OpenDiskView opens the backup disk as it was at the newest moment at or
// before at. The zero time asks for the latest. It fails, as RestoreDisk does,
// if a copy is being applied (ErrCopyInProgress), if the disk was written after
// the last copy, or if there is no moment at or before at.
func OpenDiskView(disk string, at time.Time) (dv *DiskView, err error) {
	v, err := openVolume(disk)
	if err != nil {
		return nil, err
	}
	d := &DiskView{root: v.root, now: time.Now}
	// The view keeps the folder handle open; on failure it is closed here.
	defer func() {
		if err != nil {
			d.Close()
			dv = nil
		}
	}()
	files, err := undoHeaders(v)
	if err != nil {
		return nil, err
	}
	st, err := v.stateFrom(files)
	if err != nil {
		return nil, err
	}
	if st.Unfinished {
		return nil, ErrCopyInProgress
	}
	if st.Modified {
		return nil, errors.New("the backup disk changed after its last copy; its restore points no longer fit it")
	}
	chosen, latest, err := chooseMoment(st, at)
	if err != nil {
		return nil, err
	}
	d.latest, d.momentKnown = latest, st.MomentKnown
	if latest {
		d.moment = st.Moment
	} else {
		d.moment, d.momentKnown = chosen, true
	}
	if d.disk, err = v.openDisk(os.O_RDONLY); err != nil {
		return nil, err
	}
	if d.diskInfo, err = d.disk.Stat(); err != nil {
		return nil, err
	}
	d.diskSize, d.size = d.diskInfo.Size(), d.diskInfo.Size()
	if !latest {
		start := -1
		for i, u := range files {
			if u.hdr.from.Equal(chosen) {
				start = i
			}
		}
		if start < 0 {
			return nil, fmt.Errorf("no restore point at %s", chosen.UTC().Format(time.RFC3339Nano))
		}
		for _, u := range files[start:] {
			l, lerr := indexUndo(v, u.path)
			if lerr != nil {
				return nil, lerr
			}
			d.layers = append(d.layers, l)
		}
		d.size = d.layers[0].oldSize
	}
	if d.undoNames, err = undoNames(v); err != nil {
		return nil, err
	}
	d.lastCheck.Store(d.now().UnixNano())
	return d, nil
}

// undoHeaders lists the restore point undo files, oldest first, from their
// headers and trailers only. The records are read when a file is indexed.
func undoHeaders(v *volume) ([]undoFile, error) {
	entries, err := v.undoEntries()
	if err != nil {
		return nil, err
	}
	var files []undoFile
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, undoApplied) {
			continue
		}
		seq, aerr := strconv.Atoi(strings.TrimSuffix(name, undoApplied))
		if aerr != nil {
			continue
		}
		p := undoPath(name)
		h, herr := readUndoHeader(v, p)
		if herr != nil {
			return nil, herr
		}
		files = append(files, undoFile{seq: seq, path: p, hdr: h})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].seq < files[j].seq })
	return files, nil
}

// readUndoHeader reads the header of an undo file and checks that it ends with
// the end marker.
func readUndoHeader(v *volume, path string) (undoHeader, error) {
	var h undoHeader
	f, err := v.root.Open(path)
	if err != nil {
		return h, err
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return h, err
	}
	trailer := int64(len(undoEndMagic)) + 8
	if !fi.Mode().IsRegular() || fi.Size() < 40+trailer {
		return h, fmt.Errorf("undo file %s is not complete", path)
	}
	hdr := make([]byte, 40)
	if _, err = f.ReadAt(hdr, 0); err != nil || string(hdr[:8]) != undoMagic {
		return h, fmt.Errorf("undo file %s has no valid header", path)
	}
	end := make([]byte, trailer)
	if _, err = f.ReadAt(end, fi.Size()-trailer); err != nil || string(end[:len(undoEndMagic)]) != undoEndMagic {
		return h, fmt.Errorf("undo file %s is not complete", path)
	}
	be := binary.BigEndian
	h.from = time.Unix(0, int64(be.Uint64(hdr[8:]))) // #nosec G115 -- a time, stored bit for bit
	h.to = time.Unix(0, int64(be.Uint64(hdr[16:])))  // #nosec G115 -- a time, stored bit for bit
	h.oldSize = int64(be.Uint64(hdr[24:]))           // #nosec G115 -- written from a file size
	h.newSize = int64(be.Uint64(hdr[32:]))           // #nosec G115 -- written from a file size
	if h.oldSize < 0 || h.newSize < 0 {
		return h, fmt.Errorf("undo file %s has a bad header", path)
	}
	return h, nil
}

// indexUndo checks a whole undo file and notes where each record's data is. It
// keeps the file open for reads.
func indexUndo(v *volume, path string) (viewLayer, error) {
	var l viewLayer
	var prevEnd int64
	h, err := readUndo(v, path, func(off int64, data []byte, pos int64) error {
		if off < prevEnd {
			return fmt.Errorf("%s: %w", path, ErrUndoOrder)
		}
		prevEnd = off + int64(len(data))
		l.recs = append(l.recs, viewRec{off: off, n: int32(len(data)), pos: pos}) // #nosec G115 -- a record is at most compareChunk bytes
		return nil
	})
	if err != nil {
		return l, err
	}
	l.oldSize = h.oldSize
	if l.f, err = v.root.Open(path); err != nil {
		return l, err
	}
	return l, nil
}

// undoNames returns the names in the undo folder as one string, for noticing a
// copy that starts.
func undoNames(v *volume) (string, error) {
	entries, err := v.undoEntries()
	if err != nil {
		return "", err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	return strings.Join(names, "\n"), nil
}

// Size is the size of the disk at the moment of the view.
func (d *DiskView) Size() int64 { return d.size }

// Moment is the moment the view shows. It is the zero time if it is not known.
func (d *DiskView) Moment() time.Time { return d.moment }

// MomentKnown is false for a disk that has no restore point and no moment file.
func (d *DiskView) MomentKnown() bool { return d.momentKnown }

// Latest is true if the view is the newest copy, which is the disk itself.
func (d *DiskView) Latest() bool { return d.latest }

// Close closes the files of the view.
func (d *DiskView) Close() error {
	var first error
	keep := func(err error) {
		if first == nil {
			first = err
		}
	}
	if d.disk != nil {
		keep(d.disk.Close())
	}
	for _, l := range d.layers {
		if l.f != nil {
			keep(l.f.Close())
		}
	}
	if d.root != nil {
		keep(d.root.Close())
	}
	return first
}

// check looks, at most every checkEvery, for a change of the backup disk or of
// the undo folder. Any change ends the view for good.
func (d *DiskView) check() error {
	if d.changed.Load() {
		return ErrDiskChanged
	}
	now := d.now().UnixNano()
	last := d.lastCheck.Load()
	if now-last < int64(checkEvery) || !d.lastCheck.CompareAndSwap(last, now) {
		return nil
	}
	v := &volume{root: d.root, disk: ""}
	names, nerr := undoNames(v)
	fi, serr := d.root.Lstat(d.diskName())
	if nerr != nil || serr != nil || names != d.undoNames ||
		!os.SameFile(fi, d.diskInfo) || fi.Size() != d.diskInfo.Size() || !fi.ModTime().Equal(d.diskInfo.ModTime()) {
		d.changed.Store(true)
		return ErrDiskChanged
	}
	return nil
}

// diskName is the name of the disk in the folder.
func (d *DiskView) diskName() string { return d.diskInfo.Name() }

// ReadAt reads the disk as it was at the moment of the view. It follows
// io.ReaderAt: a read that reaches the end of the disk returns io.EOF.
func (d *DiskView) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if err := d.check(); err != nil {
		return 0, err
	}
	if off >= d.size {
		return 0, io.EOF
	}
	want := int64(len(p))
	n := min(want, d.size-off)
	win := p[:n]
	if err := d.fill(win, off); err != nil {
		return 0, err
	}
	if n < want {
		return int(n), io.EOF
	}
	return int(n), nil
}

// fill builds the window [off, off+len(win)) of the view. The latest disk is the
// base; the undo files are applied newest first, each followed by the cut to the
// size the disk had before that copy, exactly as Rewind does.
func (d *DiskView) fill(win []byte, off int64) error {
	end := off + int64(len(win))
	clear(win)
	if off < d.diskSize {
		m := min(end, d.diskSize) - off
		if _, err := d.disk.ReadAt(win[:m], off); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("reading the backup disk: %w", err)
		}
	}
	for j := len(d.layers) - 1; j >= 0; j-- {
		l := &d.layers[j]
		// The records first (they lie below the old size), then the cut.
		i := sort.Search(len(l.recs), func(i int) bool { return l.recs[i].off+int64(l.recs[i].n) > off })
		for ; i < len(l.recs) && l.recs[i].off < end; i++ {
			r := l.recs[i]
			from := max(off, r.off)
			to := min(end, r.off+int64(r.n))
			if _, err := l.f.ReadAt(win[from-off:to-off], r.pos+(from-r.off)); err != nil {
				return fmt.Errorf("reading an undo file: %w", err)
			}
		}
		if l.oldSize < end {
			clear(win[max(l.oldSize, off)-off:])
		}
	}
	return nil
}
