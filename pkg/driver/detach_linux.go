//go:build linux

package driver

import "golang.org/x/sys/unix"

// detachMount takes the mount at path out of the mount tree at once, without
// waiting for its NFS server. The kernel frees it when nothing uses it.
func detachMount(path string) error {
	return unix.Unmount(path, unix.MNT_DETACH)
}
