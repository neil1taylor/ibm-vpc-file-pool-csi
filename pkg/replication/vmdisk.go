package replication

import (
	"context"
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

// openStaging opens a staging file as a qcow2 image. It refuses a name that
// is not a staging file name, so nothing else can be read or deleted through
// it. O_NOFOLLOW and the regular-file check stop a staging file that was
// replaced by a link to some other file, such as another volume's disk.
func openStaging(path string) (*os.File, *qcow2.Image, error) {
	if !stagingNameRe.MatchString(filepath.Base(path)) {
		return nil, nil, fmt.Errorf("%q does not have a staging file name", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0) // #nosec G304 -- path comes from the controller's Job spec
	if err != nil {
		return nil, nil, fmt.Errorf("opening staging file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("reading staging file size: %w", err)
	}
	if !fi.Mode().IsRegular() {
		_ = f.Close()
		return nil, nil, fmt.Errorf("staging file %s is not a regular file", path)
	}
	img, err := qcow2.Open(f, fi.Size())
	if err != nil {
		_ = f.Close()
		return nil, nil, fmt.Errorf("staging file %s: %w", path, err)
	}
	return f, img, nil
}

// SendVMDiskFile sends the VM disk copy in stagingPath to a receiver with
// SendVMDisk (a.Src is set from the file), then deletes the staging file. On
// error it keeps the file, so the next try can send it.
func SendVMDiskFile(ctx context.Context, c *SyncClient, stagingPath string, a VMSendArgs) (VMSendResult, error) {
	f, img, err := openStaging(stagingPath)
	if err != nil {
		return VMSendResult{}, err
	}
	defer func() { _ = f.Close() }()
	a.Src = img
	res, err := c.SendVMDisk(ctx, a)
	if err != nil {
		return res, err
	}
	if err = os.Remove(stagingPath); err != nil {
		return res, fmt.Errorf("deleting staging file: %w", err)
	}
	return res, nil
}

// ApplyVMDisk applies the VM disk copy in stagingPath to diskPath with
// ApplyCopy, then deletes the staging file. On error it keeps the staging
// file, so the next try can apply it. It refuses a stagingPath whose name is
// not a staging file name, so it can never delete anything else.
func ApplyVMDisk(stagingPath, diskPath string, opts ApplyOptions) (VMDiskResult, error) {
	var res VMDiskResult
	f, img, err := openStaging(stagingPath)
	if err != nil {
		return res, err
	}
	defer func() { _ = f.Close() }()

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
