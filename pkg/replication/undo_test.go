package replication

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
)

const chunk = 64 << 10

// memSource is an in-memory copy: a virtual size and extents with data.
type memSource struct {
	size int64
	exts []qcow2.Extent
	data map[int64][]byte // by extent offset, for Data extents
}

func (m *memSource) Size() int64 { return m.size }

func (m *memSource) Walk(fn func(qcow2.Extent) error) error {
	for _, e := range m.exts {
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

func (m *memSource) DataReader(e qcow2.Extent) io.Reader { return bytes.NewReader(m.data[e.Offset]) }

// change returns a source that writes fill bytes at each offset, one chunk
// each, and zeros at each zero offset.
func change(size int64, fill byte, offsets []int64, zeros []int64) *memSource {
	m := &memSource{size: size, data: map[int64][]byte{}}
	for _, o := range offsets {
		m.exts = append(m.exts, qcow2.Extent{Offset: o, Length: chunk, Kind: qcow2.Data})
		m.data[o] = bytes.Repeat([]byte{fill}, chunk)
	}
	for _, o := range zeros {
		m.exts = append(m.exts, qcow2.Extent{Offset: o, Length: chunk, Kind: qcow2.Zero})
	}
	sortExtents(m.exts)
	return m
}

func writeDisk(t *testing.T, path string, b []byte) {
	t.Helper()
	if err := os.WriteFile(path, b, 0o660); err != nil {
		t.Fatal(err)
	}
}

func readDisk(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func patterned(size int64) []byte {
	b := make([]byte, size)
	for i := range b {
		b[i] = byte(i/chunk + 1)
	}
	return b
}

var (
	t0 = time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC)
	t1 = t0.Add(time.Hour)
	t2 = t0.Add(2 * time.Hour)
	t3 = t0.Add(3 * time.Hour)
)

func TestApplyCopy_ChangeCopyAndRewind(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	old := patterned(16 * chunk)
	writeDisk(t, disk, old)

	res, err := ApplyCopy(change(16*chunk, 0xaa, []int64{2 * chunk, 3 * chunk}, []int64{9 * chunk}), disk,
		ApplyOptions{From: t0, To: t1})
	if err != nil {
		t.Fatalf("ApplyCopy: %v", err)
	}
	want := append([]byte(nil), old...)
	copy(want[2*chunk:], bytes.Repeat([]byte{0xaa}, 2*chunk))
	copy(want[9*chunk:], make([]byte, chunk))
	if !bytes.Equal(readDisk(t, disk), want) {
		t.Fatal("disk does not hold the copy")
	}
	if res.ChangedBytes != 3*chunk {
		t.Errorf("ChangedBytes = %d, want %d", res.ChangedBytes, 3*chunk)
	}

	points, err := RestorePoints(disk)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 1 || !points[0].Equal(t0) {
		t.Fatalf("RestorePoints = %v, want [%v]", points, t0)
	}

	out := filepath.Join(t.TempDir(), "rewound.img")
	if err := Rewind(disk, t0, out); err != nil {
		t.Fatalf("Rewind: %v", err)
	}
	if !bytes.Equal(readDisk(t, out), old) {
		t.Error("rewound disk does not match the disk before the copy")
	}
	if !bytes.Equal(readDisk(t, disk), want) {
		t.Error("Rewind changed the latest disk")
	}
}

func TestApplyCopy_SkipsBlocksThatDidNotChange(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	old := patterned(8 * chunk)
	writeDisk(t, disk, old)
	// Block 2 gets the bytes it already has (value 3); block 5 really changes.
	src := change(8*chunk, 0xbb, []int64{5 * chunk}, nil)
	src.exts = append(src.exts, qcow2.Extent{Offset: 2 * chunk, Length: chunk, Kind: qcow2.Data})
	src.data[2*chunk] = bytes.Repeat([]byte{3}, chunk)
	sortExtents(src.exts)

	res, err := ApplyCopy(src, disk, ApplyOptions{From: t0, To: t1})
	if err != nil {
		t.Fatal(err)
	}
	if res.ChangedBytes != chunk {
		t.Errorf("ChangedBytes = %d, want only the changed block", res.ChangedBytes)
	}
}

func TestApplyCopy_FullCopyZeroesGaps(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	old := patterned(8 * chunk)
	writeDisk(t, disk, old)
	// A full copy that holds only block 1: everything else is zero now.
	src := change(8*chunk, 0xcc, []int64{chunk}, nil)

	if _, err := ApplyCopy(src, disk, ApplyOptions{From: t0, To: t1, Full: true}); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 8*chunk)
	copy(want[chunk:], bytes.Repeat([]byte{0xcc}, chunk))
	if !bytes.Equal(readDisk(t, disk), want) {
		t.Fatal("a full copy must zero the parts it does not hold")
	}
	out := filepath.Join(t.TempDir(), "rewound.img")
	if err := Rewind(disk, t0, out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readDisk(t, out), old) {
		t.Error("rewind after a full copy does not restore the old disk")
	}
}

func TestApplyCopy_FirstCopyMakesTheDisk(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "disk.img")
	src := change(4*chunk, 0xdd, []int64{0}, nil)
	if _, err := ApplyCopy(src, disk, ApplyOptions{To: t1, Full: true}); err != nil {
		t.Fatal(err)
	}
	b := readDisk(t, disk)
	if int64(len(b)) != 4*chunk || b[0] != 0xdd || b[chunk] != 0 {
		t.Errorf("new disk: len %d, b[0]=%x b[chunk]=%x", len(b), b[0], b[chunk])
	}
	fi, _ := os.Stat(disk)
	if fi.Mode().Perm() != diskFileMode {
		t.Errorf("mode = %v", fi.Mode().Perm())
	}
	if points, _ := RestorePoints(disk); len(points) != 0 {
		t.Errorf("a new disk has no earlier moment, got restore points %v", points)
	}
}

func TestApplyCopy_DiskGrows(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "disk.img")
	old := patterned(4 * chunk)
	writeDisk(t, disk, old)
	if _, err := ApplyCopy(change(6*chunk, 0xee, []int64{5 * chunk}, nil), disk, ApplyOptions{From: t0, To: t1}); err != nil {
		t.Fatal(err)
	}
	if b := readDisk(t, disk); int64(len(b)) != 6*chunk || b[5*chunk] != 0xee {
		t.Fatalf("grown disk: len %d", len(b))
	}
	out := filepath.Join(t.TempDir(), "rewound.img")
	if err := Rewind(disk, t0, out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readDisk(t, out), old) {
		t.Error("rewind must restore the old size and contents")
	}
}

// failingDisk makes the disk write fail after n writes, as a Job that stops.
func TestApplyCopy_InterruptedApplyRollsBack(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	old := patterned(8 * chunk)
	writeDisk(t, disk, old)

	writes := 0
	_, err := ApplyCopy(change(8*chunk, 0xaa, []int64{1 * chunk, 4 * chunk, 6 * chunk}, nil), disk, ApplyOptions{
		From: t0, To: t1,
		beforeDiskWrite: func() error {
			writes++
			if writes == 2 {
				return errors.New("job killed")
			}
			return nil
		},
	})
	if err == nil {
		t.Fatal("expected the interrupted apply to fail")
	}
	if bytes.Equal(readDisk(t, disk), old) {
		t.Fatal("test setup: the disk should be half written")
	}

	rolled, err := Recover(disk)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if !rolled {
		t.Error("expected Recover to report a roll-back")
	}
	if !bytes.Equal(readDisk(t, disk), old) {
		t.Error("Recover did not restore the disk to the moment before the copy")
	}
	if points, _ := RestorePoints(disk); len(points) != 0 {
		t.Errorf("a rolled-back copy is not a restore point, got %v", points)
	}

	// The next copy works normally.
	if _, err := ApplyCopy(change(8*chunk, 0xab, []int64{chunk}, nil), disk, ApplyOptions{From: t0, To: t2}); err != nil {
		t.Fatalf("ApplyCopy after recovery: %v", err)
	}
}

func TestApplyCopy_IncompleteUndoIsDiscarded(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	old := patterned(4 * chunk)
	writeDisk(t, disk, old)
	if err := os.MkdirAll(undoDir(disk), 0o775); err != nil {
		t.Fatal(err)
	}
	part := filepath.Join(undoDir(disk), "0000000001.undo.part")
	if err := os.WriteFile(part, []byte("half"), 0o660); err != nil {
		t.Fatal(err)
	}
	rolled, err := Recover(disk)
	if err != nil || rolled {
		t.Fatalf("Recover = %v, %v; want no roll-back and no error", rolled, err)
	}
	if _, err := os.Stat(part); !os.IsNotExist(err) {
		t.Error("expected the incomplete undo file deleted")
	}
	if !bytes.Equal(readDisk(t, disk), old) {
		t.Error("Recover changed the disk")
	}
}

func TestApplyCopy_ChainRewindAndPrune(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "disk.img")
	v0 := patterned(8 * chunk)
	writeDisk(t, disk, v0)
	apply := func(fill byte, off int64, from, to time.Time, keep time.Duration, now time.Time) {
		t.Helper()
		if _, err := ApplyCopy(change(8*chunk, fill, []int64{off}, nil), disk,
			ApplyOptions{From: from, To: to, Keep: keep, Now: now}); err != nil {
			t.Fatal(err)
		}
	}
	apply(0x11, 0, t0, t1, 0, t1)
	v1 := readDisk(t, disk)
	apply(0x22, chunk, t1, t2, 0, t2)
	v2 := readDisk(t, disk)
	apply(0x33, 0, t2, t3, 0, t3)

	points, err := RestorePoints(disk)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 3 || !points[0].Equal(t0) || !points[1].Equal(t1) || !points[2].Equal(t2) {
		t.Fatalf("RestorePoints = %v", points)
	}
	for _, tc := range []struct {
		to   time.Time
		want []byte
	}{{t2, v2}, {t1, v1}, {t0, v0}} {
		out := filepath.Join(t.TempDir(), "r.img")
		if err := Rewind(disk, tc.to, out); err != nil {
			t.Fatalf("Rewind(%v): %v", tc.to, err)
		}
		if !bytes.Equal(readDisk(t, out), tc.want) {
			t.Errorf("Rewind(%v) gives the wrong contents", tc.to)
		}
	}
	if err := Rewind(disk, t0.Add(time.Minute), filepath.Join(t.TempDir(), "r.img")); err == nil ||
		!strings.Contains(err.Error(), "no restore point") {
		t.Errorf("Rewind to a moment that is not a restore point: err = %v", err)
	}

	// A copy with a 90-minute keep time drops undo files whose moment is
	// older than that, oldest first; the chain back from the latest stays.
	now := t3.Add(time.Hour)
	apply(0x44, 2*chunk, t3, now, 90*time.Minute, now)
	points, _ = RestorePoints(disk)
	if len(points) != 2 || !points[0].Equal(t2) || !points[1].Equal(t3) {
		t.Errorf("after pruning, RestorePoints = %v, want [%v %v]", points, t2, t3)
	}
}

func TestApplyCopy_RecoversBeforeApplying(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "disk.img")
	old := patterned(4 * chunk)
	writeDisk(t, disk, old)
	writes := 0
	_, _ = ApplyCopy(change(4*chunk, 0xaa, []int64{0, 2 * chunk}, nil), disk, ApplyOptions{
		From: t0, To: t1,
		beforeDiskWrite: func() error {
			writes++
			if writes == 2 {
				return errors.New("job killed")
			}
			return nil
		},
	})
	// The next Job applies the same copy again, without a separate Recover.
	if _, err := ApplyCopy(change(4*chunk, 0xaa, []int64{0, 2 * chunk}, nil), disk, ApplyOptions{From: t0, To: t1}); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "r.img")
	if err := Rewind(disk, t0, out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readDisk(t, out), old) {
		t.Error("the restore point after a retried apply must still give the old disk")
	}
}

func TestApplyCopy_RealStagingFile(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging.qcow2")
	copyTestImage(t, "v3.qcow2", staging)
	f, err := os.Open(staging)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	fi, _ := f.Stat()
	img, err := qcow2.Open(f, fi.Size())
	if err != nil {
		t.Fatal(err)
	}

	disk := filepath.Join(dir, "disk.img")
	old := bytes.Repeat([]byte{0x11}, 1049088)
	writeDisk(t, disk, old)
	if _, err := ApplyCopy(img, disk, ApplyOptions{From: t0, To: t1, Full: true}); err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 1049088)
	copy(want[0:], bytes.Repeat([]byte{0xaa}, 4096))
	copy(want[8192:], bytes.Repeat([]byte{0xbb}, 8192))
	copy(want[262144:], bytes.Repeat([]byte{0xdd}, 4096))
	copy(want[1048576:], bytes.Repeat([]byte{0xcc}, 512))
	if !bytes.Equal(readDisk(t, disk), want) {
		t.Error("disk does not match the staging file")
	}
	out := filepath.Join(t.TempDir(), "r.img")
	if err := Rewind(disk, t0, out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readDisk(t, out), old) {
		t.Error("rewind does not give the old disk")
	}
}
