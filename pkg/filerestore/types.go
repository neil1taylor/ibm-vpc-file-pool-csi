// Package filerestore copies chosen files and folders out of a backup tree into
// a destination, for the file restore Job (issue #92). A backup is data that
// someone else controlled, and so is the destination (an existing claim holds
// whatever its users put there). So no link is followed on either side, no name
// is used as a shell word, and nothing is written outside the destination root.
// See docs/specs/2026-10-22-backup-file-restore.md.
package filerestore

import (
	"errors"
	"fmt"
	"io"
	"time"
)

// Owner is the owner given to everything that is restored. A nil field keeps
// the owner the file had in the backup.
type Owner struct {
	UID, GID *int64
}

// Meta is what is set on an entry once its content is written.
type Meta struct {
	// Mode is the permission bits. setuid and setgid are always removed before
	// it reaches the destination.
	Mode  uint32
	MTime time.Time
	// UID and GID are the owner to set; nil leaves the owner as created.
	UID, GID *int64
}

// DestFile is a new regular file in the destination.
type DestFile interface {
	io.Writer
	// Finish sets times, mode and owner, in that order, and closes the file.
	Finish(Meta) error
	// Close closes the file without finishing it.
	Close() error
}

// Dest is a destination root. Paths are lists of names below the root; the
// parent of every path is a folder made earlier with Mkdir. A Dest never
// follows a link and never overwrites: a name that exists is an error.
type Dest interface {
	Mkdir(path [][]byte) error
	// FinishDir sets times, mode and owner of a folder made with Mkdir (or of
	// the root, for the empty path) and releases it. It is called for a folder
	// after everything inside it is written.
	FinishDir(path [][]byte, m Meta) error
	Create(path [][]byte) (DestFile, error)
	Symlink(path [][]byte, target []byte, m Meta) error
	// Free reports the bytes that can still be written to the destination.
	Free() (int64, error)
	// Abort removes everything the Dest wrote, and the root itself if the Dest
	// made it. It is the clean-up after a failure.
	Abort() error
}

// Limits and sizes.
const (
	// DefaultMaxBytes is the most one restore writes (200 GiB).
	DefaultMaxBytes int64 = 200 << 30
	copyBuffer            = 1 << 20
)

// Result counts what a restore wrote or left out.
type Result struct {
	Files      int   `json:"files"`
	Dirs       int   `json:"dirs"`
	Links      int   `json:"links"`
	Skipped    int   `json:"skipped"`    // devices, pipes and sockets
	Unreadable int   `json:"unreadable"` // entries the helper could not read
	Bytes      int64 `json:"bytes"`
}

// NoSpaceError says the chosen files do not fit.
type NoSpaceError struct {
	Need, Have int64
}

func (e *NoSpaceError) Error() string {
	return fmt.Sprintf("not enough space: the chosen files need %d bytes and %d bytes can be written", e.Need, e.Have)
}

// ErrNothingToRestore means no path was given.
var ErrNothingToRestore = errors.New("no path to restore")

// ErrVMDiskClaim means the claim holds a VM disk. A VM disk is never a target.
var ErrVMDiskClaim = errors.New("this claim holds a VM disk (disk.img); files are never restored into one")

// ErrClaimNotReady means the claim has no folder on the share yet. The driver
// makes it, with the claim's owner and mode, when a pod first uses the claim.
var ErrClaimNotReady = errors.New("the claim has no data folder yet; start a pod that uses the claim, then ask again")
