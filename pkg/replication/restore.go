package replication

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// ErrCopyInProgress means a copy is being written to the backup disk, or
// stopped half way, so the disk may be half changed. Try again after the next
// copy has ended. A restore never rolls a copy back, because the copy may be
// running.
var ErrCopyInProgress = errors.New("a copy is being written to the backup disk; try again when it has ended")

// RestoreResult says which moment RestoreDisk wrote.
type RestoreResult struct {
	// Moment is the moment of the disk that was written. It is the zero time
	// for a disk whose moment is not known.
	Moment time.Time
	// Latest is true if it is the newest moment, a plain copy of the disk.
	Latest bool
}

// RestoreDisk writes to out the backup disk as it was at the newest moment at
// or before at. The zero time asks for the latest. It does not change the
// backup. out must not exist, and it is never removed unless this call made
// it. A backup disk that is a link is refused.
func RestoreDisk(disk string, at time.Time, out string) (RestoreResult, error) {
	if _, err := os.Lstat(out); err == nil {
		return RestoreResult{}, fmt.Errorf("%s already exists: %w", out, os.ErrExist)
	}
	st, err := ReadDiskState(disk)
	if err != nil {
		return RestoreResult{}, err
	}
	if st.Unfinished {
		return RestoreResult{}, ErrCopyInProgress
	}
	if st.Modified {
		return RestoreResult{}, errors.New("the backup disk changed after its last copy; its restore points no longer fit it")
	}
	// A disk with no restore point and no moment file (the first direct copy)
	// is the only moment there is. Its time is not known.
	if !st.MomentKnown || at.IsZero() || !at.Before(st.Moment) {
		if err := copySparse(disk, out); err != nil {
			return RestoreResult{}, err
		}
		return RestoreResult{Moment: st.Moment, Latest: true}, nil // the zero time if not known
	}
	// The restore points are oldest first: the last one at or before at is the
	// nearest.
	var chosen time.Time
	for _, p := range st.RestorePoints {
		if !p.After(at) {
			chosen = p
		}
	}
	if chosen.IsZero() {
		oldest := st.Moment
		if len(st.RestorePoints) > 0 {
			oldest = st.RestorePoints[0]
		}
		return RestoreResult{}, fmt.Errorf("no backup at or before %s: the oldest is %s",
			at.UTC().Format(time.RFC3339), oldest.UTC().Format(time.RFC3339))
	}
	if err := Rewind(disk, chosen, out); err != nil {
		return RestoreResult{}, err
	}
	return RestoreResult{Moment: chosen}, nil
}

// copySparse copies the backup disk to the new file dst. A block of zeros
// becomes a hole, so a mostly empty disk stays small. It reads the disk
// through the folder handle that refuses links, and checks that it is a
// regular file on the handle it reads.
func copySparse(disk, dst string) (err error) {
	v, err := openVolume(disk)
	if err != nil {
		return err
	}
	defer v.close()
	in, err := v.openDisk(os.O_RDONLY)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", disk)
	}
	f, err := os.OpenFile(dst, os.O_RDWR|os.O_CREATE|os.O_EXCL, diskFileMode) // #nosec G304 -- path chosen by the caller
	if err != nil {
		return err // dst exists or cannot be made: it is not ours to remove
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			_ = os.Remove(dst)
		}
	}()
	const block = 1 << 20
	buf := make([]byte, block)
	zero := make([]byte, block)
	var off int64
	for {
		n, rerr := io.ReadFull(in, buf)
		if n > 0 {
			if !bytes.Equal(buf[:n], zero[:n]) {
				if _, werr := f.WriteAt(buf[:n], off); werr != nil {
					return werr
				}
			}
			off += int64(n)
		}
		if rerr == io.EOF || rerr == io.ErrUnexpectedEOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	return f.Truncate(fi.Size())
}
