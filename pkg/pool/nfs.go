package pool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// SyncOptions controls rsync behavior for SyncDirWithOptions.
type SyncOptions struct {
	// BandwidthLimitKBps limits rsync bandwidth in kilobytes per second. 0 means no limit.
	BandwidthLimitKBps int32
	// ExtraArgs are additional rsync flags appended after the base flags.
	ExtraArgs []string
}

// NFSOperations wraps filesystem calls for testability.
// The real implementation delegates to os.* functions.
// Tests use FakeNFSOperations (in test files).
type NFSOperations interface {
	MkdirAll(path string, perm os.FileMode) error
	MkdirAsUser(path string, perm os.FileMode, uid, gid uint32) error
	RemoveAll(path string) error
	Stat(path string) (os.FileInfo, error)
	Chown(path string, uid, gid int) error
	Chmod(path string, mode os.FileMode) error
	CopyDir(src, dst string) error
	SyncDir(ctx context.Context, src, dst string) error
	SyncDirWithOptions(ctx context.Context, src, dst string, opts SyncOptions) error
	WriteFile(path string, data []byte, perm os.FileMode) error
	ReadFile(path string) ([]byte, error)
}

type realNFSOperations struct{}

// NewRealNFSOperations returns an NFSOperations that delegates to the real OS.
func NewRealNFSOperations() NFSOperations {
	return &realNFSOperations{}
}

func (r *realNFSOperations) MkdirAll(path string, perm os.FileMode) error {
	return os.MkdirAll(path, perm)
}

// MkdirAsUser creates a directory by spawning mkdir as the specified UID/GID.
// This bypasses NFS root_squash: the mkdir process runs as the target user,
// so the directory is created already owned by that user on the NFS server.
//
// The parent directory must already exist and be writable by the target user.
// Callers should create the parent as root before calling this method.
// We use plain "mkdir" (not "mkdir -p") because the setuid process cannot
// traverse root-owned parent directories (e.g. the kubelet plugin tree).
func (r *realNFSOperations) MkdirAsUser(path string, perm os.FileMode, uid, gid uint32) error {
	// Ensure the parent exists (as root — this is a local/NFS path we can traverse).
	// On NFS with root_squash, root maps to 65534 and umask may reduce 0777 to 0755,
	// so we explicitly chmod after mkdir to ensure the target user can write.
	parent := filepath.Dir(path)
	if err := os.MkdirAll(parent, 0777); err != nil {
		return fmt.Errorf("mkdir parent %s: %w", parent, err)
	}
	if err := os.Chmod(parent, 0777); err != nil {
		return fmt.Errorf("chmod parent %s: %w", parent, err)
	}
	cmd := exec.Command("mkdir", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid},
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkdir %s as uid=%d gid=%d: %w (output: %s)", path, uid, gid, err, string(output))
	}
	// mkdir mode is affected by umask; set exact permissions with chmod.
	if err := os.Chmod(path, perm); err != nil {
		return fmt.Errorf("chmod %s: %w", path, err)
	}
	return nil
}

func (r *realNFSOperations) RemoveAll(path string) error {
	return os.RemoveAll(path)
}

func (r *realNFSOperations) Stat(path string) (os.FileInfo, error) {
	return os.Stat(path)
}

func (r *realNFSOperations) Chown(path string, uid, gid int) error {
	return os.Chown(path, uid, gid)
}

func (r *realNFSOperations) Chmod(path string, mode os.FileMode) error {
	return os.Chmod(path, mode)
}

func (r *realNFSOperations) CopyDir(src, dst string) error {
	cmd := exec.Command("cp", "-a", src, dst)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("cp -a %s %s: %w (output: %s)", src, dst, err, string(output))
	}
	return nil
}

func (r *realNFSOperations) SyncDir(ctx context.Context, src, dst string) error {
	return r.SyncDirWithOptions(ctx, src, dst, SyncOptions{})
}

func (r *realNFSOperations) SyncDirWithOptions(ctx context.Context, src, dst string, opts SyncOptions) error {
	args := []string{"-a", "--delete"}
	if opts.BandwidthLimitKBps > 0 {
		args = append(args, fmt.Sprintf("--bwlimit=%d", opts.BandwidthLimitKBps))
	}
	args = append(args, opts.ExtraArgs...)
	args = append(args, src+"/", dst+"/")

	cmd := exec.CommandContext(ctx, "rsync", args...)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rsync %s/ %s/: %w (output: %s)", src, dst, err, string(output))
	}
	return nil
}

func (r *realNFSOperations) WriteFile(path string, data []byte, perm os.FileMode) error {
	return os.WriteFile(path, data, perm)
}

func (r *realNFSOperations) ReadFile(path string) ([]byte, error) {
	return os.ReadFile(path)
}
