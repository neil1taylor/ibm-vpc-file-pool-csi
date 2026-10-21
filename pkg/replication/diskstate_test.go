package replication

import (
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReadDiskState(t *testing.T) {
	c := newVMTestClient(t)
	if code, _ := c.send("f1f1f1f1-1", change(2*chunk, 5, []int64{0, chunk}, nil), nil, t1, true); code != http.StatusOK {
		t.Fatal("first copy failed")
	}
	st, err := ReadDiskState(c.diskPath())
	if err != nil {
		t.Fatal(err)
	}
	if !st.MomentKnown || !st.Moment.Equal(t1) || st.VMGroup != "ns/vm1" || len(st.RestorePoints) != 0 {
		t.Fatalf("after the first copy: %+v", st)
	}
	if code, _ := c.send("f1f1f1f1-2", change(2*chunk, 7, []int64{0}, nil), ptr(t1), t2, false); code != http.StatusOK {
		t.Fatal("change copy failed")
	}
	if st, err = ReadDiskState(c.diskPath()); err != nil {
		t.Fatal(err)
	}
	if !st.Moment.Equal(t2) || len(st.RestorePoints) != 1 || !st.RestorePoints[0].Equal(t1) {
		t.Fatalf("after the change copy: %+v", st)
	}
}

func TestReadDiskState_LeavesOutTheEpochRestorePoint(t *testing.T) {
	c := newVMTestClient(t)
	if err := os.MkdirAll(filepath.Dir(c.diskPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, c.diskPath(), patterned(2*chunk)) // a disk with no known moment
	if code, _ := c.send("f2f2f2f2-1", change(2*chunk, 5, []int64{0, chunk}, nil), nil, t1, true); code != http.StatusOK {
		t.Fatal("full copy failed")
	}
	st, err := ReadDiskState(c.diskPath())
	if err != nil {
		t.Fatal(err)
	}
	if len(st.RestorePoints) != 0 {
		t.Fatalf("restore points = %v, want none (the one dated at the epoch is an unknown moment)", st.RestorePoints)
	}
	if pts, _ := RestorePoints(c.diskPath()); len(pts) != 1 {
		t.Fatal("the epoch restore point should still exist on disk, for a rewind")
	}
}

func TestReadDiskState_NotAVMDisk(t *testing.T) {
	dir := t.TempDir()
	writeDisk(t, filepath.Join(dir, "disk.img"), []byte("x"))
	st, err := ReadDiskState(filepath.Join(dir, "disk.img"))
	if err != nil {
		t.Fatal(err)
	}
	if st.MomentKnown || st.VMGroup != "" {
		t.Fatalf("state = %+v, want empty for a disk the receiver never wrote", st)
	}
}

func TestLease(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)
	if on, err := LeaseActive(dir, now); err != nil || on {
		t.Fatalf("no lease: %v %v", on, err)
	}
	if err := writeLease(dir, now.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if on, err := LeaseActive(dir, now); err != nil || !on {
		t.Fatalf("live lease: %v %v", on, err)
	}
	if on, err := LeaseActive(dir, now.Add(2*time.Hour)); err != nil || on {
		t.Fatalf("expired lease: %v %v", on, err)
	}
}

func TestReceiverVM_CommitLeavesNoLease(t *testing.T) {
	c := newVMTestClient(t)
	if code, _ := c.send("f3f3f3f3-1", change(2*chunk, 5, []int64{0, chunk}, nil), nil, t1, true); code != http.StatusOK {
		t.Fatal("first copy failed")
	}
	if on, err := LeaseActive(filepath.Dir(c.diskPath()), time.Now()); err != nil || on {
		t.Fatalf("lease after a good commit: %v %v", on, err)
	}
	// A refused commit leaves none either.
	if code, _ := c.send("f3f3f3f3-2", change(2*chunk, 7, []int64{0}, nil), ptr(t0), t2, false); code != http.StatusConflict {
		t.Fatal("expected 409")
	}
	if on, _ := LeaseActive(filepath.Dir(c.diskPath()), time.Now()); on {
		t.Fatal("lease after a refused commit")
	}
}

func TestReadDiskState_ModifiedAfterTheLastCopy(t *testing.T) {
	c := newVMTestClient(t)
	if code, _ := c.send("f4f4f4f4-1", change(2*chunk, 5, []int64{0, chunk}, nil), nil, t1, true); code != http.StatusOK {
		t.Fatal("first copy failed")
	}
	st, err := ReadDiskState(c.diskPath())
	if err != nil || st.Modified {
		t.Fatalf("right after the commit: modified = %v, err = %v", st.Modified, err)
	}
	// A VM that runs on the disk writes to it.
	f, err := os.OpenFile(c.diskPath(), os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteAt([]byte{9}, 10); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	later := time.Now().Add(time.Minute)
	if err = os.Chtimes(c.diskPath(), later, later); err != nil {
		t.Fatal(err)
	}
	if st, err = ReadDiskState(c.diskPath()); err != nil || !st.Modified {
		t.Fatalf("after a write: modified = %v, err = %v", st.Modified, err)
	}
}

func TestReadDiskState_UnfinishedCopyIsNotAModification(t *testing.T) {
	dir := t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	if _, err := ApplyCopy(change(2*chunk, 1, []int64{0, chunk}, nil), disk, ApplyOptions{To: t1, Full: true}); err != nil {
		t.Fatal(err)
	}
	if err := WriteMoment(dir, t1, "ns/vm"); err != nil {
		t.Fatal(err)
	}
	// A copy that stops half way changes the disk, and leaves a .ready undo file.
	stop := errors.New("stop")
	_, err := ApplyCopy(change(2*chunk, 2, []int64{0, chunk}, nil), disk, ApplyOptions{From: t1, To: t2, Full: true, beforeDiskWrite: func() error { return stop }})
	if !errors.Is(err, stop) {
		t.Fatalf("err = %v", err)
	}
	later := time.Now().Add(time.Minute)
	_ = os.Chtimes(disk, later, later)
	st, err := ReadDiskState(disk)
	if err != nil || st.Modified {
		t.Fatalf("with an unfinished copy: modified = %v, err = %v", st.Modified, err)
	}
	// After the roll back, the disk is as it was and counts as unmodified.
	if _, err = Recover(disk); err != nil {
		t.Fatal(err)
	}
	if st, err = ReadDiskState(disk); err != nil || st.Modified {
		t.Fatalf("after the roll back: modified = %v, err = %v", st.Modified, err)
	}
}
