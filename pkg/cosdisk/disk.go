// Package cosdisk shows a file that lives in COS as a disk that can be read at
// any offset. The appliance that reads a VM disk makes many small reads (about
// 4 KB each); one request to COS for each of them is far too slow (phase 6a:
// more than 9 minutes to find the file systems of a 10 GiB disk). Disk fixes
// that. It fetches whole segments, reads the next ones ahead in parallel, and
// keeps the most recent ones in a cache with a fixed size.
//
// See docs/specs/2026-10-29-vm-disks-in-cos.md, section 1 and "Phase 6a results".
package cosdisk

import (
	"container/list"
	"context"
	"fmt"
	"io"
	"sync"
)

// Source is a remote file that can be read in ranges. A COS object never
// changes, so Size is fixed.
type Source interface {
	Size() int64
	// ReadRange fills p with the bytes at off. It returns len(p) bytes or an
	// error; the caller never asks for bytes past Size.
	ReadRange(ctx context.Context, p []byte, off int64) (int, error)
}

// Options set the shape of the cache.
type Options struct {
	// SegmentSize is the size of one fetch. Zero means 4 MiB, about the size
	// of a Kopia chunk.
	SegmentSize int
	// ReadAhead is how many segments after a read are fetched in the
	// background, at most that many at a time. Zero means none.
	ReadAhead int
	// MaxBytes is the most the cache holds. Zero means 64 MiB.
	MaxBytes int64
}

const (
	defaultSegment  = 4 << 20
	defaultMaxBytes = 64 << 20
)

// Disk is a Source read through a segment cache. It is safe for concurrent use
// and satisfies nbd.Export.
type Disk struct {
	src  Source
	seg  int64
	max  int64
	size int64

	ctx    context.Context
	cancel context.CancelFunc
	ahead  chan struct{} // one token for each background fetch that may run
	wg     sync.WaitGroup

	mu     sync.Mutex
	segs   map[int64]*segment
	lru    *list.List // front is the most recently used; holds ready segments only
	cached int64
}

type segment struct {
	idx  int64
	data []byte
	err  error
	done chan struct{}
	el   *list.Element
}

// New reads src through a cache.
func New(src Source, o Options) *Disk {
	if o.SegmentSize <= 0 {
		o.SegmentSize = defaultSegment
	}
	if o.MaxBytes <= 0 {
		o.MaxBytes = defaultMaxBytes
	}
	ctx, cancel := context.WithCancel(context.Background())
	d := &Disk{
		src: src, seg: int64(o.SegmentSize), max: o.MaxBytes, size: src.Size(),
		ctx: ctx, cancel: cancel,
		segs: map[int64]*segment{}, lru: list.New(),
	}
	if o.ReadAhead > 0 {
		d.ahead = make(chan struct{}, o.ReadAhead)
	}
	return d
}

// Size is the size of the file.
func (d *Disk) Size() int64 { return d.size }

// Close stops the background fetches and waits for them.
func (d *Disk) Close() error {
	d.cancel()
	d.wg.Wait()
	return nil
}

// WaitIdle waits until the background fetches that are running have ended.
func (d *Disk) WaitIdle() { d.wg.Wait() }

// CachedBytes is how many bytes the cache holds now.
func (d *Disk) CachedBytes() int64 {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.cached
}

// ReadAt reads at off. A read that reaches the end of the file is shortened and
// returns io.EOF with the bytes that were there.
func (d *Disk) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("cosdisk: negative offset %d", off)
	}
	if off >= d.size {
		return 0, io.EOF
	}
	short := false
	if off+int64(len(p)) > d.size {
		p = p[:d.size-off]
		short = true
	}
	d.readAhead(off / d.seg)
	done := 0
	for done < len(p) {
		cur := off + int64(done)
		s, err := d.get(cur / d.seg)
		if err != nil {
			return done, err
		}
		done += copy(p[done:], s.data[cur%d.seg:])
	}
	if short {
		return done, io.EOF
	}
	return done, nil
}

// readAhead starts background fetches of the segments after idx.
func (d *Disk) readAhead(idx int64) {
	if d.ahead == nil {
		return
	}
	for j := idx + 1; j <= idx+int64(cap(d.ahead)) && j*d.seg < d.size; j++ {
		d.mu.Lock()
		_, have := d.segs[j]
		d.mu.Unlock()
		if have {
			continue
		}
		select {
		case d.ahead <- struct{}{}:
		default:
			return
		}
		d.wg.Add(1)
		go func(j int64) {
			defer d.wg.Done()
			defer func() { <-d.ahead }()
			_, _ = d.get(j)
		}(j)
	}
}

// get returns segment idx, fetching it if no one has. Callers that ask for a
// segment being fetched wait for that fetch.
func (d *Disk) get(idx int64) (*segment, error) {
	d.mu.Lock()
	s, ok := d.segs[idx]
	if ok {
		if s.el != nil {
			d.lru.MoveToFront(s.el)
		}
		d.mu.Unlock()
		<-s.done
		return s, s.err
	}
	s = &segment{idx: idx, done: make(chan struct{})}
	d.segs[idx] = s
	d.mu.Unlock()

	s.data, s.err = d.fetch(idx)

	d.mu.Lock()
	if s.err != nil {
		delete(d.segs, idx) // a failed fetch is not kept: the next read tries again
	} else {
		s.el = d.lru.PushFront(s)
		d.cached += int64(len(s.data))
		d.evict()
	}
	d.mu.Unlock()
	close(s.done)
	return s, s.err
}

// evict drops the least recently used segments until the cache fits. A reader
// that already has a segment keeps its bytes. The caller holds d.mu.
func (d *Disk) evict() {
	for d.cached > d.max && d.lru.Len() > 1 {
		el := d.lru.Back()
		s, _ := el.Value.(*segment)
		d.lru.Remove(el)
		delete(d.segs, s.idx)
		d.cached -= int64(len(s.data))
	}
}

func (d *Disk) fetch(idx int64) ([]byte, error) {
	off := idx * d.seg
	n := d.seg
	if off+n > d.size {
		n = d.size - off
	}
	buf := make([]byte, n)
	got, err := d.src.ReadRange(d.ctx, buf, off)
	if err != nil {
		return nil, err
	}
	if int64(got) != n {
		return nil, fmt.Errorf("cosdisk: short read at %d: got %d of %d bytes: %w", off, got, n, io.ErrUnexpectedEOF)
	}
	return buf, nil
}
