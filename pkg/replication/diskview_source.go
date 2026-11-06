package replication

import (
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// A DiskView on an UndoSource is the same view as OpenDiskView gives, for a
// backup that is not on a share: the disk and the undo files are read through
// interfaces. The COS disk server uses it, with files that Kopia serves from a
// snapshot (pkg/cosdisk). Design: docs/specs/2026-10-29-vm-disks-in-cos.md,
// section "Phase 6d".

// ErrNoRestorePoint means no restore point of the disk is at or before the time
// that was asked for.
var ErrNoRestorePoint = errors.New("no restore point of the disk is at or before that time")

// maxViewRecords is the most undo records one view indexes, over all its
// layers. A record costs 24 bytes, and the index slice grows by doubling, so
// 2M records use about 48 MB, 96 MB at worst while it grows. That fits the
// disk server pod (256Mi request, 1Gi limit); an undo file from a hostile
// backup must not use up the memory of the helper.
var maxViewRecords = 2 << 20

// maxViewLayers is the most undo files one view reads records from, so the
// caches and open requests of a view have a fixed total.
var maxViewLayers = 1024

// MaxViewLayers is the most undo files one view reads records from.
func MaxViewLayers() int { return maxViewLayers }

// maxUndoFiles is the most undo files one view uses.
const maxUndoFiles = 100000

// UndoUse says what an UndoSource opens a file for, so that a remote source can
// read it in the right way.
type UndoUse int

const (
	// UndoHeader: a few small reads at the start and at the end.
	UndoHeader UndoUse = iota
	// UndoScan: the whole file once, front to back.
	UndoScan
	// UndoRead: reads of single records, in any order.
	UndoRead
)

// UndoFile is an undo file that can be read at any offset.
type UndoFile interface {
	io.ReaderAt
	Size() int64
	Close() error
}

// UndoSource is the undo folder of a backup disk.
type UndoSource interface {
	// Names lists the names in the folder. Any name may be in it, as the folder
	// comes from a backup that is not trusted; the view opens only names that
	// are a number and ".undo".
	Names() ([]string, error)
	// Open opens one file of the folder.
	Open(name string, use UndoUse) (UndoFile, error)
}

var undoNameRE = regexp.MustCompile(`^[0-9]{10,18}` + regexp.QuoteMeta(undoApplied) + `$`)

// ValidUndoName says whether name is the name of a restore point undo file, in
// the one form the writer makes: the number from 1, written with %010d. Any
// other spelling of a number (1.undo, 01.undo) would give two names the same
// place in the order.
func ValidUndoName(name string) bool {
	if !undoNameRE.MatchString(name) {
		return false
	}
	seq, err := strconv.Atoi(strings.TrimSuffix(name, undoApplied))
	return err == nil && seq >= 1 && fmt.Sprintf("%010d%s", seq, undoApplied) == name
}

type sourceUndo struct {
	seq  int
	name string
	hdr  undoHeader
}

// OpenDiskViewFrom opens the backup disk as it was at the newest restore point
// at or before at. disk reads the latest copy, diskSize is its size and latest
// is the moment it shows. The zero time, or a time at or after latest, shows
// the disk itself. A time before every restore point fails with
// ErrNoRestorePoint. The caller keeps the disk open for as long as the view is
// used; Close closes the undo files only.
//
// There is no lease and no check for a change: the source must not change while
// the view is open. A disk written after its last copy cannot be told from a
// good one here; the backup is trusted as it was exported.
func OpenDiskViewFrom(disk io.ReaderAt, diskSize int64, latest time.Time, src UndoSource, at time.Time) (dv *DiskView, err error) {
	d := &DiskView{disk: disk, diskSize: diskSize, size: diskSize, now: time.Now, momentKnown: true}
	defer func() {
		if err != nil {
			_ = d.Close()
			dv = nil
		}
	}()
	files, err := headersFrom(src)
	if err != nil {
		return nil, err
	}
	st := DiskState{Moment: latest, MomentKnown: true}
	for _, f := range files {
		if f.hdr.from.Unix() > 0 {
			st.RestorePoints = append(st.RestorePoints, f.hdr.from)
		}
	}
	if at.IsZero() || !at.Before(latest) {
		d.latest, d.moment = true, latest
		return d, nil
	}
	chosen := time.Time{}
	for _, p := range st.RestorePoints {
		if !p.After(at) {
			chosen = p
		}
	}
	if chosen.IsZero() {
		return nil, fmt.Errorf("%w: the oldest restore point is %s", ErrNoRestorePoint, oldestOf(st).UTC().Format(time.RFC3339))
	}
	d.moment = chosen
	start := -1
	for i, u := range files {
		if u.hdr.from.Equal(chosen) {
			start = i
		}
	}
	if start < 0 {
		return nil, fmt.Errorf("no restore point at %s", chosen.UTC().Format(time.RFC3339Nano))
	}
	if len(files)-start > maxViewLayers {
		return nil, fmt.Errorf("the restore point is %d undo files back; a view reaches back at most %d", len(files)-start, maxViewLayers)
	}
	budget := maxViewRecords
	for _, u := range files[start:] {
		l, lerr := indexFrom(src, u.name, &budget)
		if lerr != nil {
			return nil, lerr
		}
		d.layers = append(d.layers, l)
	}
	d.size = d.layers[0].oldSize
	return d, nil
}

// headersFrom lists the undo files of src, oldest first, with their headers.
// The chosen restore point decides which files are needed, so only the headers
// are read here: a few bytes of each file, and not its records.
func headersFrom(src UndoSource) ([]sourceUndo, error) {
	names, err := src.Names()
	if err != nil {
		return nil, err
	}
	var files []sourceUndo
	for _, n := range names {
		if !ValidUndoName(n) {
			continue
		}
		seq, aerr := strconv.Atoi(strings.TrimSuffix(n, undoApplied))
		if aerr != nil {
			continue
		}
		files = append(files, sourceUndo{seq: seq, name: n})
	}
	if len(files) > maxUndoFiles {
		return nil, fmt.Errorf("the backup has more than %d undo files", maxUndoFiles)
	}
	sort.Slice(files, func(i, j int) bool { return files[i].seq < files[j].seq })
	for i := range files {
		h, herr := readHeaderOf(src, files[i].name)
		if herr != nil {
			return nil, herr
		}
		// Each copy starts from the moment the disk showed after the one before
		// (the receiver refuses any other start), so the files form a chain.
		if i > 0 && !h.from.Equal(files[i-1].hdr.to) {
			return nil, fmt.Errorf("%s does not continue from the undo file before it: %w", files[i].name, ErrUndoOrder)
		}
		files[i].hdr = h
	}
	return files, nil
}

// RestorePointsFrom lists the restore points that the undo files of src reach
// back to, oldest first, from their headers only. The disk itself is one more
// restore point, which the caller knows.
func RestorePointsFrom(src UndoSource) ([]time.Time, error) {
	files, err := headersFrom(src)
	if err != nil {
		return nil, err
	}
	var out []time.Time
	for _, f := range files {
		if f.hdr.from.Unix() > 0 {
			out = append(out, f.hdr.from)
		}
	}
	return out, nil
}

func oldestOf(st DiskState) time.Time {
	if len(st.RestorePoints) > 0 {
		return st.RestorePoints[0]
	}
	return st.Moment
}

// readHeaderOf reads the header and the trailer of one undo file.
func readHeaderOf(src UndoSource, name string) (undoHeader, error) {
	f, err := src.Open(name, UndoHeader)
	if err != nil {
		return undoHeader{}, err
	}
	defer func() { _ = f.Close() }()
	return readUndoHeaderFrom(f, f.Size(), name)
}

// indexFrom checks a whole undo file and notes where each record's data is.
func indexFrom(src UndoSource, name string, budget *int) (viewLayer, error) {
	var l viewLayer
	f, err := src.Open(name, UndoScan)
	if err != nil {
		return l, err
	}
	var prevEnd int64
	h, err := readUndoFrom(f, f.Size(), name, func(off int64, data []byte, pos int64) error {
		if off < prevEnd {
			return fmt.Errorf("%s: %w", name, ErrUndoOrder)
		}
		if *budget--; *budget < 0 {
			return fmt.Errorf("%s: this disk's older restore points are too large to browse (more than %d changed blocks in the undo files)", name, maxViewRecords)
		}
		prevEnd = off + int64(len(data))
		l.recs = append(l.recs, viewRec{off: off, n: int32(len(data)), pos: pos}) // #nosec G115 -- a record is at most compareChunk bytes
		return nil
	})
	_ = f.Close()
	if err != nil {
		return l, err
	}
	l.oldSize = h.oldSize
	rf, err := src.Open(name, UndoRead)
	if err != nil {
		return l, err
	}
	l.f, l.closer = rf, rf
	return l, nil
}
