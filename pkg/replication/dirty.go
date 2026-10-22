package replication

import (
	"io"
	"os"
)

// maxDirtyBytes is how much data the receiver writes to a file before it
// forces the data out to the share. Pages that are written but not yet on the
// share count against the memory limit of the container, and the kernel cannot
// free them. A send of a VM disk kept 2 GB of them, and the receiver, with a
// limit of 512Mi, was killed (issue #212).
const maxDirtyBytes = 32 << 20

type syncer interface{ Sync() error }

// dirtyBound syncs a file each time limit bytes were written to it, then lets
// the kernel drop the cached pages of the file.
type dirtyBound struct {
	f       syncer
	limit   int64
	pending int64
	onSync  func()
}

func newDirtyBound(f syncer, limit int64) *dirtyBound {
	if limit <= 0 {
		limit = maxDirtyBytes
	}
	return &dirtyBound{f: f, limit: limit}
}

// wrote records that n bytes were written, and syncs if the limit is reached.
func (d *dirtyBound) wrote(n int) error {
	d.pending += int64(n)
	if d.pending < d.limit {
		return nil
	}
	return d.flush()
}

// flush syncs what was written since the last sync.
func (d *dirtyBound) flush() error {
	if d.pending == 0 {
		return nil
	}
	d.pending = 0
	if err := d.f.Sync(); err != nil {
		return err
	}
	if f, ok := d.f.(*os.File); ok {
		dropCache(f)
	}
	if d.onSync != nil {
		d.onSync()
	}
	return nil
}

// bound makes the limiter for a file written during a copy.
func (o ApplyOptions) bound(f syncer) *dirtyBound {
	d := newDirtyBound(f, o.dirtyLimit)
	d.onSync = o.onDirtySync
	return d
}

// boundedWriter is an io.Writer that syncs as dirtyBound says.
type boundedWriter struct {
	w     io.Writer
	bound *dirtyBound
}

func (b *boundedWriter) Write(p []byte) (int, error) {
	n, err := b.w.Write(p)
	if n > 0 {
		if berr := b.bound.wrote(n); err == nil {
			err = berr
		}
	}
	return n, err
}

// boundedWriterAt is an io.WriterAt that syncs as dirtyBound says.
type boundedWriterAt struct {
	w     io.WriterAt
	bound *dirtyBound
}

func (b *boundedWriterAt) WriteAt(p []byte, off int64) (int, error) {
	n, err := b.w.WriteAt(p, off)
	if n > 0 {
		if berr := b.bound.wrote(n); err == nil {
			err = berr
		}
	}
	return n, err
}

// cacheDropper tells the kernel to drop the cached pages of a file after every
// `every` bytes that were written to it or read from it. Clean pages of a file
// on NFS are not always given back fast enough under a small memory limit.
type cacheDropper struct {
	every, since int64
	drop         func()
}

func newCacheDropper(f *os.File, every int64, onDrop func()) *cacheDropper {
	if every <= 0 {
		every = maxDirtyBytes / 4
	}
	return &cacheDropper{every: every, drop: func() {
		dropCache(f)
		if onDrop != nil {
			onDrop()
		}
	}}
}

func (c *cacheDropper) after(n int) {
	c.since += int64(n)
	if c.since >= c.every {
		c.since = 0
		c.drop()
	}
}

// droppingReaderAt is an io.ReaderAt that drops cached pages as cacheDropper says.
type droppingReaderAt struct {
	r       io.ReaderAt
	dropper *cacheDropper
}

func (d *droppingReaderAt) ReadAt(p []byte, off int64) (int, error) {
	n, err := d.r.ReadAt(p, off)
	d.dropper.after(n)
	return n, err
}
