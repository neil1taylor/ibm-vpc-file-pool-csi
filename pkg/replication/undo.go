package replication

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
)

// The far-site disk is changed only through ApplyCopy, which first saves the
// old contents of every block it will change in an undo file next to the disk.
// An undo file goes through three names:
//
//	<seq>.undo.part   being written; the disk is not changed yet
//	<seq>.undo.ready  complete; the disk may be half changed
//	<seq>.undo        the disk holds the copy; this is a restore point
//
// Recover rolls a disk back from a .ready file, so a copy that stopped half
// way never leaves a mixed disk. Rewind applies .undo files, newest first, to
// a copy of the disk to go back to an earlier copy. Design:
// docs/specs/2026-10-20-vm-safe-replication.md.

const (
	undoDirName   = ".repl-undo"
	undoMagic     = "REPLUND1"
	undoEndMagic  = "REPLEND1"
	undoPart      = ".undo.part"
	undoReady     = ".undo.ready"
	undoApplied   = ".undo"
	compareChunk  = 64 << 10
	gapReadLength = 1 << 20
)

// CopySource is a staged disk copy. *qcow2.Image is one.
type CopySource interface {
	Size() int64
	Walk(fn func(qcow2.Extent) error) error
	DataReader(e qcow2.Extent) io.Reader
}

// ApplyOptions describe a copy to apply.
type ApplyOptions struct {
	// From is the moment the far-site disk shows now; To is the moment the
	// copy shows. From becomes a restore point.
	From, To time.Time
	// Full means the copy holds the whole disk: parts it does not hold are
	// zeros. Otherwise the copy holds only changed blocks.
	Full bool
	// Keep is how long restore points are kept; 0 keeps all.
	Keep time.Duration
	// Now is the current time, for Keep.
	Now time.Time

	// beforeDiskWrite runs before each write to the disk (tests only).
	beforeDiskWrite func() error
}

// ApplyResult says what ApplyCopy did.
type ApplyResult struct {
	// ChangedBytes is how much of the disk changed.
	ChangedBytes int64
	// RolledBack is true if an earlier copy that stopped half way was rolled
	// back first.
	RolledBack bool
}

func undoDir(disk string) string { return filepath.Join(filepath.Dir(disk), undoDirName) }

// volume is the folder that holds a far-site disk. Every file operation goes
// through an os.Root on it, so a link planted in the folder, for example by a
// VM that used it after a failover, cannot lead to another volume on the share.
type volume struct {
	root *os.Root
	disk string // file name of the disk in the folder
}

func openVolume(disk string) (*volume, error) {
	root, err := os.OpenRoot(filepath.Dir(disk))
	if err != nil {
		return nil, err
	}
	return &volume{root: root, disk: filepath.Base(disk)}, nil
}

func (v *volume) close() { _ = v.root.Close() }

// diskInfo returns the disk's file info; the disk must be a regular file.
func (v *volume) diskInfo() (os.FileInfo, error) {
	fi, err := v.root.Lstat(v.disk)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", v.disk)
	}
	return fi, nil
}

func (v *volume) openDisk(flag int) (*os.File, error) {
	if _, err := v.diskInfo(); err != nil {
		return nil, err
	}
	return v.root.OpenFile(v.disk, flag, 0)
}

// undoEntries lists the undo folder; a missing folder is empty.
func (v *volume) undoEntries() ([]os.DirEntry, error) {
	fi, err := v.root.Lstat(undoDirName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("%s is not a folder", undoDirName)
	}
	d, err := v.root.Open(undoDirName)
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	return d.ReadDir(-1)
}

// sync makes a rename or a new file in the named folder durable.
func (v *volume) sync(name string) error {
	d, err := v.root.Open(name)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func undoPath(name string) string { return filepath.Join(undoDirName, name) }

// sortExtents sorts extents by offset.
func sortExtents(exts []qcow2.Extent) {
	sort.Slice(exts, func(i, j int) bool { return exts[i].Offset < exts[j].Offset })
}

// ApplyCopy writes a staged copy into the disk so that the disk is never left
// half changed: it rolls back an earlier copy that stopped half way, writes an
// undo file, changes the disk, and marks the undo file as a restore point. Only
// blocks whose contents differ are changed and saved. If the disk does not
// exist yet, it is made in one step and there is no restore point.
func ApplyCopy(src CopySource, disk string, opts ApplyOptions) (ApplyResult, error) {
	var res ApplyResult
	v, err := openVolume(disk)
	if err != nil {
		return res, err
	}
	defer v.close()
	rolled, err := v.recover()
	if err != nil {
		return res, fmt.Errorf("rolling back an unfinished copy: %w", err)
	}
	res.RolledBack = rolled

	fi, err := v.diskInfo()
	if errors.Is(err, os.ErrNotExist) {
		n, werr := writeNewDiskFrom(src, v)
		res.ChangedBytes = n
		return res, werr
	}
	if err != nil {
		return res, err
	}
	oldSize, newSize := fi.Size(), src.Size()

	f, err := v.openDisk(os.O_RDWR)
	if err != nil {
		return res, err
	}
	defer func() { _ = f.Close() }()

	if err = v.root.MkdirAll(undoDirName, 0o770); err != nil { // #nosec G301 -- shared with the QEMU user
		return res, err
	}
	seq, err := nextUndoSeq(v)
	if err != nil {
		return res, err
	}
	base := undoPath(fmt.Sprintf("%010d", seq))

	// 1. Save the old contents of every block that will change.
	changed, err := writeUndo(v, base+undoPart, src, f, oldSize, opts)
	if err != nil {
		_ = v.root.Remove(base + undoPart)
		return res, err
	}
	if err = v.root.Rename(base+undoPart, base+undoReady); err != nil {
		return res, err
	}
	if err = v.sync(undoDirName); err != nil {
		return res, err
	}

	// 2. Change the disk.
	if newSize > oldSize {
		if err = f.Truncate(newSize); err != nil {
			return res, err
		}
	}
	err = forEachNewChunk(src, opts.Full, func(off int64, data []byte) error {
		if !changed[off] {
			return nil
		}
		if opts.beforeDiskWrite != nil {
			if herr := opts.beforeDiskWrite(); herr != nil {
				return herr
			}
		}
		_, werr := f.WriteAt(data, off)
		return werr
	})
	if err != nil {
		return res, fmt.Errorf("writing the copy into the disk: %w", err)
	}
	if newSize < oldSize {
		if err = f.Truncate(newSize); err != nil {
			return res, err
		}
	}
	if err = f.Sync(); err != nil {
		return res, err
	}

	// 3. The disk holds the copy: the undo file is now a restore point.
	if err = v.root.Rename(base+undoReady, base+undoApplied); err != nil {
		return res, err
	}
	if err = v.sync(undoDirName); err != nil {
		return res, err
	}
	res.ChangedBytes = int64(len(changed)) * compareChunk
	if opts.Keep > 0 {
		if err = pruneUndo(v, opts.Now.Add(-opts.Keep)); err != nil {
			return res, err
		}
	}
	return res, nil
}

// writeNewDiskFrom makes a new disk from a copy, in one step.
func writeNewDiskFrom(src CopySource, v *volume) (int64, error) {
	tmp := v.disk + newDiskSuffix
	if err := v.root.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, err
	}
	f, err := v.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, diskFileMode)
	if err != nil {
		return 0, err
	}
	defer func() { _ = f.Close() }()
	if err = f.Chmod(diskFileMode); err != nil {
		return 0, err
	}
	if err = f.Truncate(src.Size()); err != nil {
		return 0, err
	}
	var n int64
	err = src.Walk(func(e qcow2.Extent) error {
		if e.Kind != qcow2.Data {
			return nil
		}
		w, cerr := io.Copy(io.NewOffsetWriter(f, e.Offset), src.DataReader(e))
		n += w
		return cerr
	})
	if err == nil {
		err = f.Sync()
	}
	if err == nil {
		err = f.Close()
	}
	if err == nil {
		err = v.root.Rename(tmp, v.disk)
	}
	if err != nil {
		_ = v.root.Remove(tmp)
		return n, err
	}
	return n, v.sync(".")
}

// forEachNewChunk calls fn with the new contents of the copy, one chunk of at
// most compareChunk bytes at a time, at chunk-aligned offsets relative to each
// region. For a full copy the gaps between extents are zeros.
func forEachNewChunk(src CopySource, full bool, fn func(off int64, data []byte) error) error {
	zeros := make([]byte, gapReadLength)
	emitZeros := func(off, n int64) error {
		for n > 0 {
			l := min(n, compareChunk)
			if err := fn(off, zeros[:l]); err != nil {
				return err
			}
			off, n = off+l, n-l
		}
		return nil
	}
	var next int64
	err := src.Walk(func(e qcow2.Extent) error {
		if full && e.Offset > next {
			if err := emitZeros(next, e.Offset-next); err != nil {
				return err
			}
		}
		next = e.Offset + e.Length
		if e.Kind == qcow2.Zero {
			return emitZeros(e.Offset, e.Length)
		}
		r := bufio.NewReaderSize(src.DataReader(e), compareChunk)
		buf := make([]byte, compareChunk)
		for off, n := e.Offset, e.Length; n > 0; {
			l := min(n, compareChunk)
			if _, err := io.ReadFull(r, buf[:l]); err != nil {
				return fmt.Errorf("reading copy at %d: %w", off, err)
			}
			if err := fn(off, buf[:l]); err != nil {
				return err
			}
			off, n = off+l, n-l
		}
		return nil
	})
	if err != nil {
		return err
	}
	if full && src.Size() > next {
		return emitZeros(next, src.Size()-next)
	}
	return nil
}

// writeUndo writes the undo file for a copy: the old contents of each chunk
// whose new contents differ. It returns the offsets of those chunks.
func writeUndo(v *volume, name string, src CopySource, disk *os.File, oldSize int64, opts ApplyOptions) (map[int64]bool, error) {
	uf, err := v.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o660)
	if err != nil {
		return nil, err
	}
	defer func() { _ = uf.Close() }()
	w := bufio.NewWriterSize(uf, 1<<20)
	hdr := make([]byte, 0, 40)
	hdr = append(hdr, undoMagic...)
	hdr = binary.BigEndian.AppendUint64(hdr, uint64(opts.From.UnixNano())) // #nosec G115 -- a time, stored bit for bit
	hdr = binary.BigEndian.AppendUint64(hdr, uint64(opts.To.UnixNano()))   // #nosec G115 -- a time, stored bit for bit
	hdr = binary.BigEndian.AppendUint64(hdr, uint64(oldSize))              // #nosec G115 -- a file size is never negative
	hdr = binary.BigEndian.AppendUint64(hdr, uint64(src.Size()))           // #nosec G115 -- a file size is never negative
	if _, err = w.Write(hdr); err != nil {
		return nil, err
	}

	changed := map[int64]bool{}
	old := make([]byte, compareChunk)
	var records uint64
	err = forEachNewChunk(src, opts.Full, func(off int64, data []byte) error {
		if off >= oldSize {
			return nil // beyond the old end: the roll-back truncates it away
		}
		n := min(int64(len(data)), oldSize-off)
		o := old[:n]
		if _, rerr := disk.ReadAt(o, off); rerr != nil && !errors.Is(rerr, io.EOF) {
			return fmt.Errorf("reading disk at %d: %w", off, rerr)
		}
		if bytes.Equal(o, data[:n]) && int64(len(data)) == n {
			return nil
		}
		changed[off] = true
		var rec [16]byte
		binary.BigEndian.PutUint64(rec[0:], uint64(off)) // #nosec G115 -- offsets are never negative
		binary.BigEndian.PutUint64(rec[8:], uint64(n))   // #nosec G115 -- lengths are never negative
		if _, werr := w.Write(rec[:]); werr != nil {
			return werr
		}
		_, werr := w.Write(o)
		records++
		return werr
	})
	if err != nil {
		return nil, err
	}
	// Chunks past the old end still change; they need no undo record.
	if src.Size() > oldSize {
		if err = forEachNewChunk(src, opts.Full, func(off int64, data []byte) error {
			if off >= oldSize && !allZero(data) {
				changed[off] = true
			}
			return nil
		}); err != nil {
			return nil, err
		}
	}
	end := append([]byte(undoEndMagic), binary.BigEndian.AppendUint64(nil, records)...)
	if _, err = w.Write(end); err != nil {
		return nil, err
	}
	if err = w.Flush(); err != nil {
		return nil, err
	}
	if err = uf.Sync(); err != nil {
		return nil, err
	}
	return changed, uf.Close()
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

type undoHeader struct {
	from, to         time.Time
	oldSize, newSize int64
}

// readUndo checks an undo file and calls fn for each record, if fn is not nil.
func readUndo(v *volume, path string, fn func(off int64, data []byte) error) (undoHeader, error) {
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
	r := bufio.NewReaderSize(f, 1<<20)
	hdr := make([]byte, 40)
	if _, err = io.ReadFull(r, hdr); err != nil || string(hdr[:8]) != undoMagic {
		return h, fmt.Errorf("undo file %s has no valid header", path)
	}
	be := binary.BigEndian
	h.from = time.Unix(0, int64(be.Uint64(hdr[8:])))           // #nosec G115 -- a time, stored bit for bit
	h.to = time.Unix(0, int64(be.Uint64(hdr[16:])))            // #nosec G115 -- a time, stored bit for bit
	h.oldSize = int64(be.Uint64(hdr[24:]))                     // #nosec G115 -- written from a file size
	h.newSize = int64(be.Uint64(hdr[32:]))                     // #nosec G115 -- written from a file size
	remaining := fi.Size() - 40 - int64(len(undoEndMagic)) - 8 // bytes of records
	buf := make([]byte, compareChunk)
	var records uint64
	for remaining > 0 {
		var rec [16]byte
		if _, err = io.ReadFull(r, rec[:]); err != nil {
			return h, fmt.Errorf("undo file %s is cut short: %w", path, err)
		}
		off, n := int64(be.Uint64(rec[0:])), int64(be.Uint64(rec[8:])) // #nosec G115 -- written from offsets and lengths
		if off < 0 || n <= 0 || n > compareChunk || off+n > h.oldSize {
			return h, fmt.Errorf("undo file %s has a bad record at offset %d", path, off)
		}
		if _, err = io.ReadFull(r, buf[:n]); err != nil {
			return h, fmt.Errorf("undo file %s is cut short: %w", path, err)
		}
		if fn != nil {
			if err = fn(off, buf[:n]); err != nil {
				return h, err
			}
		}
		records++
		remaining -= 16 + n
	}
	end := make([]byte, len(undoEndMagic)+8)
	if _, err = io.ReadFull(r, end); err != nil || string(end[:len(undoEndMagic)]) != undoEndMagic ||
		be.Uint64(end[len(undoEndMagic):]) != records || remaining != 0 {
		return h, fmt.Errorf("undo file %s is not complete", path)
	}
	return h, nil
}

// applyUndo takes the disk file f back to the moment before the undo file's
// copy. f may be the disk itself or a copy of it.
func applyUndo(v *volume, name string, f *os.File) error {
	h, err := readUndo(v, name, nil) // check the whole file before changing anything
	if err != nil {
		return err
	}
	if _, err = readUndo(v, name, func(off int64, data []byte) error {
		_, werr := f.WriteAt(data, off)
		return werr
	}); err != nil {
		return err
	}
	if err = f.Truncate(h.oldSize); err != nil {
		return err
	}
	return f.Sync()
}

// Recover rolls the disk back if a copy stopped while it changed the disk, and
// deletes undo files that were never completed. It reports whether it rolled
// back.
func Recover(disk string) (bool, error) {
	v, err := openVolume(disk)
	if err != nil {
		return false, err
	}
	defer v.close()
	return v.recover()
}

func (v *volume) recover() (bool, error) {
	entries, err := v.undoEntries()
	if err != nil || entries == nil {
		return false, err
	}
	rolled := false
	for _, e := range entries {
		p := undoPath(e.Name())
		switch {
		case strings.HasSuffix(e.Name(), undoPart):
			if err = v.root.Remove(p); err != nil {
				return rolled, err
			}
		case strings.HasSuffix(e.Name(), undoReady):
			f, err := v.openDisk(os.O_RDWR)
			if err != nil {
				return rolled, err
			}
			err = applyUndo(v, p, f)
			_ = f.Close()
			if err != nil {
				return rolled, fmt.Errorf("rolling back from %s: %w", e.Name(), err)
			}
			if err = v.root.Remove(p); err != nil {
				return rolled, err
			}
			rolled = true
		}
	}
	if rolled {
		// The disk is as the last copy left it again.
		if err = v.reseal(); err != nil {
			return rolled, err
		}
	}
	return rolled, v.sync(undoDirName)
}

type undoFile struct {
	seq  int
	path string // relative to the volume folder
	hdr  undoHeader
}

// appliedUndo lists the restore point undo files, oldest first.
func appliedUndo(v *volume) ([]undoFile, error) {
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
		h, rerr := readUndo(v, p, nil)
		if rerr != nil {
			return nil, rerr
		}
		files = append(files, undoFile{seq: seq, path: p, hdr: h})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].seq < files[j].seq })
	return files, nil
}

func nextUndoSeq(v *volume) (int, error) {
	entries, err := v.undoEntries()
	if err != nil {
		return 0, err
	}
	next := 1
	for _, e := range entries {
		name := e.Name()
		if i := strings.Index(name, ".undo"); i > 0 {
			if n, aerr := strconv.Atoi(name[:i]); aerr == nil && n >= next {
				next = n + 1
			}
		}
	}
	return next, nil
}

// pruneUndo deletes, oldest first, the undo files of copies made before
// cutoff, so the remaining ones still chain back from the latest copy.
func pruneUndo(v *volume, cutoff time.Time) error {
	files, err := appliedUndo(v)
	if err != nil {
		return err
	}
	for _, u := range files {
		if !u.hdr.to.Before(cutoff) {
			break
		}
		if err := v.root.Remove(u.path); err != nil {
			return err
		}
	}
	return nil
}

// RestorePoints lists the moments the disk can be rewound to, oldest first.
// The latest copy, in the disk itself, is not included.
func RestorePoints(disk string) ([]time.Time, error) {
	v, err := openVolume(disk)
	if err != nil {
		return nil, err
	}
	defer v.close()
	files, err := appliedUndo(v)
	if err != nil {
		return nil, err
	}
	points := make([]time.Time, 0, len(files))
	for _, u := range files {
		points = append(points, u.hdr.from)
	}
	return points, nil
}

// Rewind writes to out a disk as it was at restore point to. It does not
// change the latest disk, except to roll back a copy that stopped half way.
func Rewind(disk string, to time.Time, out string) error {
	v, err := openVolume(disk)
	if err != nil {
		return err
	}
	defer v.close()
	if _, err = v.recover(); err != nil {
		return err
	}
	files, err := appliedUndo(v)
	if err != nil {
		return err
	}
	stop := -1
	for i, u := range files {
		if u.hdr.from.Equal(to) {
			stop = i
		}
	}
	if stop < 0 {
		return fmt.Errorf("no restore point at %s", to.UTC().Format(time.RFC3339Nano))
	}
	in, err := v.openDisk(os.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	f, err := os.OpenFile(out, os.O_RDWR|os.O_CREATE|os.O_EXCL, diskFileMode) // #nosec G304 -- path chosen by the caller
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	if _, err = io.Copy(f, in); err != nil {
		return err
	}
	for i := len(files) - 1; i >= stop; i-- {
		if err = applyUndo(v, files[i].path, f); err != nil {
			return fmt.Errorf("rewinding with %s: %w", filepath.Base(files[i].path), err)
		}
	}
	return f.Close()
}
