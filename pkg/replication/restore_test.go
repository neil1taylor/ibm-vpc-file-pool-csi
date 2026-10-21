package replication

import (
	"bytes"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// backupWithMoments makes a backup disk with the moments t0, t1 and t2: the
// disk shows t2, and t0 and t1 are restore points. It returns the disk and
// what it held at each moment.
func backupWithMoments(t *testing.T) (disk string, at0, at1, at2 []byte) {
	t.Helper()
	dir := t.TempDir()
	disk = filepath.Join(dir, "disk.img")
	at0 = patterned(16 * chunk)
	writeDisk(t, disk, at0)
	if _, err := ApplyCopy(change(16*chunk, 0xaa, []int64{2 * chunk}, nil), disk, ApplyOptions{From: t0, To: t1}); err != nil {
		t.Fatal(err)
	}
	at1 = readDisk(t, disk)
	if _, err := ApplyCopy(change(16*chunk, 0xbb, []int64{5 * chunk}, nil), disk, ApplyOptions{From: t1, To: t2}); err != nil {
		t.Fatal(err)
	}
	at2 = readDisk(t, disk)
	return disk, at0, at1, at2
}

func TestRestoreDisk_ChoosesTheNearestEarlierMoment(t *testing.T) {
	disk, at0, at1, at2 := backupWithMoments(t)
	half := 30 * time.Minute
	cases := []struct {
		name   string
		at     time.Time // the zero time asks for the latest
		want   []byte
		moment time.Time
		latest bool
	}{
		{"no moment gives the latest", time.Time{}, at2, t2, true},
		{"exactly the oldest", t0, at0, t0, false},
		{"between two moments gives the earlier", t0.Add(half), at0, t0, false},
		{"exactly the middle", t1, at1, t1, false},
		{"after the middle", t1.Add(half), at1, t1, false},
		{"exactly the latest", t2, at2, t2, true},
		{"after the latest", t2.Add(time.Hour), at2, t2, true},
	}
	for _, tc := range cases {
		out := filepath.Join(t.TempDir(), "disk.img")
		res, err := RestoreDisk(disk, tc.at, out)
		if err != nil {
			t.Errorf("%s: %v", tc.name, err)
			continue
		}
		if !bytes.Equal(readDisk(t, out), tc.want) {
			t.Errorf("%s: the restored disk does not hold the data of %s", tc.name, tc.moment.Format(time.RFC3339))
		}
		if !res.Moment.Equal(tc.moment) || res.Latest != tc.latest {
			t.Errorf("%s: result = %+v, want moment %s latest %v", tc.name, res, tc.moment.Format(time.RFC3339), tc.latest)
		}
	}
	if !bytes.Equal(readDisk(t, disk), at2) {
		t.Error("RestoreDisk changed the backup disk")
	}
}

func TestRestoreDisk_RefusesAMomentBeforeTheOldestBackup(t *testing.T) {
	disk, _, _, _ := backupWithMoments(t)
	out := filepath.Join(t.TempDir(), "disk.img")
	if _, err := RestoreDisk(disk, t0.Add(-time.Minute), out); err == nil {
		t.Fatal("want an error: nothing was kept before that moment")
	}
	if _, err := os.Stat(out); err == nil {
		t.Error("a failed restore left a file")
	}
}

// usedBytes is the space a file takes on disk, not its size.
func usedBytes(t *testing.T, path string) int64 {
	t.Helper()
	var st syscall.Stat_t
	if err := syscall.Stat(path, &st); err != nil {
		t.Fatal(err)
	}
	return st.Blocks * 512
}

func TestRestoreDisk_LatestCopyKeepsHolesSparse(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	f, err := os.Create(disk)
	if err != nil {
		t.Fatal(err)
	}
	// A 256 MiB disk with data in one chunk only.
	if err := f.Truncate(256 << 20); err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteAt(bytes.Repeat([]byte{7}, chunk), 10<<20); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	out := filepath.Join(t.TempDir(), "disk.img")
	if _, err := RestoreDisk(disk, time.Time{}, out); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(readDisk(t, out), readDisk(t, disk)) {
		t.Fatal("the copy differs from the backup")
	}
	// Compare with the backup itself: how sparse a file can be depends on the
	// file system the test runs on.
	if got, limit := usedBytes(t, out), usedBytes(t, disk)+(2<<20); got > limit {
		t.Errorf("the copy uses %d bytes on disk, the backup %d: holes must stay holes", got, usedBytes(t, disk))
	}
}

// The first backup of a volume has no restore point, and a direct copy writes
// no moment file. It is the only moment there is, so it restores as the latest
// and the result says that the moment is not known.
func TestRestoreDisk_FirstBackupWithoutAMomentRestoresAsTheLatest(t *testing.T) {
	disk := filepath.Join(t.TempDir(), "disk.img")
	b := patterned(4 * chunk)
	writeDisk(t, disk, b)
	for _, at := range []time.Time{{}, t1} {
		out := filepath.Join(t.TempDir(), "disk.img")
		res, err := RestoreDisk(disk, at, out)
		if err != nil {
			t.Fatalf("at %v: %v", at, err)
		}
		if !res.Latest || !res.Moment.IsZero() || !bytes.Equal(readDisk(t, out), b) {
			t.Errorf("at %v: result = %+v, want the latest with an unknown moment", at, res)
		}
	}
}

func TestRestoreDisk_RefusesADiskThatAVMWroteTo(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	writeDisk(t, disk, patterned(4*chunk))
	if err := writeMoment(dir, t0, ""); err != nil {
		t.Fatal(err)
	}
	// Something writes to the disk after the last copy sealed it.
	later := time.Now().Add(time.Hour)
	if err := os.WriteFile(disk, patterned(4*chunk), 0o660); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(disk, later, later); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "disk.img")
	if _, err := RestoreDisk(disk, time.Time{}, out); err == nil {
		t.Error("want an error: the backup disk changed after its last copy")
	}
}
