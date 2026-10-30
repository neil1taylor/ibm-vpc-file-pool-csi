//go:build !linux

package guestfs

import "os/exec"

func setProcessGroup(cmd *exec.Cmd) {}
