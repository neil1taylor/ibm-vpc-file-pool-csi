package replication

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
)

// newDiskSuffix names the file a new disk image is written to before it
// replaces the old one.
const newDiskSuffix = ".repl-new"

// diskFileMode is the mode KubeVirt gives a disk image.
const diskFileMode = 0o660

// ReplaceDiskFromStaging writes the full VM disk copy in a qcow2 staging file
// to diskPath as a raw disk image. It writes to a new file next to diskPath and
// renames it over diskPath only when the write is complete, so a reader sees
// either the old disk or the new one, never a mix. On error the old disk stays
// and the new file is removed. It returns the number of data bytes written.
func ReplaceDiskFromStaging(stagingPath, diskPath string) (int64, error) {
	src, err := os.Open(stagingPath) // #nosec G304 -- path comes from the controller's Job spec
	if err != nil {
		return 0, fmt.Errorf("opening staging file: %w", err)
	}
	defer func() { _ = src.Close() }()

	fi, err := src.Stat()
	if err != nil {
		return 0, fmt.Errorf("reading staging file size: %w", err)
	}
	img, err := qcow2.Open(src, fi.Size())
	if err != nil {
		return 0, fmt.Errorf("staging file %s: %w", stagingPath, err)
	}

	tmp := diskPath + newDiskSuffix
	if err = os.Remove(tmp); err != nil && !errors.Is(err, os.ErrNotExist) {
		return 0, fmt.Errorf("removing leftover %s: %w", tmp, err)
	}
	n, err := writeNewDisk(img, tmp)
	if err != nil {
		_ = os.Remove(tmp)
		return n, err
	}
	if err := os.Rename(tmp, diskPath); err != nil {
		_ = os.Remove(tmp)
		return n, fmt.Errorf("replacing %s: %w", diskPath, err)
	}
	if err := syncDir(filepath.Dir(diskPath)); err != nil {
		return n, err
	}
	return n, nil
}

func writeNewDisk(img *qcow2.Image, path string) (int64, error) {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, diskFileMode) // #nosec G304 -- path comes from the controller's Job spec
	if err != nil {
		return 0, fmt.Errorf("creating %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	if err = f.Chmod(diskFileMode); err != nil {
		return 0, fmt.Errorf("setting mode of %s: %w", path, err)
	}
	if err = f.Truncate(img.VirtualSize); err != nil {
		return 0, fmt.Errorf("sizing %s: %w", path, err)
	}
	n, err := img.WriteRaw(f, false)
	if err != nil {
		return n, fmt.Errorf("writing %s: %w", path, err)
	}
	if err = f.Sync(); err != nil {
		return n, fmt.Errorf("syncing %s: %w", path, err)
	}
	if err = f.Close(); err != nil {
		return n, fmt.Errorf("closing %s: %w", path, err)
	}
	return n, nil
}

func syncDir(dir string) error {
	d, err := os.Open(dir) // #nosec G304 -- parent of the disk path
	if err != nil {
		return fmt.Errorf("opening %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()
	if err := d.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}
	return nil
}
