package main

import (
	"context"
	"path/filepath"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/guestfs"
)

const (
	consistencyFS    = "FileSystemConsistent"
	consistencyCrash = "CrashConsistent"

	crashNotice = "This copy was taken while the VM was running. The newest changes may be missing."
)

// guestSession is a VM disk opened for browsing.
type guestSession struct {
	*guestfs.Opened
	consistency string
}

// openGuestSession shows the disk in root (disk.img) to the appliance and lists
// its file systems. It takes the disk by the name disk.img only, as the backup
// of a VM disk always has.
func openGuestSession(ctx context.Context, root string, at time.Time, socket, appliance, consistency string) (*guestSession, error) {
	opened, err := guestfs.Open(ctx, guestfs.OpenOptions{
		Disk:   filepath.Join(root, "disk.img"),
		At:     at,
		Socket: socket,
		Runner: newToolRunner(appliance, guestfs.HasKVM()),
		Crash:  consistency == consistencyCrash,
	})
	if err != nil {
		return nil, err
	}
	return &guestSession{Opened: opened, consistency: consistency}, nil
}

// info is what the session reports about itself, with the notices as they are
// now: a file system that had to be read without its journal adds one.
func (g *guestSession) info() browse.Info {
	info := g.Opened.Info
	info.Consistency = g.consistency
	if g.consistency == consistencyCrash {
		info.Notices = append(info.Notices, crashNotice)
	}
	info.Notices = append(info.Notices, g.Opened.Tree.Notices()...)
	return info
}
