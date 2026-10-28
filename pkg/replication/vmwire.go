package replication

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"hash"
	"io"
	"time"
)

// The wire format of the VM disk calls (see receiver_vm.go). The sync client
// and the receiver both use these helpers.

// CompareEntry describes one chunk of a full copy. The client sends the list
// so the receiver can say which chunks differ from its own disk.
type CompareEntry struct {
	Off  int64
	Len  uint32
	Zero bool // the chunk is all zeros; no hash, and nothing to compare
	Hash [sha256.Size]byte
}

const compareEntrySize = 8 + 4 + 1 + sha256.Size

// AppendCompareEntry adds an entry to b.
func AppendCompareEntry(b []byte, e CompareEntry) []byte {
	b = binary.BigEndian.AppendUint64(b, uint64(e.Off)) // #nosec G115 -- checked by the reader
	b = binary.BigEndian.AppendUint32(b, e.Len)
	if e.Zero {
		b = append(b, 1)
	} else {
		b = append(b, 0)
	}
	return append(b, e.Hash[:]...)
}

// ReadCompareEntry reads one entry. It returns io.EOF at a clean end.
func ReadCompareEntry(r io.Reader) (CompareEntry, error) {
	var buf [compareEntrySize]byte
	if _, err := io.ReadFull(r, buf[:]); err != nil {
		return CompareEntry{}, err
	}
	var e CompareEntry
	off := binary.BigEndian.Uint64(buf[0:8])
	if off > 1<<62 {
		return e, fmt.Errorf("offset %d is out of range", off)
	}
	e.Off = int64(off) // #nosec G115 -- range checked above
	e.Len = binary.BigEndian.Uint32(buf[8:12])
	e.Zero = buf[12] == 1
	if buf[12] > 1 {
		return e, fmt.Errorf("bad entry kind %d", buf[12])
	}
	copy(e.Hash[:], buf[13:])
	return e, nil
}

// UploadRecord is one chunk of an upload: data, or a run of zeros.
type UploadRecord struct {
	Off  int64
	Zero bool
	Len  uint32 // for zeros; for data it is len(Data)
	Data []byte
}

// WriteUploadRecord writes a record to w.
func WriteUploadRecord(w io.Writer, rec UploadRecord) error {
	n := rec.Len
	kind := byte(1)
	if !rec.Zero {
		kind = 0
		n = uint32(len(rec.Data)) // #nosec G115 -- a chunk is at most compareChunk
	}
	var hdr [13]byte
	hdr[0] = kind
	binary.BigEndian.PutUint64(hdr[1:], uint64(rec.Off)) // #nosec G115 -- checked by the reader
	binary.BigEndian.PutUint32(hdr[9:], n)
	if _, err := w.Write(hdr[:]); err != nil {
		return err
	}
	if rec.Zero {
		return nil
	}
	_, err := w.Write(rec.Data)
	return err
}

// ReadUploadRecord reads one record. It returns io.EOF at a clean end. A
// record with data longer than max is refused.
func ReadUploadRecord(r io.Reader, max uint32) (UploadRecord, error) {
	var hdr [13]byte
	var rec UploadRecord
	if _, err := io.ReadFull(r, hdr[:]); err != nil {
		return rec, err
	}
	if hdr[0] > 1 {
		return rec, fmt.Errorf("bad record kind %d", hdr[0])
	}
	off := binary.BigEndian.Uint64(hdr[1:9])
	if off > 1<<62 {
		return rec, fmt.Errorf("offset %d is out of range", off)
	}
	rec.Off = int64(off) // #nosec G115 -- range checked above
	rec.Len = binary.BigEndian.Uint32(hdr[9:13])
	if rec.Len == 0 || rec.Len > max {
		return rec, fmt.Errorf("record length %d is out of range", rec.Len)
	}
	rec.Zero = hdr[0] == 1
	if rec.Zero {
		return rec, nil
	}
	rec.Data = make([]byte, rec.Len)
	if _, err := io.ReadFull(r, rec.Data); err != nil {
		return rec, io.ErrUnexpectedEOF
	}
	return rec, nil
}

// UploadDigest is a hash over the records of an upload, added in order of
// offset. The client sends it with the commit call, and the receiver
// computes the same from what it stored.
type UploadDigest struct{ h hash.Hash }

// NewUploadDigest starts a digest.
func NewUploadDigest() *UploadDigest { return &UploadDigest{h: sha256.New()} }

// Add adds a record.
func (d *UploadDigest) Add(rec UploadRecord) {
	n := rec.Len
	kind := byte(1)
	if !rec.Zero {
		kind = 0
		n = uint32(len(rec.Data)) // #nosec G115 -- a chunk is at most compareChunk
	}
	var hdr [13]byte
	hdr[0] = kind
	binary.BigEndian.PutUint64(hdr[1:], uint64(rec.Off)) // #nosec G115 -- checked by the reader
	binary.BigEndian.PutUint32(hdr[9:], n)
	d.h.Write(hdr[:])
	if !rec.Zero {
		d.h.Write(rec.Data)
	}
}

// Sum returns the digest as hex.
func (d *UploadDigest) Sum() string { return hex.EncodeToString(d.h.Sum(nil)) }

// CommitRequest is the body of the commit call.
type CommitRequest struct {
	// Size is the virtual size of the disk after the copy.
	Size int64 `json:"size"`
	// From is the moment the far-site disk must show now; nil for a new disk.
	From *time.Time `json:"from,omitempty"`
	// To is the moment the copy shows.
	To time.Time `json:"to"`
	// Full means the copy holds the whole disk.
	Full bool `json:"full"`
	// Digest is the UploadDigest of the records the client uploaded.
	Digest string `json:"digest"`
	// VMGroup is "namespace/name" of the VM that owns the disk.
	VMGroup string `json:"vmGroup,omitempty"`
	// KeepSeconds is how long restore points are kept; 0 keeps all.
	KeepSeconds int64 `json:"keepSeconds,omitempty"`
}

// CommitResponse is the answer to the commit call.
type CommitResponse struct {
	ChangedBytes int64 `json:"changedBytes"`
	RolledBack   bool  `json:"rolledBack"`
	// RestorePoints is how many restore points the far-site disk has now,
	// and OldestRestorePoint the oldest of them.
	RestorePoints      int32      `json:"restorePoints"`
	OldestRestorePoint *time.Time `json:"oldestRestorePoint,omitempty"`
	// RestorePointMoments lists them, oldest first. The source uses them to
	// find the moments all disks of a VM share.
	RestorePointMoments []time.Time `json:"restorePointMoments,omitempty"`
}

// DiskMomentHeader is the response header of the compare call. It holds the
// moment the far-site disk shows (RFC 3339), "none" if there is no disk, or
// "unknown" if the disk exists but its moment is not known.
const DiskMomentHeader = "Repl-Disk-Moment"

// Conflict is the body of a 409 answer to the commit call.
type Conflict struct {
	Message string `json:"message"`
	// DiskMoment has the same values as DiskMomentHeader.
	DiskMoment string `json:"diskMoment"`
}

// StatusResponse is the answer to the status call.
type StatusResponse struct {
	// Offsets are the offsets of the data records stored so far.
	Offsets []int64 `json:"offsets"`
	// DiskMoment has the same values as DiskMomentHeader.
	DiskMoment string `json:"diskMoment"`
}
