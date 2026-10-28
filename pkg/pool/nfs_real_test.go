package pool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestRealNFSOperations_FilesRoundTrip(t *testing.T) {
	ops := NewRealNFSOperations()
	dir := filepath.Join(t.TempDir(), "a", "b")

	if err := ops.MkdirAll(dir, 0o750); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	info, err := ops.Stat(dir)
	if err != nil || !info.IsDir() {
		t.Fatalf("Stat after MkdirAll: %v, %v", info, err)
	}

	file := filepath.Join(dir, "f.txt")
	if err = ops.WriteFile(file, []byte("hello"), 0o640); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	got, err := ops.ReadFile(file)
	if err != nil || string(got) != "hello" {
		t.Fatalf("ReadFile = %q, %v; want hello", got, err)
	}

	if err = ops.Chmod(file, 0o600); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if info, err = ops.Stat(file); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode after Chmod = %v, %v; want 0600", info.Mode().Perm(), err)
	}

	// A user may always give a file to its own owner and group.
	if err = ops.Chown(file, os.Getuid(), os.Getgid()); err != nil {
		t.Fatalf("Chown to self: %v", err)
	}
}

func TestRealNFSOperations_MissingPathsAreErrors(t *testing.T) {
	ops := NewRealNFSOperations()
	missing := filepath.Join(t.TempDir(), "absent")

	if _, err := ops.Stat(missing); !os.IsNotExist(err) {
		t.Errorf("Stat(missing) = %v, want not-exist", err)
	}
	if _, err := ops.ReadFile(missing); !os.IsNotExist(err) {
		t.Errorf("ReadFile(missing) = %v, want not-exist", err)
	}
	if err := ops.Chmod(missing, 0o600); err == nil {
		t.Error("Chmod(missing) = nil, want an error")
	}
	if err := ops.CopyDir(missing, filepath.Join(t.TempDir(), "dst")); err == nil || !strings.Contains(err.Error(), "cp -a") {
		t.Errorf("CopyDir(missing source) = %v, want an error naming cp -a", err)
	}
}

func TestRealNFSOperations_RemoveAllRemovesATreeAndOnlyThatTree(t *testing.T) {
	ops := NewRealNFSOperations()
	root := t.TempDir()
	tree := filepath.Join(root, "pvcs", "pvc-1")
	sibling := filepath.Join(root, "pvcs", "pvc-2")
	for _, d := range []string{filepath.Join(tree, "deep", "er"), sibling} {
		if err := ops.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	if err := ops.WriteFile(filepath.Join(tree, "deep", "er", "x"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := ops.RemoveAll(tree); err != nil {
		t.Fatalf("RemoveAll: %v", err)
	}
	if _, err := os.Stat(tree); !os.IsNotExist(err) {
		t.Errorf("the tree is still there: %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("a sibling folder must stay: %v", err)
	}
	if err := ops.RemoveAll(tree); err != nil {
		t.Errorf("RemoveAll of a path that is gone must not fail: %v", err)
	}
}

func TestRealNFSOperations_CopyDirKeepsTheTree(t *testing.T) {
	ops := NewRealNFSOperations()
	src := filepath.Join(t.TempDir(), "src")
	dst := filepath.Join(t.TempDir(), "dst")
	if err := os.MkdirAll(filepath.Join(src, "sub"), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "sub", "f"), []byte("data"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := ops.CopyDir(src, dst); err != nil {
		t.Fatalf("CopyDir: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "sub", "f"))
	if err != nil || string(got) != "data" {
		t.Fatalf("copied file = %q, %v; want data", got, err)
	}
}

func requireRsync(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("rsync"); err != nil {
		t.Skip("rsync is not installed")
	}
}

func TestRealNFSOperations_SyncDirMakesTheDestinationMatchAndDeletesExtras(t *testing.T) {
	requireRsync(t)
	ops := NewRealNFSOperations()
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "keep"), []byte("new"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "stale"), []byte("old"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := ops.SyncDir(context.Background(), src, dst); err != nil {
		t.Fatalf("SyncDir: %v", err)
	}
	if got, err := os.ReadFile(filepath.Join(dst, "keep")); err != nil || string(got) != "new" {
		t.Errorf("synced file = %q, %v; want new", got, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "stale")); !os.IsNotExist(err) {
		t.Errorf("a file that is not in the source must be deleted from the destination: %v", err)
	}
}

func TestRealNFSOperations_SyncDirWithOptionsPassesItsFlags(t *testing.T) {
	requireRsync(t)
	ops := NewRealNFSOperations()
	src := t.TempDir()
	dst := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f"), []byte("x"), 0o640); err != nil {
		t.Fatal(err)
	}

	if err := ops.SyncDirWithOptions(context.Background(), src, dst, SyncOptions{BandwidthLimitKBps: 10000, ExtraArgs: []string{"--exclude=skip"}}); err != nil {
		t.Fatalf("SyncDirWithOptions: %v", err)
	}
	// A bad flag shows that ExtraArgs really reach rsync.
	err := ops.SyncDirWithOptions(context.Background(), src, dst, SyncOptions{ExtraArgs: []string{"--no-such-flag"}})
	if err == nil || !strings.Contains(err.Error(), "rsync") {
		t.Errorf("a bad extra flag must fail the sync, got %v", err)
	}
}

func TestRealNFSOperations_SyncDirStopsWhenTheContextIsCancelled(t *testing.T) {
	requireRsync(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewRealNFSOperations().SyncDir(ctx, t.TempDir(), t.TempDir()); err == nil {
		t.Error("SyncDir with a cancelled context = nil, want an error")
	}
}

func TestRealNFSOperations_MkdirAsUser(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("MkdirAsUser changes its user id, which needs root")
	}
	ops := NewRealNFSOperations()
	dir := filepath.Join(t.TempDir(), "parent", "child")
	if err := ops.MkdirAsUser(dir, 0o755, 107, 107); err != nil {
		t.Fatalf("MkdirAsUser: %v", err)
	}
	info, err := os.Stat(dir)
	if err != nil || info.Mode().Perm() != 0o755 {
		t.Fatalf("child = %v, %v; want mode 0755", info, err)
	}
}
