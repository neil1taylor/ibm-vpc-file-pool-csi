package filerestore

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/guestfs"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
)

// The three things the restore Job can do.
const (
	// ModeMeasure counts the bytes of the chosen paths and writes nothing. The
	// controller uses it to size a new claim.
	ModeMeasure = "measure"
	// ModeNewClaim writes into the volume folder of a new claim.
	ModeNewClaim = "new-claim"
	// ModeFolder writes into a new folder of an existing claim.
	ModeFolder = "folder"
)

// JobSpec is what the controller tells the restore Job. It holds no user text
// except names that were checked: the paths come in a separate file.
type JobSpec struct {
	Mode string `json:"mode"`
	// Latest and Moments are where the backup is mounted. At picks a moment; it
	// is empty for the latest copy.
	Latest  string `json:"latest,omitempty"`
	Moments string `json:"moments,omitempty"`
	At      string `json:"at,omitempty"`
	// COS reads the backup from the Kopia repository in COS instead. Latest and
	// Moments are then empty. The keys are Secrets mounted as files at the fixed
	// places of package cosbrowse; none is in the spec.
	COS *cosbrowse.Source `json:"cos,omitempty"`
	// Guest says the backup is a VM disk: Latest holds disk.img, and the Job
	// reads the file systems inside it through a read-only appliance. The first
	// name of each path is then the file system (sda1, rhel-root). The earlier
	// moments of a VM disk are undo files next to the disk; only a disk that was
	// copied as plain files (a stopped VM) has a Moments folder.
	Guest *GuestSource `json:"guest,omitempty"`
	// Writer, if set, is the private socket of a separate writer container. The
	// Job then only reads: every write goes to the writer, which holds the target
	// and the capabilities (see ServeWriter). Dest, Volume, Staging and Policy
	// are then the writer's, and are not set here. Used when the backup is a VM
	// disk, whose file systems are parsed by this Job.
	Writer string `json:"writer,omitempty"`
	// Dest is the share that holds the claim.
	Dest string `json:"dest,omitempty"`
	// Volume is the new claim's folder below Dest, like pvcs/pvc-...
	Volume string `json:"volume,omitempty"`
	// Staging is the hidden folder of an existing claim that is written first.
	Staging string `json:"staging,omitempty"`
	// Policy names the restored folder.
	Policy string `json:"policy,omitempty"`
	// CapacityBytes is the size of the claim.
	CapacityBytes int64 `json:"capacityBytes,omitempty"`
	// MaxBytes limits one restore; zero means the default.
	MaxBytes int64 `json:"maxBytes,omitempty"`
	// UID and GID are the owner of everything restored; nil keeps the backup's.
	UID *int64 `json:"uid,omitempty"`
	GID *int64 `json:"gid,omitempty"`
	// Permissions is the mode of the restored folder, like 0777.
	Permissions string `json:"permissions,omitempty"`
}

// GuestSource says how to read a VM disk. The Job runs in the guest helper image.
type GuestSource struct {
	// Socket is a private socket path where the Job serves the disk to the
	// appliance, like /tmp/disk.sock.
	Socket string `json:"socket"`
	// Appliance is the folder of the prebuilt libguestfs appliance; empty uses
	// the tools' default.
	Appliance string `json:"appliance,omitempty"`
	// Consistency is how the newest copy was taken (FileSystemConsistent or
	// CrashConsistent); empty if not known.
	Consistency string `json:"consistency,omitempty"`
}

func (g *GuestSource) validate() error {
	if !strings.HasPrefix(g.Socket, "/") {
		return errors.New("guest.socket must be an absolute path")
	}
	if g.Appliance != "" && !strings.HasPrefix(g.Appliance, "/") {
		return errors.New("guest.appliance must be an absolute path")
	}
	switch g.Consistency {
	case "", "FileSystemConsistent", "CrashConsistent":
	default:
		return errors.New("guest.consistency is not one that the Job knows")
	}
	return nil
}

// Outcome is the termination message of the Job. It holds counts and one
// folder name that the controller chose, never a path from the backup.
type Outcome struct {
	Result
	// Folder is the folder made in an existing claim.
	Folder string `json:"folder,omitempty"`
	// Moment is the moment read, RFC 3339; empty if not known.
	Moment string `json:"moment,omitempty"`
	// Latest is true if the newest copy was read.
	Latest bool   `json:"latest,omitempty"`
	Error  string `json:"error,omitempty"`
}

const maxMessage = 4000

// EncodeOutcome makes the termination message. It always fits the 4 KiB limit.
func EncodeOutcome(o Outcome) string {
	if len(o.Error) > 1000 {
		o.Error = o.Error[:1000] + "..."
	}
	b, err := json.Marshal(o)
	if err != nil || len(b) > maxMessage {
		return `{"error":"the restore outcome could not be encoded"}`
	}
	return string(b)
}

// ParsePaths reads the list of paths to restore: one path in escaped form per
// line (see browse.Escape), blank lines ignored. A name never holds a newline,
// because a newline in a name is written \x0a.
func ParsePaths(r io.Reader) ([][][]byte, error) {
	var out [][][]byte
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		path, err := browse.SplitPath(line)
		if err != nil {
			return nil, errors.New("a path in the list is not valid")
		}
		if err := browse.ValidatePath(path); err != nil {
			return nil, errors.New("a path in the list is not valid")
		}
		out = append(out, path)
	}
	if err := sc.Err(); err != nil {
		return nil, errors.New("the list of paths cannot be read")
	}
	if len(out) == 0 {
		return nil, ErrNothingToRestore
	}
	return out, nil
}

var permissionsPattern = regexp.MustCompile(`^0?[0-7]{3,4}$`)

func (s JobSpec) validate() (time.Time, uint32, error) {
	var at time.Time
	switch s.Mode {
	case ModeMeasure:
	case ModeNewClaim, ModeFolder:
		if s.Writer != "" {
			if !strings.HasPrefix(s.Writer, "/") {
				return at, 0, errors.New("writer must be an absolute path")
			}
			if s.Dest != "" || s.Volume != "" || s.Staging != "" || s.Policy != "" {
				return at, 0, errors.New("a Job that sends its writes to a writer names no target")
			}
			break
		}
		if s.Dest == "" {
			return at, 0, errors.New("dest is required")
		}
		if err := util.ValidateSubDir("/" + s.Volume); err != nil {
			return at, 0, errors.New("volume is not a volume folder")
		}
		if s.Mode == ModeFolder {
			if s.Staging == "" || s.Policy == "" {
				return at, 0, errors.New("staging and policy are required")
			}
			if !strings.HasPrefix(s.Staging, stagingPrefix) || strings.ContainsAny(s.Staging, "/\x00") {
				return at, 0, errors.New("staging is not a staging folder name")
			}
		}
	default:
		return at, 0, fmt.Errorf("unknown mode %q", s.Mode)
	}
	switch {
	case s.Guest != nil:
		if s.COS != nil || s.Latest == "" {
			return at, 0, errors.New("a VM disk is read from its backup folder (latest), not from COS")
		}
		if err := s.Guest.validate(); err != nil {
			return at, 0, err
		}
	case s.COS != nil:
		if s.Latest != "" || s.Moments != "" {
			return at, 0, errors.New("a backup in COS has no latest or moments folder")
		}
		if err := s.COS.Validate(); err != nil {
			return at, 0, err
		}
	case s.Latest == "":
		return at, 0, errors.New("latest is required")
	}
	if s.At != "" {
		t, err := time.Parse(time.RFC3339, s.At)
		if err != nil {
			return at, 0, errors.New("at must be an RFC 3339 time")
		}
		at = t
	}
	var mode uint32
	if s.Permissions != "" {
		if !permissionsPattern.MatchString(s.Permissions) {
			return at, 0, errors.New("permissions must be octal, like 0777")
		}
		v, _ := strconv.ParseUint(s.Permissions, 8, 32)
		mode = uint32(v)
	}
	return at, mode, nil
}

// jobDeps are what the Job needs from the machine, so that tests can supply them.
type jobDeps struct {
	choose func(latest, moments string, at time.Time) (string, browse.Info, error)
	open   func(root string) (browse.Tree, func() error, error)
	// openCOS opens a backup in COS.
	openCOS func(ctx context.Context, src cosbrowse.Source, at time.Time) (browse.Tree, browse.Info, func() error, error)
	// openGuest opens the file systems inside the VM disk at disk.
	openGuest func(ctx context.Context, latest, moments string, g GuestSource, at time.Time) (browse.Tree, browse.Info, func() error, error)
}

func realDeps() jobDeps {
	return jobDeps{
		openCOS: func(ctx context.Context, src cosbrowse.Source, at time.Time) (browse.Tree, browse.Info, func() error, error) {
			o, err := cosbrowse.Open(ctx, src, at, cosbrowse.OpenOptions{})
			if err != nil {
				return nil, browse.Info{}, nil, err
			}
			return o.Tree, o.Info, o.Close, nil
		},
		openGuest: func(ctx context.Context, latest, moments string, g GuestSource, at time.Time) (browse.Tree, browse.Info, func() error, error) {
			o, err := guestfs.Open(ctx, guestfs.OpenOptions{
				Disk: strings.TrimSuffix(latest, "/") + "/disk.img", Moments: moments, At: at, Socket: g.Socket, Crash: g.Consistency == "CrashConsistent",
				Runner: &guestfs.ExecRunner{Env: guestfs.Environment(g.Appliance, guestfs.HasKVM())},
			})
			if err != nil {
				return nil, browse.Info{}, nil, err
			}
			return o.Tree, o.Info, func() error { o.Close(); return nil }, nil
		},
		choose: browse.ChooseRoot,
		open: func(root string) (browse.Tree, func() error, error) {
			t, err := browse.NewTree(root)
			if err != nil {
				return nil, nil, err
			}
			return t, t.Close, nil
		},
	}
}

// RunJob does what the restore Job does: it reads the list of paths from
// pathsFile, reads the backup and writes the destination. The outcome is for
// the termination message. On failure it holds the reason, and the error is set.
func RunJob(ctx context.Context, spec JobSpec, pathsFile string) (Outcome, error) {
	return runJob(ctx, spec, pathsFile, realDeps())
}

func runJob(ctx context.Context, spec JobSpec, pathsFile string, deps jobDeps) (Outcome, error) {
	fail := func(err error) (Outcome, error) { return Outcome{Error: err.Error()}, err }
	at, rootMode, err := spec.validate()
	if err != nil {
		return fail(err)
	}
	f, err := os.Open(pathsFile) //nolint:gosec // the file is named by the Job's own spec
	if err != nil {
		return fail(errors.New("the list of paths cannot be opened"))
	}
	chosen, err := ParsePaths(f)
	_ = f.Close()
	if err != nil {
		return fail(err)
	}
	// The writer is dialled first: it waits only a few minutes for this Job, which
	// may take longer to start the appliance, and a target that cannot be used
	// (a claim that holds a VM disk) is then known before the disk is read.
	var dst committer
	if spec.Writer != "" && spec.Mode != ModeMeasure {
		if dst, err = DialWriter(ctx, spec.Writer); err != nil {
			return fail(err)
		}
		defer func() { _ = dst.Close() }()
	}
	tree, info, closeTree, err := openBackup(ctx, spec, at, deps)
	if err != nil {
		return fail(err)
	}
	defer func() { _ = closeTree() }()

	out := Outcome{Latest: info.Latest}
	var momentTime time.Time
	if info.Moment != "" {
		if t, perr := time.Parse(browse.MomentLayout, info.Moment); perr == nil {
			momentTime = t
			out.Moment = t.UTC().Format(time.RFC3339)
		}
	}
	if spec.Mode == ModeMeasure {
		res, merr := Measure(ctx, tree, chosen)
		out.Result = res
		if merr != nil {
			return fail(merr)
		}
		return out, nil
	}

	if dst == nil {
		if dst, err = openDest(spec); err != nil {
			return fail(err)
		}
		defer func() { _ = dst.Close() }()
	}

	res, err := Restore(ctx, tree, dst, chosen, Options{
		Owner: Owner{UID: spec.UID, GID: spec.GID}, RootMode: rootMode, MaxBytes: spec.MaxBytes,
	})
	out.Result = res
	if err != nil {
		return fail(err)
	}
	if spec.Mode == ModeFolder {
		name, err := dst.CommitMoment(spec.Policy, momentTime)
		if err != nil {
			_ = dst.Abort()
			return fail(err)
		}
		out.Folder = name
	}
	return out, nil
}

// committer is a Dest that can end a restore into a new folder by moving the
// staging folder to its name.
type committer interface {
	Dest
	CommitMoment(policy string, moment time.Time) (string, error)
	Close() error
}

// localDest is a DirDest in this process.
type localDest struct{ *DirDest }

func (d localDest) CommitMoment(policy string, moment time.Time) (string, error) {
	return d.Commit(FolderName(policy, moment))
}

func openDest(spec JobSpec) (committer, error) {
	var dst *DirDest
	var err error
	vol, _ := browse.SplitPath(spec.Volume)
	if spec.Mode == ModeNewClaim {
		dst, err = NewVolumeDest(spec.Dest, vol, spec.CapacityBytes)
	} else {
		dst, err = NewFolderDest(spec.Dest, vol, spec.Staging, spec.CapacityBytes)
	}
	if err != nil {
		return nil, err
	}
	return localDest{dst}, nil
}

// openBackup opens the backup the spec names: a folder of the snapshot pool or
// a repository in COS.
func openBackup(ctx context.Context, spec JobSpec, at time.Time, deps jobDeps) (browse.Tree, browse.Info, func() error, error) {
	if spec.Guest != nil {
		tree, info, closeTree, err := deps.openGuest(ctx, spec.Latest, spec.Moments, *spec.Guest, at)
		if err != nil {
			// The reason is already a sentence for a person.
			return nil, browse.Info{}, nil, err
		}
		return tree, info, closeTree, nil
	}
	if spec.COS != nil {
		tree, info, closeTree, err := deps.openCOS(ctx, *spec.COS, at)
		if err != nil {
			// The reason is already a sentence for a person.
			return nil, browse.Info{}, nil, err
		}
		return tree, info, closeTree, nil
	}
	root, info, err := deps.choose(spec.Latest, spec.Moments, at)
	if err != nil {
		return nil, browse.Info{}, nil, fmt.Errorf("choose the moment: %w", err)
	}
	tree, closeTree, err := deps.open(root)
	if err != nil {
		return nil, browse.Info{}, nil, fmt.Errorf("open the backup: %w", err)
	}
	return tree, info, closeTree, nil
}
