package replication

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func copyTestImage(t *testing.T, name, dst string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "qcow2", "testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func assertOnlyFiles(t *testing.T, dir string, want ...string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("files in dir = %v, want %v", got, want)
	}
}

// v3Contents is the disk that testdata/v3.qcow2 describes.
func v3Contents() []byte {
	want := make([]byte, 1049088)
	copy(want[0:], bytes.Repeat([]byte{0xaa}, 4096))
	copy(want[8192:], bytes.Repeat([]byte{0xbb}, 8192))
	copy(want[262144:], bytes.Repeat([]byte{0xdd}, 4096))
	copy(want[1048576:], bytes.Repeat([]byte{0xcc}, 512))
	return want
}

func TestApplyVMDisk_FirstCopy(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	staging := filepath.Join(src, ".repl-staging-a1.qcow2")
	disk := filepath.Join(dst, "disk.img")
	copyTestImage(t, "v3.qcow2", staging)

	res, err := ApplyVMDisk(staging, disk, ApplyOptions{To: t1, Full: true})
	if err != nil {
		t.Fatalf("ApplyVMDisk: %v", err)
	}
	if !bytes.Equal(readDisk(t, disk), v3Contents()) {
		t.Error("disk.img does not match the staging file")
	}
	if fi, _ := os.Stat(disk); fi.Mode().Perm() != 0o660 {
		t.Errorf("disk.img mode = %v, want 0660", fi.Mode().Perm())
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("expected the staging file deleted, stat err = %v", err)
	}
	if res.RestorePoints != 0 || res.OldestRestorePoint != nil {
		t.Errorf("a first copy has no restore point, got %+v", res)
	}
	assertOnlyFiles(t, dst, "disk.img")
}

func TestApplyVMDisk_ReportsRestorePoints(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	staging := filepath.Join(src, ".repl-staging-b2.qcow2")
	disk := filepath.Join(dst, "disk.img")
	writeDisk(t, disk, bytes.Repeat([]byte{0x11}, 1049088))
	copyTestImage(t, "v3.qcow2", staging)

	res, err := ApplyVMDisk(staging, disk, ApplyOptions{From: t0, To: t1, Full: true})
	if err != nil {
		t.Fatalf("ApplyVMDisk: %v", err)
	}
	if res.RestorePoints != 1 || res.OldestRestorePoint == nil || !res.OldestRestorePoint.Equal(t0) {
		t.Errorf("result = %+v, want one restore point at %v", res, t0)
	}
	if res.ChangedBytes == 0 {
		t.Error("expected changed bytes")
	}
}

func TestApplyVMDisk_KeepsStagingAndDiskOnError(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	staging := filepath.Join(src, ".repl-staging-a1.qcow2")
	disk := filepath.Join(dst, "disk.img")
	old := bytes.Repeat([]byte{0x11}, 8192)
	writeDisk(t, disk, old)
	copyTestImage(t, "compressed.qcow2", staging)

	if _, err := ApplyVMDisk(staging, disk, ApplyOptions{From: t0, To: t1, Full: true}); err == nil || !strings.Contains(err.Error(), "compressed") {
		t.Fatalf("error = %v, want one about compressed clusters", err)
	}
	if _, err := os.Stat(staging); err != nil {
		t.Errorf("expected the staging file kept for the next try, stat err = %v", err)
	}
	if !bytes.Equal(readDisk(t, disk), old) {
		t.Error("expected the old disk.img to stay unchanged")
	}
}

func TestApplyVMDisk_RefusesOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"disk.img", "repl-staging-a1.qcow2", ".repl-staging-a1.qcow2.bak", ".repl-staging-../x.qcow2"} {
		p := filepath.Join(dir, name)
		if _, err := ApplyVMDisk(p, filepath.Join(dir, "out.img"), ApplyOptions{To: t1, Full: true}); err == nil || !strings.Contains(err.Error(), "staging file name") {
			t.Errorf("ApplyVMDisk(%q) error = %v, want a refusal", name, err)
		}
	}
}

func TestApplyVMDisk_RefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "other-tenant-disk.qcow2")
	copyTestImage(t, "v3.qcow2", other)
	staging := filepath.Join(dir, ".repl-staging-a1.qcow2")
	if err := os.Symlink(other, staging); err != nil {
		t.Fatal(err)
	}
	disk := filepath.Join(t.TempDir(), "disk.img")
	if _, err := ApplyVMDisk(staging, disk, ApplyOptions{To: t1, Full: true}); err == nil {
		t.Fatal("expected a staging file that is a symlink to be refused")
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Errorf("expected no disk written, stat err = %v", err)
	}
	if _, err := os.Lstat(staging); err != nil {
		t.Errorf("expected the symlink left alone, lstat err = %v", err)
	}
}

// TestApplyVMDisk_MatchesQemuImg checks the result against qemu-img on an
// image with many random writes, as a first copy and as a copy over an old
// disk. It runs only where qemu-img and qemu-io are installed.
func TestApplyVMDisk_MatchesQemuImg(t *testing.T) {
	for _, tool := range []string{"qemu-img", "qemu-io"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	const size = 64 << 20
	makeStaging := func(path string, seed int64) {
		run("qemu-img", "create", "-q", "-f", "qcow2", path, fmt.Sprint(size))
		rng := rand.New(rand.NewSource(seed))
		args := []string{"-f", "qcow2"}
		for i := 0; i < 200; i++ {
			off := rng.Int63n(size/512-16) * 512
			n := (rng.Int63n(16) + 1) * 512
			if i%5 == 0 {
				args = append(args, "-c", fmt.Sprintf("write -z %d %d", off, n))
			} else {
				args = append(args, "-c", fmt.Sprintf("write -P %d %d %d", rng.Intn(255)+1, off, n))
			}
		}
		run("qemu-io", append(args, path)...)
	}
	src, dst := t.TempDir(), t.TempDir()
	disk := filepath.Join(dst, "disk.img")
	for i, opts := range []ApplyOptions{{To: t1, Full: true}, {From: t1, To: t2, Full: true}} {
		staging := filepath.Join(src, fmt.Sprintf(".repl-staging-s%d.qcow2", i))
		makeStaging(staging, int64(i+1))
		ref := filepath.Join(src, "ref.img")
		run("qemu-img", "convert", "-f", "qcow2", "-O", "raw", staging, ref)
		if _, err := ApplyVMDisk(staging, disk, opts); err != nil {
			t.Fatalf("ApplyVMDisk %d: %v", i, err)
		}
		run("qemu-img", "compare", "-f", "raw", "-F", "raw", disk, ref)
		if err := os.Remove(ref); err != nil {
			t.Fatal(err)
		}
	}
}

// A finished apply Job whose result was lost is run again. The staging file is
// gone then, but the disk already shows the copy, so the second run must
// succeed instead of failing for good (#160).
func TestApplyVMDisk_StagingGoneAfterApplied(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	staging := filepath.Join(src, ".repl-staging-c3.qcow2")
	disk := filepath.Join(dst, "disk.img")
	writeDisk(t, disk, bytes.Repeat([]byte{0x11}, 1049088))
	copyTestImage(t, "v3.qcow2", staging)
	opts := ApplyOptions{From: t0, To: t1, Full: true}
	if _, err := ApplyVMDisk(staging, disk, opts); err != nil {
		t.Fatalf("first ApplyVMDisk: %v", err)
	}

	res, err := ApplyVMDisk(staging, disk, opts)
	if err != nil {
		t.Fatalf("second ApplyVMDisk with the staging file gone: %v", err)
	}
	if !bytes.Equal(readDisk(t, disk), v3Contents()) {
		t.Error("disk.img changed on the second run")
	}
	if res.RestorePoints != 1 || res.OldestRestorePoint == nil || !res.OldestRestorePoint.Equal(t0) {
		t.Errorf("result = %+v, want one restore point at %v", res, t0)
	}
}

func TestApplyVMDisk_StagingGoneAfterFirstCopy(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	staging := filepath.Join(src, ".repl-staging-d4.qcow2")
	disk := filepath.Join(dst, "disk.img")
	copyTestImage(t, "v3.qcow2", staging)
	opts := ApplyOptions{To: t1, Full: true}
	if _, err := ApplyVMDisk(staging, disk, opts); err != nil {
		t.Fatalf("first ApplyVMDisk: %v", err)
	}
	if _, err := ApplyVMDisk(staging, disk, opts); err != nil {
		t.Fatalf("second ApplyVMDisk with the staging file gone: %v", err)
	}
	if !bytes.Equal(readDisk(t, disk), v3Contents()) {
		t.Error("disk.img changed on the second run")
	}
}

// A missing staging file is not proof of a finished copy: if the disk does
// not show this copy's moment, the run must still fail.
func TestApplyVMDisk_StagingGoneNotApplied(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	staging := filepath.Join(src, ".repl-staging-e5.qcow2")
	disk := filepath.Join(dst, "disk.img")
	writeDisk(t, disk, bytes.Repeat([]byte{0x11}, 1049088))
	copyTestImage(t, "v3.qcow2", staging)
	if _, err := ApplyVMDisk(staging, disk, ApplyOptions{From: t0, To: t1, Full: true}); err != nil {
		t.Fatalf("first ApplyVMDisk: %v", err)
	}

	if _, err := ApplyVMDisk(staging, disk, ApplyOptions{From: t1, To: t2, Full: true}); err == nil {
		t.Error("expected an error for a different copy moment with no staging file")
	}
}
