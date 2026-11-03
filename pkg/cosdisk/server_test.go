package cosdisk

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

const testToken = "0123456789abcdef0123456789abcdef"

// tlsDisk serves a disk of random bytes over HTTPS with a session certificate.
func tlsDisk(t *testing.T, data []byte) (*httptest.Server, *http.Client) {
	t.Helper()
	certPEM, keyPEM, err := browse.NewSessionCert(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := browse.ServerTLSConfig(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	d := New(&memSource{data: data}, Options{SegmentSize: 1024, ReadAhead: 2, MaxBytes: 8192})
	t.Cleanup(func() { _ = d.Close() })
	h, err := NewHandler(d, testToken)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = cfg
	ts.StartTLS()
	t.Cleanup(ts.Close)
	c, err := browse.PinnedClient(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	return ts, c
}

func do(t *testing.T, c *http.Client, method, url, token, rng string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, url, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func TestServerRefusesARequestWithoutTheToken(t *testing.T) {
	ts, c := tlsDisk(t, newData(4096))
	for _, tok := range []string{"", "wrong", strings.Repeat("a", 32)} {
		if got := do(t, c, http.MethodGet, ts.URL+"/disk", tok, "bytes=0-9").StatusCode; got != http.StatusUnauthorized {
			t.Errorf("token %q: status %d, want 401", tok, got)
		}
	}
}

func TestServerOnlyReadsAndOnlyTheDisk(t *testing.T) {
	ts, c := tlsDisk(t, newData(4096))
	for _, m := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodPatch} {
		if got := do(t, c, m, ts.URL+"/disk", testToken, "").StatusCode; got != http.StatusMethodNotAllowed {
			t.Errorf("%s: status %d, want 405", m, got)
		}
	}
	for _, p := range []string{"/", "/disk/", "/disk/x", "/etc/passwd", "/../disk", "/disk?x=1"} {
		resp := do(t, c, http.MethodGet, ts.URL+p, testToken, "bytes=0-9")
		if p == "/disk?x=1" {
			continue // a query is ignored; the path is still /disk
		}
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, resp.StatusCode)
		}
	}
}

func TestServerAnswersRangesWithTheRightBytes(t *testing.T) {
	data := newData(10_000)
	ts, c := tlsDisk(t, data)
	head := do(t, c, http.MethodHead, ts.URL+"/disk", testToken, "")
	if head.StatusCode != http.StatusOK || head.ContentLength != int64(len(data)) {
		t.Fatalf("HEAD = %d, %d bytes", head.StatusCode, head.ContentLength)
	}
	resp := do(t, c, http.MethodGet, ts.URL+"/disk", testToken, "bytes=1500-3499")
	if resp.StatusCode != http.StatusPartialContent {
		t.Fatalf("status %d, want 206", resp.StatusCode)
	}
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	if !bytes.Equal(buf.Bytes(), data[1500:3500]) {
		t.Error("wrong bytes")
	}
}

func TestNewHandlerRefusesAShortToken(t *testing.T) {
	d := New(&memSource{data: newData(10)}, Options{})
	defer func() { _ = d.Close() }()
	if _, err := NewHandler(d, "short"); err == nil {
		t.Fatal("want an error for a short token")
	}
}

func TestRemoteSourceReadsThroughTheServer(t *testing.T) {
	data := newData(50_000)
	ts, c := tlsDisk(t, data)
	src, err := NewRemoteSource(context.Background(), c, ts.URL+"/disk", testToken)
	if err != nil {
		t.Fatal(err)
	}
	if src.Size() != int64(len(data)) {
		t.Fatalf("size = %d", src.Size())
	}
	got := make([]byte, 7000)
	if n, err := src.ReadRange(context.Background(), got, 12_345); err != nil || n != 7000 || !bytes.Equal(got, data[12_345:19_345]) {
		t.Fatalf("ReadRange = %d, %v", n, err)
	}
}

func TestRemoteSourceRefusesPlainHTTPAndABadToken(t *testing.T) {
	ts, c := tlsDisk(t, newData(100))
	if _, err := NewRemoteSource(context.Background(), c, "http://10.0.0.1/disk", testToken); err == nil {
		t.Error("plain http was accepted")
	}
	if _, err := NewRemoteSource(context.Background(), c, ts.URL+"/disk", "wrong-token-wrong-token-wrong-token"); err == nil {
		t.Error("a wrong token was accepted")
	}
}

func tlsDiskWithList(t *testing.T, list []byte, size int64) (*httptest.Server, *http.Client) {
	t.Helper()
	certPEM, keyPEM, err := browse.NewSessionCert(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := browse.ServerTLSConfig(certPEM, keyPEM)
	d := New(&memSource{data: newData(100)}, Options{})
	t.Cleanup(func() { _ = d.Close() })
	h, err := NewHandler(d, testToken, WithFileList(func(context.Context) (io.ReadCloser, int64, error) {
		if list == nil {
			return nil, 0, ErrNoFileList
		}
		return io.NopCloser(bytes.NewReader(list)), size, nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = cfg
	ts.StartTLS()
	t.Cleanup(ts.Close)
	c, _ := browse.PinnedClient(certPEM)
	return ts, c
}

func TestFileListIsServedWithTheToken(t *testing.T) {
	list := []byte("not parsed here")
	ts, c := tlsDiskWithList(t, list, int64(len(list)))
	if got := do(t, c, http.MethodGet, ts.URL+FileListPath, "", "").StatusCode; got != http.StatusUnauthorized {
		t.Errorf("no token: %d", got)
	}
	rc, err := FetchFileList(context.Background(), c, ts.URL+FileListPath, testToken, 1<<20)
	if err != nil || rc == nil {
		t.Fatalf("FetchFileList = %v, %v", rc, err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, list) {
		t.Errorf("got %q", got)
	}
}

func TestFileListIsMissingWhenThereIsNone(t *testing.T) {
	ts, c := tlsDiskWithList(t, nil, 0)
	rc, err := FetchFileList(context.Background(), c, ts.URL+FileListPath, testToken, 1<<20)
	if err != nil || rc != nil {
		t.Fatalf("FetchFileList = %v, %v; want nil, nil", rc, err)
	}
}

func TestFileListThatIsTooBigIsNotServedOrRead(t *testing.T) {
	big := make([]byte, 5000)
	ts, c := tlsDiskWithList(t, big, int64(len(big)))
	rc, err := FetchFileList(context.Background(), c, ts.URL+FileListPath, testToken, 1000)
	if err == nil {
		_ = rc.Close()
		t.Fatal("a list over the client's limit must be refused")
	}
	// The server refuses a list over its own limit, whatever size it says.
	ts2, c2 := tlsDiskWithList(t, big, MaxFileListBytes+1)
	if got := do(t, c2, http.MethodGet, ts2.URL+FileListPath, testToken, "").StatusCode; got != http.StatusNotFound {
		t.Errorf("an oversize list: status %d, want 404", got)
	}
}

func TestHandlerWithoutAFileListHasNoSuchPath(t *testing.T) {
	ts, c := tlsDisk(t, newData(100))
	if got := do(t, c, http.MethodGet, ts.URL+FileListPath, testToken, "").StatusCode; got != http.StatusNotFound {
		t.Errorf("status %d, want 404", got)
	}
}

func TestOnAuthedRunsOnlyForTheRightToken(t *testing.T) {
	certPEM, keyPEM, _ := browse.NewSessionCert(time.Hour)
	cfg, _ := browse.ServerTLSConfig(certPEM, keyPEM)
	d := New(&memSource{data: newData(100)}, Options{})
	defer func() { _ = d.Close() }()
	var starts, ends int
	h, _ := NewHandler(d, testToken, WithOnAuthed(func(w http.ResponseWriter, _ *http.Request) func() {
		starts++
		w.Header().Set(MomentHeader, "2026-10-22T09:15:00Z")
		return func() { ends++ }
	}))
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = cfg
	ts.StartTLS()
	defer ts.Close()
	c, _ := browse.PinnedClient(certPEM)
	bad := do(t, c, http.MethodGet, ts.URL+"/disk", "wrong", "bytes=0-9")
	if starts != 0 || bad.Header.Get(MomentHeader) != "" {
		t.Errorf("a request without the token reached the hook (%d) or got the header %q", starts, bad.Header.Get(MomentHeader))
	}
	good := do(t, c, http.MethodGet, ts.URL+"/disk", testToken, "bytes=0-9")
	if good.Header.Get(MomentHeader) == "" {
		t.Error("the header is missing for the right token")
	}
	_, _ = io.Copy(io.Discard, good.Body)
	_ = good.Body.Close()
	time.Sleep(50 * time.Millisecond)
	if starts != 1 || ends != 1 {
		t.Errorf("starts=%d ends=%d, want 1 and 1", starts, ends)
	}
}
