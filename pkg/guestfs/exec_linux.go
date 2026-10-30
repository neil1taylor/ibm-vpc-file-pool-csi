package guestfs

import (
	"os/exec"
	"syscall"
)

// setProcessGroup puts the tool and the appliance it starts in one group, so
// that stopping the tool stops the appliance too.
func setProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
}
