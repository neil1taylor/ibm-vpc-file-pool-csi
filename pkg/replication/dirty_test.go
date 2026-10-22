package replication

import (
	"bytes"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

// Dirty pages of a file on NFS count against the memory limit of the container
// that writes them. A send of a VM disk filled 2 GB of cache and the receiver
// was killed at 512Mi (#212). The receiver forces its data out every so often.

type fakeSyncer struct{ syncs int }

func (f *fakeSyncer) Sync() error { f.syncs++; return nil }

func TestDirtyBound_SyncsAfterEveryLimit(t *testing.T) {
	f := &fakeSyncer{}
	d := newDirtyBound(f, 100)
	for i := 0; i < 9; i++ {
		if err := d.wrote(30); err != nil {
			t.Fatal(err)
		}
	}
	// 270 bytes written in steps of 30: a sync at 120 and at 240.
	if f.syncs != 2 {
		t.Fatalf("syncs = %d, want 2", f.syncs)
	}
	if err := d.flush(); err != nil {
		t.Fatal(err)
	}
	if f.syncs != 3 {
		t.Fatalf("a flush syncs what is left: syncs = %d, want 3", f.syncs)
	}
	if err := d.flush(); err != nil || f.syncs != 3 {
		t.Fatalf("a flush with nothing left does nothing: syncs = %d (%v)", f.syncs, err)
	}
}

func TestDirtyBound_WritersSyncWhileWriting(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "x"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	fs := &fakeSyncer{}
	w := &boundedWriter{w: f, bound: newDirtyBound(fs, 2*chunk)}
	for i := 0; i < 8; i++ {
		if _, err = w.Write(bytes.Repeat([]byte{1}, chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if fs.syncs != 4 {
		t.Fatalf("syncs = %d, want 4 for 8 chunks at a limit of 2", fs.syncs)
	}
	wa := &boundedWriterAt{w: f, bound: newDirtyBound(fs, 2*chunk)}
	for i := 0; i < 8; i++ {
		if _, err = wa.WriteAt(bytes.Repeat([]byte{2}, chunk), int64(i)*chunk); err != nil {
			t.Fatal(err)
		}
	}
	if fs.syncs != 8 {
		t.Fatalf("syncs = %d, want 8", fs.syncs)
	}
}

func applyBig(t *testing.T, disk string, opts ApplyOptions, src CopySource) {
	t.Helper()
	if _, err := ApplyCopy(src, disk, opts); err != nil {
		t.Fatal(err)
	}
}

func fullChunks(n int, fill byte) *memSource {
	var offs []int64
	for i := 0; i < n; i++ {
		offs = append(offs, int64(i)*chunk)
	}
	return change(int64(n)*chunk, fill, offs, nil)
}

// ApplyCopy never leaves more than the limit unsynced, for a new disk and for
// a change to an existing one (the undo file and the disk).
func TestApplyCopy_ForcesDataOutWhileWriting(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "pvcs", "v", "disk.img")
	if err := os.MkdirAll(filepath.Dir(disk), 0o777); err != nil {
		t.Fatal(err)
	}
	var syncs int
	opts := ApplyOptions{To: t1, Full: true, Now: t1, dirtyLimit: 2 * chunk, onDirtySync: func() { syncs++ }}
	applyBig(t, disk, opts, fullChunks(8, 1))
	if syncs < 3 {
		t.Fatalf("new disk of 8 chunks at a limit of 2: %d syncs, want at least 3", syncs)
	}
	syncs = 0
	opts = ApplyOptions{From: t1, To: t2, Full: true, Now: t2, dirtyLimit: 2 * chunk, onDirtySync: func() { syncs++ }}
	applyBig(t, disk, opts, fullChunks(8, 2))
	// The 8 changed chunks go to the disk (4 syncs at a limit of 2) and to the
	// undo file, which is written through a 1 MiB buffer (1 sync).
	if syncs < 5 {
		t.Fatalf("change of 8 chunks at a limit of 2: %d syncs, want at least 5", syncs)
	}
	for i, b := range readDisk(t, disk) {
		if b != 2 {
			t.Fatalf("byte %d = %d, want 2", i, b)
		}
	}
}

// Clean pages of a file on NFS are not given back fast enough under a small
// memory limit either: the receiver was killed at 512Mi while it stored
// records, with 485 MB of cache and 4 MB dirty (#212). The receiver tells the
// kernel to drop the cached pages of what it stored and what it read.

func TestCacheDropper_DropsAfterEveryLimit(t *testing.T) {
	drops := 0
	d := &cacheDropper{every: 100, drop: func() { drops++ }}
	for i := 0; i < 9; i++ {
		d.after(30)
	}
	if drops != 2 {
		t.Fatalf("drops = %d, want 2", drops)
	}
}

func TestDroppingReaderAt_DropsWhileReading(t *testing.T) {
	r := bytes.NewReader(bytes.Repeat([]byte{1}, 8*chunk))
	drops := 0
	d := &droppingReaderAt{r: r, dropper: &cacheDropper{every: 2 * chunk, drop: func() { drops++ }}}
	buf := make([]byte, chunk)
	for i := 0; i < 8; i++ {
		if _, err := d.ReadAt(buf, int64(i)*chunk); err != nil {
			t.Fatal(err)
		}
	}
	if drops != 4 {
		t.Fatalf("drops = %d, want 4", drops)
	}
}

func TestReceiverVM_UploadDropsCachedPages(t *testing.T) {
	c := newVMTestClient(t)
	drops := 0
	c.rcv.cacheDropEvery = 2 * chunk
	c.rcv.onCacheDrop = func() { drops++ }
	_, recs := fullCopy()
	if code := c.upload("dddddddd-9", recs, "test-token-123"); code != http.StatusOK {
		t.Fatal("upload failed")
	}
	if drops < 2 {
		t.Fatalf("drops = %d, want at least 2 for 4 records at a limit of 2", drops)
	}
}
