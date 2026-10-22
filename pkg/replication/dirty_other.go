//go:build !linux

package replication

import "os"

// dropCache does nothing off Linux.
func dropCache(*os.File) {}
