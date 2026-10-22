package browse

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const testToken = "0123456789abcdef0123456789abcdef"

// fakeTree is a Tree for tests that need no disk.
type fakeTree struct {
	mu      sync.Mutex
	calls   int
	list    func(path [][]byte, cursor []byte, limit int) (ListPage, error)
	stat    func(path [][]byte) (Entry, error)
	open    func(path [][]byte) (File, error)
	tarFunc func(w io.Writer, path [][]byte) (TarReport, error)
	lastLim int
	lastCur []byte
	lastPth [][]byte
}

func (f *fakeTree) touch() { f.mu.Lock(); f.calls++; f.mu.Unlock() }

func (f *fakeTree) List(path [][]byte, cursor []byte, limit int) (ListPage, error) {
	f.touch()
	f.mu.Lock()
	f.lastLim, f.lastCur, f.lastPth = limit, cursor, path
	f.mu.Unlock()
	return f.list(path, cursor, limit)
}
func (f *fakeTree) Stat(path [][]byte) (Entry, error) { f.touch(); return f.stat(path) }
func (f *fakeTree) Open(path [][]byte) (File, error)  { f.touch(); return f.open(path) }
func (f *fakeTree) Tar(w io.Writer, path [][]byte) (TarReport, error) {
	f.touch()
	return f.tarFunc(w, path)
}

type memFile struct{ data []byte }

func (m memFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= int64(len(m.data)) {
		return 0, io.EOF
	}
	n := copy(p, m.data[off:])
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}
func (m memFile) Size() int64  { return int64(len(m.data)) }
func (m memFile) Close() error { return nil }

func newTestServer(t *testing.T, tree Tree, opts Options) (*httptest.Server, *Server) {
	t.Helper()
	s, err := NewServer(tree, testToken, opts)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts, s
}

func post(t *testing.T, ts *httptest.Server, path, token string, body any) *http.Response {
	t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(body)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(http.MethodPost, ts.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}

func apiError(t *testing.T, resp *http.Response) (string, string) {
	t.Helper()
	var e struct{ Code, Message string }
	if err := json.NewDecoder(resp.Body).Decode(&e); err != nil {
		t.Fatalf("error body: %v", err)
	}
	return e.Code, e.Message
}

func TestNewServerRefusesShortToken(t *testing.T) {
	for _, tok := range []string{"", "short", strings.Repeat("a", 15)} {
		if _, err := NewServer(&fakeTree{}, tok, Options{}); err == nil {
			t.Errorf("token %q should be refused", tok)
		}
	}
}

func TestEveryCallNeedsTheToken(t *testing.T) {
	tree := &fakeTree{}
	ts, _ := newTestServer(t, tree, Options{})
	for _, ep := range []string{"/v1/list", "/v1/stat", "/v1/file", "/v1/tar"} {
		for _, tok := range []string{"", "wrong-wrong-wrong-wrong-wrong-12", testToken + "x", testToken[:31]} {
			resp := post(t, ts, ep, tok, map[string]any{"path": []string{}})
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s with token %q: status %d, want 401", ep, tok, resp.StatusCode)
			}
		}
		// the wrong scheme
		req, _ := http.NewRequest(http.MethodPost, ts.URL+ep, strings.NewReader("{}"))
		req.Header.Set("Authorization", "Basic "+testToken)
		resp, _ := http.DefaultClient.Do(req)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%s with Basic: status %d", ep, resp.StatusCode)
		}
		_ = resp.Body.Close()
	}
	if tree.calls != 0 {
		t.Errorf("the tree was reached %d times without a token", tree.calls)
	}
}

func TestHealthzNeedsNoToken(t *testing.T) {
	ts, _ := newTestServer(t, &fakeTree{}, Options{})
	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("healthz: %v %v", resp, err)
	}
	_ = resp.Body.Close()
	resp, _ = http.Get(ts.URL + "/v1/list")
	if resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET /v1/list = %d", resp.StatusCode)
	}
	_ = resp.Body.Close()
}

func TestListPassesPathCursorAndLimit(t *testing.T) {
	odd := []byte{0xff, 'a', '\n'}
	tree := &fakeTree{list: func(path [][]byte, cursor []byte, limit int) (ListPage, error) {
		return ListPage{Entries: []Entry{{Name: odd, Type: TypeFile, Link: []byte{0xfe}}}, Next: []byte("n")}, nil
	}}
	ts, _ := newTestServer(t, tree, Options{})
	resp := post(t, ts, "/v1/list", testToken, map[string]any{"path": [][]byte{[]byte("a"), odd}, "cursor": []byte("c"), "limit": 50})
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var page ListPage
	if err := json.NewDecoder(resp.Body).Decode(&page); err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 1 || !bytes.Equal(page.Entries[0].Name, odd) || !bytes.Equal(page.Entries[0].Link, []byte{0xfe}) || string(page.Next) != "n" {
		t.Errorf("page = %+v", page)
	}
	if tree.lastLim != 50 || string(tree.lastCur) != "c" || len(tree.lastPth) != 2 || !bytes.Equal(tree.lastPth[1], odd) {
		t.Errorf("the tree saw limit %d cursor %q path %q", tree.lastLim, tree.lastCur, tree.lastPth)
	}
	post(t, ts, "/v1/list", testToken, map[string]any{"path": [][]byte{}, "limit": 100000})
	if tree.lastLim != 1000 {
		t.Errorf("limit not clamped to 1000: %d", tree.lastLim)
	}
	post(t, ts, "/v1/list", testToken, map[string]any{"path": [][]byte{}})
	if tree.lastLim != 1000 {
		t.Errorf("default limit %d, want 1000", tree.lastLim)
	}
}

func TestBadRequests(t *testing.T) {
	tree := &fakeTree{list: func([][]byte, []byte, int) (ListPage, error) { return ListPage{}, nil }}
	ts, _ := newTestServer(t, tree, Options{})
	big := `{"path":[],"x":"` + strings.Repeat("a", 2<<20) + `"}`
	for name, body := range map[string]string{
		"not json":      "path=/",
		"unknown field": `{"path":[],"shell":"rm -rf /"}`,
		"too big":       big,
		"path not b64":  `{"path":["!!!"]}`,
		"trailing junk": `{"path":[]} {"path":[]}`,
	} {
		resp := post(t, ts, "/v1/list", testToken, body)
		if resp.StatusCode != http.StatusBadRequest {
			t.Errorf("%s: status %d, want 400", name, resp.StatusCode)
		}
	}
	// a path that holds ".." is refused before it reaches the tree
	before := tree.calls
	resp := post(t, ts, "/v1/list", testToken, map[string]any{"path": [][]byte{[]byte("..")}})
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf(".. : status %d", resp.StatusCode)
	}
	if code, _ := apiError(t, resp); code != CodeBadPath {
		t.Errorf("code %q", code)
	}
	if tree.calls != before {
		t.Error("a bad path reached the tree")
	}
	req, _ := http.NewRequest(http.MethodGet, ts.URL+"/v1/list", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	r2, _ := http.DefaultClient.Do(req)
	if r2.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("GET = %d", r2.StatusCode)
	}
	_ = r2.Body.Close()
}

func TestErrorCodesBecomeStatuses(t *testing.T) {
	for code, status := range map[string]int{
		CodeBadPath: 400, CodeNotFound: 404, CodeNotDir: 409, CodeNotFile: 409,
		CodeLink: 409, CodeSpecial: 409, CodeNotReadable: 403, CodeInternal: 500,
	} {
		tree := &fakeTree{stat: func([][]byte) (Entry, error) { return Entry{}, &Error{Code: code, Msg: "m"} }}
		ts, _ := newTestServer(t, tree, Options{})
		resp := post(t, ts, "/v1/stat", testToken, map[string]any{"path": [][]byte{[]byte("a")}})
		if resp.StatusCode != status {
			t.Errorf("%s: status %d, want %d", code, resp.StatusCode, status)
		}
		if got, _ := apiError(t, resp); got != code {
			t.Errorf("%s: body code %q", code, got)
		}
	}
	// A plain error is internal and does not leak its text.
	tree := &fakeTree{stat: func([][]byte) (Entry, error) { return Entry{}, errors.New("open /secret/path: boom") }}
	ts, _ := newTestServer(t, tree, Options{})
	resp := post(t, ts, "/v1/stat", testToken, map[string]any{"path": [][]byte{}})
	code, msg := apiError(t, resp)
	if resp.StatusCode != 500 || code != CodeInternal || strings.Contains(msg, "/secret") {
		t.Errorf("plain error: %d %q %q", resp.StatusCode, code, msg)
	}
}

func TestFileStreamAndRange(t *testing.T) {
	data := bytes.Repeat([]byte("0123456789"), 100000) // 1 MB
	tree := &fakeTree{open: func(path [][]byte) (File, error) { return memFile{data}, nil }}
	ts, _ := newTestServer(t, tree, Options{})
	read := func(off, length int64) (*http.Response, []byte) {
		resp := post(t, ts, "/v1/file", testToken, map[string]any{"path": [][]byte{[]byte("f")}, "offset": off, "length": length})
		b, _ := io.ReadAll(resp.Body)
		return resp, b
	}
	resp, b := read(0, -1)
	if resp.StatusCode != 200 || !bytes.Equal(b, data) || resp.Header.Get("Content-Length") != "1000000" || resp.Header.Get("X-Browse-File-Size") != "1000000" {
		t.Errorf("whole file: %d, %d bytes, headers %v", resp.StatusCode, len(b), resp.Header)
	}
	_, b = read(5, 10)
	if string(b) != "5678901234" {
		t.Errorf("range = %q", b)
	}
	resp, b = read(999995, 100)
	if string(b) != "56789" || resp.Header.Get("Content-Length") != "5" {
		t.Errorf("range past the end = %q %v", b, resp.Header)
	}
	resp, b = read(2000000, -1)
	if resp.StatusCode != 200 || len(b) != 0 || resp.Header.Get("X-Browse-File-Size") != "1000000" {
		t.Errorf("offset past the end: %d %d", resp.StatusCode, len(b))
	}
	resp = post(t, ts, "/v1/file", testToken, map[string]any{"path": [][]byte{[]byte("f")}, "offset": -1})
	if resp.StatusCode != 400 {
		t.Errorf("negative offset: %d", resp.StatusCode)
	}
}

func TestFileOpenErrorIsAStatus(t *testing.T) {
	tree := &fakeTree{open: func([][]byte) (File, error) { return nil, &Error{Code: CodeLink, Msg: "link"} }}
	ts, _ := newTestServer(t, tree, Options{})
	resp := post(t, ts, "/v1/file", testToken, map[string]any{"path": [][]byte{[]byte("l")}, "length": -1})
	if resp.StatusCode != 409 {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestTarStreamsAndReports(t *testing.T) {
	tree := &fakeTree{tarFunc: func(w io.Writer, path [][]byte) (TarReport, error) {
		tw := tar.NewWriter(w)
		_ = tw.WriteHeader(&tar.Header{Name: "a.txt", Size: 2, Mode: 0o644})
		_, _ = tw.Write([]byte("hi"))
		return TarReport{Files: 1, Skipped: 2}, tw.Close()
	}}
	ts, _ := newTestServer(t, tree, Options{})
	resp := post(t, ts, "/v1/tar", testToken, map[string]any{"path": [][]byte{}})
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "application/x-tar" {
		t.Fatalf("status %d type %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	tr := tar.NewReader(resp.Body)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "a.txt" {
		t.Fatalf("tar: %v %v", hdr, err)
	}
	_, _ = io.ReadAll(resp.Body)
	var rep TarReport
	if err := json.Unmarshal([]byte(resp.Trailer.Get("X-Browse-Report")), &rep); err != nil || rep.Skipped != 2 || rep.Files != 1 {
		t.Errorf("trailer = %q (%v)", resp.Trailer.Get("X-Browse-Report"), err)
	}
}

func TestTarErrorBeforeAndAfterStart(t *testing.T) {
	before := &fakeTree{tarFunc: func(io.Writer, [][]byte) (TarReport, error) {
		return TarReport{}, &Error{Code: CodeLink, Msg: "link"}
	}}
	ts, _ := newTestServer(t, before, Options{})
	resp := post(t, ts, "/v1/tar", testToken, map[string]any{"path": [][]byte{[]byte("l")}})
	if resp.StatusCode != 409 {
		t.Errorf("error before the first byte: status %d, want 409", resp.StatusCode)
	}
	after := &fakeTree{tarFunc: func(w io.Writer, _ [][]byte) (TarReport, error) {
		_, _ = w.Write(bytes.Repeat([]byte("x"), 1024))
		return TarReport{}, errors.New("disk went away")
	}}
	ts2, _ := newTestServer(t, after, Options{})
	// The client may see the cut at once (nothing was flushed yet) or while it
	// reads the body. Either way it must not see a stream that looks whole.
	req, _ := http.NewRequest(http.MethodPost, ts2.URL+"/v1/tar", strings.NewReader(`{"path":[]}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	resp2, err := http.DefaultClient.Do(req)
	if err == nil {
		defer func() { _ = resp2.Body.Close() }()
		_, err = io.ReadAll(resp2.Body)
	}
	if err == nil {
		t.Error("a tar that failed half way must not look complete")
	}
}

func startRun(t *testing.T, s *Server) (context.CancelFunc, <-chan string, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan string, 1)
	go func() {
		reason, _ := s.Run(ctx, ln)
		done <- reason
	}()
	t.Cleanup(cancel)
	return cancel, done, "http://" + ln.Addr().String()
}

func TestRunEndsWhenIdle(t *testing.T) {
	tree := &fakeTree{stat: func([][]byte) (Entry, error) { return Entry{Type: TypeDir}, nil }}
	s, _ := NewServer(tree, testToken, Options{Idle: 300 * time.Millisecond})
	_, done, base := startRun(t, s)
	// Use is activity: keep calling for longer than the idle time.
	deadline := time.Now().Add(700 * time.Millisecond)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, base+"/v1/stat", strings.NewReader(`{"path":[]}`))
		req.Header.Set("Authorization", "Bearer "+testToken)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("session ended while in use: %v", err)
		}
		_ = resp.Body.Close()
		select {
		case r := <-done:
			t.Fatalf("session ended (%s) while in use", r)
		case <-time.After(100 * time.Millisecond):
		}
	}
	select {
	case r := <-done:
		if r != EndIdle {
			t.Errorf("reason %q, want idle", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the session did not end when idle")
	}
}

func TestUnauthorizedCallsAreNotActivity(t *testing.T) {
	s, _ := NewServer(&fakeTree{}, testToken, Options{Idle: 300 * time.Millisecond})
	_, done, base := startRun(t, s)
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				if resp, err := http.Post(base+"/v1/list", "application/json", strings.NewReader("{}")); err == nil {
					_ = resp.Body.Close()
				}
				if resp, err := http.Get(base + "/healthz"); err == nil {
					_ = resp.Body.Close()
				}
				time.Sleep(20 * time.Millisecond)
			}
		}
	}()
	defer close(stop)
	select {
	case r := <-done:
		if r != EndIdle {
			t.Errorf("reason %q", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("calls without the token kept the session alive")
	}
}

func TestARunningStreamIsNotIdle(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{})
	tree := &fakeTree{tarFunc: func(w io.Writer, _ [][]byte) (TarReport, error) {
		_, _ = w.Write([]byte("x"))
		close(started)
		<-release
		return TarReport{}, nil
	}}
	s, _ := NewServer(tree, testToken, Options{Idle: 200 * time.Millisecond})
	_, done, base := startRun(t, s)
	req, _ := http.NewRequest(http.MethodPost, base+"/v1/tar", strings.NewReader(`{"path":[]}`))
	req.Header.Set("Authorization", "Bearer "+testToken)
	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
		}
	}()
	<-started
	select {
	case r := <-done:
		t.Fatalf("session ended (%s) with a stream running", r)
	case <-time.After(600 * time.Millisecond):
	}
	close(release)
	select {
	case r := <-done:
		if r != EndIdle {
			t.Errorf("reason %q", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not go idle after the stream ended")
	}
}

func TestRunEndsAtMaxLifeEvenWhenBusy(t *testing.T) {
	var n atomic.Int32
	tree := &fakeTree{stat: func([][]byte) (Entry, error) { n.Add(1); return Entry{Type: TypeDir}, nil }}
	s, _ := NewServer(tree, testToken, Options{Idle: time.Hour, MaxLife: 300 * time.Millisecond})
	_, done, base := startRun(t, s)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				req, _ := http.NewRequest(http.MethodPost, base+"/v1/stat", strings.NewReader(`{"path":[]}`))
				req.Header.Set("Authorization", "Bearer "+testToken)
				if resp, err := http.DefaultClient.Do(req); err == nil {
					_ = resp.Body.Close()
				}
				time.Sleep(10 * time.Millisecond)
			}
		}
	}()
	select {
	case r := <-done:
		if r != EndMaxLife {
			t.Errorf("reason %q, want max-life", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("the hard limit did not end the session")
	}
	if n.Load() == 0 {
		t.Error("the server never served a call")
	}
}

func TestRunStopsWhenAsked(t *testing.T) {
	s, _ := NewServer(&fakeTree{}, testToken, Options{Idle: time.Hour, MaxLife: time.Hour})
	cancel, done, _ := startRun(t, s)
	cancel()
	select {
	case r := <-done:
		if r != EndStopped {
			t.Errorf("reason %q", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not stop")
	}
}

func TestTooManyStreams(t *testing.T) {
	hold := make(chan struct{})
	started := make(chan struct{}, 4)
	tree := &fakeTree{tarFunc: func(w io.Writer, _ [][]byte) (TarReport, error) {
		_, _ = w.Write([]byte("x"))
		started <- struct{}{}
		<-hold
		return TarReport{}, nil
	}}
	ts, _ := newTestServer(t, tree, Options{MaxStreams: 2})
	defer close(hold)
	for i := 0; i < 2; i++ {
		go func() {
			resp := post(t, ts, "/v1/tar", testToken, map[string]any{"path": [][]byte{}})
			_, _ = io.Copy(io.Discard, resp.Body)
		}()
		<-started
	}
	resp := post(t, ts, "/v1/tar", testToken, map[string]any{"path": [][]byte{}})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("third stream: status %d, want 503", resp.StatusCode)
	}
}

func TestTooManyListsAtOnce(t *testing.T) {
	hold := make(chan struct{})
	started := make(chan struct{}, 4)
	tree := &fakeTree{list: func([][]byte, []byte, int) (ListPage, error) {
		started <- struct{}{}
		<-hold
		return ListPage{}, nil
	}}
	ts, _ := newTestServer(t, tree, Options{MaxLists: 2})
	defer close(hold)
	for i := 0; i < 2; i++ {
		go func() {
			resp := post(t, ts, "/v1/list", testToken, map[string]any{"path": [][]byte{}})
			_, _ = io.Copy(io.Discard, resp.Body)
		}()
		<-started
	}
	resp := post(t, ts, "/v1/list", testToken, map[string]any{"path": [][]byte{}})
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("third list: status %d, want 503", resp.StatusCode)
	}
}

func TestInfoReportsTheMoment(t *testing.T) {
	s, err := NewServer(&fakeTree{}, testToken, Options{Info: Info{Moment: "20261022T100000Z", Latest: false}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if resp := post(t, ts, "/v1/info", "", map[string]any{}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("info without a token: %d", resp.StatusCode)
	}
	resp := post(t, ts, "/v1/info", testToken, map[string]any{})
	var got Info
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != 200 {
		t.Fatalf("info: %d %v", resp.StatusCode, err)
	}
	if got.Moment != "20261022T100000Z" || got.Latest {
		t.Errorf("info = %+v", got)
	}
}

func TestMomentsListsTheMomentNames(t *testing.T) {
	s, err := NewServer(&fakeTree{}, testToken, Options{Moments: func() ([]string, error) {
		return []string{"20261022T090000Z", "20261022T100000Z"}, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	if resp := post(t, ts, "/v1/moments", "", map[string]any{}); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("moments without a token: %d", resp.StatusCode)
	}
	resp := post(t, ts, "/v1/moments", testToken, map[string]any{})
	var got struct{ Moments []string }
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil || resp.StatusCode != 200 {
		t.Fatalf("moments: %d %v", resp.StatusCode, err)
	}
	if len(got.Moments) != 2 || got.Moments[1] != "20261022T100000Z" {
		t.Errorf("moments = %v", got.Moments)
	}
	// With no function the list is empty, not an error.
	s2, _ := NewServer(&fakeTree{}, testToken, Options{})
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()
	resp2 := post(t, ts2, "/v1/moments", testToken, map[string]any{})
	got.Moments = nil
	_ = json.NewDecoder(resp2.Body).Decode(&got)
	if resp2.StatusCode != 200 || len(got.Moments) != 0 {
		t.Errorf("no function: %d %v", resp2.StatusCode, got.Moments)
	}
}
