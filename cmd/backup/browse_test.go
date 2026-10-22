package main

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/gateway"
)

// stubGateway is a gateway with canned answers. It records the calls.
type stubGateway struct {
	mu       sync.Mutex
	calls    []string
	sessions []gateway.SessionRequest
	pages    map[string]browse.ListPage // by cursor
	stat     browse.Entry
	statErr  *gateway.ErrorBody
	file     []byte
	tarBytes []byte
	fileErr  *gateway.ErrorBody
	denied   bool
}

func (s *stubGateway) handler(t *testing.T) http.Handler {
	mux := http.NewServeMux()
	rec := func(r *http.Request) {
		s.mu.Lock()
		s.calls = append(s.calls, r.Method+" "+r.URL.Path)
		s.mu.Unlock()
	}
	fail := func(w http.ResponseWriter, code int, e gateway.ErrorBody) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(e)
	}
	mux.HandleFunc("POST /v1/sessions", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		if s.denied {
			fail(w, 403, gateway.ErrorBody{Code: "forbidden", Message: "you may not read the backups of that namespace"})
			return
		}
		var req gateway.SessionRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		s.mu.Lock()
		s.sessions = append(s.sessions, req)
		s.mu.Unlock()
		_ = json.NewEncoder(w).Encode(gateway.Session{Name: "bb-abc", Phase: "Ready"})
	})
	mux.HandleFunc("GET /v1/sessions/{name}", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_ = json.NewEncoder(w).Encode(gateway.Session{Name: r.PathValue("name"), Phase: "Ready"})
	})
	mux.HandleFunc("DELETE /v1/sessions/{name}", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_ = json.NewEncoder(w).Encode(gateway.Session{Name: r.PathValue("name"), Phase: "Ready"})
	})
	mux.HandleFunc("POST /v1/sessions/{name}/list", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		var req browse.ListRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_ = json.NewEncoder(w).Encode(s.pages[string(req.Cursor)])
	})
	mux.HandleFunc("POST /v1/sessions/{name}/stat", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		if s.statErr != nil {
			fail(w, 404, *s.statErr)
			return
		}
		_ = json.NewEncoder(w).Encode(s.stat)
	})
	mux.HandleFunc("POST /v1/sessions/{name}/file", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		if s.fileErr != nil {
			fail(w, 409, *s.fileErr)
			return
		}
		_, _ = w.Write(s.file)
	})
	mux.HandleFunc("POST /v1/sessions/{name}/tar", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_, _ = w.Write(s.tarBytes)
	})
	mux.HandleFunc("POST /v1/sessions/{name}/moments", func(w http.ResponseWriter, r *http.Request) {
		rec(r)
		_, _ = io.WriteString(w, `{"moments":["20261022T090000Z","20261022T100000Z"]}`)
	})
	return mux
}

func newStub(t *testing.T) (*stubGateway, *gwClient) {
	t.Helper()
	s := &stubGateway{pages: map[string]browse.ListPage{}}
	ts := httptest.NewServer(s.handler(t))
	t.Cleanup(ts.Close)
	return s, &gwClient{base: ts.URL, hc: ts.Client()}
}

var browseFlags = []string{"--policy", "nightly", "--pvc", "apps/db"}

func args(extra ...string) []string { return append(append([]string{}, browseFlags...), extra...) }

func TestPositional(t *testing.T) {
	got := positional([]string{"--policy", "nightly", "--pvc", "apps/db", "/var/www", "--out", "x"})
	if len(got) != 1 || got[0] != "/var/www" {
		t.Errorf("positional = %q", got)
	}
	if got := positional([]string{"--policy=nightly", "--pvc=apps/db"}); len(got) != 0 {
		t.Errorf("positional = %q", got)
	}
	if got := positional([]string{"--wait", "/a", "--yes", "/b"}); len(got) != 2 {
		t.Errorf("boolean flags: %q", got)
	}
}

func TestLs_PagesAndEscapedNames(t *testing.T) {
	s, gw := newStub(t)
	s.pages[""] = browse.ListPage{Entries: []browse.Entry{
		{Name: []byte("index.php"), Type: browse.TypeFile, Size: 120, Mode: 0o644, UID: 33, GID: 33, Readable: true},
		{Name: []byte("line\nbreak"), Type: browse.TypeFile, Size: 1, Mode: 0o644, Readable: true},
	}, Next: []byte("line\nbreak")}
	s.pages["line\nbreak"] = browse.ListPage{Entries: []browse.Entry{
		{Name: []byte{0xff, 'b'}, Type: browse.TypeFile, Mode: 0o600, Readable: false},
		{Name: []byte("docs"), Type: browse.TypeDir, Mode: 0o755, Readable: true},
		{Name: []byte("to-etc"), Type: browse.TypeSymlink, Link: []byte("/etc"), Readable: true},
	}}
	var out bytes.Buffer
	if err := runLs(context.Background(), gw, &out, args("/var/www")); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"index.php", `line\x0abreak`, `\xffb`, "docs/", "to-etc -> /etc", "not readable", "rw-r--r--"} {
		if !strings.Contains(got, want) {
			t.Errorf("ls output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "line\nbreak") {
		t.Error("a raw newline from a file name reached the terminal")
	}
	if len(s.sessions) != 1 || s.sessions[0].Policy != "nightly" || s.sessions[0].Namespace != "apps" || s.sessions[0].PVC != "db" || s.sessions[0].Moment != nil {
		t.Errorf("session request = %+v", s.sessions)
	}
	lists := 0
	for _, c := range s.calls {
		if strings.HasSuffix(c, "/list") {
			lists++
		}
	}
	if lists != 2 {
		t.Errorf("%d list calls, want 2 pages", lists)
	}
}

func TestLs_MomentIsSent(t *testing.T) {
	s, gw := newStub(t)
	s.pages[""] = browse.ListPage{}
	var out bytes.Buffer
	if err := runLs(context.Background(), gw, &out, args("--moment", "2026-10-21T09:30:00Z")); err != nil {
		t.Fatal(err)
	}
	if len(s.sessions) != 1 || s.sessions[0].Moment == nil || s.sessions[0].Moment.UTC().Format("2006-01-02T15:04:05Z") != "2026-10-21T09:30:00Z" {
		t.Errorf("session request = %+v", s.sessions)
	}
	if err := runLs(context.Background(), gw, &out, args("--moment", "yesterday")); err == nil {
		t.Error("a bad moment must be refused before any call")
	}
}

func TestLs_ListsTheMoments(t *testing.T) {
	_, gw := newStub(t)
	var out bytes.Buffer
	if err := runLs(context.Background(), gw, &out, args("--moments")); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "2026-10-22T09:00:00Z") || !strings.Contains(out.String(), "2026-10-22T10:00:00Z") {
		t.Errorf("moments output:\n%s", out.String())
	}
}

func TestLs_ForbiddenIsReportedPlainly(t *testing.T) {
	s, gw := newStub(t)
	s.denied = true
	err := runLs(context.Background(), gw, &bytes.Buffer{}, args())
	if err == nil || !strings.Contains(err.Error(), "may not read") {
		t.Errorf("error = %v", err)
	}
}

func TestLs_BadArguments(t *testing.T) {
	_, gw := newStub(t)
	for name, a := range map[string][]string{
		"no policy":   {"--pvc", "apps/db"},
		"no pvc":      {"--policy", "nightly"},
		"bad pvc":     {"--policy", "nightly", "--pvc", "db"},
		"bad path":    args("/a/../b"),
		"two paths":   args("/a", "/b"),
		"bad escapes": args(`/a\xzz`),
	} {
		if err := runLs(context.Background(), gw, &bytes.Buffer{}, a); err == nil {
			t.Errorf("%s: want an error", name)
		}
	}
}

func TestGet_FileToADirectory(t *testing.T) {
	s, gw := newStub(t)
	s.stat = browse.Entry{Name: []byte("index.php"), Type: browse.TypeFile, Size: 5, Mode: 0o644}
	s.file = []byte("hello")
	dir := t.TempDir()
	var out bytes.Buffer
	if err := runGet(context.Background(), gw, &out, args("/var/www/index.php", "--out", dir)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "index.php"))
	if err != nil || string(b) != "hello" {
		t.Errorf("file = %q, %v", b, err)
	}
	// An existing file is never replaced.
	if err := runGet(context.Background(), gw, &out, args("/var/www/index.php", "--out", dir)); err == nil {
		t.Error("an existing file must not be overwritten")
	}
}

func TestGet_FileToStdout(t *testing.T) {
	s, gw := newStub(t)
	s.stat = browse.Entry{Name: []byte("a"), Type: browse.TypeFile, Size: 5}
	s.file = []byte("hello")
	var out bytes.Buffer
	if err := runGet(context.Background(), gw, &out, args("/a", "--out", "-")); err != nil {
		t.Fatal(err)
	}
	if out.String() != "hello" {
		t.Errorf("stdout = %q", out.String())
	}
}

func TestGet_LinkIsRefused(t *testing.T) {
	s, gw := newStub(t)
	s.stat = browse.Entry{Name: []byte("to-etc"), Type: browse.TypeSymlink, Link: []byte("/etc")}
	dir := t.TempDir()
	err := runGet(context.Background(), gw, &bytes.Buffer{}, args("/to-etc", "--out", dir))
	if err == nil || !strings.Contains(err.Error(), "link") {
		t.Errorf("error = %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Error("something was written for a link")
	}
}

func makeTar(t *testing.T, entries []tar.Header, data map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, h := range entries {
		h := h
		if h.Typeflag == tar.TypeReg {
			h.Size = int64(len(data[h.Name]))
		}
		if err := tw.WriteHeader(&h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			_, _ = tw.Write([]byte(data[h.Name]))
		}
	}
	_ = tw.Close()
	return buf.Bytes()
}

func TestGet_FolderAsATarFile(t *testing.T) {
	s, gw := newStub(t)
	s.stat = browse.Entry{Name: []byte("www"), Type: browse.TypeDir}
	s.tarBytes = makeTar(t, []tar.Header{{Name: "www/", Typeflag: tar.TypeDir, Mode: 0o755}}, nil)
	dir := t.TempDir()
	if err := runGet(context.Background(), gw, &bytes.Buffer{}, args("/var/www", "--out", dir)); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "www.tar"))
	if err != nil || !bytes.Equal(b, s.tarBytes) {
		t.Errorf("tar file: %v (%d bytes)", err, len(b))
	}
}

func TestGet_ExtractNeverWritesOutsideTheFolder(t *testing.T) {
	s, gw := newStub(t)
	s.stat = browse.Entry{Name: []byte("www"), Type: browse.TypeDir}
	s.tarBytes = makeTar(t, []tar.Header{
		{Name: "www/", Typeflag: tar.TypeDir, Mode: 0o755},
		{Name: "www/ok.txt", Typeflag: tar.TypeReg, Mode: 0o4755}, // setuid bit must not be applied
		{Name: "www/to-etc", Typeflag: tar.TypeSymlink, Linkname: "/etc"},
		{Name: "../escape.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "/abs.txt", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "www/to-etc/passwd", Typeflag: tar.TypeReg, Mode: 0o644},
		{Name: "www/dev", Typeflag: tar.TypeChar},
	}, map[string]string{"www/ok.txt": "ok", "../escape.txt": "bad", "/abs.txt": "bad", "www/to-etc/passwd": "bad"})
	parent := t.TempDir()
	dest := filepath.Join(parent, "out")
	var out bytes.Buffer
	if err := runGet(context.Background(), gw, &out, args("/var/www", "--extract", dest)); err != nil {
		t.Logf("extract reported: %v", err)
	}
	if b, err := os.ReadFile(filepath.Join(dest, "www", "ok.txt")); err != nil || string(b) != "ok" {
		t.Errorf("ok.txt = %q, %v", b, err)
	}
	if fi, err := os.Stat(filepath.Join(dest, "www", "ok.txt")); err == nil && fi.Mode()&(os.ModeSetuid|os.ModeSetgid|os.ModeSticky) != 0 {
		t.Error("a special permission bit was applied")
	}
	for _, p := range []string{filepath.Join(parent, "escape.txt"), filepath.Join(dest, "..", "escape.txt"), "/abs.txt"} {
		if _, err := os.Lstat(p); err == nil {
			t.Errorf("%s was written", p)
		}
	}
	if fi, err := os.Lstat(filepath.Join(dest, "www", "to-etc")); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		t.Error("a symbolic link was created")
	}
	if _, err := os.Lstat(filepath.Join(dest, "www", "dev")); err == nil {
		t.Error("a device was created")
	}
	if !strings.Contains(out.String(), "skipped") {
		t.Errorf("skipped entries are not reported:\n%s", out.String())
	}
}

func TestGet_GatewayErrorIsShown(t *testing.T) {
	s, gw := newStub(t)
	s.statErr = &gateway.ErrorBody{Code: "not_found", Message: "no such file or folder in this backup"}
	err := runGet(context.Background(), gw, &bytes.Buffer{}, args("/nope", "--out", t.TempDir()))
	if err == nil || !strings.Contains(err.Error(), "no such file") {
		t.Errorf("error = %v", err)
	}
}

func TestBrowse_OpensAndCloses(t *testing.T) {
	s, gw := newStub(t)
	var out bytes.Buffer
	if err := runBrowse(context.Background(), gw, &out, args()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "bb-abc") {
		t.Errorf("browse output:\n%s", out.String())
	}
	out.Reset()
	if err := runBrowse(context.Background(), gw, &out, []string{"--close", "bb-abc"}); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, c := range s.calls {
		if c == "DELETE /v1/sessions/bb-abc" {
			found = true
		}
	}
	if !found {
		t.Errorf("calls = %v", s.calls)
	}
}

func TestLs_UsesAGivenSession(t *testing.T) {
	s, gw := newStub(t)
	s.pages[""] = browse.ListPage{}
	if err := runLs(context.Background(), gw, &bytes.Buffer{}, []string{"--session", "bb-xyz"}); err != nil {
		t.Fatal(err)
	}
	if len(s.sessions) != 0 {
		t.Error("a session was opened although one was given")
	}
	found := false
	for _, c := range s.calls {
		if c == "POST /v1/sessions/bb-xyz/list" {
			found = true
		}
	}
	if !found {
		t.Errorf("calls = %v", s.calls)
	}
}

func TestGatewayInfoFromTheConfigMap(t *testing.T) {
	c := fakeclient.NewClientBuilder().WithScheme(newScheme()).WithObjects(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: gatewayInfoName, Namespace: "kube-system"},
		Data:       map[string]string{"url": "https://gw.example.com", "ca.crt": "PEM"},
	}).Build()
	url, ca, err := gatewayInfo(context.Background(), c, "kube-system")
	if err != nil || url != "https://gw.example.com" || ca != "PEM" {
		t.Errorf("gatewayInfo = %q %q %v", url, ca, err)
	}
	if _, _, err := gatewayInfo(context.Background(), c, "other"); err == nil || !strings.Contains(err.Error(), "--gateway") {
		t.Errorf("no ConfigMap: %v", err)
	}
	empty := fakeclient.NewClientBuilder().WithScheme(newScheme()).WithObjects(&corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: gatewayInfoName, Namespace: "kube-system"}}).Build()
	if _, _, err := gatewayInfo(context.Background(), empty, "kube-system"); err == nil {
		t.Error("a ConfigMap with no url must be refused")
	}
}
