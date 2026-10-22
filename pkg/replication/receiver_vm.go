package replication

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
)

// The VM disk calls of the receiver. A running VM's disk is sent as a staging
// copy, not as files. Design: docs/specs/2026-10-20-vm-replication-step5-receiver-failover.md.
//
//	GET  /api/v1/vmcopy/{sv}/{copyID}          the offsets stored so far
//	POST /api/v1/vmcopy/{sv}/{copyID}/compare  which chunks of a full copy differ
//	PUT  /api/v1/vmcopy/{sv}/{copyID}          store records (repeating is safe)
//	POST /api/v1/vmcopy/{sv}/{copyID}/commit   check, then apply with ApplyCopy
//
// Query: basePath and subPath as for sync, and disk (default "disk.img").

const (
	// DefaultMaxVMDiskBytes is the largest disk the receiver accepts (2 TiB).
	DefaultMaxVMDiskBytes int64 = 2 << 40

	incomingDirName = ".repl-incoming"
	incomingMaxAge  = 24 * time.Hour
	defaultDiskName = "disk.img"

	manifestName      = "manifest"
	stagingName       = "data"
	baseName          = "base"
	manifestEntrySize = 1 + 8 + 4 + 8
	maxCommitBody     = 64 << 10
)

var (
	copyIDPattern   = regexp.MustCompile(`^[a-f0-9][a-f0-9-]{7,63}$`)
	diskNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
)

type segKind byte

const (
	segData segKind = 0 // stored in the staging file
	segZero segKind = 1
	segSame segKind = 2 // equal to the far-site disk; read from it at commit
)

type seg struct {
	kind   segKind
	off    int64
	length uint32
	pos    int64 // in the staging file, for segData
}

// SetMaxVMDiskBytes overrides the largest disk the receiver accepts.
func (r *Receiver) SetMaxVMDiskBytes(n int64) { r.maxVMDiskBytes = n }

func (r *Receiver) vmLimit() int64 {
	if r.maxVMDiskBytes > 0 {
		return r.maxVMDiskBytes
	}
	return DefaultMaxVMDiskBytes
}

// vmCall is one request, with the disk's folder open and locked.
type vmCall struct {
	r      *Receiver
	w      http.ResponseWriter
	req    *http.Request
	sv     string
	copyID string
	action string
	disk   string // full path of the disk file
	v      *volume
	unlock func()
}

func (c *vmCall) fail(code int, format string, args ...any) {
	http.Error(c.w, fmt.Sprintf(format, args...), code)
}

// conflict answers 409 with the moment the disk shows now.
func (c *vmCall) conflict(format string, args ...any) {
	moment, err := c.baseMoment()
	if err != nil {
		moment = "unknown"
	}
	c.w.Header().Set("Content-Type", "application/json")
	c.w.WriteHeader(http.StatusConflict)
	b, _ := json.Marshal(Conflict{Message: fmt.Sprintf(format, args...), DiskMoment: moment})
	_, _ = c.w.Write(b)
}

func (c *vmCall) release() {
	c.v.close()
	c.unlock()
}

func (c *vmCall) sessionDir() string { return filepath.Join(incomingDirName, c.copyID) }

// open checks the request, locks the disk and opens its folder. On a refusal
// it has answered and returns nil.
func (r *Receiver) openVMCall(w http.ResponseWriter, req *http.Request) *vmCall {
	if !r.authenticate(req) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return nil
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(req.URL.Path, "/api/v1/vmcopy/"), "/"), "/")
	if len(parts) < 2 || len(parts) > 3 {
		http.Error(w, "path must be /api/v1/vmcopy/{subvolume}/{copyID}[/compare|/commit]", http.StatusBadRequest)
		return nil
	}
	c := &vmCall{r: r, w: w, req: req, sv: parts[0], copyID: parts[1]}
	if len(parts) == 3 {
		c.action = parts[2]
	}
	if !subVolumeNamePattern.MatchString(c.sv) {
		c.fail(http.StatusBadRequest, "invalid subvolume name")
		return nil
	}
	if !copyIDPattern.MatchString(c.copyID) {
		c.fail(http.StatusBadRequest, "invalid copy ID")
		return nil
	}
	q := req.URL.Query()
	diskName := q.Get("disk")
	if diskName == "" {
		diskName = defaultDiskName
	}
	if !diskNamePattern.MatchString(diskName) || strings.Contains(diskName, "..") {
		c.fail(http.StatusBadRequest, "invalid disk name")
		return nil
	}
	if q.Get("basePath") == "" || q.Get("subPath") == "" {
		c.fail(http.StatusBadRequest, "basePath and subPath query parameters are required")
		return nil
	}
	dir, err := r.resolveDestPath(q.Get("basePath"), q.Get("subPath"))
	if err == nil && filepath.Base(dir) != c.sv {
		err = fmt.Errorf("the destination %q is not the folder of volume %s", dir, c.sv)
	}
	if err != nil {
		c.fail(http.StatusBadRequest, "%v", err)
		return nil
	}
	c.disk = filepath.Join(dir, diskName)

	lock := &sync.Mutex{}
	if existing, loaded := r.vmLocks.LoadOrStore(c.disk, lock); loaded {
		// vmLocks only ever stores *sync.Mutex values.
		if m, ok := existing.(*sync.Mutex); ok {
			lock = m
		}
	}
	if !lock.TryLock() {
		c.fail(http.StatusTooManyRequests, "another call is working on this disk")
		return nil
	}
	c.unlock = lock.Unlock

	if err = os.MkdirAll(dir, dataDirMode); err != nil {
		c.unlock()
		klog.ErrorS(err, "Failed to create destination directory", "dir", dir)
		c.fail(http.StatusInternalServerError, "failed to create destination directory")
		return nil
	}
	// The VM that boots from a failed-over copy runs as another user and
	// must be able to use the folder, as in direct NFS mode.
	if err = os.Chmod(dir, 0o777); err != nil { //nolint:gosec // G302: see above
		c.unlock()
		c.fail(http.StatusInternalServerError, "failed to set the folder mode")
		return nil
	}
	if c.v, err = openVolume(c.disk); err != nil {
		c.unlock()
		c.fail(http.StatusInternalServerError, "failed to open the volume folder")
		return nil
	}
	return c
}

func (r *Receiver) handleVMCopy(w http.ResponseWriter, req *http.Request) {
	c := r.openVMCall(w, req)
	if c == nil {
		return
	}
	defer c.release()
	if err := c.v.removeOldUploads(incomingMaxAge); err != nil {
		klog.ErrorS(err, "Could not remove old upload folders", "disk", c.disk)
	}
	switch {
	case req.Method == http.MethodGet && c.action == "":
		c.status()
	case req.Method == http.MethodPost && c.action == "compare":
		c.compare()
	case req.Method == http.MethodPut && c.action == "":
		c.upload()
	case req.Method == http.MethodPost && c.action == "commit":
		c.commit()
	default:
		c.fail(http.StatusMethodNotAllowed, "method not allowed")
	}
}

// removeOldUploads removes upload folders that nothing has touched for age.
func (v *volume) removeOldUploads(age time.Duration) error {
	fi, err := v.root.Lstat(incomingDirName)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !fi.IsDir() {
		return fmt.Errorf("%s is not a folder", incomingDirName)
	}
	d, err := v.root.Open(incomingDirName)
	if err != nil {
		return err
	}
	entries, err := d.ReadDir(-1)
	_ = d.Close()
	if err != nil {
		return err
	}
	for _, e := range entries {
		info, ierr := e.Info()
		if ierr != nil || time.Since(info.ModTime()) < age {
			continue
		}
		if err = v.root.RemoveAll(filepath.Join(incomingDirName, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

// loadSession reads what is stored for the copy, later entries winning for the
// same offset. Entries whose data is not in the staging file are dropped: they
// were cut off by a stop.
func (c *vmCall) loadSession() (map[int64]seg, error) {
	segs := map[int64]seg{}
	f, err := c.v.root.Open(filepath.Join(c.sessionDir(), manifestName))
	if errors.Is(err, os.ErrNotExist) {
		return segs, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var dataSize int64
	if fi, serr := c.v.root.Lstat(filepath.Join(c.sessionDir(), stagingName)); serr == nil {
		dataSize = fi.Size()
	}
	br := bufio.NewReaderSize(f, 1<<20)
	var buf [manifestEntrySize]byte
	for {
		if _, err = io.ReadFull(br, buf[:]); err != nil {
			break // end, or a torn last entry
		}
		s := seg{
			kind:   segKind(buf[0]),
			off:    int64(binary.BigEndian.Uint64(buf[1:9])), // #nosec G115 -- written by us, checked below
			length: binary.BigEndian.Uint32(buf[9:13]),
			pos:    int64(binary.BigEndian.Uint64(buf[13:21])), // #nosec G115 -- written by us, checked below
		}
		if s.off < 0 || s.pos < 0 || s.kind > segSame {
			return nil, fmt.Errorf("the upload record file is damaged")
		}
		if s.kind == segData && s.pos+int64(s.length) > dataSize {
			continue
		}
		segs[s.off] = s
	}
	if !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return segs, nil
}

func (c *vmCall) openSession() (manifest, staging *os.File, err error) {
	if err = c.v.root.MkdirAll(c.sessionDir(), 0o770); err != nil { // #nosec G301 -- shared with the QEMU user
		return nil, nil, err
	}
	manifest, err = c.v.root.OpenFile(filepath.Join(c.sessionDir(), manifestName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o660)
	if err != nil {
		return nil, nil, err
	}
	staging, err = c.v.root.OpenFile(filepath.Join(c.sessionDir(), stagingName), os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o660)
	if err != nil {
		_ = manifest.Close()
		return nil, nil, err
	}
	return manifest, staging, nil
}

func appendSeg(w io.Writer, s seg) error {
	var buf [manifestEntrySize]byte
	buf[0] = byte(s.kind)
	binary.BigEndian.PutUint64(buf[1:], uint64(s.off)) // #nosec G115 -- checked by the caller
	binary.BigEndian.PutUint32(buf[9:], s.length)
	binary.BigEndian.PutUint64(buf[13:], uint64(s.pos)) // #nosec G115 -- a file size
	_, err := w.Write(buf[:])
	return err
}

// status lists the offsets of data records stored so far.
func (c *vmCall) status() {
	segs, err := c.loadSession()
	if err != nil {
		c.fail(http.StatusInternalServerError, "%v", err)
		return
	}
	offs := make([]int64, 0, len(segs))
	for _, s := range segs {
		if s.kind == segData {
			offs = append(offs, s.off)
		}
	}
	sort.Slice(offs, func(i, j int) bool { return offs[i] < offs[j] })
	moment, err := c.baseMoment()
	if err != nil {
		c.fail(http.StatusInternalServerError, "%v", err)
		return
	}
	c.w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(StatusResponse{Offsets: offs, DiskMoment: moment})
	_, _ = c.w.Write(b)
}

// baseMoment is the moment the disk shows, or "none" if it has no disk.
func (c *vmCall) baseMoment() (string, error) {
	if _, err := c.v.diskInfo(); errors.Is(err, os.ErrNotExist) {
		return "none", nil
	} else if err != nil {
		return "", err
	}
	t, ok, err := c.v.moment()
	if err != nil {
		return "", err
	}
	if !ok {
		return "unknown", nil
	}
	return t.UTC().Format(time.RFC3339Nano), nil
}

// compare answers which chunks of a full copy differ from the disk, as a list
// of 8-byte offsets. Chunks that match are noted, and read from the disk at
// commit; zero chunks are noted too.
func (c *vmCall) compare() {
	if _, err := c.v.recover(); err != nil {
		c.fail(http.StatusInternalServerError, "rolling back an unfinished copy: %v", err)
		return
	}
	base, err := c.baseMoment()
	if err != nil {
		c.fail(http.StatusInternalServerError, "%v", err)
		return
	}
	var disk *os.File
	var oldSize int64
	if fi, derr := c.v.diskInfo(); derr == nil {
		oldSize = fi.Size()
		if disk, err = c.v.openDisk(os.O_RDONLY); err != nil {
			c.fail(http.StatusInternalServerError, "%v", err)
			return
		}
		defer func() { _ = disk.Close() }()
	}
	manifest, staging, err := c.openSession()
	if err != nil {
		c.fail(http.StatusInternalServerError, "%v", err)
		return
	}
	defer func() { _ = manifest.Close(); _ = staging.Close() }()
	mw := bufio.NewWriterSize(manifest, 1<<20)

	br := bufio.NewReaderSize(c.req.Body, 1<<20)
	var need bytes.Buffer
	buf := make([]byte, compareChunk)
	for {
		e, rerr := ReadCompareEntry(br)
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			c.fail(http.StatusBadRequest, "bad compare list: %v", rerr)
			return
		}
		if e.Len == 0 || e.Len > compareChunk || e.Off+int64(e.Len) > c.r.vmLimit() {
			c.fail(http.StatusBadRequest, "entry at %d is out of range", e.Off)
			return
		}
		s := seg{off: e.Off, length: e.Len}
		switch {
		case e.Zero:
			s.kind = segZero
		case disk != nil && e.Off+int64(e.Len) <= oldSize:
			b := buf[:e.Len]
			if _, rerr = disk.ReadAt(b, e.Off); rerr != nil && !errors.Is(rerr, io.EOF) {
				c.fail(http.StatusInternalServerError, "reading the disk at %d: %v", e.Off, rerr)
				return
			}
			if sha256.Sum256(b) == e.Hash {
				s.kind = segSame
			}
		}
		if e.Zero || s.kind == segSame {
			if err = appendSeg(mw, s); err != nil {
				c.fail(http.StatusInternalServerError, "%v", err)
				return
			}
			continue
		}
		need.Write(binary.BigEndian.AppendUint64(nil, uint64(e.Off))) // #nosec G115 -- checked above
	}
	if err = mw.Flush(); err == nil {
		err = manifest.Sync()
	}
	if err == nil {
		err = c.writeSessionFile(baseName, []byte(base))
	}
	if err != nil {
		c.fail(http.StatusInternalServerError, "%v", err)
		return
	}
	c.w.Header().Set("Content-Type", "application/octet-stream")
	c.w.Header().Set(DiskMomentHeader, base)
	_, _ = c.w.Write(need.Bytes())
}

func (c *vmCall) writeSessionFile(name string, b []byte) error {
	f, err := c.v.root.OpenFile(filepath.Join(c.sessionDir(), name), os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o660)
	if err != nil {
		return err
	}
	_, err = f.Write(b)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// upload stores records. A record sent twice is stored twice, and the later
// one is used.
func (c *vmCall) upload() {
	manifest, staging, err := c.openSession()
	if err != nil {
		c.fail(http.StatusInternalServerError, "%v", err)
		return
	}
	defer func() { _ = manifest.Close(); _ = staging.Close() }()
	pos, err := staging.Seek(0, io.SeekEnd)
	if err != nil {
		c.fail(http.StatusInternalServerError, "%v", err)
		return
	}
	sw := bufio.NewWriterSize(staging, 1<<20)
	mw := bufio.NewWriterSize(manifest, 1<<20)
	br := bufio.NewReaderSize(c.req.Body, 1<<20)
	for {
		rec, rerr := ReadUploadRecord(br, compareChunk)
		if errors.Is(rerr, io.EOF) {
			break
		}
		if rerr != nil {
			c.fail(http.StatusBadRequest, "bad record: %v", rerr)
			return
		}
		if rec.Off+int64(rec.Len) > c.r.vmLimit() {
			c.fail(http.StatusBadRequest, "record at %d is past the disk size limit", rec.Off)
			return
		}
		s := seg{off: rec.Off, length: rec.Len, kind: segZero}
		if !rec.Zero {
			s.kind, s.pos = segData, pos
			if _, err = sw.Write(rec.Data); err != nil {
				c.fail(http.StatusInternalServerError, "%v", err)
				return
			}
			pos += int64(rec.Len)
		}
		// The data goes out before the record that names it, so a stop
		// never leaves a record without its data.
		if !rec.Zero {
			if err = sw.Flush(); err == nil {
				err = staging.Sync()
			}
			if err != nil {
				c.fail(http.StatusInternalServerError, "%v", err)
				return
			}
		}
		if err = appendSeg(mw, s); err != nil {
			c.fail(http.StatusInternalServerError, "%v", err)
			return
		}
	}
	if err = sw.Flush(); err == nil {
		err = staging.Sync()
	}
	if err == nil {
		err = mw.Flush()
	}
	if err == nil {
		err = manifest.Sync()
	}
	if err != nil {
		c.fail(http.StatusInternalServerError, "%v", err)
		return
	}
	c.w.Header().Set("Content-Type", "application/json")
	_, _ = c.w.Write([]byte(`{"status":"ok"}`))
}

// receivedCopy is a staged copy stored by the receiver, as a CopySource.
type receivedCopy struct {
	size    int64
	segs    []seg
	byOff   map[int64]seg
	staging io.ReaderAt
	disk    io.ReaderAt // for segSame
}

func (rc *receivedCopy) Size() int64 { return rc.size }

func (rc *receivedCopy) Walk(fn func(qcow2.Extent) error) error {
	for _, s := range rc.segs {
		e := qcow2.Extent{Offset: s.off, Length: int64(s.length), Kind: qcow2.Data}
		if s.kind == segZero {
			e.Kind = qcow2.Zero
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

func (rc *receivedCopy) DataReader(e qcow2.Extent) io.Reader {
	s := rc.byOff[e.Offset]
	if s.kind == segSame {
		return io.NewSectionReader(rc.disk, s.off, int64(s.length))
	}
	return io.NewSectionReader(rc.staging, s.pos, int64(s.length))
}

// momentOf formats a moment for the log.
func momentOf(t *time.Time) string {
	if t == nil {
		return "none"
	}
	return t.UTC().Format(time.RFC3339Nano)
}

func (c *vmCall) commit() {
	var cr CommitRequest
	dec := json.NewDecoder(io.LimitReader(c.req.Body, maxCommitBody))
	if err := dec.Decode(&cr); err != nil {
		c.fail(http.StatusBadRequest, "bad commit request: %v", err)
		return
	}
	if cr.Size <= 0 || cr.To.IsZero() {
		c.fail(http.StatusBadRequest, "size and to are required")
		return
	}
	if cr.Size > c.r.vmLimit() {
		c.fail(http.StatusRequestEntityTooLarge, "disk size %d is above the limit %d", cr.Size, c.r.vmLimit())
		return
	}
	// Failover waits while a commit may be changing the disk.
	if err := c.v.writeLeaseFile(time.Now().Add(leaseTTL)); err != nil {
		c.fail(http.StatusInternalServerError, "writing the lease: %v", err)
		return
	}
	defer c.v.removeLeaseFile()
	if _, err := c.v.recover(); err != nil {
		c.fail(http.StatusInternalServerError, "rolling back an unfinished copy: %v", err)
		return
	}

	// The copy must start from the moment the disk shows now. A full copy
	// may replace a disk whose moment is not known, because it writes every
	// block.
	fi, derr := c.v.diskInfo()
	exists := derr == nil
	if derr != nil && !errors.Is(derr, os.ErrNotExist) {
		c.fail(http.StatusInternalServerError, "%v", derr)
		return
	}
	var cur time.Time
	var known bool
	if exists {
		var merr error
		if cur, known, merr = c.v.moment(); merr != nil {
			c.fail(http.StatusInternalServerError, "%v", merr)
			return
		}
	}
	switch {
	case !exists && cr.From != nil:
		c.conflict("the copy starts from %s but there is no disk", momentOf(cr.From))
		return
	case !exists && !cr.Full:
		c.conflict("there is no disk, so the copy must be a full copy")
		return
	case exists && cr.From == nil && (known || !cr.Full):
		c.conflict("the disk exists; a first copy cannot replace it")
		return
	case exists && cr.From != nil && (!known || !cur.Equal(*cr.From)):
		c.conflict("the disk shows %v, not %s", cur, momentOf(cr.From))
		return
	}
	// A compare answer holds only while the disk is the same.
	if b, err := c.v.root.Open(filepath.Join(c.sessionDir(), baseName)); err == nil {
		saved, _ := io.ReadAll(io.LimitReader(b, 256))
		_ = b.Close()
		now, berr := c.baseMoment()
		if berr != nil {
			c.fail(http.StatusInternalServerError, "%v", berr)
			return
		}
		if string(saved) != now {
			c.fail(http.StatusConflict, "the disk changed after the compare call")
			return
		}
	}

	stored, err := c.loadSession()
	if err != nil {
		c.fail(http.StatusInternalServerError, "%v", err)
		return
	}
	segs := make([]seg, 0, len(stored))
	for _, s := range stored {
		segs = append(segs, s)
	}
	sort.Slice(segs, func(i, j int) bool { return segs[i].off < segs[j].off })
	var oldSize int64
	if exists {
		oldSize = fi.Size()
	}
	var end int64
	for _, s := range segs {
		if s.off < end {
			c.fail(http.StatusBadRequest, "records overlap at %d", s.off)
			return
		}
		end = s.off + int64(s.length)
		if end > cr.Size {
			c.fail(http.StatusBadRequest, "record at %d is past the end of the disk", s.off)
			return
		}
		if s.kind == segSame && end > oldSize {
			c.fail(http.StatusBadRequest, "record at %d is past the end of the old disk", s.off)
			return
		}
	}

	staging, err := c.v.root.Open(filepath.Join(c.sessionDir(), stagingName))
	if errors.Is(err, os.ErrNotExist) && len(segs) == 0 {
		staging, err = nil, nil
	}
	if err != nil {
		c.fail(http.StatusInternalServerError, "%v", err)
		return
	}
	if staging != nil {
		defer func() { _ = staging.Close() }()
	}
	if err = checkDigest(staging, segs, cr.Digest); err != nil {
		c.fail(http.StatusBadRequest, "%v", err)
		return
	}

	rc := &receivedCopy{size: cr.Size, segs: segs, byOff: stored}
	if staging != nil {
		rc.staging = staging
	}
	if exists {
		d, derr2 := c.v.openDisk(os.O_RDONLY)
		if derr2 != nil {
			c.fail(http.StatusInternalServerError, "%v", derr2)
			return
		}
		defer func() { _ = d.Close() }()
		rc.disk = d
	}

	opts := ApplyOptions{To: cr.To, Full: cr.Full, Now: time.Now(), Keep: time.Duration(cr.KeepSeconds) * time.Second}
	switch {
	case cr.From != nil:
		opts.From = *cr.From
	case exists:
		// The old moment is not known. The restore point made from this
		// disk is dated at the epoch.
		opts.From = time.Unix(0, 0).UTC()
	}
	res, err := ApplyCopy(rc, c.disk, opts)
	if err != nil {
		klog.ErrorS(err, "Applying a VM disk copy failed", "disk", c.disk)
		c.fail(http.StatusInternalServerError, "applying the copy: %v", err)
		return
	}
	if err = c.v.writeMomentFile(momentFile{Moment: cr.To, VMGroup: cr.VMGroup}); err != nil {
		c.fail(http.StatusInternalServerError, "recording the moment: %v", err)
		return
	}
	if err = c.v.root.RemoveAll(c.sessionDir()); err != nil {
		klog.ErrorS(err, "Could not remove the upload folder", "dir", c.sessionDir())
	}
	klog.V(2).InfoS("VM disk copy applied", "disk", c.disk, "to", cr.To, "changedBytes", res.ChangedBytes, "rolledBack", res.RolledBack)
	out := CommitResponse{ChangedBytes: res.ChangedBytes, RolledBack: res.RolledBack}
	if points, perr := RestorePoints(c.disk); perr == nil {
		out.RestorePoints = int32(len(points)) // #nosec G115 -- a handful
		if len(points) > 0 {
			out.OldestRestorePoint = &points[0]
		}
	}
	c.w.Header().Set("Content-Type", "application/json")
	b, _ := json.Marshal(out)
	_, _ = c.w.Write(b)
}

// checkDigest recomputes the upload digest from what is stored.
func checkDigest(staging io.ReaderAt, segs []seg, want string) error {
	d := NewUploadDigest()
	for _, s := range segs {
		switch s.kind {
		case segZero:
			d.Add(UploadRecord{Off: s.off, Zero: true, Len: s.length})
		case segData:
			b := make([]byte, s.length)
			if _, err := staging.ReadAt(b, s.pos); err != nil {
				return fmt.Errorf("reading stored record at %d: %w", s.off, err)
			}
			d.Add(UploadRecord{Off: s.off, Data: b})
		}
	}
	if d.Sum() != want {
		return errors.New("the stored records do not match the digest")
	}
	return nil
}
