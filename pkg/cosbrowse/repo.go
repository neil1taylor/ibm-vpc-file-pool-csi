package cosbrowse

import (
	"context"
	"io"
	"regexp"
	"time"
)

// Repo is the part of Kopia that browsing needs, and nothing else: the list of
// snapshots, and the content of one object. Tests use a fake. The Kopia
// program (KopiaRepo) is the real one.
type Repo interface {
	// Snapshots lists every snapshot in the repository.
	Snapshots(ctx context.Context) ([]Snapshot, error)
	// Object streams the content of an object. The caller closes it. A folder is
	// a JSON manifest; a file is its bytes; a link is its target. oid must have
	// passed ValidOID.
	Object(ctx context.Context, oid string) (io.ReadCloser, error)
}

// Snapshot is one Kopia snapshot, as `kopia snapshot list --json` reports it.
type Snapshot struct {
	ID string
	// SourcePath is the folder that was backed up, as the export saw it.
	SourcePath string
	// Volume and Moment are the tags the export set. Moment is the zero time if
	// the tag is missing or not a time.
	Volume string
	Moment time.Time
	// Start is when the snapshot was made.
	Start time.Time
	// Root is the object of the snapshot's top folder, and RootMode, RootUID,
	// RootGID and RootMTime say who owns it.
	Root      string
	RootMode  uint32
	RootUID   uint32
	RootGID   uint32
	RootMTime time.Time
}

// oidPattern is the shape of a Kopia object ID: letters and digits. An ID comes
// out of the repository, which a tenant's backup cannot write to, but it is
// still checked before it is the argument of a command: it must never start
// with a dash or hold a space.
var oidPattern = regexp.MustCompile(`^[A-Za-z0-9]{1,512}$`)

// ValidOID reports whether s may be given to Kopia as an object ID.
func ValidOID(s string) bool { return oidPattern.MatchString(s) }
