package poolbench

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// maxPostBytes is the largest body the collector reads. One fio result is
// about 10 KiB.
const maxPostBytes = 1 << 20

// maxResultsPerVM and maxErrorsPerVM bound what one VM can store. A VM sends
// about 12 results an hour, so the cap is far above any run; it stops a VM
// that sends too much from filling the results volume.
const (
	maxResultsPerVM = 500
	maxErrorsPerVM  = 20
)

// VM is a test VM as the collector knows it.
type VM struct {
	Name string
	OS   string // linux or windows
}

// Runs is what the collector asks the runner: the token of a run, and which
// of its VMs has an IP address.
type Runs interface {
	// Token is the run's secret; an error means the run is not known.
	Token(ctx context.Context, run string) (string, error)
	// VMByIP finds the VM of the run whose pod has this IP address.
	VMByIP(ctx context.Context, run, ip string) (VM, bool)
}

// validName is a Kubernetes object name; it keeps file paths inside the store.
var validName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)

type vmRecord struct {
	vm      VM
	ready   time.Time
	results []Result
	errors  []string
}

// Store keeps every line the VMs send. The raw lines go to one file for each
// VM, <dir>/<run>/<vm>.log, in the format of a serial console log, so the Bash
// tooling can read them too. A new store reads the files back.
type Store struct {
	dir     string
	metrics *Metrics
	mu      sync.Mutex
	runs    map[string]map[string]*vmRecord
}

// NewStore opens the store in dir and reads what is there.
func NewStore(dir string, metrics *Metrics) (*Store, error) {
	s := &Store{dir: dir, metrics: metrics, runs: map[string]map[string]*vmRecord{}}
	runs, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read the results folder: %w", err)
	}
	for _, run := range runs {
		if !run.IsDir() || !validName.MatchString(run.Name()) {
			continue
		}
		logs, _ := filepath.Glob(filepath.Join(dir, run.Name(), "*.log"))
		for _, f := range logs {
			name := strings.TrimSuffix(filepath.Base(f), ".log")
			if err := s.load(run.Name(), name, f); err != nil {
				return nil, err
			}
		}
	}
	return s, nil
}

func (s *Store) load(run, vm, path string) error {
	fh, err := os.Open(path) //nolint:gosec // the path is built from names checked against validName
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	defer func() { _ = fh.Close() }()
	info, _ := fh.Stat()
	rec := s.record(run, VM{Name: vm})
	sc := bufio.NewScanner(fh)
	sc.Buffer(make([]byte, 64*1024), maxPostBytes)
	for sc.Scan() {
		s.apply(rec, sc.Text(), info.ModTime())
	}
	return sc.Err()
}

func (s *Store) record(run string, vm VM) *vmRecord {
	if s.runs[run] == nil {
		s.runs[run] = map[string]*vmRecord{}
	}
	rec := s.runs[run][vm.Name]
	if rec == nil {
		rec = &vmRecord{vm: vm}
		s.runs[run][vm.Name] = rec
	}
	if vm.OS != "" {
		rec.vm.OS = vm.OS
	}
	return rec
}

// apply records one line. It is false for a line that is not ours, or one
// past the VM's cap.
func (s *Store) apply(rec *vmRecord, line string, now time.Time) bool {
	switch {
	case line == "PERF-READY":
		if rec.ready.IsZero() {
			rec.ready = now
		}
	case strings.HasPrefix(line, "PERF-ERROR "):
		if len(rec.errors) >= maxErrorsPerVM {
			return false
		}
		rec.errors = append(rec.errors, strings.TrimPrefix(line, "PERF-ERROR "))
	default:
		r, ok := ParseLine(line)
		if !ok || len(rec.results) >= maxResultsPerVM {
			return false
		}
		rec.results = append(rec.results, r)
	}
	return true
}

// Add records one line a VM sent. Lines that are not ours are dropped.
func (s *Store) Add(run string, vm VM, line string, now time.Time) error {
	if !validName.MatchString(run) || !validName.MatchString(vm.Name) {
		return fmt.Errorf("bad run or VM name %q/%q", run, vm.Name)
	}
	line = strings.TrimRight(line, "\r")
	s.mu.Lock()
	defer s.mu.Unlock()
	rec := s.record(run, vm)
	n := len(rec.results)
	if !s.apply(rec, line, now) {
		return nil
	}
	if len(rec.results) > n {
		s.metrics.observe(run, rec.vm, rec.results[n])
	}
	if err := os.MkdirAll(filepath.Join(s.dir, run), 0o750); err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(s.dir, run, vm.Name+".log"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600) //nolint:gosec // run and VM names are checked against validName above
	if err != nil {
		return err
	}
	if _, err := f.WriteString(line + "\n"); err != nil {
		_ = f.Close()
		return err
	}
	return f.Close()
}

// Results is every result of a run, by VM name.
func (s *Store) Results(run string) map[string][]Result {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]Result{}
	for name, rec := range s.runs[run] {
		out[name] = append([]Result(nil), rec.results...)
	}
	return out
}

// Ready is the time each VM of a run first said PERF-READY.
func (s *Store) Ready(run string) map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]time.Time{}
	for name, rec := range s.runs[run] {
		if !rec.ready.IsZero() {
			out[name] = rec.ready
		}
	}
	return out
}

// Errors is what each VM of a run reported as PERF-ERROR.
func (s *Store) Errors(run string) map[string][]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string][]string{}
	for name, rec := range s.runs[run] {
		if len(rec.errors) > 0 {
			out[name] = append([]string(nil), rec.errors...)
		}
	}
	return out
}

// Forget removes a run's lines, files and metrics.
func (s *Store) Forget(run string) error {
	if !validName.MatchString(run) {
		return fmt.Errorf("bad run name %q", run)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.runs, run)
	s.metrics.forget(run)
	return os.RemoveAll(filepath.Join(s.dir, run))
}

// Collector is the HTTP endpoint the test VMs post to:
//
//	POST /v1/runs/<run>/results
//	Authorization: Bearer <the run's token>
//	<one or more lines>
//
// The VM is found from the address the post comes from, so a VM cannot post
// as another one.
type Collector struct {
	Store *Store
	Runs  Runs
}

func (c *Collector) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	run, ok := strings.CutPrefix(r.URL.Path, "/v1/runs/")
	run, ok2 := strings.CutSuffix(run, "/results")
	if !ok || !ok2 || !validName.MatchString(run) {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "use POST", http.StatusMethodNotAllowed)
		return
	}
	token, err := c.Runs.Token(r.Context(), run)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	got, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if got == "" || subtle.ConstantTimeCompare([]byte(got), []byte(token)) != 1 {
		http.Error(w, "bad token", http.StatusUnauthorized)
		return
	}
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	vm, ok := c.Runs.VMByIP(r.Context(), run, ip)
	if !ok {
		http.Error(w, "not a VM of this run", http.StatusForbidden)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxPostBytes))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	now := time.Now()
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if err := c.Store.Add(run, vm, line, now); err != nil {
			klog.ErrorS(err, "PoolBench collector could not store a line", "run", run, "vm", vm.Name)
			http.Error(w, "store failed", http.StatusInternalServerError)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
