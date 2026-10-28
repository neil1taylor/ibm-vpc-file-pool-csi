package replication

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// leaseFileName holds the time until which a commit may still be changing a
// disk in this folder. Failover waits for it. A commit that stops without
// removing it (a receiver crash) stops blocking failover when it expires.
const (
	leaseFileName = ".repl-lease"
	leaseTTL      = time.Hour
)

// DiskState is what a far-site VM disk shows, for failover.
type DiskState struct {
	// Moment is the moment the disk shows now; MomentKnown is false if
	// neither a restore point nor the moment file says.
	Moment      time.Time
	MomentKnown bool
	// VMGroup is "namespace/name" of the VM that owns the disk; empty if the
	// receiver never wrote this folder.
	VMGroup string
	// RestorePoints are the moments the disk can be rewound to, oldest
	// first, not counting Moment. A restore point dated at the epoch stands
	// for a moment that was not known; it is left out.
	RestorePoints []time.Time
	// Unfinished is true if a copy has written an undo file that is not yet a
	// restore point: a copy is being applied, or stopped half way. The disk
	// may then be half changed.
	Unfinished bool
	// Modified is true if something wrote to the disk after the last copy,
	// for example a VM that booted from it. Its restore points then no longer
	// fit it. A copy that stopped half way does not count.
	Modified bool
}

// ReadDiskState reads the state of the disk. It does not change anything. A
// copy that stopped half way does not affect it: only completed copies count.
func ReadDiskState(disk string) (DiskState, error) {
	v, err := openVolume(disk)
	if err != nil {
		return DiskState{}, err
	}
	defer v.close()
	files, err := appliedUndo(v)
	if err != nil {
		return DiskState{}, err
	}
	return v.stateFrom(files)
}

// stateFrom is ReadDiskState for the restore point undo files already listed.
func (v *volume) stateFrom(files []undoFile) (DiskState, error) {
	var st DiskState
	var err error
	if st.Moment, st.MomentKnown, err = v.momentFrom(files); err != nil {
		return st, err
	}
	mf, err := v.readMomentFile()
	if err != nil {
		return st, err
	}
	if st.Unfinished, err = v.hasUnfinishedCopy(); err != nil {
		return st, err
	}
	if mf != nil {
		st.VMGroup = mf.VMGroup
		if fi, derr := v.diskInfo(); derr == nil && mf.DiskMtimeNs != 0 && fi.ModTime().UnixNano() != mf.DiskMtimeNs {
			st.Modified = !st.Unfinished
		}
	}
	for _, u := range files {
		if u.hdr.from.Unix() > 0 {
			st.RestorePoints = append(st.RestorePoints, u.hdr.from)
		}
	}
	return st, nil
}

// LeaseActive reports whether a commit may be changing a disk in dir at now.
func LeaseActive(dir string, now time.Time) (bool, error) {
	v, err := openVolume(filepath.Join(dir, "x"))
	if err != nil {
		return false, err
	}
	defer v.close()
	f, err := v.root.Open(leaseFileName)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 256))
	if err != nil {
		return false, err
	}
	exp, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(string(b)))
	if err != nil {
		return true, nil // a lease we cannot read is treated as live
	}
	return now.Before(exp), nil
}

func (v *volume) writeLeaseFile(expires time.Time) error {
	f, err := v.root.OpenFile(leaseFileName, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o660)
	if err != nil {
		return err
	}
	_, err = f.WriteString(expires.UTC().Format(time.RFC3339Nano))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

func (v *volume) removeLeaseFile() {
	_ = v.root.Remove(leaseFileName)
}

func writeLease(dir string, expires time.Time) error {
	v, err := openVolume(filepath.Join(dir, "x"))
	if err != nil {
		return err
	}
	defer v.close()
	return v.writeLeaseFile(expires)
}

// WriteMoment records the moment of a disk and its VM in dir, as the receiver
// does after a commit. The receiver and tests use it.
func WriteMoment(dir string, t time.Time, vmGroup string) error {
	return writeMoment(dir, t, vmGroup)
}

// hasUnfinishedCopy reports whether a copy changed the disk and did not end:
// its undo file is complete but not marked applied.
func (v *volume) hasUnfinishedCopy() (bool, error) {
	entries, err := v.undoEntries()
	if err != nil {
		return false, err
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), undoReady) {
			return true, nil
		}
	}
	return false, nil
}
