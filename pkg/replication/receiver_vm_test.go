package replication

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
)

const testSV = "pvc-0123456789abcdef0123456789abcdef0123"

type vmTestClient struct {
	t    *testing.T
	srv  *httptest.Server
	rcv  *Receiver
	base string
}

func newVMTestClient(t *testing.T) *vmTestClient {
	t.Helper()
	rcv, base := createTestReceiver(t)
	srv := httptest.NewServer(rcv.httpServer.Handler)
	t.Cleanup(srv.Close)
	return &vmTestClient{t: t, srv: srv, rcv: rcv, base: base}
}

func (c *vmTestClient) diskPath() string { return filepath.Join(c.base, "pvcs", testSV, "disk.img") }

func (c *vmTestClient) url(copyID, action string) string {
	p := "/api/v1/vmcopy/" + testSV + "/" + copyID
	if action != "" {
		p += "/" + action
	}
	q := url.Values{"basePath": {"/"}, "subPath": {"/pvcs/" + testSV}}
	return c.srv.URL + p + "?" + q.Encode()
}

func (c *vmTestClient) do(method, u string, body io.Reader, token string) (int, []byte) {
	c.t.Helper()
	req, err := http.NewRequest(method, u, body)
	if err != nil {
		c.t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		c.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

// chunks lists the chunks of a copy the way the sync client does.
func chunks(t *testing.T, src CopySource, full bool) []UploadRecord {
	t.Helper()
	var recs []UploadRecord
	err := forEachNewChunk(src, full, func(off int64, data []byte) error {
		if allZero(data) {
			recs = append(recs, UploadRecord{Off: off, Zero: true, Len: uint32(len(data))})
		} else {
			recs = append(recs, UploadRecord{Off: off, Data: append([]byte(nil), data...)})
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return recs
}

func (c *vmTestClient) compare(copyID string, recs []UploadRecord) (int, map[int64]bool) {
	c.t.Helper()
	var body []byte
	for _, r := range recs {
		e := CompareEntry{Off: r.Off, Zero: r.Zero, Len: r.Len}
		if !r.Zero {
			e.Len = uint32(len(r.Data))
			e.Hash = sha256.Sum256(r.Data)
		}
		body = AppendCompareEntry(body, e)
	}
	code, resp := c.do(http.MethodPost, c.url(copyID, "compare"), bytes.NewReader(body), "test-token-123")
	need := map[int64]bool{}
	if code != http.StatusOK {
		return code, nil
	}
	for i := 0; i+8 <= len(resp); i += 8 {
		need[int64(be64(resp[i:]))] = true
	}
	return code, need
}

func be64(b []byte) uint64 {
	var v uint64
	for _, x := range b[:8] {
		v = v<<8 | uint64(x)
	}
	return v
}

func (c *vmTestClient) upload(copyID string, recs []UploadRecord, token string) int {
	c.t.Helper()
	var body bytes.Buffer
	for _, r := range recs {
		if err := WriteUploadRecord(&body, r); err != nil {
			c.t.Fatal(err)
		}
	}
	code, _ := c.do(http.MethodPut, c.url(copyID, ""), &body, token)
	return code
}

func (c *vmTestClient) commit(copyID string, req CommitRequest) (int, CommitResponse) {
	c.t.Helper()
	b, _ := json.Marshal(req)
	code, resp := c.do(http.MethodPost, c.url(copyID, "commit"), bytes.NewReader(b), "test-token-123")
	var out CommitResponse
	if code == http.StatusOK {
		if err := json.Unmarshal(resp, &out); err != nil {
			c.t.Fatal(err)
		}
	}
	return code, out
}

func digestOf(recs []UploadRecord) string {
	d := NewUploadDigest()
	for _, r := range recs {
		d.Add(r)
	}
	return d.Sum()
}

// send runs a whole copy: compare for a full copy, upload what is needed,
// commit.
func (c *vmTestClient) send(copyID string, src CopySource, from *time.Time, to time.Time, full bool) (int, CommitResponse) {
	c.t.Helper()
	recs := chunks(c.t, src, full)
	send := recs
	if full {
		code, need := c.compare(copyID, recs)
		if code != http.StatusOK {
			return code, CommitResponse{}
		}
		send = nil
		for _, r := range recs {
			if r.Zero || need[r.Off] {
				send = append(send, r)
			}
		}
	}
	if code := c.upload(copyID, send, "test-token-123"); code != http.StatusOK {
		return code, CommitResponse{}
	}
	return c.commit(copyID, CommitRequest{Size: src.Size(), From: from, To: to, Full: full, Digest: digestOf(send), VMGroup: "ns/vm1"})
}

func ptr(t time.Time) *time.Time { return &t }

func TestReceiverVM_FirstCopyMakesTheDisk(t *testing.T) {
	c := newVMTestClient(t)
	disk := patterned(4 * chunk)
	src := change(4*chunk, 0, nil, nil)
	for i := int64(0); i < 4; i++ {
		src.exts = append(src.exts, qcow2.Extent{Offset: i * chunk, Length: chunk, Kind: qcow2.Data})
		src.data[i*chunk] = disk[i*chunk : (i+1)*chunk]
	}
	code, _ := c.send("aaaaaaaa-1", src, nil, t1, true)
	if code != http.StatusOK {
		t.Fatalf("commit = %d", code)
	}
	if got := readDisk(t, c.diskPath()); !bytes.Equal(got, disk) {
		t.Fatal("disk contents differ from the copy")
	}
	m, ok, err := CurrentMoment(c.diskPath())
	if err != nil || !ok || !m.Equal(t1) {
		t.Fatalf("moment = %v %v %v, want %v", m, ok, err, t1)
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(c.diskPath()), incomingDirName, "aaaaaaaa-1")); !os.IsNotExist(err) {
		t.Fatal("the upload folder was not removed after the commit")
	}
}

func TestReceiverVM_ChangeCopyWritesUndoAndNeedsNoCompare(t *testing.T) {
	c := newVMTestClient(t)
	writeFirst := func() {
		disk := patterned(4 * chunk)
		src := &memSource{size: 4 * chunk, data: map[int64][]byte{}}
		for i := int64(0); i < 4; i++ {
			src.exts = append(src.exts, qcow2.Extent{Offset: i * chunk, Length: chunk, Kind: qcow2.Data})
			src.data[i*chunk] = disk[i*chunk : (i+1)*chunk]
		}
		if code, _ := c.send("aaaaaaaa-1", src, nil, t1, true); code != http.StatusOK {
			t.Fatalf("first commit = %d", code)
		}
	}
	writeFirst()

	code, res := c.send("aaaaaaaa-2", change(4*chunk, 0x7f, []int64{chunk}, nil), ptr(t1), t2, false)
	if code != http.StatusOK {
		t.Fatalf("change commit = %d", code)
	}
	if res.ChangedBytes != chunk {
		t.Fatalf("changed bytes = %d, want %d", res.ChangedBytes, chunk)
	}
	got := readDisk(t, c.diskPath())
	if got[chunk] != 0x7f || got[0] != 1 {
		t.Fatalf("disk has wrong data: %x %x", got[0], got[chunk])
	}
	pts, err := RestorePoints(c.diskPath())
	if err != nil || len(pts) != 1 || !pts[0].Equal(t1) {
		t.Fatalf("restore points = %v %v", pts, err)
	}
}

func TestReceiverVM_ComparePassSendsOnlyDifferingChunks(t *testing.T) {
	c := newVMTestClient(t)
	base := patterned(4 * chunk)
	if err := os.MkdirAll(filepath.Dir(c.diskPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	writeDisk(t, c.diskPath(), base)
	if err := writeMoment(filepath.Dir(c.diskPath()), t1, "ns/vm1"); err != nil {
		t.Fatal(err)
	}

	// The full copy equals the disk except chunk 2.
	want := append([]byte(nil), base...)
	for i := 2 * chunk; i < 3*chunk; i++ {
		want[i] = 0xee
	}
	src := &memSource{size: 4 * chunk, data: map[int64][]byte{}}
	for i := int64(0); i < 4; i++ {
		src.exts = append(src.exts, qcow2.Extent{Offset: i * chunk, Length: chunk, Kind: qcow2.Data})
		src.data[i*chunk] = want[i*chunk : (i+1)*chunk]
	}
	recs := chunks(t, src, true)
	code, need := c.compare("bbbbbbbb-1", recs)
	if code != http.StatusOK {
		t.Fatalf("compare = %d", code)
	}
	if len(need) != 1 || !need[2*chunk] {
		t.Fatalf("receiver asked for %v, want only chunk 2", need)
	}
	code, res := c.send("bbbbbbbb-1", src, ptr(t1), t2, true)
	if code != http.StatusOK {
		t.Fatalf("commit = %d", code)
	}
	if !bytes.Equal(readDisk(t, c.diskPath()), want) {
		t.Fatal("disk differs from the copy")
	}
	if res.ChangedBytes != chunk {
		t.Fatalf("changed = %d, want %d", res.ChangedBytes, chunk)
	}
}

func TestReceiverVM_WrongFromMomentIs409(t *testing.T) {
	c := newVMTestClient(t)
	first := change(2*chunk, 5, []int64{0, chunk}, nil)
	if code, _ := c.send("cccccccc-1", first, nil, t1, true); code != http.StatusOK {
		t.Fatalf("first = %d", code)
	}
	code, _ := c.send("cccccccc-2", change(2*chunk, 9, []int64{0}, nil), ptr(t0), t2, false)
	if code != http.StatusConflict {
		t.Fatalf("wrong from = %d, want 409", code)
	}
	if b := readDisk(t, c.diskPath()); b[0] != 5 {
		t.Fatal("the disk changed although the commit was refused")
	}
	// A new disk copy onto an existing disk is also refused.
	code, _ = c.send("cccccccc-3", first, nil, t2, true)
	if code != http.StatusConflict {
		t.Fatalf("nil from on an existing disk = %d, want 409", code)
	}
}

func TestReceiverVM_DiskChangedSinceCompareIs409(t *testing.T) {
	c := newVMTestClient(t)
	first := change(2*chunk, 5, []int64{0, chunk}, nil)
	if code, _ := c.send("dddddddd-1", first, nil, t1, true); code != http.StatusOK {
		t.Fatal("first copy failed")
	}
	full := change(2*chunk, 5, []int64{0, chunk}, nil)
	recs := chunks(t, full, true)
	if code, _ := c.compare("dddddddd-2", recs); code != http.StatusOK {
		t.Fatal("compare failed")
	}
	// Another copy lands between the compare and the commit.
	if code, _ := c.send("dddddddd-3", change(2*chunk, 8, []int64{0}, nil), ptr(t1), t2, false); code != http.StatusOK {
		t.Fatal("interleaved copy failed")
	}
	code, _ := c.commit("dddddddd-2", CommitRequest{Size: 2 * chunk, From: ptr(t2), To: t3, Full: true, Digest: digestOf(nil)})
	if code != http.StatusConflict {
		t.Fatalf("commit after the disk changed = %d, want 409", code)
	}
}

func TestReceiverVM_BadDigestIs400AndDiskUntouched(t *testing.T) {
	c := newVMTestClient(t)
	first := change(2*chunk, 5, []int64{0, chunk}, nil)
	if code, _ := c.send("eeeeeeee-1", first, nil, t1, true); code != http.StatusOK {
		t.Fatal("first copy failed")
	}
	recs := chunks(t, change(2*chunk, 9, []int64{0}, nil), false)
	if code := c.upload("eeeeeeee-2", recs, "test-token-123"); code != http.StatusOK {
		t.Fatal("upload failed")
	}
	code, _ := c.commit("eeeeeeee-2", CommitRequest{Size: 2 * chunk, From: ptr(t1), To: t2, Digest: "00"})
	if code != http.StatusBadRequest {
		t.Fatalf("bad digest = %d, want 400", code)
	}
	if readDisk(t, c.diskPath())[0] != 5 {
		t.Fatal("the disk changed although the digest was wrong")
	}
}

func TestReceiverVM_RefusesBadRequests(t *testing.T) {
	c := newVMTestClient(t)
	ok := chunks(t, change(2*chunk, 5, []int64{0}, nil), false)

	t.Run("wrong token", func(t *testing.T) {
		if code := c.upload("ffffffff-1", ok, "wrong"); code != http.StatusUnauthorized {
			t.Fatalf("= %d", code)
		}
	})
	t.Run("record past the end of the disk", func(t *testing.T) {
		if code := c.upload("ffffffff-2", ok, "test-token-123"); code != http.StatusOK {
			t.Fatal("upload failed")
		}
		code, _ := c.commit("ffffffff-2", CommitRequest{Size: chunk / 2, To: t1, Full: true, Digest: digestOf(ok)})
		if code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400", code)
		}
	})
	t.Run("size above the limit", func(t *testing.T) {
		c.rcv.SetMaxVMDiskBytes(chunk)
		defer c.rcv.SetMaxVMDiskBytes(DefaultMaxVMDiskBytes)
		if code := c.upload("ffffffff-3", ok, "test-token-123"); code != http.StatusOK {
			t.Fatal("upload failed")
		}
		code, _ := c.commit("ffffffff-3", CommitRequest{Size: 2 * chunk, To: t1, Full: true, Digest: digestOf(ok)})
		if code != http.StatusRequestEntityTooLarge {
			t.Fatalf("= %d, want 413", code)
		}
	})
	t.Run("overlapping records", func(t *testing.T) {
		recs := []UploadRecord{
			{Off: 0, Data: bytes.Repeat([]byte{1}, chunk)},
			{Off: chunk / 2, Data: bytes.Repeat([]byte{2}, chunk)},
		}
		if code := c.upload("ffffffff-4", recs, "test-token-123"); code != http.StatusOK {
			t.Fatal("upload failed")
		}
		code, _ := c.commit("ffffffff-4", CommitRequest{Size: 2 * chunk, To: t1, Full: true, Digest: digestOf(recs)})
		if code != http.StatusBadRequest {
			t.Fatalf("= %d, want 400", code)
		}
	})
	t.Run("bad names", func(t *testing.T) {
		for _, u := range []string{
			c.srv.URL + "/api/v1/vmcopy/not-a-volume/aaaaaaaa-1?basePath=/&subPath=/pvcs/x",
			c.srv.URL + "/api/v1/vmcopy/" + testSV + "/..%2f..?basePath=/&subPath=/pvcs/" + testSV,
			c.srv.URL + "/api/v1/vmcopy/" + testSV + "/aaaaaaaa-1?basePath=/&subPath=/pvcs/" + testSV + "&disk=..%2fx",
		} {
			code, _ := c.do(http.MethodPut, u, bytes.NewReader(nil), "test-token-123")
			if code != http.StatusBadRequest && code != http.StatusNotFound {
				t.Fatalf("%s = %d, want 400", u, code)
			}
		}
	})
}

func TestReceiverVM_UploadResumes(t *testing.T) {
	c := newVMTestClient(t)
	recs := chunks(t, change(4*chunk, 3, []int64{0, chunk, 2 * chunk}, nil), false)
	if code := c.upload("99999999-1", recs[:2], "test-token-123"); code != http.StatusOK {
		t.Fatal("first part failed")
	}
	code, body := c.do(http.MethodGet, c.url("99999999-1", ""), nil, "test-token-123")
	if code != http.StatusOK {
		t.Fatalf("status = %d", code)
	}
	var st struct {
		Offsets []int64 `json:"offsets"`
	}
	if err := json.Unmarshal(body, &st); err != nil || len(st.Offsets) != 2 {
		t.Fatalf("status body %s: %v", body, err)
	}
	// Send the rest, and send the first chunk again: the repeat is harmless.
	if up := c.upload("99999999-1", recs[1:], "test-token-123"); up != http.StatusOK {
		t.Fatal("rest failed")
	}
	code, _ = c.commit("99999999-1", CommitRequest{Size: 4 * chunk, To: t1, Digest: digestOf(recs)})
	// The disk does not exist, so a change copy cannot make it.
	if code != http.StatusConflict {
		t.Fatalf("change copy onto no disk = %d, want 409", code)
	}
	if code = c.upload("99999999-2", recs[:1], "test-token-123"); code != http.StatusOK {
		t.Fatal("upload failed")
	}
}

func TestReceiverVM_OneCallPerDiskAtATime(t *testing.T) {
	c := newVMTestClient(t)
	release := make(chan struct{})
	started := make(chan struct{})
	pr, pw := io.Pipe()
	go func() {
		req, _ := http.NewRequest(http.MethodPut, c.url("88888888-1", ""), pr)
		req.Header.Set("Authorization", "Bearer test-token-123")
		close(started)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			_ = resp.Body.Close()
		}
		close(release)
	}()
	<-started
	// Let the first call take the lock: write one record and wait until it
	// is stored.
	var rec bytes.Buffer
	_ = WriteUploadRecord(&rec, UploadRecord{Off: 0, Data: bytes.Repeat([]byte{1}, chunk)})
	_, _ = pw.Write(rec.Bytes())
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, _ := c.do(http.MethodPut, c.url("88888888-2", ""), bytes.NewReader(nil), "test-token-123")
		if code == http.StatusTooManyRequests {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("second call got %d, want 429", code)
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = pw.Close()
	<-release
}

func TestReceiverVM_RemovesOldUploadFolders(t *testing.T) {
	c := newVMTestClient(t)
	dir := filepath.Join(c.base, "pvcs", testSV)
	old := filepath.Join(dir, incomingDirName, "77777777-0ad0")
	if err := os.MkdirAll(old, 0o770); err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-48 * time.Hour)
	if err := os.Chtimes(old, past, past); err != nil {
		t.Fatal(err)
	}
	recs := chunks(t, change(2*chunk, 3, []int64{0}, nil), false)
	if code := c.upload("77777777-0e30", recs, "test-token-123"); code != http.StatusOK {
		t.Fatal("upload failed")
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatal("old upload folder was not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, incomingDirName, "77777777-0e30")); err != nil {
		t.Fatal("new upload folder is gone")
	}
}

func TestReceiverVM_LinkInVolumeCannotLeadOut(t *testing.T) {
	c := newVMTestClient(t)
	dir := filepath.Join(c.base, "pvcs", testSV)
	outside := t.TempDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, incomingDirName)); err != nil {
		t.Fatal(err)
	}
	recs := chunks(t, change(2*chunk, 3, []int64{0}, nil), false)
	if code := c.upload("66666666-1", recs, "test-token-123"); code == http.StatusOK {
		t.Fatal("upload followed a link out of the volume")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("files were written outside the volume: %v", entries)
	}
}
