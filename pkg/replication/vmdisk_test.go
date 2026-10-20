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

func TestReplaceDiskFromStaging(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging.qcow2")
	disk := filepath.Join(dir, "disk.img")
	copyTestImage(t, "v3.qcow2", staging)
	if err := os.WriteFile(disk, bytes.Repeat([]byte{0x11}, 2<<20), 0o660); err != nil {
		t.Fatal(err)
	}

	n, err := ReplaceDiskFromStaging(staging, disk)
	if err != nil {
		t.Fatalf("ReplaceDiskFromStaging: %v", err)
	}
	if n != 4096+8192+4096+512 {
		t.Errorf("reported %d data bytes", n)
	}

	got, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	want := make([]byte, 1049088)
	copy(want[0:], bytes.Repeat([]byte{0xaa}, 4096))
	copy(want[8192:], bytes.Repeat([]byte{0xbb}, 8192))
	copy(want[262144:], bytes.Repeat([]byte{0xdd}, 4096))
	copy(want[1048576:], bytes.Repeat([]byte{0xcc}, 512))
	if !bytes.Equal(got, want) {
		t.Errorf("disk.img does not match the staging image (len %d, want %d)", len(got), len(want))
	}
	assertOnlyFiles(t, dir, "disk.img", "staging.qcow2")
}

func TestReplaceDiskFromStaging_KeepsOldDiskOnError(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging.qcow2")
	disk := filepath.Join(dir, "disk.img")
	copyTestImage(t, "compressed.qcow2", staging)
	old := bytes.Repeat([]byte{0x11}, 8192)
	if err := os.WriteFile(disk, old, 0o660); err != nil {
		t.Fatal(err)
	}
	// A leftover from an earlier run that stopped part way.
	if err := os.WriteFile(disk+newDiskSuffix, []byte("partial"), 0o660); err != nil {
		t.Fatal(err)
	}

	if _, err := ReplaceDiskFromStaging(staging, disk); err == nil || !strings.Contains(err.Error(), "compressed") {
		t.Fatalf("error = %v, want one about compressed clusters", err)
	}
	got, err := os.ReadFile(disk)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, old) {
		t.Error("expected the old disk.img to stay unchanged")
	}
	assertOnlyFiles(t, dir, "disk.img", "staging.qcow2")
}

func TestReplaceDiskFromStaging_NewDisk(t *testing.T) {
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging.qcow2")
	disk := filepath.Join(dir, "disk.img")
	copyTestImage(t, "default.qcow2", staging)

	if _, err := ReplaceDiskFromStaging(staging, disk); err != nil {
		t.Fatalf("ReplaceDiskFromStaging: %v", err)
	}
	fi, err := os.Stat(disk)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Size() != 4194304 {
		t.Errorf("disk.img size = %d, want 4194304", fi.Size())
	}
	if fi.Mode().Perm() != 0o660 {
		t.Errorf("disk.img mode = %v, want 0660", fi.Mode().Perm())
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

// TestReplaceDiskFromStaging_MatchesQemuImg checks the result against
// qemu-img on an image with many random writes. It runs only where qemu-img
// and qemu-io are installed.
func TestReplaceDiskFromStaging_MatchesQemuImg(t *testing.T) {
	for _, tool := range []string{"qemu-img", "qemu-io"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	dir := t.TempDir()
	staging := filepath.Join(dir, "staging.qcow2")
	run := func(name string, args ...string) {
		t.Helper()
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			t.Fatalf("%s %v: %v\n%s", name, args, err, out)
		}
	}
	const size = 64 << 20
	run("qemu-img", "create", "-q", "-f", "qcow2", staging, fmt.Sprint(size))
	rng := rand.New(rand.NewSource(1))
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
	run("qemu-io", append(args, staging)...)

	disk := filepath.Join(dir, "disk.img")
	if _, err := ReplaceDiskFromStaging(staging, disk); err != nil {
		t.Fatalf("ReplaceDiskFromStaging: %v", err)
	}
	ref := filepath.Join(dir, "ref.img")
	run("qemu-img", "convert", "-f", "qcow2", "-O", "raw", staging, ref)
	run("qemu-img", "compare", "-f", "raw", "-F", "raw", disk, ref)
}

func TestApplyVMDisk(t *testing.T) {
	src := t.TempDir()
	dst := t.TempDir()
	staging := filepath.Join(src, ".repl-staging-a1.qcow2")
	disk := filepath.Join(dst, "disk.img")
	copyTestImage(t, "v3.qcow2", staging)

	if _, err := ApplyVMDisk(staging, disk); err != nil {
		t.Fatalf("ApplyVMDisk: %v", err)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Errorf("expected the staging file deleted, stat err = %v", err)
	}
	if fi, err := os.Stat(disk); err != nil || fi.Size() != 1049088 {
		t.Errorf("disk.img: %v, %v", fi, err)
	}
}

func TestApplyVMDisk_KeepsStagingOnError(t *testing.T) {
	src := t.TempDir()
	staging := filepath.Join(src, ".repl-staging-a1.qcow2")
	copyTestImage(t, "compressed.qcow2", staging)

	if _, err := ApplyVMDisk(staging, filepath.Join(t.TempDir(), "disk.img")); err == nil {
		t.Fatal("expected an error")
	}
	if _, err := os.Stat(staging); err != nil {
		t.Errorf("expected the staging file kept for the next try, stat err = %v", err)
	}
}

func TestApplyVMDisk_RefusesOtherFiles(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"disk.img", "repl-staging-a1.qcow2", ".repl-staging-a1.qcow2.bak", ".repl-staging-../x.qcow2"} {
		p := filepath.Join(dir, name)
		if _, err := ApplyVMDisk(p, filepath.Join(dir, "out.img")); err == nil || !strings.Contains(err.Error(), "staging file name") {
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
	disk := filepath.Join(dir, "disk.img")
	if _, err := ApplyVMDisk(staging, disk); err == nil {
		t.Fatal("expected a staging file that is a symlink to be refused")
	}
	if _, err := os.Stat(disk); !os.IsNotExist(err) {
		t.Errorf("expected no disk written, stat err = %v", err)
	}
	if _, err := os.Lstat(staging); err != nil {
		t.Errorf("expected the symlink left alone, lstat err = %v", err)
	}
}
