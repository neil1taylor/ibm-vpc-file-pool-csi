package poolbench

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
)

type fakeRuns struct {
	token string
	vms   map[string]VM // by IP
}

func (f fakeRuns) Token(_ context.Context, run string) (string, error) {
	if run != "t10k" {
		return "", os.ErrNotExist
	}
	return f.token, nil
}

func (f fakeRuns) VMByIP(_ context.Context, run, ip string) (VM, bool) {
	vm, ok := f.vms[ip]
	return vm, ok && run == "t10k"
}

func newTestCollector(t *testing.T) (*Collector, *prometheus.Registry) {
	t.Helper()
	reg := prometheus.NewRegistry()
	store, err := NewStore(t.TempDir(), NewMetrics(reg))
	if err != nil {
		t.Fatal(err)
	}
	runs := fakeRuns{token: "secret-token", vms: map[string]VM{
		"10.128.0.5": {Name: "pb-001", OS: "linux"},
		"10.128.0.6": {Name: "pb-003", OS: "windows"},
	}}
	return &Collector{Store: store, Runs: runs}, reg
}

func post(c *Collector, run, ip, token, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, "/v1/runs/"+run+"/results", strings.NewReader(body))
	req.RemoteAddr = ip + ":40000"
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	return rec
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func steadyLine(epoch int64) string {
	return "PERF " + itoa(epoch) + " steady " + fioJSON("lat_ns", 20, 9, 1000000, 4000000, 9000000, 12000000, 2000000, 3000000)
}

func TestCollector_StoresResults(t *testing.T) {
	c, reg := newTestCollector(t)
	if rec := post(c, "t10k", "10.128.0.5", "secret-token", "PERF-READY\n"+steadyLine(1700000600)+"\n"); rec.Code != http.StatusNoContent {
		t.Fatalf("got %d: %s", rec.Code, rec.Body)
	}
	got := c.Store.Results("t10k")
	if len(got["pb-001"]) != 1 || got["pb-001"][0].Epoch != 1700000600 {
		t.Fatalf("got %+v", got)
	}
	if _, ok := c.Store.Ready("t10k")["pb-001"]; !ok {
		t.Fatal("pb-001 said PERF-READY but is not ready")
	}
	if n := testutil.ToFloat64(c.Store.metrics.iops.WithLabelValues("t10k", "pb-001", "linux", "steady")); n != 29 {
		t.Fatalf("iops metric: got %v, want 29", n)
	}
	if n := testutil.CollectAndCount(reg, "poolbench_vm_latency_seconds"); n != 3 {
		t.Fatalf("want p50, p95 and p99 series, got %d", n)
	}
}

func TestCollector_Refuses(t *testing.T) {
	c, _ := newTestCollector(t)
	cases := []struct {
		name, run, ip, token string
		want                 int
	}{
		{"no token", "t10k", "10.128.0.5", "", http.StatusUnauthorized},
		{"wrong token", "t10k", "10.128.0.5", "nope", http.StatusUnauthorized},
		{"unknown run", "other", "10.128.0.5", "secret-token", http.StatusNotFound},
		{"not a VM of the run", "t10k", "10.128.0.99", "secret-token", http.StatusForbidden},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := post(c, tc.run, tc.ip, tc.token, steadyLine(1700000600)); rec.Code != tc.want {
				t.Fatalf("got %d, want %d", rec.Code, tc.want)
			}
		})
	}
	if len(c.Store.Results("t10k")) != 0 {
		t.Fatal("a refused post was stored")
	}
	req := httptest.NewRequest(http.MethodGet, "/v1/runs/t10k/results", nil)
	rec := httptest.NewRecorder()
	c.ServeHTTP(rec, req)
	if rec.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: got %d", rec.Code)
	}
}

func TestCollector_RefusesLargeBody(t *testing.T) {
	c, _ := newTestCollector(t)
	big := strings.Repeat("x", maxPostBytes+1)
	if rec := post(c, "t10k", "10.128.0.5", "secret-token", big); rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("got %d", rec.Code)
	}
}

// TestStore_KeepsLinesAndReloads checks that the raw lines are kept in the
// console log format, and that a new store (after a restart) reads them back.
func TestStore_KeepsLinesAndReloads(t *testing.T) {
	dir := t.TempDir()
	s, err := NewStore(dir, NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	for _, l := range []string{"PERF-READY", steadyLine(1700000600), "PERF-ERROR no disk", "noise"} {
		if err := s.Add("t10k", VM{Name: "pb-001", OS: "linux"}, l, now); err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(filepath.Join(dir, "t10k", "pb-001.log"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "PERF-READY\n") || !strings.Contains(string(raw), "PERF-ERROR no disk\n") || strings.Contains(string(raw), "noise") {
		t.Fatalf("raw log:\n%s", raw)
	}
	s2, err := NewStore(dir, NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	if len(s2.Results("t10k")["pb-001"]) != 1 {
		t.Fatalf("after reload: %+v", s2.Results("t10k"))
	}
	if _, ok := s2.Ready("t10k")["pb-001"]; !ok {
		t.Fatal("after reload pb-001 is not ready")
	}
	if errs := s2.Errors("t10k")["pb-001"]; len(errs) != 1 || errs[0] != "no disk" {
		t.Fatalf("errors: %v", errs)
	}
}

func TestStore_Forget(t *testing.T) {
	dir := t.TempDir()
	reg := prometheus.NewRegistry()
	s, _ := NewStore(dir, NewMetrics(reg))
	_ = s.Add("t10k", VM{Name: "pb-001", OS: "linux"}, steadyLine(1700000600), time.Now())
	if err := s.Forget("t10k"); err != nil {
		t.Fatal(err)
	}
	if len(s.Results("t10k")) != 0 {
		t.Fatal("results remain")
	}
	if _, err := os.Stat(filepath.Join(dir, "t10k")); !os.IsNotExist(err) {
		t.Fatalf("folder remains: %v", err)
	}
	if n := testutil.CollectAndCount(reg, "poolbench_vm_iops"); n != 0 {
		t.Fatalf("metric series remain: %d", n)
	}
}

func TestStore_RefusesBadNames(t *testing.T) {
	s, _ := NewStore(t.TempDir(), NewMetrics(prometheus.NewRegistry()))
	for _, bad := range []struct{ run, vm string }{{"../x", "pb-001"}, {"t10k", "../../etc"}, {"", "pb-001"}, {"t10k", "a/b"}} {
		if err := s.Add(bad.run, VM{Name: bad.vm, OS: "linux"}, "PERF-READY", time.Now()); err == nil {
			t.Errorf("%+v: want an error", bad)
		}
	}
}
