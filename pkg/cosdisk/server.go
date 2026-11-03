package cosdisk

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
	"strconv"
)

// minTokenLen is the shortest session token the disk server accepts.
const minTokenLen = 32

// DiskPath is the one path the disk server answers.
const DiskPath = "/disk"

// FileListPath is where the disk server hands out the file list of the export,
// if it has one.
const FileListPath = "/filelist"

// MaxFileListBytes is the largest file list the disk server hands out.
const MaxFileListBytes = 256 << 20

// ErrNoFileList is what a file list source returns when the export has none.
var ErrNoFileList = errors.New("cosdisk: the export has no file list")

// HandlerOption changes what NewHandler serves.
type HandlerOption func(*handlerConfig)

type handlerConfig struct {
	fileList func(ctx context.Context) (io.ReadCloser, int64, error)
	onAuthed func(w http.ResponseWriter, r *http.Request) func()
}

// WithOnAuthed calls fn for every request that carries the right token, before
// it is answered; the function it returns is called when the answer is done. A
// request without the token never reaches fn, so it can neither keep a session
// alive nor read a header that fn sets.
func WithOnAuthed(fn func(w http.ResponseWriter, r *http.Request) func()) HandlerOption {
	return func(c *handlerConfig) { c.onAuthed = fn }
}

// WithFileList serves the file list at FileListPath. fetch opens it and says
// its size, or returns ErrNoFileList. The server passes the bytes on and does
// not read them: the list comes from a hostile disk and is checked by the guest
// reader, which holds no keys.
func WithFileList(fetch func(ctx context.Context) (io.ReadCloser, int64, error)) HandlerOption {
	return func(c *handlerConfig) { c.fileList = fetch }
}

// NewHandler serves a disk read-only over HTTP, for the guest helper to read
// with range requests. Run it behind TLS with the session certificate: the
// handler checks the session token, answers only GET and HEAD on DiskPath, and
// has no way to write. It never reads the guest's file system; it hands out
// bytes at offsets.
//
// Why HTTP and not NBD with TLS: the libguestfs tools cannot be given the
// session certificate for an NBD link, and the guest helper already bridges a
// disk to its appliance over a local socket. See docs/specs/2026-10-29-vm-disks-in-cos.md.
func NewHandler(d *Disk, token string, opts ...HandlerOption) (http.Handler, error) {
	if len(token) < minTokenLen {
		return nil, errors.New("cosdisk: the session token is too short")
	}
	var cfg handlerConfig
	for _, o := range opts {
		o(&cfg)
	}
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(bearer(r)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if cfg.onAuthed != nil {
			defer cfg.onAuthed(w, r)()
		}
		if r.URL.Path == FileListPath && cfg.fileList != nil {
			serveFileList(w, r, cfg.fileList)
			return
		}
		if r.URL.Path != DiskPath {
			http.NotFound(w, r)
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/octet-stream")
		w.Header().Set("Cache-Control", "no-store")
		http.ServeContent(w, r, "", zeroTime, io.NewSectionReader(d, 0, d.Size()))
	}), nil
}

func bearer(r *http.Request) string {
	const p = "Bearer "
	h := r.Header.Get("Authorization")
	if len(h) > len(p) && h[:len(p)] == p {
		return h[len(p):]
	}
	return ""
}

func serveFileList(w http.ResponseWriter, r *http.Request, fetch func(ctx context.Context) (io.ReadCloser, int64, error)) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rc, size, err := fetch(r.Context())
	if err != nil || size < 0 || size > MaxFileListBytes {
		if rc != nil {
			_ = rc.Close()
		}
		http.NotFound(w, r)
		return
	}
	defer func() { _ = rc.Close() }()
	w.Header().Set("Content-Type", "application/gzip")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.FormatInt(size, 10))
	_, _ = io.Copy(w, io.LimitReader(rc, size))
}
