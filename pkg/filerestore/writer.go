package filerestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"k8s.io/klog/v2"
)

// WriterSpec is what the controller tells the writer container of a file restore
// that reads a VM disk. It is the only source of the target, the owner and the
// limits: the reader's messages cannot change any of it.
type WriterSpec struct {
	Mode string `json:"mode"`
	// Socket is where the writer listens for the reader.
	Socket string `json:"socket"`
	// Dest is the mounted target. For ModeFolder with an empty Volume it is the
	// claim's folder itself (only that folder is mounted). Otherwise it is the
	// share, and Volume is the claim's folder below it.
	Dest   string `json:"dest"`
	Volume string `json:"volume,omitempty"`
	// Staging and Policy are for ModeFolder.
	Staging string `json:"staging,omitempty"`
	Policy  string `json:"policy,omitempty"`
	// CapacityBytes is the size of the claim; MaxBytes limits one restore.
	CapacityBytes int64 `json:"capacityBytes,omitempty"`
	MaxBytes      int64 `json:"maxBytes,omitempty"`
	// UID and GID are the owner of everything restored. The reader never chooses
	// an owner: it comes from here, or from OwnerFromFolder.
	UID *int64 `json:"uid,omitempty"`
	GID *int64 `json:"gid,omitempty"`
	// OwnerFromFolder (ModeFolder, UID and GID unset) makes the writer use the
	// owner that the claim's folder has when the session opens, before any data
	// is accepted. It is fixed for the session.
	OwnerFromFolder bool `json:"ownerFromFolder,omitempty"`
	// Permissions is the mode of the root folder, like 0777; empty means 0755.
	Permissions string `json:"permissions,omitempty"`
}

func (s WriterSpec) validate() error {
	if !strings.HasPrefix(s.Socket, "/") || !strings.HasPrefix(s.Dest, "/") {
		return errors.New("socket and dest must be absolute paths")
	}
	if s.OwnerFromFolder {
		if s.Mode != ModeFolder || s.UID != nil || s.GID != nil {
			return errors.New("the owner of the folder is used only for a new folder, with no uid and gid")
		}
	} else if s.UID == nil || s.GID == nil {
		return errors.New("the writer needs the owner (uid and gid) of the restored files")
	}
	if s.Permissions != "" && !permissionsPattern.MatchString(s.Permissions) {
		return errors.New("permissions must be octal, like 0777")
	}
	if _, err := rootModeOf(s.Permissions); err != nil {
		return err
	}
	switch s.Mode {
	case ModeNewClaim:
		if s.Volume == "" {
			return errors.New("a new claim needs its volume folder")
		}
	case ModeFolder:
		if s.Staging == "" || s.Policy == "" || !strings.HasPrefix(s.Staging, stagingPrefix) || strings.ContainsAny(s.Staging, "/\x00") {
			return errors.New("staging and policy are required")
		}
	default:
		return fmt.Errorf("unknown mode %q", s.Mode)
	}
	return nil
}

func rootModeOf(p string) (uint32, error) {
	if p == "" {
		return 0, nil
	}
	v, err := strconv.ParseUint(p, 8, 32)
	if err != nil || v&^0o777 != 0 {
		return 0, errors.New("permissions must be 0777 or less: no setuid, setgid or sticky bit")
	}
	return uint32(v), nil
}

func (s WriterSpec) open() (WriterDest, error) {
	switch {
	case s.Mode == ModeNewClaim:
		vol, err := browse.SplitPath(s.Volume)
		if err != nil {
			return nil, errors.New("volume is not a folder name")
		}
		return NewVolumeDest(s.Dest, vol, s.CapacityBytes)
	case s.Volume == "":
		return NewFolderDestAt(s.Dest, s.Staging, s.CapacityBytes)
	}
	vol, err := browse.SplitPath(s.Volume)
	if err != nil {
		return nil, errors.New("volume is not a folder name")
	}
	return NewFolderDest(s.Dest, vol, s.Staging, s.CapacityBytes)
}

// RunWriter waits for the reader on ln and applies its calls to the target. It
// returns once the reader has ended the session. Whatever the reader did not
// commit is removed.
func RunWriter(ctx context.Context, ln net.Listener, spec WriterSpec) (WriterOutcome, error) {
	if err := spec.validate(); err != nil {
		return WriterOutcome{}, err
	}
	conn, err := acceptWithin(ctx, ln, acceptTimeout)
	if err != nil {
		return WriterOutcome{}, err
	}
	defer func() { _ = conn.Close() }()
	owner := Owner{UID: spec.UID, GID: spec.GID}
	var dest WriterDest
	if spec.OwnerFromFolder {
		// Measured now, before the staging folder is made and before any data.
		uid, gid, oerr := folderOwner(spec.Dest)
		if oerr != nil {
			err = oerr
		} else {
			owner = Owner{UID: &uid, GID: &gid}
		}
	}
	if err == nil {
		dest, err = spec.open()
	}
	if err != nil {
		// The reader gets the reason, in place of its first reply.
		payload, _ := json.Marshal(message{Err: err.Error()})
		_ = writeFrame(conn, replyErr, payload)
		return WriterOutcome{}, err
	}
	defer func() { _ = dest.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	rootMode, _ := rootModeOf(spec.Permissions)
	return ServeWriter(conn, dest, WriterPolicy{
		Mode: spec.Mode, Policy: spec.Policy, RootMode: rootMode, Owner: owner, MaxBytes: spec.MaxBytes,
	})
}

func acceptWithin(ctx context.Context, ln net.Listener, d time.Duration) (net.Conn, error) {
	type result struct {
		c   net.Conn
		err error
	}
	ch := make(chan result, 1)
	go func() {
		c, err := ln.Accept()
		ch <- result{c, err}
	}()
	select {
	case r := <-ch:
		return r.c, r.err
	case <-time.After(d):
		_ = ln.Close()
		return nil, errors.New("the reader of the restore did not come")
	case <-ctx.Done():
		_ = ln.Close()
		return nil, ctx.Err()
	}
}

// MainWriter runs the writer container. It writes what it wrote (WriterOutcome)
// to the termination log. It returns the exit code: 0 when the
// session ended (the reader reports the reason of a failure; a writer that failed has
// nothing more to say that would help), 2 for a bad command line, 1 when it was
// stopped or could not listen.
func MainWriter(specJSON, terminationLog string) int {
	var spec WriterSpec
	if err := json.Unmarshal([]byte(specJSON), &spec); err != nil || spec.validate() != nil {
		klog.ErrorS(nil, "restore-writer needs --restore-writer-spec as JSON")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, mainTimeout)
	defer cancel()
	_ = os.Remove(spec.Socket)
	ln, err := net.Listen("unix", spec.Socket)
	if err != nil {
		klog.ErrorS(err, "The writer cannot listen")
		return 1
	}
	out, err := RunWriter(ctx, ln, spec)
	_ = ln.Close()
	// The controller takes the counts and the folder from this message. The
	// reader's message cannot be trusted for them: the reader parses the disk.
	if b, merr := json.Marshal(out); merr == nil {
		_ = os.WriteFile(terminationLog, b, 0o600) //nolint:gosec // the path is a fixed argument of the caller
	}
	switch {
	case ctx.Err() != nil:
		return 1
	case err != nil:
		klog.ErrorS(err, "File restore writer ended with an error", "mode", spec.Mode)
	default:
		klog.InfoS("File restore writer done", "mode", spec.Mode)
	}
	return 0
}
