package guestfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/nbd"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
	"k8s.io/klog/v2"
)

// OpenOptions say which VM disk to open and how.
type OpenOptions struct {
	// Disk is the backup disk file, like /latest/disk.img, on a read-only mount.
	Disk string
	// At picks the moment; the zero time is the latest. A moment named in whole
	// seconds picks the newest restore point up to the end of that second (see
	// AtEndOfSecond).
	At time.Time
	// Socket is the path of the private socket where the disk is served.
	Socket string
	// Runner runs the libguestfs tools.
	Runner Runner
	// Crash is the Crash option of Guest.
	Crash bool
	// StartTimeout limits how long the appliance may take to list the file
	// systems; zero means 10 minutes.
	StartTimeout time.Duration
	// Wait is how long to wait for a backup copy that is being written to end;
	// zero means 5 minutes. Poll is the pause between looks (zero: 5 seconds).
	// Sleep replaces time.Sleep in tests.
	Wait, Poll time.Duration
	Sleep      func(time.Duration)
}

// Opened is a VM disk ready to browse.
type Opened struct {
	Tree *Guest
	View *replication.DiskView
	Info browse.Info
	// Moments lists the moments of the disk, like the helper's /v1/moments.
	Moments func() ([]string, error)

	cancel context.CancelFunc
	done   chan struct{}
	socket string
}

// Close stops the disk server and removes its socket.
func (o *Opened) Close() {
	o.cancel()
	<-o.done
	_ = o.View.Close()
	_ = os.Remove(o.socket)
}

var socketRe = regexp.MustCompile(`^/[A-Za-z0-9._/-]{1,200}$`)

// AtEndOfSecond turns a moment named in whole seconds into the time to pass to
// the disk view. Restore points keep nanoseconds but moments are named in
// seconds, so "10:00:00" must pick a restore point at 10:00:00.5.
func AtEndOfSecond(t time.Time) time.Time {
	if t.IsZero() {
		return t
	}
	return t.Truncate(time.Second).Add(time.Second - time.Nanosecond)
}

// Moments lists the moments of a VM disk in the form of a moment folder
// name, oldest first: its restore points, then the moment the disk shows.
func Moments(disk string) ([]string, error) {
	st, err := replication.ReadDiskState(disk)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, p := range st.RestorePoints {
		out = append(out, p.UTC().Format(browse.MomentLayout))
	}
	if st.MomentKnown {
		out = append(out, st.Moment.UTC().Format(browse.MomentLayout))
	}
	return out, nil
}

// Open shows the disk as it was at the moment, to the appliance, and lists its
// file systems. It waits for a backup copy that is being written to end. It
// fails in words when the disk cannot be read, so the session can show why.
func Open(ctx context.Context, o OpenOptions) (*Opened, error) {
	if !socketRe.MatchString(o.Socket) {
		return nil, errors.New("the socket path is not a plain absolute path")
	}
	sleep := o.Sleep
	if sleep == nil {
		sleep = time.Sleep
	}
	wait, poll := orDuration(o.Wait, 5*time.Minute), orDuration(o.Poll, 5*time.Second)
	view, err := replication.OpenDiskView(o.Disk, AtEndOfSecond(o.At))
	for waited := time.Duration(0); errors.Is(err, replication.ErrCopyInProgress) && waited < wait; waited += poll {
		sleep(poll)
		view, err = replication.OpenDiskView(o.Disk, AtEndOfSecond(o.At))
	}
	if err != nil {
		return nil, fmt.Errorf("the backup disk cannot be opened: %w", err)
	}
	ln, err := nbd.Listen(o.Socket)
	if err != nil {
		_ = view.Close()
		return nil, fmt.Errorf("the disk server cannot start: %w", err)
	}
	sctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		if serr := nbd.Serve(sctx, ln, view, nbd.Options{Name: "disk"}); serr != nil {
			klog.ErrorS(serr, "The disk server stopped")
		}
	}()
	op := &Opened{View: view, cancel: cancel, done: done, socket: o.Socket}
	op.Tree = New(sctx, Options{Drive: "nbd+unix:///?socket=" + o.Socket, Runner: o.Runner, Crash: o.Crash, Check: view.Err})
	op.Moments = func() ([]string, error) { return Moments(o.Disk) }
	lctx, lcancel := context.WithTimeout(sctx, orDuration(o.StartTimeout, 10*time.Minute))
	defer lcancel()
	if _, err = op.Tree.Filesystems(lctx); err != nil {
		timedOut := errors.Is(lctx.Err(), context.DeadlineExceeded)
		op.Close()
		if timedOut {
			return nil, errors.New("the reader did not start within the time limit; without the KVM device it can be slow")
		}
		return nil, err
	}
	op.Info = browse.Info{Latest: view.Latest(), Guest: true}
	if view.MomentKnown() {
		op.Info.Moment = view.Moment().UTC().Format(browse.MomentLayout)
	}
	return op, nil
}

func orDuration(d, def time.Duration) time.Duration {
	if d > 0 {
		return d
	}
	return def
}
