package filerestore

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/signal"
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
	CapacityBytes int64  `json:"capacityBytes,omitempty"`
	MaxBytes      int64  `json:"maxBytes,omitempty"`
	UID           *int64 `json:"uid,omitempty"`
	GID           *int64 `json:"gid,omitempty"`
}

func (s WriterSpec) validate() error {
	if !strings.HasPrefix(s.Socket, "/") || !strings.HasPrefix(s.Dest, "/") {
		return errors.New("socket and dest must be absolute paths")
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
func RunWriter(ctx context.Context, ln net.Listener, spec WriterSpec) error {
	if err := spec.validate(); err != nil {
		return err
	}
	conn, err := acceptWithin(ctx, ln, acceptTimeout)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()
	dest, err := spec.open()
	if err != nil {
		// The reader gets the reason, in place of its first reply.
		payload, _ := json.Marshal(message{Err: err.Error()})
		_ = writeFrame(conn, replyErr, payload)
		return err
	}
	defer func() { _ = dest.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	_, err = ServeWriter(conn, dest, WriterPolicy{
		Mode: spec.Mode, Policy: spec.Policy, Owner: Owner{UID: spec.UID, GID: spec.GID}, MaxBytes: spec.MaxBytes,
	})
	return err
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

// MainWriter runs the writer container. It returns the exit code: 0 when the
// session ended (the reader reports the outcome; a writer that failed has
// nothing more to say that would help), 2 for a bad command line, 1 when it was
// stopped or could not listen.
func MainWriter(specJSON string) int {
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
	err = RunWriter(ctx, ln, spec)
	_ = ln.Close()
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
