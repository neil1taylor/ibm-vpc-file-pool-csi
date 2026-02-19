package driver

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// mkdirAsUserFunc creates a directory as the specified UID/GID by spawning
// mkdir with SysProcAttr.Credential. This bypasses NFS root_squash: the
// directory is created already owned by the target user on the NFS server.
//
// This is a package-level variable so tests can override it.
var mkdirAsUserFunc = mkdirAsUser

func mkdirAsUser(path string, perm os.FileMode, uid, gid uint32) error {
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
