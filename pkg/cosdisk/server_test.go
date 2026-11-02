package cosdisk

import (
	"bytes"
	"context"
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
	defer d.Close()
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
