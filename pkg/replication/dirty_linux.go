//go:build linux

package replication

import (
	"os"

	"golang.org/x/sys/unix"
)

// dropCache tells the kernel it may drop the cached pages of a file that was
// just synced. It is a hint; a failure is not an error.
func dropCache(f *os.File) {
	_ = unix.Fadvise(int(f.Fd()), 0, 0, unix.FADV_DONTNEED) // #nosec G115 -- a file descriptor
}
