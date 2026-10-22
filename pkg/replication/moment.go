package replication

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// momentFileName holds the moment a far-site disk shows, for a disk that has
// no restore point yet, and the VM that owns it.
const momentFileName = ".repl-moment"

type momentFile struct {
	Moment  time.Time `json:"moment"`
	VMGroup string    `json:"vmGroup,omitempty"`
	// DiskMtimeNs and DiskSize seal the disk as the last copy left it. A
	// different modification time later means something else wrote to the
	// disk, for example a VM that booted from it.
	DiskMtimeNs int64 `json:"diskMtimeNs,omitempty"`
	DiskSize    int64 `json:"diskSize,omitempty"`
}

// moment returns the moment the disk shows now. The newest restore point
// knows it, because its copy ended there. A disk with no restore point
// relies on the moment file. ok is false if the moment is not known.
func (v *volume) moment() (t time.Time, ok bool, err error) {
	files, err := appliedUndo(v)
	if err != nil {
		return t, false, err
	}
	if len(files) > 0 {
		return files[len(files)-1].hdr.to, true, nil
	}
	mf, err := v.readMomentFile()
	if err != nil || mf == nil || mf.Moment.IsZero() {
		return t, false, err
	}
	return mf.Moment, true, nil
}

func (v *volume) readMomentFile() (*momentFile, error) {
	f, err := v.root.Open(momentFileName)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return nil, err
	}
	var mf momentFile
	if err = json.Unmarshal(b, &mf); err != nil {
		return nil, fmt.Errorf("reading %s: %w", momentFileName, err)
	}
	return &mf, nil
}

func (v *volume) writeMomentFile(mf momentFile) error {
	if fi, err := v.diskInfo(); err == nil {
		mf.DiskMtimeNs, mf.DiskSize = fi.ModTime().UnixNano(), fi.Size()
	}
	b, err := json.Marshal(mf)
	if err != nil {
		return err
	}
	tmp := momentFileName + ".tmp"
	f, err := v.root.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o660)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if err = v.root.Rename(tmp, momentFileName); err != nil {
		return err
	}
	return v.sync(".")
}

// CurrentMoment returns the moment the far-site disk shows. ok is false if the
// disk has no restore point and no moment file.
func CurrentMoment(disk string) (t time.Time, ok bool, err error) {
	v, err := openVolume(disk)
	if err != nil {
		return t, false, err
	}
	defer v.close()
	return v.moment()
}

// reseal records the disk's modification time again, after a roll back put it
// back to what the last copy left.
func (v *volume) reseal() error {
	mf, err := v.readMomentFile()
	if err != nil || mf == nil {
		return err
	}
	return v.writeMomentFile(*mf)
}

// writeMoment records the moment of a disk in the folder that holds it, and
// seals the disk.
func writeMoment(dir string, t time.Time, vmGroup string) error {
	v, err := openVolume(filepath.Join(dir, defaultDiskName))
	if err != nil {
		return err
	}
	defer v.close()
	return v.writeMomentFile(momentFile{Moment: t, VMGroup: vmGroup})
}
