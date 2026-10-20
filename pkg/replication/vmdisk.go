package replication

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
)

// newDiskSuffix names the file a new disk image is written to before it
// becomes the disk.
const newDiskSuffix = ".repl-new"

// diskFileMode is the mode KubeVirt gives a disk image.
const diskFileMode = 0o660

// stagingNameRe matches the name of a staging file that pkg/vmcopy makes.
var stagingNameRe = regexp.MustCompile(`^\.repl-staging-[a-z0-9]{1,16}\.qcow2$`)

// VMDiskResult says what ApplyVMDisk did.
type VMDiskResult struct {
	ApplyResult
	// RestorePoints is how many restore points the far-site disk now has,
	// and OldestRestorePoint the oldest of them.
	RestorePoints      int
	OldestRestorePoint *time.Time
}

// ApplyVMDisk applies the VM disk copy in stagingPath to diskPath with
// ApplyCopy, then deletes the staging file. On error it keeps the staging
// file, so the next try can apply it. It refuses a stagingPath whose name is
// not a staging file name, so it can never delete anything else.
func ApplyVMDisk(stagingPath, diskPath string, opts ApplyOptions) (VMDiskResult, error) {
	var res VMDiskResult
	if !stagingNameRe.MatchString(filepath.Base(stagingPath)) {
		return res, fmt.Errorf("%q does not have a staging file name", stagingPath)
	}
	// O_NOFOLLOW and the regular-file check stop a staging file that was
	// replaced by a link to some other file, such as another volume's disk.
	src, err := os.OpenFile(stagingPath, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- path comes from the controller's Job spec
	if err != nil {
		return res, fmt.Errorf("opening staging file: %w", err)
	}
	defer func() { _ = src.Close() }()
	fi, err := src.Stat()
	if err != nil {
		return res, fmt.Errorf("reading staging file size: %w", err)
	}
	if !fi.Mode().IsRegular() {
		return res, fmt.Errorf("staging file %s is not a regular file", stagingPath)
	}
	img, err := qcow2.Open(src, fi.Size())
	if err != nil {
		return res, fmt.Errorf("staging file %s: %w", stagingPath, err)
	}

	if res.ApplyResult, err = ApplyCopy(img, diskPath, opts); err != nil {
		return res, err
	}
	if err = os.Remove(stagingPath); err != nil {
		return res, fmt.Errorf("deleting staging file: %w", err)
	}
	points, err := RestorePoints(diskPath)
	if err != nil {
		return res, err
	}
	res.RestorePoints = len(points)
	if len(points) > 0 {
		res.OldestRestorePoint = &points[0]
	}
	return res, nil
}
