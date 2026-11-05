package cosbrowse

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// This file is what the COS disk server needs of Kopia: which snapshot holds
// the VM disk, and a WebDAV server on that snapshot that answers range requests.
// The disk server never parses the guest's file system. See
// docs/specs/2026-10-29-vm-disks-in-cos.md, section 2.

// BackupPick is the export chosen for a VM disk.
type BackupPick struct {
	// Snapshot is the Kopia snapshot of the volume's backup folder (it holds
	// disk.img). Its ID has passed ValidOID.
	Snapshot Snapshot
	// Moment is the moment of the export, which is the moment of disk.img.
	Moment time.Time
	// Newest says that no later export exists.
	Newest bool
}

// ChooseBackup picks the backup snapshot of the volume to serve as a disk. The
// disk of an export is at the export's moment; earlier moments of a VM disk are
// undo files that are not served yet, so a time before the chosen export is
// refused in words.
func ChooseBackup(snaps []Snapshot, p Params) (BackupPick, error) {
	exports := exportsOf(snaps, p)
	if len(exports) == 0 {
		return BackupPick{}, &ReadError{Msg: fmt.Sprintf("no backup of volume %s was found in the COS bucket area of policy %s", p.Volume, p.Policy)}
	}
	idx := exportFor(exports, p.At)
	e := exports[idx]
	if !p.At.IsZero() && p.At.Before(e.moment) {
		return BackupPick{}, &ReadError{Msg: "the files inside a VM disk can be browsed in COS only at the restore point of an export, not at an older restore point in it"}
	}
	if !ValidOID(e.backup.ID) {
		return BackupPick{}, &ReadError{Msg: "the backup holds a damaged entry"}
	}
	return BackupPick{Snapshot: e.backup, Moment: e.moment, Newest: idx == len(exports)-1}, nil
}

// OpenKopia reads the keys and connects to the repository of the source, like
// Open does, and returns the Kopia runner for further commands.
func OpenKopia(ctx context.Context, src Source, opts OpenOptions) (*KopiaRepo, error) {
	if err := src.Validate(); err != nil {
		return nil, err
	}
	timeout := opts.Timeout
	if timeout <= 0 {
		timeout = 3 * time.Minute
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return newKopiaFor(ctx, src, opts)
}

// WebDAV is a running `kopia mount` of one snapshot with its WebDAV server.
type WebDAV struct {
	// URL is where disk.img of the snapshot is served, on the loopback address.
	URL  string
	stop func()
}

// NewWebDAV makes a WebDAV from an address and the function that stops it.
// StartWebDAV makes the real one; tests make their own.
func NewWebDAV(url string, stop func()) *WebDAV { return &WebDAV{URL: url, stop: stop} }

// Stop ends the Kopia child and waits for it.
func (w *WebDAV) Stop() { w.stop() }

// StartWebDAV runs `kopia mount SNAPSHOT '*' --webdav --no-browse`. The mount
// point '*' means that nothing is mounted in the operating system; only the
// WebDAV server runs, on a loopback port that Kopia chooses. find says which
// port that is (nil reads /proc/net/tcp); StartWebDAV waits up to wait for it.
func (k *KopiaRepo) StartWebDAV(ctx context.Context, snapshotID string, find func() (int, bool), wait time.Duration) (*WebDAV, error) {
	if !ValidOID(snapshotID) {
		return nil, &ReadError{Msg: "the backup holds a damaged entry"}
	}
	if find == nil {
		find = procListenPort
	}
	cctx, cancel := context.WithCancel(ctx)
	cmd := k.command(cctx, "mount", snapshotID, "*", "--webdav", "--no-browse")
	var stderr limitBuffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, k.failure("mount", err, "")
	}
	done := make(chan struct{})
	w := &WebDAV{stop: func() { cancel(); <-done }}
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	deadline := time.Now().Add(wait)
	for {
		if port, ok := find(); ok {
			w.URL = "http://127.0.0.1:" + strconv.Itoa(port) + "/disk.img"
			return w, nil
		}
		select {
		case <-done:
			return nil, k.failure("mount", fmt.Errorf("the reader of the backup ended at once"), stderr.String())
		case <-ctx.Done():
			w.Stop()
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			w.Stop()
			return nil, k.failure("mount", fmt.Errorf("the reader of the backup did not start in time"), stderr.String())
		}
	}
}

// procListenPort reads the first listening TCP port on 127.0.0.1 from /proc.
// In the disk server pod only the Kopia child listens there.
func procListenPort() (int, bool) {
	b, err := os.ReadFile("/proc/net/tcp")
	if err != nil {
		return 0, false
	}
	return parseListenPort(string(b))
}

// parseListenPort finds the first line in the state LISTEN (0A) whose local
// address is 127.0.0.1 (0100007F in /proc order).
func parseListenPort(proc string) (int, bool) {
	for _, line := range strings.Split(proc, "\n") {
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "0A" {
			continue
		}
		addr, port, ok := strings.Cut(f[1], ":")
		if !ok || addr != "0100007F" {
			continue
		}
		n, err := strconv.ParseUint(port, 16, 16)
		if err != nil || n == 0 {
			continue
		}
		return int(n), true
	}
	return 0, false
}
