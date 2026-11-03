package cosdisk

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/rand"
	"sync"
	"sync/atomic"
	"testing"
)

// memSource is a remote file held in memory. It counts what is fetched.
type memSource struct {
	data  []byte
	reqs  atomic.Int64
	bytes atomic.Int64
	fail  atomic.Int64 // number of next reads that fail
}

func (m *memSource) Size() int64 { return int64(len(m.data)) }

func (m *memSource) ReadRange(_ context.Context, p []byte, off int64) (int, error) {
	if m.fail.Load() > 0 {
		m.fail.Add(-1)
		return 0, errors.New("boom")
	}
	m.reqs.Add(1)
	n := copy(p, m.data[off:])
	m.bytes.Add(int64(n))
	return n, nil
}

func newData(n int) []byte {
	b := make([]byte, n)
	rand.New(rand.NewSource(1)).Read(b)
	return b
}

func TestReadAtMatchesTheFileAcrossSegments(t *testing.T) {
	src := &memSource{data: newData(10*1024 + 123)}
	d := New(src, Options{SegmentSize: 1024, ReadAhead: 2, MaxBytes: 4096})
	defer func() { _ = d.Close() }()
	cases := []struct{ off, n int }{{0, 10}, {1000, 100}, {1024, 1024}, {500, 5000}, {10 * 1024, 123}, {0, 10*1024 + 123}}
	for _, c := range cases {
		got := make([]byte, c.n)
		n, err := d.ReadAt(got, int64(c.off))
		if err != nil || n != c.n {
			t.Fatalf("ReadAt(%d,%d) = %d, %v", c.off, c.n, n, err)
		}
		if !bytes.Equal(got, src.data[c.off:c.off+c.n]) {
			t.Fatalf("ReadAt(%d,%d): wrong bytes", c.off, c.n)
		}
	}
}

func TestReadAtPastTheEndShortensAndReturnsEOF(t *testing.T) {
	src := &memSource{data: newData(2500)}
	d := New(src, Options{SegmentSize: 1024, MaxBytes: 4096})
	defer func() { _ = d.Close() }()
	got := make([]byte, 100)
	n, err := d.ReadAt(got, 2450)
	if n != 50 || !errors.Is(err, io.EOF) {
		t.Fatalf("got %d, %v; want 50, EOF", n, err)
	}
	if n, err = d.ReadAt(got, 2500); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("at the end: got %d, %v", n, err)
	}
}

func TestSmallReadsInOneSegmentFetchOnce(t *testing.T) {
	src := &memSource{data: newData(8 * 1024)}
	d := New(src, Options{SegmentSize: 1024, ReadAhead: 0, MaxBytes: 8192})
	defer func() { _ = d.Close() }()
	buf := make([]byte, 16)
	for i := 0; i < 50; i++ {
		if _, err := d.ReadAt(buf, int64(i*16)); err != nil {
			t.Fatal(err)
		}
	}
	if got := src.reqs.Load(); got != 1 {
		t.Fatalf("fetched %d times for reads inside one segment; want 1", got)
	}
}

func TestReadAheadFetchesTheNextSegments(t *testing.T) {
	src := &memSource{data: newData(16 * 1024)}
	d := New(src, Options{SegmentSize: 1024, ReadAhead: 3, MaxBytes: 16 * 1024})
	defer func() { _ = d.Close() }()
	if _, err := d.ReadAt(make([]byte, 10), 0); err != nil {
		t.Fatal(err)
	}
	d.WaitIdle()
	if got := src.reqs.Load(); got != 4 {
		t.Fatalf("fetched %d segments; want 1 read plus 3 ahead = 4", got)
	}
	before := src.reqs.Load()
	if _, err := d.ReadAt(make([]byte, 3000), 1024); err != nil {
		t.Fatal(err)
	}
	d.WaitIdle()
	if src.reqs.Load()-before > 3 {
		t.Fatalf("a read of segments 1 to 3 fetched %d new segments; the first three were cached", src.reqs.Load()-before)
	}
}

func TestCacheIsBounded(t *testing.T) {
	src := &memSource{data: newData(64 * 1024)}
	d := New(src, Options{SegmentSize: 1024, ReadAhead: 2, MaxBytes: 8 * 1024})
	defer func() { _ = d.Close() }()
	buf := make([]byte, 1024)
	for i := 0; i < 64; i++ {
		if _, err := d.ReadAt(buf, int64(i*1024)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, src.data[i*1024:(i+1)*1024]) {
			t.Fatalf("segment %d: wrong bytes", i)
		}
	}
	d.WaitIdle()
	if got := d.CachedBytes(); got > 8*1024 {
		t.Fatalf("cache holds %d bytes; limit is %d", got, 8*1024)
	}
}

func TestEvictedSegmentIsFetchedAgainAndStillCorrect(t *testing.T) {
	src := &memSource{data: newData(32 * 1024)}
	d := New(src, Options{SegmentSize: 1024, ReadAhead: 0, MaxBytes: 2 * 1024})
	defer func() { _ = d.Close() }()
	buf := make([]byte, 100)
	for _, seg := range []int{0, 5, 9, 0} {
		if _, err := d.ReadAt(buf, int64(seg*1024)); err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(buf, src.data[seg*1024:seg*1024+100]) {
			t.Fatalf("segment %d: wrong bytes", seg)
		}
	}
	if got := src.reqs.Load(); got != 4 {
		t.Fatalf("fetched %d times; segment 0 was evicted and must be fetched again (want 4)", got)
	}
}

func TestAFailedFetchIsNotKept(t *testing.T) {
	src := &memSource{data: newData(4 * 1024)}
	d := New(src, Options{SegmentSize: 1024, MaxBytes: 4096})
	defer func() { _ = d.Close() }()
	src.fail.Store(1)
	if _, err := d.ReadAt(make([]byte, 10), 0); err == nil {
		t.Fatal("want an error from the failed fetch")
	}
	got := make([]byte, 10)
	if _, err := d.ReadAt(got, 0); err != nil {
		t.Fatalf("second try: %v", err)
	}
	if !bytes.Equal(got, src.data[:10]) {
		t.Fatal("wrong bytes after retry")
	}
}

func TestConcurrentReadsAreSafeAndCorrect(t *testing.T) {
	src := &memSource{data: newData(256 * 1024)}
	d := New(src, Options{SegmentSize: 4096, ReadAhead: 4, MaxBytes: 64 * 1024})
	defer func() { _ = d.Close() }()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(seed int64) {
			defer wg.Done()
			r := rand.New(rand.NewSource(seed))
			for i := 0; i < 200; i++ {
				off := r.Intn(len(src.data) - 10000)
				n := 1 + r.Intn(9000)
				got := make([]byte, n)
				if _, err := d.ReadAt(got, int64(off)); err != nil {
					t.Error(err)
					return
				}
				if !bytes.Equal(got, src.data[off:off+n]) {
					t.Errorf("wrong bytes at %d+%d", off, n)
					return
				}
			}
		}(int64(g))
	}
	wg.Wait()
}

func TestSizeComesFromTheSource(t *testing.T) {
	d := New(&memSource{data: newData(77)}, Options{SegmentSize: 16, MaxBytes: 64})
	defer func() { _ = d.Close() }()
	if d.Size() != 77 {
		t.Fatalf("Size = %d", d.Size())
	}
}
