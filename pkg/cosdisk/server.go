package cosdisk

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"io"
	"net/http"
)

// minTokenLen is the shortest session token the disk server accepts.
const minTokenLen = 32

// DiskPath is the one path the disk server answers.
const DiskPath = "/disk"

// NewHandler serves a disk read-only over HTTP, for the guest helper to read
// with range requests. Run it behind TLS with the session certificate: the
// handler checks the session token, answers only GET and HEAD on DiskPath, and
// has no way to write. It never reads the guest's file system; it hands out
// bytes at offsets.
//
// Why HTTP and not NBD with TLS: the libguestfs tools cannot be given the
// session certificate for an NBD link, and the guest helper already bridges a
// disk to its appliance over a local socket. See docs/specs/2026-10-29-vm-disks-in-cos.md.
func NewHandler(d *Disk, token string) (http.Handler, error) {
	if len(token) < minTokenLen {
		return nil, errors.New("cosdisk: the session token is too short")
	}
	want := sha256.Sum256([]byte(token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got := sha256.Sum256([]byte(bearer(r)))
		if subtle.ConstantTimeCompare(got[:], want[:]) != 1 {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
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
