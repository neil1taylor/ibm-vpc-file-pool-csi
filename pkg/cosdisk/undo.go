package cosdisk

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
)

// The undo files of a VM disk are in the folder .repl-undo next to disk.img.
// The export snapshots that folder, so the Kopia WebDAV server that serves
// disk.img serves them too. This file reads them from there, so that a disk
// server can show an older restore point of the disk. See
// docs/specs/2026-10-29-vm-disks-in-cos.md, section "Phase 6d".

// undoFolder is the folder of the undo files, next to disk.img.
const undoFolder = "/.repl-undo/"

const (
	// maxListBytes is the most of a folder listing that is read.
	maxListBytes = 16 << 20
	// maxListEntries is the most names in a folder listing.
	maxListEntries = 200000

	scanCacheBytes = 16 << 20 // for the one pass that indexes a file
	// undoReadSegment is the fetch size, and the whole cache, of an undo file in
	// use: a record is at most 64 KiB, and a view opens at most
	// replication.MaxViewLayers() files, so the total is bounded.
	undoReadSegment = 64 << 10
)

// UndoHTTP is the undo folder of a backup on the Kopia WebDAV server, as a
// replication.UndoSource. Like HTTPSource it talks only to 127.0.0.1.
type UndoHTTP struct {
	ctx    context.Context
	client *http.Client
	base   string // http://127.0.0.1:PORT

	mu    sync.Mutex
	reads []*Disk // the caches of the files opened for reads
}

// NewUndoHTTP makes the source from the URL of disk.img on the server.
func NewUndoHTTP(ctx context.Context, client *http.Client, diskURL string) (*UndoHTTP, error) {
	u, err := url.Parse(diskURL)
	if err != nil {
		return nil, fmt.Errorf("cosdisk: bad URL: %w", err)
	}
	if u.Scheme != "http" || u.Hostname() != "127.0.0.1" || u.User != nil {
		return nil, fmt.Errorf("cosdisk: the disk must be on http://127.0.0.1, got %q", u.Redacted())
	}
	return &UndoHTTP{ctx: ctx, client: client, base: u.Scheme + "://" + u.Host}, nil
}

type multistatus struct {
	Responses []struct {
		Href string `xml:"href"`
	} `xml:"response"`
}

// Names lists the folder with one PROPFIND. A backup without the folder has no
// older restore points: that is an empty list, not an error.
func (h *UndoHTTP) Names() ([]string, error) {
	body := `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:resourcetype/></D:prop></D:propfind>`
	req, err := http.NewRequestWithContext(h.ctx, "PROPFIND", h.base+undoFolder, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	resp, err := h.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("cosdisk: listing the undo files: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// Kopia's WebDAV server answers 405 for a path that is not there.
	if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusMethodNotAllowed {
		return nil, nil
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, fmt.Errorf("cosdisk: listing the undo files: %s", resp.Status)
	}
	var ms multistatus
	if err = xml.NewDecoder(io.LimitReader(resp.Body, maxListBytes)).Decode(&ms); err != nil {
		return nil, fmt.Errorf("cosdisk: the list of undo files is not valid: %w", err)
	}
	if len(ms.Responses) > maxListEntries {
		return nil, errors.New("cosdisk: the undo folder holds too many names")
	}
	var names []string
	for _, r := range ms.Responses {
		u, perr := url.Parse(r.Href)
		if perr != nil || !strings.HasPrefix(u.Path, undoFolder) {
			continue
		}
		// Whatever else is listed is left for the view to ignore.
		names = append(names, strings.TrimPrefix(u.Path, undoFolder))
	}
	return names, nil
}

// Open opens one undo file. Only the name of an undo file is opened: anything
// else would let a crafted backup send the request to another path.
func (h *UndoHTTP) Open(name string, use replication.UndoUse) (replication.UndoFile, error) {
	if !replication.ValidUndoName(name) {
		return nil, fmt.Errorf("cosdisk: %q is not the name of an undo file", name)
	}
	hctx, cancel := context.WithTimeout(h.ctx, time.Minute)
	defer cancel()
	src, err := NewHTTPSource(hctx, h.client, h.base+undoFolder+name)
	if err != nil {
		return nil, err
	}
	switch use {
	case replication.UndoHeader:
		// A few bytes at each end: no cache, so no segment is fetched for them.
		return &undoRemote{size: src.Size(), r: rangeReader{src: src, ctx: h.ctx}, close: func() error { return nil }}, nil
	case replication.UndoScan:
		d := New(src, Options{ReadAhead: 4, MaxBytes: scanCacheBytes})
		return &undoRemote{size: src.Size(), r: d, close: d.Close}, nil
	default:
		d := New(src, Options{SegmentSize: undoReadSegment, MaxBytes: undoReadSegment})
		h.mu.Lock()
		h.reads = append(h.reads, d)
		h.mu.Unlock()
		return &undoRemote{size: src.Size(), r: d, close: d.Close}, nil
	}
}

type undoRemote struct {
	size  int64
	r     io.ReaderAt
	close func() error
}

func (u *undoRemote) Size() int64                           { return u.size }
func (u *undoRemote) ReadAt(p []byte, o int64) (int, error) { return u.r.ReadAt(p, o) }
func (u *undoRemote) Close() error                          { return u.close() }

// rangeReader reads one range for each call.
type rangeReader struct {
	src *HTTPSource
	ctx context.Context
}

func (r rangeReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, errors.New("negative offset")
	}
	if off >= r.src.Size() {
		return 0, io.EOF
	}
	short := false
	if off+int64(len(p)) > r.src.Size() {
		p, short = p[:r.src.Size()-off], true
	}
	n, err := r.src.ReadRange(r.ctx, p, off)
	if err == nil && short {
		err = io.EOF
	}
	return n, err
}

// RestorePointsOf lists the restore points that the undo files next to the
// disk.img at diskURL reach back to, oldest first. It reads a few bytes at each
// end of each undo file, and no records. A backup without the folder has none.
func RestorePointsOf(ctx context.Context, client *http.Client, diskURL string) ([]time.Time, error) {
	us, err := NewUndoHTTP(ctx, client, diskURL)
	if err != nil {
		return nil, err
	}
	return replication.RestorePointsFrom(us)
}

// UndoView is a VM disk in COS at one restore point.
type UndoView struct {
	undo *UndoHTTP
	// View reads the disk as it was at the restore point.
	View *replication.DiskView
	disk *Disk
}

// OpenUndoView shows the disk.img that diskURL serves (the disk of an export,
// at the moment latest) as it was at the newest restore point at or before at,
// from the undo files next to it. It fails with replication.ErrNoRestorePoint
// if the undo files do not reach back to at. o shapes the cache of the disk
// itself. The caller closes the result.
func OpenUndoView(ctx context.Context, client *http.Client, diskURL string, latest, at time.Time, o Options) (*UndoView, error) {
	us, err := NewUndoHTTP(ctx, client, diskURL)
	if err != nil {
		return nil, err
	}
	hctx, cancel := context.WithTimeout(ctx, time.Minute)
	httpSrc, err := NewHTTPSource(hctx, client, diskURL)
	cancel()
	if err != nil {
		return nil, fmt.Errorf("the backup has no readable disk: %w", err)
	}
	disk := New(httpSrc, o)
	v, err := replication.OpenDiskViewFrom(disk, disk.Size(), latest, us, at)
	if err != nil {
		_ = disk.Close()
		return nil, err
	}
	return &UndoView{View: v, disk: disk, undo: us}, nil
}

// Close closes the view and the disk.
func (u *UndoView) Close() error {
	err := u.View.Close()
	if cerr := u.disk.Close(); err == nil {
		err = cerr
	}
	return err
}

// UndoCachedBytes is what the caches of the undo files hold now.
func (u *UndoView) UndoCachedBytes() int64 {
	u.undo.mu.Lock()
	defer u.undo.mu.Unlock()
	var n int64
	for _, d := range u.undo.reads {
		n += d.CachedBytes()
	}
	return n
}
