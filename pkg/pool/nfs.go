package pool

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

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
func (r *realNFSOperations) MkdirAsUser(path string, perm os.FileMode, uid, gid uint32) error {
	cmd := exec.Command("mkdir", "-p", path)
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Credential: &syscall.Credential{Uid: uid, Gid: gid},
	}
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("mkdir -p %s as uid=%d gid=%d: %w (output: %s)", path, uid, gid, err, string(output))
	}
	// mkdir -p mode is affected by umask; set exact permissions with chmod.
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
	cmd := exec.CommandContext(ctx, "rsync", "-a", "--delete", src+"/", dst+"/")
	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("rsync %s/ %s/: %w (output: %s)", src, dst, err, string(output))
	}
	return nil
}
