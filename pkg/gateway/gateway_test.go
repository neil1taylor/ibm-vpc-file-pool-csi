package gateway

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

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	driverNS = "kube-system"
	browseNS = "ibm-vpc-file-pool-csi-browse"
)

// fakeAuthn knows a few tokens.
type fakeAuthn struct{ users map[string]User }

func (f fakeAuthn) Authenticate(_ context.Context, token string) (User, error) {
	if u, ok := f.users[token]; ok {
		return u, nil
	}
	return User{}, ErrUnauthenticated
}

// fakeAuthz holds the rights, and records what it was asked.
type fakeAuthz struct {
	mu     sync.Mutex
	rights map[string]map[string]bool // user -> namespace -> may
	asked  []string
	// access holds the other rights, by user, namespace, verb and resource.
	access map[string]bool
	err    bool // every Can fails
}

func accessKey(user string, a Access) string {
	return user + "|" + a.Namespace + "|" + a.Verb + "|" + a.Group + "|" + a.Resource
}

func (f *fakeAuthz) Can(_ context.Context, u User, a Access) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, accessKey(u.Name, a))
	if f.err {
		return false, errors.New("the access review failed")
	}
	return f.access[accessKey(u.Name, a)], nil
}

// allow gives a user a right on a resource in a namespace.
func (f *fakeAuthz) allow(user, ns, verb, resource string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.access == nil {
		f.access = map[string]bool{}
	}
	f.access[accessKey(user, Access{Namespace: ns, Verb: verb, Resource: resource})] = true
}

func (f *fakeAuthz) CanReadBackups(_ context.Context, u User, ns string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, u.Name+"@"+ns)
	return f.rights[u.Name][ns], nil
}
func (f *fakeAuthz) set(user, ns string, may bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rights[user] == nil {
		f.rights[user] = map[string]bool{}
	}
	f.rights[user][ns] = may
}

// memTree is a backup with one folder and a few files.
type memTree struct{ big int64 }

func (m memTree) List(path [][]byte, _ []byte, _ int) (browse.ListPage, error) {
	return browse.ListPage{Entries: []browse.Entry{{Name: []byte("index.php"), Type: browse.TypeFile, Size: 5, Readable: true}}}, nil
}
func (m memTree) Stat(path [][]byte) (browse.Entry, error) {
	return browse.Entry{Name: []byte("index.php"), Type: browse.TypeFile, Size: 5, Readable: true}, nil
}

type bigFile struct{ size int64 }

func (b bigFile) ReadAt(p []byte, off int64) (int, error) {
	if off >= b.size {
		return 0, io.EOF
	}
	n := int64(len(p))
	if off+n > b.size {
		n = b.size - off
	}
	for i := int64(0); i < n; i++ {
		p[i] = 'x'
	}
	if off+n == b.size {
		return int(n), io.EOF
	}
	return int(n), nil
}
func (b bigFile) Size() int64  { return b.size }
func (b bigFile) Close() error { return nil }

func (m memTree) Open(path [][]byte) (browse.File, error) {
	if m.big > 0 {
		return bigFile{m.big}, nil
	}
	return bigFile{5}, nil
}
func (m memTree) Tar(w io.Writer, path [][]byte) (browse.TarReport, error) {
	tw := tar.NewWriter(w)
	n := int64(5)
	if m.big > 0 {
		n = m.big
	}
	_ = tw.WriteHeader(&tar.Header{Name: "a.txt", Size: n, Mode: 0o644})
	_, _ = io.Copy(tw, io.LimitReader(repeatX{}, n))
	return browse.TarReport{Files: 1}, tw.Close()
}

type repeatX struct{}

func (repeatX) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// helper is a real browse server over TLS, as a session's pod would be.
type helper struct {
	ts        *httptest.Server
	cert, key []byte
	token     string
	seenAuth  atomic.Value // last Authorization header
	calls     atomic.Int32
}

func newHelper(t *testing.T, tree browse.Tree) *helper {
	t.Helper()
	cert, key, err := browse.NewSessionCert(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h := &helper{cert: cert, key: key, token: "session-token-session-token-0123456789"}
	srv, err := browse.NewServer(tree, h.token, browse.Options{Moments: func() ([]string, error) { return []string{"20261022T090000Z"}, nil }})
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := browse.ServerTLSConfig(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	inner := srv.Handler()
	h.ts = httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.seenAuth.Store(r.Header.Get("Authorization"))
		h.calls.Add(1)
		inner.ServeHTTP(w, r)
	}))
	h.ts.TLS = cfg
	h.ts.StartTLS()
	t.Cleanup(h.ts.Close)
	return h
}

func (h *helper) addr() (string, int32) {
	host, port, _ := net.SplitHostPort(h.ts.Listener.Addr().String())
	var p int32
	for _, c := range port {
		p = p*10 + int32(c-'0')
	}
	return host, p
}

type env struct {
	t     *testing.T
	c     client.Client
	g     *Gateway
	ts    *httptest.Server
	authz *fakeAuthz
	h     *helper
}

func newEnv(t *testing.T, cfg Config, tree browse.Tree) *env {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	c := fakeclient.NewClientBuilder().WithScheme(s).WithStatusSubresource(&v1alpha1.BackupBrowse{}, &v1alpha1.BackupPolicy{}, &v1alpha1.BackupFileRestore{}).Build()
	e := &env{t: t, c: c, authz: &fakeAuthz{rights: map[string]map[string]bool{}}}
	e.authz.set("alice", "apps", true)
	e.authz.set("bob", "shop", true)
	authn := fakeAuthn{users: map[string]User{
		"alice-token": {Name: "alice", UID: "u-alice", Groups: []string{"devs"}, Extra: map[string][]string{"scopes": {"a"}}},
		"bob-token":   {Name: "bob"},
	}}
	if cfg.SessionNamespace == "" {
		cfg.SessionNamespace = browseNS
	}
	if cfg.EventNamespace == "" {
		cfg.EventNamespace = driverNS
	}
	if cfg.PollInterval == 0 {
		cfg.PollInterval = 10 * time.Millisecond
	}
	if cfg.MaxDownloadBytes == 0 {
		cfg.MaxDownloadBytes = 1 << 20
	}
	e.g = New(c, authn, e.authz, cfg)
	e.ts = httptest.NewServer(e.g.Handler())
	t.Cleanup(e.ts.Close)
	e.h = newHelper(t, tree)

	e.create(&v1alpha1.BackupPolicy{ObjectMeta: metav1.ObjectMeta{Name: "nightly"}})
	pol := &v1alpha1.BackupPolicy{}
	_ = c.Get(context.Background(), types.NamespacedName{Name: "nightly"}, pol)
	last := metav1.NewTime(time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC))
	pol.Status.Volumes = []v1alpha1.BackupVolumeStatus{
		{PVCNamespace: "apps", PVCName: "db", SubVolumeName: "pvc-1", SubPath: "/pvcs/pvc-1", RestorePoints: 3, LastBackupTime: &last, Consistency: "FileCopy"},
		{PVCNamespace: "shop", PVCName: "cart", SubVolumeName: "pvc-2", SubPath: "/pvcs/pvc-2", RestorePoints: 0, LastBackupTime: &last},
	}
	if err := c.Status().Update(context.Background(), pol); err != nil {
		t.Fatal(err)
	}
	return e
}

func (e *env) create(o client.Object) {
	e.t.Helper()
	if err := e.c.Create(context.Background(), o); err != nil {
		e.t.Fatal(err)
	}
}

// readySession makes a session as the controller would leave it: Ready, with
// its Secret, pointing at the helper.
func (e *env) readySession(name, user, ns, pvc string) *v1alpha1.BackupBrowse {
	e.t.Helper()
	bb := &v1alpha1.BackupBrowse{
		ObjectMeta: metav1.ObjectMeta{Name: name, UID: types.UID("uid-" + name), Labels: map[string]string{userLabel: userHash(user)}},
		Spec:       v1alpha1.BackupBrowseSpec{Policy: "nightly", PVCNamespace: ns, PVCName: pvc, RequestedBy: user},
	}
	e.create(bb)
	host, port := e.h.addr()
	bb.Status = v1alpha1.BackupBrowseStatus{Phase: "Ready", PodIP: host, Port: port, SecretName: "browse-uid-" + name}
	if err := e.c.Status().Update(context.Background(), bb); err != nil {
		e.t.Fatal(err)
	}
	e.create(&corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "browse-uid-" + name, Namespace: browseNS},
		Data:       map[string][]byte{"token": []byte(e.h.token), "tls.crt": e.h.cert, "tls.key": e.h.key},
	})
	return bb
}

func (e *env) do(method, path, token string, body any) (*http.Response, []byte) {
	e.t.Helper()
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case string:
		rd = strings.NewReader(b)
	default:
		j, _ := json.Marshal(b)
		rd = bytes.NewReader(j)
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, rd)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp, b
}

func (e *env) events() []corev1.Event {
	e.t.Helper()
	l := &corev1.EventList{}
	if err := e.c.List(context.Background(), l, client.InNamespace(driverNS)); err != nil {
		e.t.Fatal(err)
	}
	return l.Items
}

func (e *env) sessions() []v1alpha1.BackupBrowse {
	e.t.Helper()
	l := &v1alpha1.BackupBrowseList{}
	if err := e.c.List(context.Background(), l); err != nil {
		e.t.Fatal(err)
	}
	return l.Items
}

func b64(s string) string { return string(mustJSON([]byte(s))) }
func mustJSON(b []byte) []byte {
	j, _ := json.Marshal(b)
	return bytes.Trim(j, `"`)
}

func TestEveryCallNeedsAValidToken(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.readySession("s1", "alice", "apps", "db")
	for _, call := range []struct{ method, path string }{
		{"GET", "/v1/volumes?namespace=apps"}, {"POST", "/v1/sessions"}, {"GET", "/v1/sessions/s1"},
		{"DELETE", "/v1/sessions/s1"}, {"POST", "/v1/sessions/s1/list"}, {"POST", "/v1/sessions/s1/file"},
	} {
		for _, tok := range []string{"", "nope", "alice-tokenx"} {
			resp, body := e.do(call.method, call.path, tok, `{}`)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Errorf("%s %s with %q: %d %s", call.method, call.path, tok, resp.StatusCode, body)
			}
		}
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/v1/volumes?namespace=apps", nil)
	req.Header.Set("Authorization", "Basic alice-token")
	resp, _ := http.DefaultClient.Do(req)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("Basic: %d", resp.StatusCode)
	}
	if len(e.authz.asked) != 0 {
		t.Errorf("rights were checked for %v without a token", e.authz.asked)
	}
	resp, _ = e.do("GET", "/healthz", "", nil)
	if resp.StatusCode != 200 {
		t.Errorf("healthz = %d", resp.StatusCode)
	}
}

func TestVolumesNeedRightsAndHideTheRest(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	resp, body := e.do("GET", "/v1/volumes?namespace=apps", "alice-token", nil)
	var vols []Volume
	if resp.StatusCode != 200 || json.Unmarshal(body, &vols) != nil || len(vols) != 1 {
		t.Fatalf("apps: %d %s", resp.StatusCode, body)
	}
	if v := vols[0]; v.Policy != "nightly" || v.PVC != "db" || v.Namespace != "apps" || v.RestorePoints != 3 || v.LastBackupTime == nil {
		t.Errorf("volume = %+v", v)
	}
	// shop has a backup, nothing exists in "ghost": alice must see the same thing.
	r1, b1 := e.do("GET", "/v1/volumes?namespace=shop", "alice-token", nil)
	r2, b2 := e.do("GET", "/v1/volumes?namespace=ghost", "alice-token", nil)
	if r1.StatusCode != 403 || r2.StatusCode != 403 || string(b1) != string(b2) {
		t.Errorf("forbidden differs: %d %q vs %d %q", r1.StatusCode, b1, r2.StatusCode, b2)
	}
	if r, _ := e.do("GET", "/v1/volumes", "alice-token", nil); r.StatusCode != 400 {
		t.Errorf("no namespace: %d", r.StatusCode)
	}
}

func TestSessionCreation(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	moment := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	resp, body := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "db", Moment: &moment})
	var s Session
	if resp.StatusCode != http.StatusAccepted || json.Unmarshal(body, &s) != nil || s.Name == "" {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	got := e.sessions()
	if len(got) != 1 {
		t.Fatalf("%d sessions", len(got))
	}
	bb := got[0]
	if bb.Spec.Policy != "nightly" || bb.Spec.PVCNamespace != "apps" || bb.Spec.PVCName != "db" || bb.Spec.RequestedBy != "alice" ||
		bb.Spec.Moment == nil || !bb.Spec.Moment.Time.Equal(moment) {
		t.Errorf("spec = %+v", bb.Spec)
	}
	if bb.Labels[userLabel] != userHash("alice") || strings.Contains(bb.Labels[userLabel], "alice") {
		t.Errorf("labels = %v", bb.Labels)
	}
	evs := e.events()
	if len(evs) != 1 || evs[0].Reason != "SessionStarted" || evs[0].InvolvedObject.Kind != "BackupBrowse" || evs[0].InvolvedObject.Name != bb.Name {
		t.Fatalf("events = %+v", evs)
	}
	for _, want := range []string{"alice", "apps", "db"} {
		if !strings.Contains(evs[0].Message, want) {
			t.Errorf("event %q lacks %q", evs[0].Message, want)
		}
	}
}

func TestSessionCreationIsRefusedWithoutRightsOrBackup(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	r1, b1 := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "shop", PVC: "cart"})
	r2, b2 := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "ghost", PVC: "nothing"})
	if r1.StatusCode != 403 || r2.StatusCode != 403 || string(b1) != string(b2) {
		t.Errorf("forbidden differs: %d %q vs %d %q", r1.StatusCode, b1, r2.StatusCode, b2)
	}
	if r, _ := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "nothing"}); r.StatusCode != 404 {
		t.Errorf("no backup of the volume: %d", r.StatusCode)
	}
	if r, _ := e.do("POST", "/v1/sessions", "alice-token", SessionRequest{Policy: "nope", Namespace: "apps", PVC: "db"}); r.StatusCode != 404 {
		t.Errorf("no policy: %d", r.StatusCode)
	}
	if r, _ := e.do("POST", "/v1/sessions", "alice-token", `{"policy":"nightly","namespace":"apps","pvc":"db","shell":"x"}`); r.StatusCode != 400 {
		t.Errorf("unknown field: %d", r.StatusCode)
	}
	if n := len(e.sessions()); n != 0 {
		t.Errorf("%d sessions made", n)
	}
	if n := len(e.events()); n != 0 {
		t.Errorf("%d events for refused requests", n)
	}
}

func TestSessionIsReusedByTheSameUserForTheSameBackup(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	req := SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "db"}
	var a, b Session
	_, body := e.do("POST", "/v1/sessions", "alice-token", req)
	_ = json.Unmarshal(body, &a)
	_, body = e.do("POST", "/v1/sessions", "alice-token", req)
	_ = json.Unmarshal(body, &b)
	if a.Name != b.Name || len(e.sessions()) != 1 {
		t.Errorf("a new session was made: %s vs %s (%d)", a.Name, b.Name, len(e.sessions()))
	}
	m := time.Date(2026, 10, 21, 9, 30, 0, 0, time.UTC)
	req.Moment = &m
	var c Session
	_, body = e.do("POST", "/v1/sessions", "alice-token", req)
	_ = json.Unmarshal(body, &c)
	if c.Name == a.Name {
		t.Error("another moment reused the latest session")
	}
	// Another user with rights in the same namespace gets a session of their own.
	e.authz.set("bob", "apps", true)
	var d Session
	req.Moment = nil
	_, body = e.do("POST", "/v1/sessions", "bob-token", req)
	_ = json.Unmarshal(body, &d)
	if d.Name == "" || d.Name == a.Name {
		t.Errorf("bob got alice's session: %q", d.Name)
	}
	// A closed session is not reused.
	bb := &v1alpha1.BackupBrowse{}
	_ = e.c.Get(context.Background(), types.NamespacedName{Name: a.Name}, bb)
	bb.Spec.Close = true
	_ = e.c.Update(context.Background(), bb)
	var f Session
	req.Moment = nil
	_, body = e.do("POST", "/v1/sessions", "alice-token", req)
	_ = json.Unmarshal(body, &f)
	if f.Name == a.Name {
		t.Error("a closed session was reused")
	}
}

func TestSessionRequestWaitsForReady(t *testing.T) {
	e := newEnv(t, Config{SessionWait: 3 * time.Second}, memTree{})
	go func() {
		for i := 0; i < 200; i++ {
			l := &v1alpha1.BackupBrowseList{}
			_ = e.c.List(context.Background(), l)
			if len(l.Items) == 1 {
				bb := l.Items[0]
				bb.Status.Phase = "Ready"
				_ = e.c.Status().Update(context.Background(), &bb)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	resp, body := e.do("POST", "/v1/sessions?wait=true", "alice-token", SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "db"})
	var s Session
	_ = json.Unmarshal(body, &s)
	if resp.StatusCode != 200 || s.Phase != "Ready" {
		t.Errorf("wait: %d %s", resp.StatusCode, body)
	}
	// Nobody makes the second one Ready: the wait ends at the limit.
	e2 := newEnv(t, Config{SessionWait: 100 * time.Millisecond}, memTree{})
	resp, body = e2.do("POST", "/v1/sessions?wait=true", "alice-token", SessionRequest{Policy: "nightly", Namespace: "apps", PVC: "db"})
	_ = json.Unmarshal(body, &s)
	if resp.StatusCode != http.StatusAccepted || s.Phase == "Ready" {
		t.Errorf("timeout: %d %s", resp.StatusCode, body)
	}
}

func TestAnotherUsersSessionLooksMissing(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.readySession("s1", "alice", "apps", "db")
	e.authz.set("bob", "apps", true) // bob may read the same namespace's backups
	missing, mb := e.do("GET", "/v1/sessions/nothing", "bob-token", nil)
	for _, call := range []struct{ method, path string }{
		{"GET", "/v1/sessions/s1"}, {"DELETE", "/v1/sessions/s1"}, {"POST", "/v1/sessions/s1/list"}, {"POST", "/v1/sessions/s1/file"},
	} {
		resp, body := e.do(call.method, call.path, "bob-token", map[string]any{"path": []string{}})
		if resp.StatusCode != missing.StatusCode || missing.StatusCode != 404 {
			t.Errorf("%s %s: %d %s (missing is %d %s)", call.method, call.path, resp.StatusCode, body, missing.StatusCode, mb)
		}
	}
	if e.h.calls.Load() != 0 {
		t.Error("bob reached alice's helper")
	}
	bb := &v1alpha1.BackupBrowse{}
	_ = e.c.Get(context.Background(), types.NamespacedName{Name: "s1"}, bb)
	if bb.Spec.Close {
		t.Error("bob closed alice's session")
	}
}

func TestRightsAreCheckedOnEveryCall(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.readySession("s1", "alice", "apps", "db")
	body := map[string]any{"path": []string{}}
	if r, b := e.do("POST", "/v1/sessions/s1/list", "alice-token", body); r.StatusCode != 200 {
		t.Fatalf("list: %d %s", r.StatusCode, b)
	}
	e.authz.set("alice", "apps", false) // the binding is removed
	for _, call := range []struct{ method, path string }{
		{"POST", "/v1/sessions/s1/list"}, {"POST", "/v1/sessions/s1/file"}, {"GET", "/v1/sessions/s1"}, {"POST", "/v1/sessions/s1/tar"},
	} {
		if r, b := e.do(call.method, call.path, "alice-token", body); r.StatusCode != 403 {
			t.Errorf("%s %s after the right was removed: %d %s", call.method, call.path, r.StatusCode, b)
		}
	}
}

func TestRelayToTheHelper(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.readySession("s1", "alice", "apps", "db")
	path := map[string]any{"path": []string{b64("var"), b64("www")}}

	resp, body := e.do("POST", "/v1/sessions/s1/list", "alice-token", path)
	var page browse.ListPage
	if resp.StatusCode != 200 || json.Unmarshal(body, &page) != nil || len(page.Entries) != 1 || string(page.Entries[0].Name) != "index.php" {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
	if got := e.h.seenAuth.Load(); got != "Bearer "+e.h.token {
		t.Errorf("the helper saw %q; it must see the session token and never the user's", got)
	}
	resp, body = e.do("POST", "/v1/sessions/s1/stat", "alice-token", path)
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"type":"file"`) {
		t.Errorf("stat: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do("POST", "/v1/sessions/s1/info", "alice-token", `{}`)
	if resp.StatusCode != 200 {
		t.Errorf("info: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do("POST", "/v1/sessions/s1/moments", "alice-token", `{}`)
	if resp.StatusCode != 200 || !strings.Contains(string(body), "20261022T090000Z") {
		t.Errorf("moments: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do("POST", "/v1/sessions/s1/file", "alice-token", map[string]any{"path": []string{b64("index.php")}, "length": -1})
	if resp.StatusCode != 200 || string(body) != "xxxxx" || resp.Header.Get("X-Browse-File-Size") != "5" {
		t.Errorf("file: %d %q %v", resp.StatusCode, body, resp.Header)
	}
	resp, body = e.do("POST", "/v1/sessions/s1/tar", "alice-token", path)
	if resp.StatusCode != 200 || !bytes.Contains(body, []byte("a.txt")) {
		t.Errorf("tar: %d (%d bytes)", resp.StatusCode, len(body))
	}
	for _, b := range []string{string(body)} {
		if strings.Contains(b, e.h.token) {
			t.Error("the session token reached the user")
		}
	}
	if r, _ := e.do("POST", "/v1/sessions/s1/shell", "alice-token", `{}`); r.StatusCode != 404 {
		t.Errorf("unknown call: %d", r.StatusCode)
	}
}

func TestBadPathsNeverReachTheHelper(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.readySession("s1", "alice", "apps", "db")
	for _, p := range [][]string{{b64("..")}, {b64("a/b")}, {b64("")}, {b64(".")}} {
		resp, _ := e.do("POST", "/v1/sessions/s1/file", "alice-token", map[string]any{"path": p})
		if resp.StatusCode != 400 {
			t.Errorf("path %v: %d", p, resp.StatusCode)
		}
	}
	if r, _ := e.do("POST", "/v1/sessions/s1/list", "alice-token", `{"path":[],"evil":1}`); r.StatusCode != 400 {
		t.Errorf("unknown field: %d", r.StatusCode)
	}
	if e.h.calls.Load() != 0 {
		t.Errorf("the helper was called %d times", e.h.calls.Load())
	}
}

func TestAHelperWithAnotherCertificateIsRefused(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.readySession("s1", "alice", "apps", "db")
	// The Secret holds a different session's certificate than the one the helper serves.
	other, _, err := browse.NewSessionCert(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	sec := &corev1.Secret{}
	_ = e.c.Get(context.Background(), types.NamespacedName{Namespace: browseNS, Name: "browse-uid-s1"}, sec)
	sec.Data["tls.crt"] = other
	if err := e.c.Update(context.Background(), sec); err != nil {
		t.Fatal(err)
	}
	for _, op := range []string{"list", "stat", "file", "tar", "info"} {
		var in any = map[string]any{"path": []string{}}
		if op == "info" {
			in = `{}`
		}
		resp, body := e.do("POST", "/v1/sessions/s1/"+op, "alice-token", in)
		if resp.StatusCode != http.StatusBadGateway {
			t.Errorf("%s: %d %s", op, resp.StatusCode, body)
		}
		if strings.Contains(string(body), "index.php") || strings.Contains(string(body), e.h.token) {
			t.Errorf("%s leaked: %s", op, body)
		}
	}
	if e.h.calls.Load() != 0 {
		t.Errorf("the helper saw %d requests; the handshake must fail first", e.h.calls.Load())
	}
}

func TestASessionThatIsNotReadyAnswersConflict(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	bb := e.readySession("s1", "alice", "apps", "db")
	bb.Status.Phase = "Starting"
	_ = e.c.Status().Update(context.Background(), bb)
	resp, _ := e.do("POST", "/v1/sessions/s1/list", "alice-token", map[string]any{"path": []string{}})
	if resp.StatusCode != http.StatusConflict {
		t.Errorf("starting: %d", resp.StatusCode)
	}
	bb.Status.Phase = "Closed"
	_ = e.c.Status().Update(context.Background(), bb)
	resp, _ = e.do("POST", "/v1/sessions/s1/list", "alice-token", map[string]any{"path": []string{}})
	if resp.StatusCode != http.StatusGone && resp.StatusCode != http.StatusConflict {
		t.Errorf("closed: %d", resp.StatusCode)
	}
}

func TestDownloadLimits(t *testing.T) {
	e := newEnv(t, Config{MaxDownloadBytes: 1000}, memTree{big: 5000})
	e.readySession("s1", "alice", "apps", "db")
	resp, body := e.do("POST", "/v1/sessions/s1/file", "alice-token", map[string]any{"path": []string{b64("big")}, "length": -1})
	if resp.StatusCode != http.StatusRequestEntityTooLarge || len(body) > 1000 {
		t.Errorf("file: %d (%d bytes)", resp.StatusCode, len(body))
	}
	// A range inside the limit is fine.
	resp, body = e.do("POST", "/v1/sessions/s1/file", "alice-token", map[string]any{"path": []string{b64("big")}, "offset": 10, "length": 500})
	if resp.StatusCode != 200 || len(body) != 500 {
		t.Errorf("range: %d (%d bytes)", resp.StatusCode, len(body))
	}
	// A tar over the limit is cut, so it cannot look complete.
	req, _ := http.NewRequest("POST", e.ts.URL+"/v1/sessions/s1/tar", strings.NewReader(`{"path":[]}`))
	req.Header.Set("Authorization", "Bearer alice-token")
	r, err := http.DefaultClient.Do(req)
	if err == nil {
		defer func() { _ = r.Body.Close() }()
		var n int64
		n, err = io.Copy(io.Discard, r.Body)
		if n > 1000+4096 {
			t.Errorf("the tar sent %d bytes past the limit", n)
		}
	}
	if err == nil {
		t.Error("a tar over the limit must end with an error")
	}
}

func TestAuditEvents(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.readySession("s1", "alice", "apps", "db")
	odd := "line\nbreak"
	e.do("POST", "/v1/sessions/s1/file", "alice-token", map[string]any{"path": []string{b64("var"), b64(odd)}, "length": -1})
	e.do("POST", "/v1/sessions/s1/tar", "alice-token", map[string]any{"path": []string{b64("var")}})
	e.do("DELETE", "/v1/sessions/s1", "alice-token", nil)
	e.do("POST", "/v1/sessions/s1/list", "alice-token", map[string]any{"path": []string{}}) // not audited as an event

	byReason := map[string]corev1.Event{}
	for _, ev := range e.events() {
		byReason[ev.Reason] = ev
		for _, secret := range []string{e.h.token, "alice-token", "xxxxx"} {
			if strings.Contains(ev.Message, secret) {
				t.Errorf("event %s holds a secret or content: %q", ev.Reason, ev.Message)
			}
		}
		if strings.ContainsAny(ev.Message, "\n\x00") {
			t.Errorf("event %s holds a raw control character: %q", ev.Reason, ev.Message)
		}
		if ev.InvolvedObject.Kind != "BackupBrowse" || ev.InvolvedObject.Name != "s1" {
			t.Errorf("event %s is about %+v", ev.Reason, ev.InvolvedObject)
		}
	}
	if m := byReason["FileDownloaded"].Message; !strings.Contains(m, `/var/line\x0abreak`) || !strings.Contains(m, "alice") {
		t.Errorf("file event = %q", m)
	}
	if m := byReason["FolderDownloaded"].Message; !strings.Contains(m, "/var") || !strings.Contains(m, "alice") {
		t.Errorf("folder event = %q", m)
	}
	if m := byReason["SessionClosed"].Message; !strings.Contains(m, "alice") {
		t.Errorf("close event = %q", m)
	}
	if len(byReason) != 3 {
		t.Errorf("events = %v", byReason)
	}
}

func TestCloseSetsTheCloseFlag(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.readySession("s1", "alice", "apps", "db")
	resp, _ := e.do("DELETE", "/v1/sessions/s1", "alice-token", nil)
	if resp.StatusCode != 200 && resp.StatusCode != 202 {
		t.Fatalf("close: %d", resp.StatusCode)
	}
	bb := &v1alpha1.BackupBrowse{}
	_ = e.c.Get(context.Background(), types.NamespacedName{Name: "s1"}, bb)
	if !bb.Spec.Close {
		t.Error("spec.close not set")
	}
}

func TestSessionStatusForTheOwner(t *testing.T) {
	e := newEnv(t, Config{}, memTree{})
	e.readySession("s1", "alice", "apps", "db")
	resp, body := e.do("GET", "/v1/sessions/s1", "alice-token", nil)
	var s Session
	if resp.StatusCode != 200 || json.Unmarshal(body, &s) != nil || s.Phase != "Ready" || s.Name != "s1" {
		t.Errorf("status: %d %s", resp.StatusCode, body)
	}
	if strings.Contains(string(body), e.h.token) || strings.Contains(string(body), "127.0.0.1") {
		t.Errorf("status leaks internals: %s", body)
	}
}
