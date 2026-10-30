package replication

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
)

// randomCopy makes a copy of a disk of the given size: extents at random,
// unaligned places, with random data or zeros. A full copy treats the gaps as
// zeros.
func randomCopy(rng *rand.Rand, size int64) *memSource {
	m := &memSource{size: size, data: map[int64][]byte{}}
	pos := rng.Int63n(chunk)
	for pos < size {
		length := 1 + rng.Int63n(3*chunk)
		if pos+length > size {
			length = size - pos
		}
		e := qcow2.Extent{Offset: pos, Length: length, Kind: qcow2.Data}
		if rng.Intn(4) == 0 {
			e.Kind = qcow2.Zero
		} else {
			b := make([]byte, length)
			rng.Read(b)
			m.data[pos] = b
		}
		m.exts = append(m.exts, e)
		pos += length + rng.Int63n(2*chunk)
	}
	return m
}

// history makes a backup disk with several copies applied, some of which grow
// or shrink the disk. It returns the disk and the times of the copies.
func history(t *testing.T, rng *rand.Rand, copies int) (disk string, moments []time.Time) {
	t.Helper()
	disk = filepath.Join(t.TempDir(), "disk.img")
	size := int64(8+rng.Intn(8))*chunk + rng.Int63n(chunk)
	first := make([]byte, size)
	rng.Read(first)
	writeDisk(t, disk, first)
	moments = []time.Time{t0}
	for i := 0; i < copies; i++ {
		switch rng.Intn(4) {
		case 0:
			size += 1 + rng.Int63n(4*chunk) // grow
		case 1:
			size -= 1 + rng.Int63n(3*chunk) // shrink
			if size < 2*chunk {
				size = 2 * chunk
			}
		}
		from, to := moments[len(moments)-1], moments[len(moments)-1].Add(time.Hour)
		if _, err := ApplyCopy(randomCopy(rng, size), disk, ApplyOptions{From: from, To: to, Full: rng.Intn(2) == 0}); err != nil {
			t.Fatalf("ApplyCopy %d: %v", i, err)
		}
		moments = append(moments, to)
	}
	return disk, moments
}

func readAll(t *testing.T, r io.ReaderAt, size int64) []byte {
	t.Helper()
	b := make([]byte, size)
	n, err := r.ReadAt(b, 0)
	if int64(n) != size || (err != nil && !errors.Is(err, io.EOF)) {
		t.Fatalf("ReadAt: %d of %d bytes, %v", n, size, err)
	}
	return b
}

func viewBytes(t *testing.T, disk string, at time.Time) ([]byte, *DiskView) {
	t.Helper()
	dv, err := OpenDiskView(disk, at)
	if err != nil {
		t.Fatalf("OpenDiskView(%v): %v", at, err)
	}
	t.Cleanup(func() { _ = dv.Close() })
	return readAll(t, dv, dv.Size()), dv
}

func TestDiskView_EqualsRestoreDisk(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		rng := rand.New(rand.NewSource(seed))
		disk, moments := history(t, rng, 2+rng.Intn(4))
		half := 30 * time.Minute
		ats := []time.Time{{}}
		for _, m := range moments {
			ats = append(ats, m, m.Add(half), m.Add(-half))
		}
		for _, at := range ats {
			out := filepath.Join(t.TempDir(), "out.img")
			want, werr := RestoreDisk(disk, at, out)
			dv, verr := OpenDiskView(disk, at)
			if (werr == nil) != (verr == nil) {
				t.Fatalf("seed %d at %v: RestoreDisk error %v, OpenDiskView error %v", seed, at, werr, verr)
			}
			if werr != nil {
				continue
			}
			if dv.Latest() != want.Latest || !dv.Moment().Equal(want.Moment) {
				t.Errorf("seed %d at %v: view is %v (latest %v), RestoreDisk is %v (latest %v)", seed, at, dv.Moment(), dv.Latest(), want.Moment, want.Latest)
			}
			expect := readDisk(t, out)
			if dv.Size() != int64(len(expect)) {
				t.Fatalf("seed %d at %v: size %d, want %d", seed, at, dv.Size(), len(expect))
			}
			if got := readAll(t, dv, dv.Size()); !bytes.Equal(got, expect) {
				t.Fatalf("seed %d at %v: the view differs from the restored disk", seed, at)
			}
			// Reads that cross record edges, size changes and chunk edges.
			for i := 0; i < 40; i++ {
				off := rng.Int63n(dv.Size())
				n := 1 + rng.Intn(3*chunk)
				buf := make([]byte, n)
				got, err := dv.ReadAt(buf, off)
				end := min(off+int64(n), dv.Size())
				if !bytes.Equal(buf[:got], expect[off:end]) || int64(got) != end-off {
					t.Fatalf("seed %d at %v: ReadAt(%d bytes at %d) differs", seed, at, n, off)
				}
				if got < n && !errors.Is(err, io.EOF) {
					t.Fatalf("a short read must say io.EOF, got %v", err)
				}
			}
			_ = dv.Close()
		}
	}
}

func TestDiskView_ReadAtTheEnd(t *testing.T) {
	disk, _ := history(t, rand.New(rand.NewSource(7)), 2)
	_, dv := viewBytes(t, disk, time.Time{})
	if n, err := dv.ReadAt(make([]byte, 10), dv.Size()); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("read at the end: %d, %v", n, err)
	}
	if n, err := dv.ReadAt(make([]byte, 10), dv.Size()+5); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("read past the end: %d, %v", n, err)
	}
	if _, err := dv.ReadAt(make([]byte, 1), -1); err == nil {
		t.Error("a negative offset must fail")
	}
}

func TestDiskView_NeverChangesTheBackup(t *testing.T) {
	disk, moments := history(t, rand.New(rand.NewSource(3)), 3)
	before := snapshotTree(t, filepath.Dir(disk))
	for _, m := range moments {
		_, dv := viewBytes(t, disk, m)
		_ = dv.Close()
	}
	if after := snapshotTree(t, filepath.Dir(disk)); !equalTrees(before, after) {
		t.Fatal("OpenDiskView changed the backup folder")
	}
}

// snapshotTree records every file under dir: name, size, mode, mtime.
func snapshotTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		out[p] = fi.Mode().String() + "/" + fi.ModTime().String() + "/" + string(rune(fi.Size()))
		if fi.Mode().IsRegular() {
			b, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out[p] += "/" + string(b)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func equalTrees(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

func TestDiskView_IgnoresAnUnfinishedUndoPart(t *testing.T) {
	disk, _ := history(t, rand.New(rand.NewSource(11)), 2)
	want, _ := viewBytes(t, disk, time.Time{})
	// An undo file that is still being written: the disk is not changed yet.
	part := filepath.Join(filepath.Dir(disk), undoDirName, "0000000099.undo.part")
	if err := os.WriteFile(part, []byte("REPLUND1 junk"), 0o660); err != nil {
		t.Fatal(err)
	}
	got, _ := viewBytes(t, disk, time.Time{})
	if !bytes.Equal(got, want) {
		t.Fatal("a .part file changed the view")
	}
	if _, err := os.Stat(part); err != nil {
		t.Fatal("the view must not remove the .part file: the backup is read-only")
	}
}

func TestDiskView_RefusesACopyInProgress(t *testing.T) {
	disk, _ := history(t, rand.New(rand.NewSource(12)), 2)
	ready := filepath.Join(filepath.Dir(disk), undoDirName, "0000000098.undo.ready")
	if err := os.WriteFile(ready, []byte("x"), 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDiskView(disk, time.Time{}); !errors.Is(err, ErrCopyInProgress) {
		t.Fatalf("got %v, want ErrCopyInProgress", err)
	}
	out := filepath.Join(t.TempDir(), "o.img")
	if _, err := RestoreDisk(disk, time.Time{}, out); !errors.Is(err, ErrCopyInProgress) {
		t.Fatalf("RestoreDisk must agree: %v", err)
	}
}

func TestDiskView_RefusesADiskThatWasWrittenAfterTheLastCopy(t *testing.T) {
	disk, moments := history(t, rand.New(rand.NewSource(13)), 2)
	if err := WriteMoment(filepath.Dir(disk), moments[len(moments)-1], "ns/vm"); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDiskView(disk, time.Time{}); err != nil {
		t.Fatalf("a sealed disk must open: %v", err)
	}
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(disk, later, later); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDiskView(disk, time.Time{}); err == nil {
		t.Fatal("a disk written after its last copy must be refused")
	}
}

func TestDiskView_FirstCopyHasNoMoment(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "disk.img")
	writeDisk(t, disk, patterned(5*chunk))
	got, dv := viewBytes(t, disk, t0)
	if !dv.Latest() || dv.MomentKnown() {
		t.Errorf("latest %v, moment known %v", dv.Latest(), dv.MomentKnown())
	}
	if !bytes.Equal(got, patterned(5*chunk)) {
		t.Error("the view of a first copy is the disk")
	}
}

func TestDiskView_NoBackupAtThatTime(t *testing.T) {
	disk, _ := history(t, rand.New(rand.NewSource(14)), 2)
	if _, err := OpenDiskView(disk, t0.Add(-time.Hour)); err == nil {
		t.Fatal("a time before the oldest moment must fail")
	}
}

func TestDiskView_RefusesALinkedDisk(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(t.TempDir(), "other.img")
	writeDisk(t, real, patterned(2*chunk))
	if err := os.Symlink(real, filepath.Join(dir, "disk.img")); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDiskView(filepath.Join(dir, "disk.img"), time.Time{}); err == nil {
		t.Fatal("a link must be refused")
	}
}

func TestDiskView_UndoFolderLinkedOutIsRefused(t *testing.T) {
	disk, moments := history(t, rand.New(rand.NewSource(15)), 2)
	other := t.TempDir()
	dir := filepath.Join(filepath.Dir(disk), undoDirName)
	if err := os.Rename(dir, filepath.Join(other, "u")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(other, "u"), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenDiskView(disk, moments[0]); err == nil {
		t.Fatal("an undo folder that is a link must be refused")
	}
}

func TestDiskView_StopsWhenTheDiskChanges(t *testing.T) {
	disk, _ := history(t, rand.New(rand.NewSource(16)), 2)
	now := time.Unix(1000, 0)
	dv, err := OpenDiskView(disk, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dv.Close() }()
	dv.now = func() time.Time { return now }
	dv.lastCheck.Store(now.UnixNano())
	buf := make([]byte, 100)
	if _, err = dv.ReadAt(buf, 0); err != nil {
		t.Fatalf("an unchanged backup reads: %v", err)
	}
	// A new copy starts: it writes an undo file next to the disk.
	if err = os.WriteFile(filepath.Join(filepath.Dir(disk), undoDirName, "0000000077.undo.part"), []byte("x"), 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err = dv.ReadAt(buf, 0); err != nil {
		t.Fatalf("a check is not made on every read: %v", err)
	}
	now = now.Add(time.Minute)
	if _, err = dv.ReadAt(buf, 0); !errors.Is(err, ErrDiskChanged) {
		t.Fatalf("got %v, want ErrDiskChanged", err)
	}
	if _, err = dv.ReadAt(buf, 0); !errors.Is(err, ErrDiskChanged) {
		t.Fatalf("once changed, always changed: %v", err)
	}
}

func TestDiskView_StopsWhenTheDiskFileIsWritten(t *testing.T) {
	disk, _ := history(t, rand.New(rand.NewSource(17)), 1)
	now := time.Unix(1000, 0)
	dv, err := OpenDiskView(disk, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dv.Close() }()
	dv.now = func() time.Time { return now }
	dv.lastCheck.Store(now.UnixNano())
	f, err := os.OpenFile(disk, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte{1, 2, 3}, 5); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	later := time.Now().Add(time.Hour)
	if err = os.Chtimes(disk, later, later); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if _, err = dv.ReadAt(make([]byte, 10), 0); !errors.Is(err, ErrDiskChanged) {
		t.Fatalf("got %v, want ErrDiskChanged", err)
	}
}

func TestDiskView_UndoFilesOutOfOrderAreRefused(t *testing.T) {
	disk, moments := history(t, rand.New(rand.NewSource(18)), 1)
	// Rewrite the undo file with two records that overlap.
	dir := filepath.Join(filepath.Dir(disk), undoDirName)
	entries, err := os.ReadDir(dir)
	if err != nil || len(entries) != 1 {
		t.Fatalf("expected one undo file: %v %v", entries, err)
	}
	p := filepath.Join(dir, entries[0].Name())
	h := undoHeader{from: moments[0], to: moments[1], oldSize: 4 * chunk, newSize: 4 * chunk}
	writeUndoFile(t, p, h, [][2]int64{{0, 100}, {50, 100}})
	if _, err := OpenDiskView(disk, moments[0]); err == nil {
		t.Fatal("records that overlap must be refused")
	}
}

// writeUndoFile writes a well-formed undo file with the given records, each
// (offset, length), filled with 0xAB.
func writeUndoFile(t *testing.T, path string, h undoHeader, recs [][2]int64) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(undoMagic)
	for _, v := range []uint64{uint64(h.from.UnixNano()), uint64(h.to.UnixNano()), uint64(h.oldSize), uint64(h.newSize)} {
		b.Write(binary.BigEndian.AppendUint64(nil, v))
	}
	for _, r := range recs {
		b.Write(binary.BigEndian.AppendUint64(nil, uint64(r[0])))
		b.Write(binary.BigEndian.AppendUint64(nil, uint64(r[1])))
		b.Write(bytes.Repeat([]byte{0xab}, int(r[1])))
	}
	b.WriteString(undoEndMagic)
	b.Write(binary.BigEndian.AppendUint64(nil, uint64(len(recs))))
	if err := os.WriteFile(path, b.Bytes(), 0o660); err != nil {
		t.Fatal(err)
	}
}
