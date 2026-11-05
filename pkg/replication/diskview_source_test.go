package replication

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

// dirUndoSource serves the undo folder of a backup disk through UndoSource, and
// counts the bytes that were read from the undo files by use.
type dirUndoSource struct {
	dir    string
	extra  []string // names to add to the listing
	opened []string
	read   [3]atomic.Int64
}

func (s *dirUndoSource) Names() ([]string, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	return append(out, s.extra...), nil
}

type countedFile struct {
	*os.File
	n *atomic.Int64
}

func (c countedFile) Size() int64 {
	fi, _ := c.Stat()
	return fi.Size()
}

func (c countedFile) ReadAt(p []byte, off int64) (int, error) {
	n, err := c.File.ReadAt(p, off)
	c.n.Add(int64(n))
	return n, err
}

func (s *dirUndoSource) Open(name string, use UndoUse) (UndoFile, error) {
	s.opened = append(s.opened, name)
	f, err := os.Open(filepath.Join(s.dir, name)) // test names are checked by the caller
	if err != nil {
		return nil, err
	}
	return countedFile{File: f, n: &s.read[use]}, nil
}

func openSourceView(t *testing.T, disk string, latest time.Time, src UndoSource, at time.Time) (*DiskView, error) {
	t.Helper()
	f, err := os.Open(disk)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	fi, _ := f.Stat()
	return OpenDiskViewFrom(f, fi.Size(), latest, src, at)
}

func TestDiskViewFrom_EqualsRestoreDiskForEveryRestorePoint(t *testing.T) {
	for seed := int64(1); seed <= 25; seed++ {
		rng := rand.New(rand.NewSource(seed))
		disk, moments := history(t, rng, 2+rng.Intn(4))
		latest := moments[len(moments)-1]
		half := 30 * time.Minute
		ats := []time.Time{{}}
		for _, m := range moments {
			ats = append(ats, m, m.Add(half), m.Add(-half))
		}
		for _, at := range ats {
			out := filepath.Join(t.TempDir(), "out.img")
			want, werr := RestoreDisk(disk, at, out)
			src := &dirUndoSource{dir: filepath.Join(filepath.Dir(disk), undoDirName)}
			dv, verr := openSourceView(t, disk, latest, src, at)
			if (werr == nil) != (verr == nil) {
				t.Fatalf("seed %d at %v: RestoreDisk error %v, view error %v", seed, at, werr, verr)
			}
			if werr != nil {
				if !errors.Is(verr, ErrNoRestorePoint) {
					t.Fatalf("seed %d at %v: error %v is not ErrNoRestorePoint", seed, at, verr)
				}
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
			_ = dv.Close()
		}
	}
}

func TestDiskViewFrom_ReadsOnlyTheUndoFilesFromTheRestorePoint(t *testing.T) {
	disk, moments := history(t, rand.New(rand.NewSource(3)), 4)
	dir := filepath.Join(filepath.Dir(disk), undoDirName)
	entries, _ := os.ReadDir(dir)
	sizes := map[string]int64{}
	for _, e := range entries {
		fi, _ := e.Info()
		sizes[e.Name()] = fi.Size()
	}
	// The newest restore point: only the newest undo file is scanned.
	src := &dirUndoSource{dir: dir}
	dv, err := openSourceView(t, disk, moments[len(moments)-1], src, moments[len(moments)-2])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dv.Close() }()
	newest := sizes[fmt.Sprintf("%010d.undo", len(entries))]
	if got := src.read[UndoScan].Load(); got != newest {
		t.Errorf("scanned %d bytes; the newest undo file is %d", got, newest)
	}
	if got := src.read[UndoHeader].Load(); got > int64(len(entries))*(40+16) {
		t.Errorf("headers and trailers took %d bytes for %d files", got, len(entries))
	}
}

func TestDiskViewFrom_OlderThanEveryUndoFileIsRefused(t *testing.T) {
	disk, moments := history(t, rand.New(rand.NewSource(5)), 2)
	src := &dirUndoSource{dir: filepath.Join(filepath.Dir(disk), undoDirName)}
	_, err := openSourceView(t, disk, moments[len(moments)-1], src, moments[0].Add(-time.Hour))
	if !errors.Is(err, ErrNoRestorePoint) {
		t.Fatalf("err = %v; want ErrNoRestorePoint", err)
	}
	if err != nil && bytes.Contains([]byte(err.Error()), []byte("moment")) {
		t.Errorf("the words must say restore point, not moment: %v", err)
	}
}

// hostileDir makes an undo folder with the given files (name -> header, records).
type hostileFile struct {
	name string
	h    undoHeader
	recs [][2]int64
}

func hostileSource(t *testing.T, files ...hostileFile) (*dirUndoSource, string, time.Time) {
	t.Helper()
	root := t.TempDir()
	dir := filepath.Join(root, undoDirName)
	if err := os.Mkdir(dir, 0o750); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(root, "disk.img")
	if err := os.WriteFile(disk, make([]byte, 4*chunk), 0o660); err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		writeUndoFile(t, filepath.Join(dir, f.name), f.h, f.recs)
	}
	return &dirUndoSource{dir: dir}, disk, t0.Add(10 * time.Hour)
}

func hdr(from int, old int64) undoHeader {
	f := t0.Add(time.Duration(from) * time.Hour)
	return undoHeader{from: f, to: f.Add(time.Hour), oldSize: old, newSize: old}
}

func TestDiskViewFrom_HostileUndoFiles(t *testing.T) {
	cases := []struct {
		name  string
		files []hostileFile
		want  error // nil: any error
		pre   func()
	}{
		{"records overlap", []hostileFile{{"0000000001.undo", hdr(1, 4*chunk), [][2]int64{{0, 100}, {50, 100}}}}, ErrUndoOrder, nil},
		{"record past the old size", []hostileFile{{"0000000001.undo", hdr(1, 1000), [][2]int64{{950, 100}}}}, nil, nil},
		{"record longer than a chunk", []hostileFile{{"0000000001.undo", hdr(1, 4*chunk), [][2]int64{{0, chunk + 1}}}}, nil, nil},
		{"files out of order", []hostileFile{
			{"0000000001.undo", hdr(5, 4*chunk), [][2]int64{{0, 10}}},
			{"0000000002.undo", hdr(2, 4*chunk), [][2]int64{{0, 10}}},
		}, ErrUndoOrder, nil},
		{"too many records", []hostileFile{{"0000000001.undo", hdr(1, 4*chunk), [][2]int64{{0, 1}, {2, 1}, {4, 1}}}}, nil, func() { maxViewRecords = 2 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			saved := maxViewRecords
			defer func() { maxViewRecords = saved }()
			if c.pre != nil {
				c.pre()
			}
			src, disk, latest := hostileSource(t, c.files...)
			_, err := openSourceView(t, disk, latest, src, t0.Add(time.Hour))
			if err == nil {
				t.Fatal("a hostile undo file must be refused")
			}
			if c.want != nil && !errors.Is(err, c.want) {
				t.Errorf("err = %v; want %v", err, c.want)
			}
		})
	}
}

func TestDiskViewFrom_RecordCountInTheTrailerMustMatch(t *testing.T) {
	src, disk, latest := hostileSource(t, hostileFile{"0000000001.undo", hdr(1, 4*chunk), [][2]int64{{0, 10}, {20, 10}}})
	p := filepath.Join(src.dir, "0000000001.undo")
	b, _ := os.ReadFile(p)
	b[len(b)-1] = 99 // the trailer says 99 records
	if err := os.WriteFile(p, b, 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := openSourceView(t, disk, latest, src, t0.Add(time.Hour)); err == nil {
		t.Fatal("a wrong record count must be refused")
	}
}

func TestDiskViewFrom_OnlyUndoNamesAreOpened(t *testing.T) {
	src, disk, latest := hostileSource(t, hostileFile{"0000000001.undo", hdr(1, 4*chunk), [][2]int64{{0, 10}}})
	src.extra = []string{"../1.undo", "a/b.undo", "x.undo", "2.undo.part", "3.undo.ready", "readme", ".undo", "-1.undo", "99999999999999999999.undo"}
	dv, err := openSourceView(t, disk, latest, src, t0.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dv.Close() }()
	for _, n := range src.opened {
		if n != "0000000001.undo" {
			t.Errorf("opened %q; only 1.undo is a name of an undo file", n)
		}
	}
}

func TestDiskViewFrom_NeverReadsPastTheEnd(t *testing.T) {
	disk, moments := history(t, rand.New(rand.NewSource(9)), 2)
	src := &dirUndoSource{dir: filepath.Join(filepath.Dir(disk), undoDirName)}
	dv, err := openSourceView(t, disk, moments[len(moments)-1], src, moments[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dv.Close() }()
	if n, err := dv.ReadAt(make([]byte, 10), dv.Size()); n != 0 || !errors.Is(err, io.EOF) {
		t.Errorf("ReadAt at the end = %d, %v", n, err)
	}
}

// rawUndoFile writes an undo file whose records are given as raw (offset,
// length) words with no data after them, so any word can be hostile.
func rawUndoFile(t *testing.T, path string, h undoHeader, off, n uint64, data int) {
	t.Helper()
	var b bytes.Buffer
	b.WriteString(undoMagic)
	for _, v := range []uint64{uint64(h.from.UnixNano()), uint64(h.to.UnixNano()), uint64(h.oldSize), uint64(h.newSize)} {
		b.Write(binary.BigEndian.AppendUint64(nil, v))
	}
	b.Write(binary.BigEndian.AppendUint64(nil, off))
	b.Write(binary.BigEndian.AppendUint64(nil, n))
	b.Write(make([]byte, data))
	b.WriteString(undoEndMagic)
	b.Write(binary.BigEndian.AppendUint64(nil, 1))
	if err := os.WriteFile(path, b.Bytes(), 0o660); err != nil {
		t.Fatal(err)
	}
}

func TestDiskViewFrom_HostileHeaderAndRecordNumbers(t *testing.T) {
	const max = uint64(1<<63 - 1)
	cases := []struct {
		name      string
		old, new_ int64
		off, n    uint64
		dataLen   int
	}{
		{"offset so large that offset plus length wraps", 1 << 40, 1 << 40, max - 10, 100, 100},
		{"negative offset", 1 << 40, 1 << 40, ^uint64(4), 100, 100},
		{"huge length", 1 << 40, 1 << 40, 0, max, 0},
		{"negative length", 1 << 40, 1 << 40, 0, ^uint64(4), 0},
		{"offset at the old size", 1000, 1000, 1000, 1, 1},
		{"negative old size", -1, 1000, 0, 1, 1},
		{"negative new size", 1000, -1, 0, 1, 1},
		{"old size beyond any disk", 1<<63 - 1, 1000, 0, 1, 1},
		{"new size beyond any disk", 1000, 1<<63 - 1, 0, 1, 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			src, disk, latest := hostileSource(t)
			h := hdr(1, c.old)
			h.newSize = c.new_
			rawUndoFile(t, filepath.Join(src.dir, "0000000001.undo"), h, c.off, c.n, c.dataLen)
			if _, err := openSourceView(t, disk, latest, src, t0.Add(time.Hour)); err == nil {
				t.Fatal("hostile numbers must be refused")
			}
		})
	}
}

func TestDiskViewFrom_TooManyLayersAreRefused(t *testing.T) {
	saved := maxViewLayers
	defer func() { maxViewLayers = saved }()
	maxViewLayers = 2
	var files []hostileFile
	for i := 1; i <= 3; i++ {
		files = append(files, hostileFile{fmt.Sprintf("%010d.undo", i), hdr(i, 4*chunk), [][2]int64{{0, 10}}})
	}
	src, disk, latest := hostileSource(t, files...)
	if _, err := openSourceView(t, disk, latest, src, t0.Add(time.Hour)); err == nil {
		t.Fatal("a view over more undo files than the limit must be refused")
	}
	if _, err := openSourceView(t, disk, latest, src, t0.Add(3*time.Hour)); err != nil {
		t.Fatalf("a view over %d undo files is within the limit: %v", maxViewLayers, err)
	}
}
