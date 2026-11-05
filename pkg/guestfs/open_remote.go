package guestfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/nbd"
	"k8s.io/klog/v2"
)

// RemoteOptions say which disk to open when it is read from a disk server.
type RemoteOptions struct {
	// Disk is the remote disk, read through a cache (cosdisk.Disk over a
	// RemoteSource). OpenRemote does not close it.
	Disk nbd.Export
	// Moment is the moment of the disk; the zero time leaves it out of Info.
	Moment time.Time
	// Socket is the path of the private socket where the disk is served to the
	// appliance.
	Socket string
	// Runner runs the libguestfs tools.
	Runner Runner
	// Crash is the Crash option of Guest.
	Crash bool
	// Check, if set, runs before every call of the tree (see Options.Check).
	Check func() error
	// Moments, if set, lists the moments of the disk instead of the one the disk
	// shows. A disk server that holds older restore points lists them here.
	Moments func() ([]string, error)
	// StartTimeout limits how long the appliance may take to list the file
	// systems; zero means 10 minutes.
	StartTimeout time.Duration
}

// OpenRemote is Open for a disk that a disk server holds. The disk is the
// backup of one moment in COS: it has no undo files and no copy in progress, so
// there is nothing to wait for. The appliance reads it through a local socket,
// as with a local disk.
func OpenRemote(ctx context.Context, o RemoteOptions) (*Opened, error) {
	if !socketRe.MatchString(o.Socket) {
		return nil, errors.New("the socket path is not a plain absolute path")
	}
	ln, err := nbd.Listen(o.Socket)
	if err != nil {
		return nil, fmt.Errorf("the disk server cannot start: %w", err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if serr := nbd.Serve(sctx, ln, o.Disk, nbd.Options{Name: "disk"}); serr != nil {
			klog.ErrorS(serr, "The disk server stopped")
		}
	}()
	op := &Opened{cancel: cancel, done: done, socket: o.Socket}
	op.Tree = New(sctx, Options{Drive: "nbd+unix:///?socket=" + o.Socket, Runner: o.Runner, Crash: o.Crash, Check: o.Check})
	op.Info = browse.Info{Guest: true}
	op.Moments = func() ([]string, error) { return nil, nil }
	if !o.Moment.IsZero() {
		name := o.Moment.UTC().Format(browse.MomentLayout)
		op.Info.Moment = name
		op.Moments = func() ([]string, error) { return []string{name}, nil }
	}
	if o.Moments != nil {
		op.Moments = o.Moments
	}
	lctx, lcancel := context.WithTimeout(sctx, orDuration(o.StartTimeout, 10*time.Minute))
	defer lcancel()
	if _, err = op.Tree.Filesystems(lctx); err != nil {
		timedOut := errors.Is(lctx.Err(), context.DeadlineExceeded)
		op.Close()
		_ = os.Remove(o.Socket)
		if timedOut {
			return nil, errors.New("the reader did not start within the time limit; without the KVM device it can be slow")
		}
		return nil, err
	}
	return op, nil
}
