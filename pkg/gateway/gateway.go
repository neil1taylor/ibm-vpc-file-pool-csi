package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Authenticator turns a bearer token into a user.
type Authenticator interface {
	Authenticate(ctx context.Context, token string) (User, error)
}

// Authorizer says whether a user may read the backups of a namespace.
type Authorizer interface {
	CanReadBackups(ctx context.Context, u User, namespace string) (bool, error)
	// Can says whether the user may do one thing in a namespace.
	Can(ctx context.Context, u User, a Access) (bool, error)
}

// AuditConfigMap is the namespaced object that audit Events are written on.
const AuditConfigMap = "backup-gateway-info"

// Config sets the gateway's limits.
type Config struct {
	// SessionNamespace is where the session Secrets live. It holds nothing else,
	// so the gateway's right to read Secrets reaches nothing else.
	SessionNamespace string
	// EventNamespace is where the audit Events are written. They name users and
	// paths, so they stay in the driver namespace and not in "default", which
	// many people can read. The objects they are about have no namespace, and
	// the API server refuses an Event for such an object outside "default", so
	// the Event is on the gateway's own ConfigMap and names the session or
	// restore in Related.
	EventNamespace string
	// DriverNamespace is where the COS and Key Protect Secrets are. A person who
	// names a bucket directly must be allowed to read the Secrets it names there.
	// Empty means EventNamespace, which is the driver namespace.
	DriverNamespace string
	// SessionWait is how long a request to open a session waits for it to be Ready.
	SessionWait time.Duration
	// PollInterval is how often it looks while waiting.
	PollInterval time.Duration
	// MaxDownloadBytes limits one file or one tar.
	MaxDownloadBytes int64
	// AuthCacheTTL keeps a token review result for this long. Zero turns the cache off.
	AuthCacheTTL time.Duration
	// MaxOpenRestores limits the file restores one user has that have not ended.
	// Each restore takes one of that many numbered slots (bfr-<user>-<n>), so
	// the limit holds for parallel requests and several replicas. Zero means 10.
	MaxOpenRestores int
}

const (
	maxBody    = 1 << 20
	maxMessage = 1000
)

// Gateway serves the user API.
type Gateway struct {
	c     client.Client
	authn Authenticator
	authz Authorizer
	cfg   Config

	mu      sync.Mutex
	cache   map[[sha256.Size]byte]cachedUser
	helpers map[[sha256.Size]byte]*http.Client
}

type cachedUser struct {
	user User
	exp  time.Time
}

// New makes a gateway. c must read without a cache of every namespace's objects.
func New(c client.Client, authn Authenticator, authz Authorizer, cfg Config) *Gateway {
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = time.Second
	}
	if cfg.MaxOpenRestores <= 0 {
		cfg.MaxOpenRestores = defaultMaxOpenRestores
	}
	return &Gateway{c: c, authn: authn, authz: authz, cfg: cfg,
		cache: map[[sha256.Size]byte]cachedUser{}, helpers: map[[sha256.Size]byte]*http.Client{}}
}

// Handler is the HTTP handler.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") })
	mux.HandleFunc("GET /v1/volumes", g.auth(g.handleVolumes))
	mux.HandleFunc("POST /v1/sessions", g.auth(g.handleCreate))
	mux.HandleFunc("GET /v1/sessions/{name}", g.auth(g.handleStatus))
	mux.HandleFunc("DELETE /v1/sessions/{name}", g.auth(g.handleClose))
	mux.HandleFunc("POST /v1/sessions/{name}/{op}", g.auth(g.handleOp))
	mux.HandleFunc("POST /v1/restores", g.auth(g.handleRestoreCreate))
	mux.HandleFunc("GET /v1/restores/{name}", g.auth(g.handleRestoreStatus))
	mux.HandleFunc("GET /v1/sessions/{name}/download", g.auth(g.handleDownload))
	// Anything else needs a token too, and is not found.
	mux.HandleFunc("/", g.auth(func(w http.ResponseWriter, _ *http.Request, _ User) {
		writeError(w, http.StatusNotFound, "not_found", "no such call")
	}))
	return mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, ErrorBody{Code: code, Message: msg})
}

// forbidden says the same thing for a namespace that has backups and for one
// that has none, so it does not show what exists.
func forbidden(w http.ResponseWriter) {
	writeError(w, http.StatusForbidden, "forbidden", forbiddenMessage)
}

const forbiddenMessage = "you may not read the backups of that namespace"

func notFoundSession(w http.ResponseWriter) {
	writeError(w, http.StatusNotFound, "not_found", "no such session")
}

// auth checks the token and gives the handler the user.
func (g *Gateway) auth(h func(http.ResponseWriter, *http.Request, User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || token == "" {
			writeError(w, http.StatusUnauthorized, "unauthenticated", "send your Kubernetes token as a bearer token")
			return
		}
		u, err := g.authenticate(r.Context(), token)
		if err != nil {
			if !errors.Is(err, ErrUnauthenticated) {
				klog.ErrorS(err, "Token review failed")
				writeError(w, http.StatusServiceUnavailable, "unavailable", "cannot check your token now")
				return
			}
			writeError(w, http.StatusUnauthorized, "unauthenticated", "the token is not valid")
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		h(w, r, u)
	}
}

func (g *Gateway) authenticate(ctx context.Context, token string) (User, error) {
	key := sha256.Sum256([]byte(token))
	if g.cfg.AuthCacheTTL > 0 {
		g.mu.Lock()
		if c, ok := g.cache[key]; ok && time.Now().Before(c.exp) {
			g.mu.Unlock()
			return c.user, nil
		}
		g.mu.Unlock()
	}
	u, err := g.authn.Authenticate(ctx, token)
	if err != nil {
		return User{}, err
	}
	if g.cfg.AuthCacheTTL > 0 {
		g.mu.Lock()
		g.cache[key] = cachedUser{u, time.Now().Add(g.cfg.AuthCacheTTL)}
		g.mu.Unlock()
	}
	return u, nil
}

// allowed checks the right on every call. It answers false on an error, after
// logging it, so a failure to check is never a yes.
func (g *Gateway) allowed(ctx context.Context, u User, ns string) bool {
	ok, err := g.authz.CanReadBackups(ctx, u, ns)
	if err != nil {
		klog.ErrorS(err, "Rights check failed")
		return false
	}
	return ok
}

func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if dec.More() {
		return errors.New("more than one value")
	}
	return nil
}

func (g *Gateway) handleVolumes(w http.ResponseWriter, r *http.Request, u User) {
	ns := r.URL.Query().Get("namespace")
	if ns == "" {
		writeError(w, http.StatusBadRequest, "bad_request", "namespace is required")
		return
	}
	if !g.allowed(r.Context(), u, ns) {
		forbidden(w)
		return
	}
	list := &v1alpha1.BackupPolicyList{}
	if err := g.c.List(r.Context(), list); err != nil {
		klog.ErrorS(err, "List BackupPolicies failed")
		writeError(w, http.StatusInternalServerError, "internal", "cannot list backups")
		return
	}
	vols := []Volume{}
	for _, p := range list.Items {
		for _, v := range p.Status.Volumes {
			if v.PVCNamespace != ns {
				continue
			}
			out := Volume{Policy: p.Name, Namespace: ns, PVC: v.PVCName, VM: v.VMName, RestorePoints: v.RestorePoints,
				Consistency: v.Consistency, Retained: v.Retained}
			if p.Spec.Export != nil && v.ExportedTime != nil {
				t := v.ExportedTime.Time
				out.Exported, out.ExportedTime = true, &t
			}
			if v.LastBackupTime != nil {
				t := v.LastBackupTime.Time
				out.LastBackupTime = &t
			}
			if v.OldestBackupTime != nil {
				t := v.OldestBackupTime.Time
				out.OldestBackupTime = &t
			}
			vols = append(vols, out)
		}
	}
	writeJSON(w, http.StatusOK, vols)
}

func (g *Gateway) handleCreate(w http.ResponseWriter, r *http.Request, u User) {
	var req SessionRequest
	if err := decodeStrict(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "bad_request", "the request is not valid")
		return
	}
	spec, serr := g.sessionSpec(r.Context(), u, &req)
	if serr != nil {
		writeError(w, serr.status, serr.code, serr.msg)
		return
	}
	bb, created, err := g.findOrCreate(r.Context(), u, spec, req.Moment)
	if err != nil {
		klog.ErrorS(err, "Make a browse session failed")
		writeError(w, http.StatusInternalServerError, "internal", "cannot make the session")
		return
	}
	if created {
		g.audit(r.Context(), bb, "SessionStarted", fmt.Sprintf("user %s opened a browse session on %s of policy %s, %s, %s",
			u.Name, claimText(&spec), spec.Policy, momentText(req.Moment), sourceText(&spec)))
	}
	status := http.StatusAccepted
	if r.URL.Query().Get("wait") == "true" {
		bb = g.waitReady(r.Context(), bb.Name, bb)
	}
	if bb.Status.Phase == "Ready" {
		status = http.StatusOK
	}
	writeJSON(w, status, sessionOf(bb))
}

// claimText names the claim of a session for the audit trail.
func claimText(spec *v1alpha1.BackupBrowseSpec) string {
	if spec.FromCOS != nil {
		return "volume " + spec.FromCOS.Volume
	}
	return spec.PVCNamespace + "/" + spec.PVCName
}

// sessionSpec checks a request and works out the session it asks for. The right
// comes first: nothing is looked up for a user who may not read.
func (g *Gateway) sessionSpec(ctx context.Context, u User, req *SessionRequest) (v1alpha1.BackupBrowseSpec, *sourceError) {
	bad := func(msg string) (v1alpha1.BackupBrowseSpec, *sourceError) {
		return v1alpha1.BackupBrowseSpec{}, &sourceError{http.StatusBadRequest, "bad_request", msg}
	}
	spec := v1alpha1.BackupBrowseSpec{Policy: req.Policy, RequestedBy: u.Name}
	if req.FromCOS != nil {
		if req.Namespace != "" || req.PVC != "" {
			return bad("namespace and pvc are not used with fromCOS")
		}
		if req.Source != "" && req.Source != v1alpha1.BackupSourceCOS {
			return bad(`source must be "cos" with fromCOS`)
		}
		if err := checkFromCOS(req.Policy, req.FromCOS); err != nil {
			return bad(err.Error())
		}
		if !g.mayUseBucket(ctx, u, req.FromCOS) {
			return v1alpha1.BackupBrowseSpec{}, &sourceError{http.StatusForbidden, "forbidden", "you may not read the Secrets of that bucket in the driver namespace, and naming a bucket directly needs that right"}
		}
		spec.PVCNamespace, spec.PVCName = v1alpha1.NoNamespace, req.FromCOS.Volume
		spec.Source, spec.FromCOS = v1alpha1.BackupSourceCOS, req.FromCOS.DeepCopy()
		return spec, nil
	}
	if req.Policy == "" || req.Namespace == "" || req.PVC == "" {
		return bad("policy, namespace and pvc are required")
	}
	if !g.allowed(ctx, u, req.Namespace) {
		return v1alpha1.BackupBrowseSpec{}, &sourceError{http.StatusForbidden, "forbidden", forbiddenMessage}
	}
	policy := &v1alpha1.BackupPolicy{}
	var vol *v1alpha1.BackupVolumeStatus
	if err := g.c.Get(ctx, types.NamespacedName{Name: req.Policy}, policy); err == nil {
		vol = findVolume(policy, req.Namespace, req.PVC)
	}
	if vol == nil {
		return v1alpha1.BackupBrowseSpec{}, &sourceError{http.StatusNotFound, "not_found", "no backup of that volume"}
	}
	src, serr := chooseSource(policy, vol, req.Moment, req.Source)
	if serr != nil {
		return v1alpha1.BackupBrowseSpec{}, serr
	}
	spec.PVCNamespace, spec.PVCName, spec.Source = req.Namespace, req.PVC, src
	return spec, nil
}

func momentText(m *time.Time) string {
	if m == nil {
		return "latest copy"
	}
	return "moment " + m.UTC().Format(time.RFC3339)
}

func sessionOf(bb *v1alpha1.BackupBrowse) Session {
	phase := bb.Status.Phase
	if phase == "" {
		phase = "Pending"
	}
	return Session{Name: bb.Name, Phase: phase, Message: bb.Status.Message, Source: sourceName(bb.Spec.Source)}
}

func sameMoment(a *metav1.Time, b *time.Time) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	return a.Time.Equal(*b)
}

// sameBackup says whether an open session reads the backup a request asks for:
// the same policy, claim, moment and source.
func sameBackup(a, b *v1alpha1.BackupBrowseSpec, moment *time.Time) bool {
	if a.Policy != b.Policy || a.PVCNamespace != b.PVCNamespace || a.PVCName != b.PVCName ||
		sourceName(a.Source) != sourceName(b.Source) || !sameMoment(a.Moment, moment) {
		return false
	}
	switch {
	case a.FromCOS == nil && b.FromCOS == nil:
		return true
	case a.FromCOS == nil || b.FromCOS == nil:
		return false
	}
	return equality.Semantic.DeepEqual(a.FromCOS, b.FromCOS)
}

// findOrCreate returns the user's open session on this backup, or makes one. A
// session on the snapshot pool is never reused for a request for COS, nor the
// other way round.
func (g *Gateway) findOrCreate(ctx context.Context, u User, want v1alpha1.BackupBrowseSpec, moment *time.Time) (*v1alpha1.BackupBrowse, bool, error) {
	list := &v1alpha1.BackupBrowseList{}
	if err := g.c.List(ctx, list, client.MatchingLabels{userLabel: userHash(u.Name)}); err != nil {
		return nil, false, err
	}
	for i := range list.Items {
		b := &list.Items[i]
		if b.Spec.RequestedBy != u.Name || b.Spec.Close || b.Status.Phase == "Closed" || b.Status.Phase == "Failed" {
			continue
		}
		if sameBackup(&b.Spec, &want, moment) {
			return b, false, nil
		}
	}
	bb := &v1alpha1.BackupBrowse{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "bb-", Labels: map[string]string{userLabel: userHash(u.Name)}},
		Spec:       want,
	}
	if moment != nil {
		m := metav1.NewTime(*moment)
		bb.Spec.Moment = &m
	}
	if err := g.c.Create(ctx, bb); err != nil {
		return nil, false, err
	}
	return bb, true, nil
}

// waitReady polls until the session is Ready, ended, or the wait is over.
func (g *Gateway) waitReady(ctx context.Context, name string, bb *v1alpha1.BackupBrowse) *v1alpha1.BackupBrowse {
	deadline := time.Now().Add(g.cfg.SessionWait)
	for {
		cur := &v1alpha1.BackupBrowse{}
		if err := g.c.Get(ctx, types.NamespacedName{Name: name}, cur); err == nil {
			bb = cur
			switch cur.Status.Phase {
			case "Ready", "Closed", "Failed":
				return bb
			}
		}
		if !time.Now().Before(deadline) {
			return bb
		}
		select {
		case <-ctx.Done():
			return bb
		case <-time.After(g.cfg.PollInterval):
		}
	}
}

// session finds a session of this user. A session of another user, and one that
// does not exist, look the same. Then the user's right on the volume's namespace
// is checked again.
func (g *Gateway) session(w http.ResponseWriter, r *http.Request, u User) (*v1alpha1.BackupBrowse, bool) {
	bb := &v1alpha1.BackupBrowse{}
	err := g.c.Get(r.Context(), types.NamespacedName{Name: r.PathValue("name")}, bb)
	if err != nil || bb.Spec.RequestedBy != u.Name || bb.Labels[userLabel] != userHash(u.Name) {
		if err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Get a browse session failed")
		}
		notFoundSession(w)
		return nil, false
	}
	if !g.mayRead(r.Context(), u, bb.Spec.PVCNamespace, bb.Spec.FromCOS) {
		forbidden(w)
		return nil, false
	}
	return bb, true
}

func (g *Gateway) handleStatus(w http.ResponseWriter, r *http.Request, u User) {
	bb, ok := g.session(w, r, u)
	if !ok {
		return
	}
	writeJSON(w, http.StatusOK, sessionOf(bb))
}

func (g *Gateway) handleClose(w http.ResponseWriter, r *http.Request, u User) {
	bb, ok := g.session(w, r, u)
	if !ok {
		return
	}
	if !bb.Spec.Close {
		bb.Spec.Close = true
		if err := g.c.Update(r.Context(), bb); err != nil {
			klog.ErrorS(err, "Close a browse session failed")
			writeError(w, http.StatusInternalServerError, "internal", "cannot close the session")
			return
		}
		g.audit(r.Context(), bb, "SessionClosed", fmt.Sprintf("user %s closed the browse session on %s of policy %s, %s",
			u.Name, claimText(&bb.Spec), bb.Spec.Policy, sourceText(&bb.Spec)))
	}
	writeJSON(w, http.StatusOK, sessionOf(bb))
}

// helperClient returns an HTTP client that trusts only this session's certificate,
// and the session token.
func (g *Gateway) helperClient(ctx context.Context, bb *v1alpha1.BackupBrowse) (*http.Client, string, error) {
	sec := &corev1.Secret{}
	if err := g.c.Get(ctx, types.NamespacedName{Namespace: g.cfg.SessionNamespace, Name: bb.Status.SecretName}, sec); err != nil {
		return nil, "", err
	}
	cert := sec.Data["tls.crt"]
	key := sha256.Sum256(cert)
	g.mu.Lock()
	defer g.mu.Unlock()
	if c, ok := g.helpers[key]; ok {
		return c, string(sec.Data["token"]), nil
	}
	c, err := browse.PinnedClient(cert)
	if err != nil {
		return nil, "", err
	}
	g.helpers[key] = c
	return c, string(sec.Data["token"]), nil
}

// opBody checks the body of one call and returns what to send on, and the path.
func opBody(op string, r *http.Request) ([]byte, [][]byte, error) {
	var (
		v    any
		path func() [][]byte
	)
	switch op {
	case "list":
		req := &browse.ListRequest{}
		v, path = req, func() [][]byte { return req.Path }
	case "stat", "tar":
		req := &browse.PathRequest{}
		v, path = req, func() [][]byte { return req.Path }
	case "file":
		req := &browse.FileRequest{}
		v, path = req, func() [][]byte { return req.Path }
	case "info", "moments":
		v, path = &struct{}{}, func() [][]byte { return nil }
	default:
		return nil, nil, errUnknownOp
	}
	if err := decodeStrict(r, v); err != nil {
		return nil, nil, errBadBody
	}
	if err := browse.ValidatePath(path()); err != nil {
		return nil, nil, errBadBody
	}
	b, err := json.Marshal(v)
	return b, path(), err
}

var (
	errUnknownOp = errors.New("unknown call")
	errBadBody   = errors.New("bad body")
)

func (g *Gateway) handleOp(w http.ResponseWriter, r *http.Request, u User) {
	bb, ok := g.session(w, r, u)
	if !ok {
		return
	}
	g.forward(w, r, u, bb, r.PathValue("op"))
}

// handleDownload serves one file ("type=file") or one folder as a tar
// ("type=tar") to a GET, which is what a browser can stream to disk. The path
// is one "p" value per name, each name in base64 as in the other calls. It takes
// the same steps as the POST calls and sets headers that make a browser save the
// reply and never show it.
func (g *Gateway) handleDownload(w http.ResponseWriter, r *http.Request, u User) {
	bb, ok := g.session(w, r, u)
	if !ok {
		return
	}
	q := r.URL.Query()
	op := q.Get("type")
	if (op != "file" && op != "tar") || (op == "file" && len(q["p"]) == 0) {
		writeError(w, http.StatusBadRequest, "bad_path", "the request is not valid")
		return
	}
	names := make([]string, 0, len(q["p"]))
	for _, p := range q["p"] {
		j, _ := json.Marshal(p)
		names = append(names, string(j))
	}
	body := `{"path":[` + strings.Join(names, ",") + `]}`
	if op == "file" {
		body = `{"path":[` + strings.Join(names, ",") + `],"length":-1}`
	}
	r.Body = io.NopCloser(strings.NewReader(body))
	g.forward(w, r, u, bb, op)
}

// forward checks one call to the helper, sends it on and relays the reply.
func (g *Gateway) forward(w http.ResponseWriter, r *http.Request, u User, bb *v1alpha1.BackupBrowse, op string) {
	body, path, err := opBody(op, r)
	switch {
	case errors.Is(err, errUnknownOp):
		writeError(w, http.StatusNotFound, "not_found", "no such call")
		return
	case err != nil:
		writeError(w, http.StatusBadRequest, "bad_path", "the request is not valid")
		return
	}
	switch bb.Status.Phase {
	case "Ready":
	case "Closed", "Failed":
		writeError(w, http.StatusGone, "session_ended", "the session has ended; open a new one")
		return
	default:
		writeError(w, http.StatusConflict, "not_ready", "the session is not ready yet")
		return
	}
	hc, token, err := g.helperClient(r.Context(), bb)
	if err != nil {
		klog.ErrorS(err, "Cannot prepare the helper connection", "session", bb.Name)
		writeError(w, http.StatusBadGateway, "helper_unreachable", "cannot reach the browse helper")
		return
	}
	url := "https://" + net.JoinHostPort(bb.Status.PodIP, strconv.Itoa(int(bb.Status.Port))) + "/v1/" + op
	req, _ := http.NewRequestWithContext(r.Context(), http.MethodPost, url, strings.NewReader(string(body))) //nolint:gosec // G704: the address is the pod IP the controller wrote, not user input
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req) //nolint:gosec // G704: the address is the pod IP the controller wrote, not user input
	if err != nil {
		// The handshake failed, or the helper is gone. The reason may name an address.
		klog.V(2).InfoS("Helper call failed", "session", bb.Name, "op", op)
		writeError(w, http.StatusBadGateway, "helper_unreachable", "cannot reach the browse helper")
		return
	}
	defer func() { _ = resp.Body.Close() }()
	g.relay(w, r, u, bb, op, path, resp)
}

func (g *Gateway) relay(w http.ResponseWriter, r *http.Request, u User, bb *v1alpha1.BackupBrowse, op string, path [][]byte, resp *http.Response) {
	max := g.cfg.MaxDownloadBytes
	if op == "file" && resp.StatusCode == http.StatusOK && max > 0 && resp.ContentLength > max {
		writeError(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("a download is limited to %d bytes; ask for a range", max))
		return
	}
	for _, h := range []string{"Content-Type", "Content-Length", "X-Browse-File-Size"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	if (op == "file" || op == "tar") && resp.StatusCode == http.StatusOK {
		setDownloadHeaders(w, op, path)
	}
	if op == "tar" && resp.StatusCode == http.StatusOK {
		w.Header().Set("Trailer", "X-Browse-Report")
	}
	if resp.StatusCode == http.StatusOK {
		switch op {
		case "file":
			g.audit(r.Context(), bb, "FileDownloaded", fmt.Sprintf("user %s downloaded %s from %s of policy %s, %s, %s",
				u.Name, browse.JoinPath(path), claimText(&bb.Spec), bb.Spec.Policy, momentTextOf(bb), sourceText(&bb.Spec)))
		case "tar":
			g.audit(r.Context(), bb, "FolderDownloaded", fmt.Sprintf("user %s downloaded %s as a tar from %s of policy %s, %s, %s",
				u.Name, browse.JoinPath(path), claimText(&bb.Spec), bb.Spec.Policy, momentTextOf(bb), sourceText(&bb.Spec)))
		}
	}
	w.WriteHeader(resp.StatusCode)
	var dst io.Writer = w
	if op == "tar" && max > 0 {
		dst = &limitWriter{w: w, left: max}
	}
	if _, err := io.Copy(dst, resp.Body); err != nil {
		// Cut the connection so that a short or over-limit download cannot look whole.
		panic(http.ErrAbortHandler)
	}
	if rep := resp.Trailer.Get("X-Browse-Report"); rep != "" {
		w.Header().Set("X-Browse-Report", rep)
	}
}

func momentTextOf(bb *v1alpha1.BackupBrowse) string {
	if bb.Spec.Moment == nil {
		return "latest copy"
	}
	return "moment " + bb.Spec.Moment.UTC().Format(time.RFC3339)
}

// limitWriter fails once more than left bytes were written.
type limitWriter struct {
	w    io.Writer
	left int64
}

var errLimit = errors.New("download limit reached")

func (l *limitWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > l.left {
		p = p[:l.left]
		n, _ := l.w.Write(p)
		l.left = 0
		return n, errLimit
	}
	l.left -= int64(len(p))
	return l.w.Write(p)
}

// audit writes a Kubernetes Event on the session: who, which volume, which
// paths. Never contents, tokens or keys. A failure to write is logged only.
func (g *Gateway) audit(ctx context.Context, bb *v1alpha1.BackupBrowse, reason, msg string) {
	g.auditObject(ctx, "BackupBrowse", bb.Name, bb.UID, reason, msg)
}

// auditObject writes an Event on a session or a restore.
func (g *Gateway) auditObject(ctx context.Context, kind, name string, uid types.UID, reason, msg string) {
	if len(msg) > maxMessage {
		msg = msg[:maxMessage] + "..."
	}
	klog.InfoS("Backup audit", "kind", kind, "name", name, "reason", reason, "message", msg)
	now := metav1.NewTime(time.Now())
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "browse-audit-", Namespace: g.cfg.EventNamespace},
		InvolvedObject: corev1.ObjectReference{APIVersion: "v1", Kind: "ConfigMap",
			Namespace: g.cfg.EventNamespace, Name: AuditConfigMap},
		Related: &corev1.ObjectReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: kind, Name: name, UID: uid},
		Reason:  reason, Message: msg, Type: corev1.EventTypeNormal,
		Source:         corev1.EventSource{Component: "backup-gateway"},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	if err := g.c.Create(ctx, ev); err != nil {
		klog.ErrorS(err, "Could not write an audit Event", "kind", kind, "name", name)
	}
}

// setDownloadHeaders makes a browser save a reply and never show it. The reply
// is a backup file that someone else controlled, and the console serves it from
// its own origin, so it must never be taken for a page: it is always a download,
// never sniffed, and sandboxed if it is opened anyway.
func setDownloadHeaders(w http.ResponseWriter, op string, path [][]byte) {
	h := w.Header()
	if op == "tar" {
		h.Set("Content-Type", "application/x-tar")
	} else {
		h.Set("Content-Type", "application/octet-stream")
	}
	h.Set("Content-Disposition", contentDisposition(op, path))
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Content-Security-Policy", "sandbox; default-src 'none'")
	h.Set("Cache-Control", "no-store")
}

// contentDisposition names the saved file after the last name of the path. A
// name that is not plain printable text, or that holds a quote, a backslash,
// a slash or a percent sign, is replaced, so the header cannot be broken out of.
func contentDisposition(op string, path [][]byte) string {
	ext, name := "", "download"
	if op == "tar" {
		ext, name = ".tar", "backup"
	}
	fallback := name
	if len(path) > 0 {
		if n := path[len(path)-1]; safeFileName(n) {
			name = string(n)
			if isASCII(name) {
				fallback = name
			}
		}
	}
	return fmt.Sprintf(`attachment; filename="%s%s"; filename*=UTF-8''%s%s`, fallback, ext, attrEscape(name), ext)
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] > unicode.MaxASCII {
			return false
		}
	}
	return true
}

func safeFileName(n []byte) bool {
	if len(n) == 0 || !utf8.Valid(n) {
		return false
	}
	for _, r := range string(n) {
		if r == '"' || r == '\\' || r == '/' || r == '%' || !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

// attrEscape percent-encodes everything but the characters RFC 5987 allows in a value.
func attrEscape(s string) string {
	var b strings.Builder
	for _, c := range []byte(s) {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', strings.IndexByte("!#$&+-.^_`|~", c) >= 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, "%%%02X", c)
		}
	}
	return b.String()
}
