package browse

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"k8s.io/klog/v2"
)

// Options sets the limits of a Server.
type Options struct {
	// Idle ends the session when no authenticated request has been served or
	// running for this long. Zero means no limit.
	Idle time.Duration
	// MaxLife ends the session this long after Run starts, even if it is in use.
	// Zero means no limit.
	MaxLife time.Duration
	// MaxStreams is how many file and tar streams may run at once. Zero means 8.
	MaxStreams int
}

// Why a session ended.
const (
	EndIdle    = "idle"
	EndMaxLife = "max-life"
	EndStopped = "stopped"
)

const (
	minTokenLen    = 16
	maxRequestBody = 1 << 20
	maxListLimit   = 1000
	streamChunk    = 256 << 10
)

// Server serves the browse API of one session over HTTP. The API takes paths
// below the root only, as lists of names, and never a string that a shell or a
// path parser could read differently.
type Server struct {
	tree      Tree
	tokenHash [sha256.Size]byte
	opts      Options
	streams   chan struct{}

	active     atomic.Int32
	lastActive atomic.Int64 // unix nanoseconds
}

// NewServer makes a server over tree. Every call needs the bearer token.
func NewServer(tree Tree, token string, opts Options) (*Server, error) {
	if err := CheckToken(token); err != nil {
		return nil, err
	}
	if opts.MaxStreams <= 0 {
		opts.MaxStreams = 8
	}
	s := &Server{tree: tree, tokenHash: sha256.Sum256([]byte(token)), opts: opts, streams: make(chan struct{}, opts.MaxStreams)}
	s.lastActive.Store(time.Now().UnixNano())
	return s, nil
}

// CheckToken refuses a session token that is too short to guess-proof the API.
func CheckToken(token string) error {
	if len(token) < minTokenLen {
		return fmt.Errorf("the session token must be at least %d characters", minTokenLen)
	}
	return nil
}

// Handler is the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		_, _ = io.WriteString(w, "ok")
	})
	mux.HandleFunc("/v1/list", s.api("list", s.handleList))
	mux.HandleFunc("/v1/stat", s.api("stat", s.handleStat))
	mux.HandleFunc("/v1/file", s.api("file", s.handleFile))
	mux.HandleFunc("/v1/tar", s.api("tar", s.handleTar))
	return mux
}

func (s *Server) touch() { s.lastActive.Store(time.Now().UnixNano()) }

// api checks the token and the method, and counts the call as activity.
func (s *Server) api(op string, h func(http.ResponseWriter, *http.Request)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.authorized(r) {
			writeJSON(w, http.StatusUnauthorized, errorBody{Code: "unauthorized", Message: "a valid session token is needed"})
			return
		}
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, errorBody{Code: CodeBadPath, Message: "use POST"})
			return
		}
		s.active.Add(1)
		s.touch()
		defer func() {
			s.active.Add(-1)
			s.touch()
		}()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			// Only the kind of call and its result: never a name or content.
			klog.V(2).InfoS("Browse call", "op", op, "status", sw.status)
		}()
		h(sw, r)
	}
}

func (s *Server) authorized(r *http.Request) bool {
	auth := r.Header.Get("Authorization")
	tok, ok := strings.CutPrefix(auth, "Bearer ")
	if !ok {
		return false
	}
	sum := sha256.Sum256([]byte(tok))
	return subtle.ConstantTimeCompare(sum[:], s.tokenHash[:]) == 1
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

type errorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

var statusOfCode = map[string]int{
	CodeBadPath: http.StatusBadRequest, CodeNotFound: http.StatusNotFound,
	CodeNotDir: http.StatusConflict, CodeNotFile: http.StatusConflict,
	CodeLink: http.StatusConflict, CodeSpecial: http.StatusConflict,
	CodeNotReadable: http.StatusForbidden, CodeInternal: http.StatusInternalServerError,
}

// writeErr sends err. An error that is not an *Error is internal, and its
// text (which may hold a path) is not sent.
func writeErr(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		klog.V(2).InfoS("Browse call failed", "err", "internal error")
		e = &Error{Code: CodeInternal, Msg: "internal error"}
	}
	status, ok := statusOfCode[e.Code]
	if !ok {
		status = http.StatusInternalServerError
	}
	writeJSON(w, status, errorBody{Code: e.Code, Message: e.Msg})
}

func badRequest(w http.ResponseWriter, msg string) {
	writeJSON(w, http.StatusBadRequest, errorBody{Code: CodeBadPath, Message: msg})
}

// decode reads one JSON request. Unknown fields and a second value are errors.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRequestBody))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		badRequest(w, "the request is not valid")
		return false
	}
	if dec.More() {
		badRequest(w, "the request holds more than one value")
		return false
	}
	return true
}

type pathReq struct {
	Path [][]byte `json:"path"`
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	var req struct {
		pathReq
		Cursor []byte `json:"cursor"`
		Limit  int    `json:"limit"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := ValidatePath(req.Path); err != nil {
		writeErr(w, err)
		return
	}
	if req.Limit <= 0 || req.Limit > maxListLimit {
		req.Limit = maxListLimit
	}
	page, err := s.tree.List(req.Path, req.Cursor, req.Limit)
	if err != nil {
		writeErr(w, err)
		return
	}
	if page.Entries == nil {
		page.Entries = []Entry{}
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) handleStat(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if !decode(w, r, &req) {
		return
	}
	if err := ValidatePath(req.Path); err != nil {
		writeErr(w, err)
		return
	}
	e, err := s.tree.Stat(req.Path)
	if err != nil {
		writeErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, e)
}

// acquire takes a stream slot, or answers 503.
func (s *Server) acquire(w http.ResponseWriter) bool {
	select {
	case s.streams <- struct{}{}:
		return true
	default:
		writeJSON(w, http.StatusServiceUnavailable, errorBody{Code: CodeInternal, Message: "too many downloads at once; try again"})
		return false
	}
}

func (s *Server) handleFile(w http.ResponseWriter, r *http.Request) {
	var req struct {
		pathReq
		Offset int64  `json:"offset"`
		Length *int64 `json:"length"`
	}
	if !decode(w, r, &req) {
		return
	}
	if err := ValidatePath(req.Path); err != nil {
		writeErr(w, err)
		return
	}
	if req.Offset < 0 {
		badRequest(w, "the offset is negative")
		return
	}
	if !s.acquire(w) {
		return
	}
	defer func() { <-s.streams }()
	f, err := s.tree.Open(req.Path)
	if err != nil {
		writeErr(w, err)
		return
	}
	defer func() { _ = f.Close() }()
	size := f.Size()
	off := min(req.Offset, size)
	n := size - off
	if req.Length != nil && *req.Length >= 0 && *req.Length < n {
		n = *req.Length
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(n))
	w.Header().Set("X-Browse-File-Size", fmt.Sprint(size))
	w.WriteHeader(http.StatusOK)
	copied, err := copyChunks(w, io.NewSectionReader(f, off, n))
	if err != nil || copied != n {
		// The file changed under us. Cut the connection so that the client
		// sees a short body and not a file that looks whole.
		panic(http.ErrAbortHandler)
	}
}

// copyChunks copies in chunks, and lets the client stop it.
func copyChunks(w io.Writer, r io.Reader) (int64, error) {
	buf := make([]byte, streamChunk)
	return io.CopyBuffer(w, struct{ io.Reader }{r}, buf)
}

// firstWriteWriter sets the headers of a tar stream when the first byte goes
// out, so that an error before it can still be a proper status.
type firstWriteWriter struct {
	w       http.ResponseWriter
	started bool
}

func (f *firstWriteWriter) Write(p []byte) (int, error) {
	if !f.started {
		f.started = true
		f.w.Header().Set("Content-Type", "application/x-tar")
		f.w.Header().Set("Trailer", "X-Browse-Report")
		f.w.WriteHeader(http.StatusOK)
	}
	return f.w.Write(p)
}

func (s *Server) handleTar(w http.ResponseWriter, r *http.Request) {
	var req pathReq
	if !decode(w, r, &req) {
		return
	}
	if err := ValidatePath(req.Path); err != nil {
		writeErr(w, err)
		return
	}
	if !s.acquire(w) {
		return
	}
	defer func() { <-s.streams }()
	fw := &firstWriteWriter{w: w}
	rep, err := s.tree.Tar(fw, req.Path)
	if err != nil {
		if !fw.started {
			writeErr(w, err)
			return
		}
		panic(http.ErrAbortHandler)
	}
	if !fw.started {
		// An empty tar still has its end marker, so this does not happen; be safe.
		_, _ = fw.Write(nil)
	}
	b, _ := json.Marshal(rep)
	w.Header().Set("X-Browse-Report", string(b))
}

// Run serves on ln until the session ends. It returns why.
func (s *Server) Run(ctx context.Context, ln net.Listener) (string, error) {
	srv := &http.Server{Handler: s.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	start := time.Now()
	s.touch()
	tick := time.Second
	for _, d := range []time.Duration{s.opts.Idle, s.opts.MaxLife} {
		if d > 0 && d/4 < tick {
			tick = max(d/4, 5*time.Millisecond)
		}
	}
	ticker := time.NewTicker(tick)
	defer ticker.Stop()

	reason := EndStopped
loop:
	for {
		select {
		case <-ctx.Done():
			break loop
		case err := <-errc:
			return "", err
		case now := <-ticker.C:
			if s.opts.MaxLife > 0 && now.Sub(start) >= s.opts.MaxLife {
				reason = EndMaxLife
				break loop
			}
			idleFor := now.Sub(time.Unix(0, s.lastActive.Load()))
			if s.opts.Idle > 0 && s.active.Load() == 0 && idleFor >= s.opts.Idle {
				reason = EndIdle
				break loop
			}
		}
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdown); err != nil {
		_ = srv.Close()
	}
	return reason, nil
}
