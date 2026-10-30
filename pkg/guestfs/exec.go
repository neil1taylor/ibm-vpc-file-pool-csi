package guestfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Runner runs the libguestfs tools. The first argument is the tool.
type Runner interface {
	// Output runs a tool to the end and returns what it wrote to its output.
	Output(ctx context.Context, args []string) ([]byte, error)
	// Stream starts a tool and returns its output as it comes. Closing the
	// stream stops the tool.
	Stream(ctx context.Context, args []string) (io.ReadCloser, error)
}

// ExitError is a tool that failed. Stderr is what the tool said (cut short); it
// may hold names from the guest, so it is never sent to a user.
type ExitError struct {
	Stderr string
	Err    error
}

func (e *ExitError) Error() string {
	if e.Err != nil {
		return fmt.Sprintf("tool failed: %v", e.Err)
	}
	return "tool failed"
}

func (e *ExitError) Unwrap() error { return e.Err }

const maxStderr = 4 << 10

// ExecRunner runs the tools as child processes. It never uses a shell.
type ExecRunner struct {
	// Env is the whole environment of the tools; nothing else is passed on.
	Env []string
	// Timeout limits one run; zero means 10 minutes, the limit for the appliance
	// to start and answer.
	Timeout time.Duration
	// MaxOutput limits what Output keeps; zero means 128 MiB.
	MaxOutput int64
}

func (r *ExecRunner) timeout() time.Duration {
	if r.Timeout > 0 {
		return r.Timeout
	}
	return 10 * time.Minute
}

func (r *ExecRunner) maxOutput() int64 {
	if r.MaxOutput > 0 {
		return r.MaxOutput
	}
	return 128 << 20
}

type capWriter struct {
	buf bytes.Buffer
	max int64
}

var errTooMuchOutput = errors.New("the tool wrote more than the limit")

func (w *capWriter) Write(p []byte) (int, error) {
	if int64(w.buf.Len()+len(p)) > w.max {
		return 0, errTooMuchOutput
	}
	return w.buf.Write(p)
}

func (r *ExecRunner) command(ctx context.Context, args []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, args[0], args[1:]...) // #nosec G204 -- the tool is fixed by this package; arguments are separate words
	cmd.Env = r.Env
	cmd.Stdin = nil
	setProcessGroup(cmd)
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// Output runs the tool to the end.
func (r *ExecRunner) Output(ctx context.Context, args []string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout())
	defer cancel()
	cmd := r.command(ctx, args)
	out := &capWriter{max: r.maxOutput()}
	errBuf := &capWriter{max: maxStderr}
	cmd.Stdout, cmd.Stderr = out, ignoreFull{errBuf}
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &ExitError{Stderr: errBuf.buf.String(), Err: err}
	}
	return out.buf.Bytes(), nil
}

// ignoreFull drops what does not fit instead of failing the tool.
type ignoreFull struct{ w *capWriter }

func (i ignoreFull) Write(p []byte) (int, error) {
	if room := i.w.max - int64(i.w.buf.Len()); room > 0 {
		_, _ = i.w.buf.Write(p[:min(int64(len(p)), room)])
	}
	return len(p), nil
}

type stream struct {
	cmd    *exec.Cmd
	out    io.ReadCloser
	stderr *capWriter
	cancel context.CancelFunc
	once   sync.Once
	done   bool
}

// Stream starts the tool.
func (r *ExecRunner) Stream(ctx context.Context, args []string) (io.ReadCloser, error) {
	ctx, cancel := context.WithTimeout(ctx, r.timeout()+time.Hour) // a download may take long once the appliance is up
	cmd := r.command(ctx, args)
	errBuf := &capWriter{max: maxStderr}
	cmd.Stderr = ignoreFull{errBuf}
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err = cmd.Start(); err != nil {
		cancel()
		return nil, &ExitError{Err: err}
	}
	return &stream{cmd: cmd, out: out, stderr: errBuf, cancel: cancel}, nil
}

// Read passes the output on. At the end it reports a tool that failed, so that a
// cut stream is never taken for a whole file.
func (s *stream) Read(p []byte) (int, error) {
	n, err := s.out.Read(p)
	if errors.Is(err, io.EOF) && !s.done {
		s.done = true
		if werr := s.cmd.Wait(); werr != nil {
			return n, &ExitError{Stderr: s.stderr.buf.String(), Err: werr}
		}
	}
	return n, err
}

func (s *stream) Close() error {
	s.once.Do(func() {
		s.cancel()
		_ = s.out.Close()
		if !s.done {
			_ = s.cmd.Wait()
		}
	})
	return nil
}

// Environment returns the environment for the libguestfs tools in the helper
// pod. The appliance is started directly (no libvirt), prebuilt in the image at
// appliance, with its scratch files in the two writable folders of the pod. With
// no KVM device the appliance is emulated, which is slower but works.
func Environment(appliance string, kvm bool) []string {
	env := []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=/tmp",
		"LIBGUESTFS_BACKEND=direct",
		"LIBGUESTFS_TMPDIR=/tmp",
		"LIBGUESTFS_CACHEDIR=/var/tmp",
		"LC_ALL=C.UTF-8",
	}
	if appliance != "" {
		env = append(env, "LIBGUESTFS_PATH="+appliance)
	}
	if !kvm {
		env = append(env, "LIBGUESTFS_BACKEND_SETTINGS=force_tcg")
	}
	return env
}

// HasKVM reports whether the KVM device is there.
func HasKVM() bool {
	f, err := os.OpenFile("/dev/kvm", os.O_RDWR, 0)
	if err != nil {
		return false
	}
	_ = f.Close()
	return true
}
