package cosbrowse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

const (
	// kopiaUser and kopiaHost are the names the export Jobs use. Kopia needs a
	// user and a host to open a repository, and the pod has no account for its
	// user ID.
	kopiaUser = "driver"
	kopiaHost = "pool-csi"

	maxSnapshotListBytes = 256 << 20
	maxStderrBytes       = 8 << 10
	defaultKopiaProcs    = 12
)

// KopiaConfig is how to run the Kopia program against one repository.
type KopiaConfig struct {
	// Binary is the Kopia program. Empty means "kopia" on the PATH.
	Binary string
	// StateDir is Kopia's private folder: its settings, cache, logs and temporary
	// files. It must be writable.
	StateDir string
	Source   Source
	// AccessKey and SecretKey are the COS key, and Password is the backup key.
	// They are passed to the Kopia child in its environment, and only there: not
	// on its command line, and not to any other process.
	AccessKey, SecretKey, Password string
	// ContentCacheMB and MetadataCacheMB limit Kopia's cache on disk.
	ContentCacheMB, MetadataCacheMB int
	// Procs is how many Kopia programs run at once. Zero means 12.
	Procs int
	// backend replaces the S3 words of the connect command. Tests use it to
	// open a repository in a folder.
	backend []string
}

// KopiaRepo runs the Kopia program to read a repository. It implements Repo.
type KopiaRepo struct {
	cfg KopiaConfig
	sem chan struct{}
}

// NewKopia makes a KopiaRepo. Call Connect before the first read.
func NewKopia(cfg KopiaConfig) *KopiaRepo {
	if cfg.Binary == "" {
		cfg.Binary = "kopia"
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	if cfg.ContentCacheMB <= 0 {
		cfg.ContentCacheMB = 256
	}
	if cfg.MetadataCacheMB <= 0 {
		cfg.MetadataCacheMB = 128
	}
	if cfg.Procs <= 0 {
		cfg.Procs = defaultKopiaProcs
	}
	return &KopiaRepo{cfg: cfg, sem: make(chan struct{}, cfg.Procs)}
}

var _ Repo = (*KopiaRepo)(nil)

// childEnv is the whole environment of a Kopia child. It is built from nothing:
// the helper's own environment is not passed on.
func (k *KopiaRepo) childEnv() []string {
	st := k.cfg.StateDir
	return []string{
		"PATH=/usr/local/bin:/usr/bin:/bin",
		"HOME=" + st,
		"TMPDIR=" + filepath.Join(st, "tmp"),
		"KOPIA_CONFIG_PATH=" + filepath.Join(st, "repository.config"),
		"KOPIA_CACHE_DIRECTORY=" + filepath.Join(st, "cache"),
		"KOPIA_LOG_DIR=" + filepath.Join(st, "logs"),
		"KOPIA_CHECK_FOR_UPDATES=false",
		"KOPIA_PASSWORD=" + k.cfg.Password,
		"AWS_ACCESS_KEY_ID=" + k.cfg.AccessKey,
		"AWS_SECRET_ACCESS_KEY=" + k.cfg.SecretKey,
	}
}

// quiet keeps Kopia from writing log files and keeps its console output to
// errors, so a long session does not fill the state folder.
var quiet = []string{"--disable-file-logging", "--disable-content-log", "--log-level=error", "--no-progress"}

func (k *KopiaRepo) command(ctx context.Context, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, k.cfg.Binary, append(append([]string{}, args...), quiet...)...) //nolint:gosec // fixed arguments and object IDs that passed ValidOID
	cmd.Env = k.childEnv()
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// Connect opens the repository read-only and keeps its settings in StateDir. It
// stores no password.
func (k *KopiaRepo) Connect(ctx context.Context) error {
	for _, d := range []string{"", "cache", "logs", "tmp"} {
		if err := os.MkdirAll(filepath.Join(k.cfg.StateDir, d), 0o700); err != nil {
			return fmt.Errorf("make Kopia's state folder: %w", err)
		}
	}
	args := append([]string{"repository", "connect"}, k.backendWords()...)
	args = append(args, "--readonly", "--no-persist-credentials",
		"--override-username="+kopiaUser, "--override-hostname="+kopiaHost,
		"--content-cache-size-mb="+strconv.Itoa(k.cfg.ContentCacheMB),
		"--metadata-cache-size-mb="+strconv.Itoa(k.cfg.MetadataCacheMB))
	cmd := k.command(ctx, args...)
	var stderr limitBuffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return k.failure("connect", err, stderr.String())
	}
	return nil
}

func (k *KopiaRepo) backendWords() []string {
	if k.cfg.backend != nil {
		return k.cfg.backend
	}
	s := k.cfg.Source
	endpoint := strings.TrimPrefix(strings.TrimPrefix(s.Endpoint, "https://"), "http://")
	words := []string{"s3", "--bucket=" + s.Bucket, "--endpoint=" + endpoint}
	if strings.HasPrefix(s.Endpoint, "http://") {
		words = append(words, "--disable-tls")
	}
	if s.Prefix != "" {
		words = append(words, "--prefix="+s.Prefix)
	}
	return words
}

// failure turns a failed Kopia run into a ReadError for people. The raw text
// goes to the log of the helper, trimmed, at a low level.
func (k *KopiaRepo) failure(what string, err error, stderr string) error {
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	klog.V(2).InfoS("Kopia failed", "step", what, "error", err.Error(), "stderr", trim(stderr, 600))
	return &ReadError{Msg: explain(stderr)}
}

func trim(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}

type kopiaSnapshot struct {
	ID     string `json:"id"`
	Source struct {
		Path string `json:"path"`
	} `json:"source"`
	StartTime time.Time `json:"startTime"`
	RootEntry struct {
		Obj   string    `json:"obj"`
		Mode  string    `json:"mode"`
		MTime time.Time `json:"mtime"`
		UID   uint32    `json:"uid"`
		GID   uint32    `json:"gid"`
	} `json:"rootEntry"`
	Tags map[string]string `json:"tags"`
}

// Snapshots lists every snapshot of the repository.
func (k *KopiaRepo) Snapshots(ctx context.Context) ([]Snapshot, error) {
	if err := k.acquire(ctx); err != nil {
		return nil, err
	}
	defer k.release()
	cmd := k.command(ctx, "snapshot", "list", "--all", "--json")
	var stderr limitBuffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, k.failure("snapshot list", err, "")
	}
	var raw []kopiaSnapshot
	lr := &io.LimitedReader{R: out, N: maxSnapshotListBytes + 1}
	derr := json.NewDecoder(lr).Decode(&raw)
	_, _ = io.Copy(io.Discard, out)
	if werr := cmd.Wait(); werr != nil {
		return nil, k.failure("snapshot list", werr, stderr.String())
	}
	if derr != nil {
		if lr.N <= 0 {
			return nil, &ReadError{Msg: "the list of backups in COS is too large to read"}
		}
		return nil, &ReadError{Msg: "the list of backups in COS could not be read"}
	}
	snaps := make([]Snapshot, 0, len(raw))
	for _, r := range raw {
		s := Snapshot{ID: r.ID, SourcePath: r.Source.Path, Volume: r.Tags["tag:volume"], Start: r.StartTime,
			Root: r.RootEntry.Obj, RootUID: r.RootEntry.UID, RootGID: r.RootEntry.GID, RootMTime: r.RootEntry.MTime}
		if v, err := strconv.ParseUint(r.RootEntry.Mode, 8, 32); err == nil {
			s.RootMode = uint32(v) & 0o7777
		}
		if t, err := time.Parse(time.RFC3339, r.Tags["tag:moment"]); err == nil {
			s.Moment = t.UTC()
		}
		snaps = append(snaps, s)
	}
	return snaps, nil
}

func (k *KopiaRepo) acquire(ctx context.Context) error {
	select {
	case k.sem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (k *KopiaRepo) release() { <-k.sem }

// Object streams the content of an object with `kopia show`.
func (k *KopiaRepo) Object(ctx context.Context, oid string) (io.ReadCloser, error) {
	if !ValidOID(oid) {
		return nil, &ReadError{Msg: "the backup holds a damaged entry"}
	}
	if err := k.acquire(ctx); err != nil {
		return nil, err
	}
	pctx, cancel := context.WithCancel(ctx)
	cmd := k.command(pctx, "show", oid)
	var stderr limitBuffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		k.release()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		k.release()
		return nil, k.failure("show", err, "")
	}
	return &procReader{k: k, cmd: cmd, out: out, stderr: &stderr, cancel: cancel}, nil
}

// procReader is the output of a running Kopia child. When the child ends with
// an error, the read that reaches the end returns that error and not io.EOF, so
// a cut-off file is never taken for a whole one.
type procReader struct {
	k      *KopiaRepo
	cmd    *exec.Cmd
	out    io.ReadCloser
	stderr *limitBuffer
	cancel context.CancelFunc

	once sync.Once
	err  error
}

func (p *procReader) Read(b []byte) (int, error) {
	n, err := p.out.Read(b)
	if errors.Is(err, io.EOF) {
		if werr := p.finish(); werr != nil {
			return n, werr
		}
	}
	return n, err
}

// finish waits for the child and gives its error, once.
func (p *procReader) finish() error {
	p.once.Do(func() {
		if err := p.cmd.Wait(); err != nil {
			p.err = p.k.failure("show", err, p.stderr.String())
		}
		p.cancel()
		p.k.release()
	})
	return p.err
}

// Close stops the child if it is still running.
func (p *procReader) Close() error {
	p.cancel()
	_ = p.out.Close()
	_ = p.finish()
	return nil
}

// limitBuffer keeps the first maxStderrBytes bytes that are written to it.
type limitBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *limitBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := maxStderrBytes - b.buf.Len(); room > 0 {
		b.buf.Write(p[:min(room, len(p))])
	}
	return len(p), nil
}

func (b *limitBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
