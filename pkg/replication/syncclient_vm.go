package replication

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"k8s.io/klog/v2"
)

// VMSendArgs describe one VM disk copy to send to a receiver.
type VMSendArgs struct {
	// DestBasePath and SubPath say where the volume is at the far site, as
	// for SyncSubVolume. Disk is the file name of the disk; empty means
	// "disk.img".
	DestBasePath, SubPath, SVName, Disk string
	// CopyID names this attempt. Sending the same copy again with the same
	// ID resumes it.
	CopyID string
	// Src is the staged copy.
	Src CopySource
	// Full means Src holds the whole disk. A full copy ignores From: it uses
	// the moment the far-site disk shows.
	Full bool
	// From is the moment the far-site disk must show; nil for a first copy.
	From *time.Time
	// To is the moment the copy shows.
	To time.Time
	// VMGroup is "namespace/name" of the VM that owns the disk.
	VMGroup string
	// Keep is how long restore points are kept; 0 keeps all.
	Keep time.Duration
}

// VMSendResult says what SendVMDisk did.
type VMSendResult struct {
	CommitResponse
	// SentBytes is the disk data sent in this call.
	SentBytes int64
	// AlreadyApplied is true if the far-site disk already showed To, so
	// nothing was sent.
	AlreadyApplied bool
}

// WrongBaseError means a change copy cannot be applied because the far-site
// disk does not show the moment the copy starts from. The next copy must be a
// full copy.
type WrongBaseError struct {
	DiskMoment string // see DiskMomentHeader
	Message    string
}

func (e *WrongBaseError) Error() string {
	return fmt.Sprintf("the far-site disk is not at the expected moment (it shows %s): %s", e.DiskMoment, e.Message)
}

// vmCopyID turns any copy name into an ID the receiver accepts.
func vmCopyID(name string) string {
	sum := sha256.Sum256([]byte(name))
	return hex.EncodeToString(sum[:16])
}

func (c *SyncClient) vmURL(a VMSendArgs, action string) string {
	u := fmt.Sprintf("%s/api/v1/vmcopy/%s/%s", c.endpoint, url.PathEscape(a.SVName), vmCopyID(a.CopyID))
	if action != "" {
		u += "/" + action
	}
	q := url.Values{"basePath": {a.DestBasePath}, "subPath": {a.SubPath}}
	if a.Disk != "" {
		q.Set("disk", a.Disk)
	}
	return u + "?" + q.Encode()
}

// vmRecords calls fn for each chunk of the copy, in order of offset. A chunk
// of zeros is a zero record.
func vmRecords(src CopySource, full bool, fn func(UploadRecord) error) error {
	return forEachNewChunk(src, full, func(off int64, data []byte) error {
		if allZero(data) {
			return fn(UploadRecord{Off: off, Zero: true, Len: uint32(len(data))}) // #nosec G115 -- at most compareChunk
		}
		return fn(UploadRecord{Off: off, Data: data})
	})
}

// SendVMDisk sends a staged VM disk copy to the receiver and has it applied:
// for a full copy a compare call first, so only chunks that differ cross the
// network; then the records not stored yet; then the commit.
func (c *SyncClient) SendVMDisk(ctx context.Context, a VMSendArgs) (VMSendResult, error) {
	var res VMSendResult
	toText := a.To.UTC().Format(time.RFC3339Nano)

	stored, diskMoment, err := c.vmStatus(ctx, a)
	if err != nil {
		return res, err
	}
	if diskMoment == toText {
		res.AlreadyApplied = true
		return res, nil
	}

	from := a.From
	var need map[int64]bool
	if a.Full {
		var header string
		if need, header, err = c.vmCompare(ctx, a); err != nil {
			return res, err
		}
		switch header {
		case "none", "unknown":
			from = nil
		default:
			t, perr := time.Parse(time.RFC3339Nano, header)
			if perr != nil {
				return res, fmt.Errorf("the receiver sent a bad disk moment %q", header)
			}
			from = &t
		}
	}

	digest := NewUploadDigest()
	pr, pw := io.Pipe()
	var sent int64
	go func() {
		bw := bufio.NewWriterSize(pw, 1<<20)
		werr := vmRecords(a.Src, a.Full, func(rec UploadRecord) error {
			if !rec.Zero && a.Full && !need[rec.Off] {
				return nil // equal to the far-site disk
			}
			digest.Add(rec)
			if !rec.Zero && stored[rec.Off] {
				return nil // stored by an earlier try
			}
			if !rec.Zero {
				sent += int64(len(rec.Data))
			}
			return WriteUploadRecord(bw, rec)
		})
		if werr == nil {
			werr = bw.Flush()
		}
		_ = pw.CloseWithError(werr)
	}()
	if err = c.vmDo(ctx, http.MethodPut, c.vmURL(a, ""), pr, "", nil); err != nil {
		_ = pr.CloseWithError(err)
		return res, fmt.Errorf("uploading records: %w", err)
	}
	res.SentBytes = sent

	req := CommitRequest{Size: a.Src.Size(), From: from, To: a.To, Full: a.Full, Digest: digest.Sum(),
		VMGroup: a.VMGroup, KeepSeconds: int64(a.Keep / time.Second)}
	body, err := json.Marshal(req)
	if err != nil {
		return res, err
	}
	var out CommitResponse
	err = c.vmDo(ctx, http.MethodPost, c.vmURL(a, "commit"), bytes.NewReader(body), "application/json", &out)
	var wb *WrongBaseError
	if errors.As(err, &wb) {
		if wb.DiskMoment == toText {
			// A try that ended after the commit applied the copy.
			res.AlreadyApplied = true
			return res, nil
		}
		return res, err
	}
	if err != nil {
		return res, fmt.Errorf("commit: %w", err)
	}
	res.CommitResponse = out
	klog.V(2).InfoS("VM disk copy sent", "subVolume", a.SVName, "sentBytes", sent, "changedBytes", out.ChangedBytes)
	return res, nil
}

// vmStatus returns the offsets stored for the copy and the moment the disk shows.
func (c *SyncClient) vmStatus(ctx context.Context, a VMSendArgs) (map[int64]bool, string, error) {
	var st StatusResponse
	if err := c.vmDo(ctx, http.MethodGet, c.vmURL(a, ""), nil, "", &st); err != nil {
		return nil, "", fmt.Errorf("status: %w", err)
	}
	stored := make(map[int64]bool, len(st.Offsets))
	for _, o := range st.Offsets {
		stored[o] = true
	}
	return stored, st.DiskMoment, nil
}

// vmCompare sends the hashes of a full copy and returns the offsets of the
// chunks that differ, and the moment the far-site disk shows.
func (c *SyncClient) vmCompare(ctx context.Context, a VMSendArgs) (map[int64]bool, string, error) {
	pr, pw := io.Pipe()
	go func() {
		bw := bufio.NewWriterSize(pw, 1<<20)
		var buf []byte
		err := vmRecords(a.Src, true, func(rec UploadRecord) error {
			e := CompareEntry{Off: rec.Off, Len: rec.Len, Zero: rec.Zero}
			if !rec.Zero {
				e.Len = uint32(len(rec.Data)) // #nosec G115 -- at most compareChunk
				e.Hash = sha256.Sum256(rec.Data)
			}
			buf = AppendCompareEntry(buf[:0], e)
			_, werr := bw.Write(buf)
			return werr
		})
		if err == nil {
			err = bw.Flush()
		}
		_ = pw.CloseWithError(err)
	}()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.vmURL(a, "compare"), pr)
	if err != nil {
		_ = pr.Close()
		return nil, "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.authToken)
	req.Header.Set("Content-Type", "application/octet-stream")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		_ = pr.CloseWithError(err)
		return nil, "", fmt.Errorf("compare: %w", err)
	}
	defer closeBody(resp.Body)
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, "", fmt.Errorf("compare returned %d: %s", resp.StatusCode, b)
	}
	need := map[int64]bool{}
	br := bufio.NewReader(resp.Body)
	var buf [8]byte
	for {
		if _, err = io.ReadFull(br, buf[:]); err != nil {
			break
		}
		need[int64(binary.BigEndian.Uint64(buf[:]))] = true // #nosec G115 -- offsets we sent
	}
	if !errors.Is(err, io.EOF) {
		return nil, "", fmt.Errorf("reading the compare answer: %w", err)
	}
	return need, resp.Header.Get(DiskMomentHeader), nil
}

// vmDo makes one call. A JSON answer goes into out, if out is not nil. A 409
// becomes a *WrongBaseError.
func (c *SyncClient) vmDo(ctx context.Context, method, u string, body io.Reader, contentType string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.authToken)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer closeBody(resp.Body)
	if resp.StatusCode == http.StatusConflict {
		var cf Conflict
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		if json.Unmarshal(b, &cf) != nil {
			cf = Conflict{Message: string(b), DiskMoment: "unknown"}
		}
		return &WrongBaseError{DiskMoment: cf.DiskMoment, Message: cf.Message}
	}
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return fmt.Errorf("receiver returned %d: %s", resp.StatusCode, bytes.TrimSpace(b))
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, resp.Body)
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// UploadVMMetadata uploads the SubVolume metadata file for a VM disk, after
// its copy was applied.
func (c *SyncClient) UploadVMMetadata(ctx context.Context, destBasePath, subPath, svName string, metadata []byte) error {
	return c.uploadMetadata(ctx, destBasePath, subPath, svName, metadata)
}
