package poolbench

import (
	"context"
	"crypto/sha256"
	"encoding/csv"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
)

// Authorizer says whether the person with this token may read a run.
type Authorizer interface {
	Allowed(ctx context.Context, token, run string) (bool, error)
}

// ResultsServer gives the console the raw results of a run, through the
// console's plugin proxy, which passes on the person's own token:
//
//	GET /v1/runs/<run>/raw.csv          every result line of every VM, as CSV
//	GET /v1/runs/<run>/vms/<vm>/log     the raw lines one VM sent
//
// Only someone who may get the PoolBench object may read its results.
type ResultsServer struct {
	Store *Store
	Auth  Authorizer
}

func (s *ResultsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "use GET", http.StatusMethodNotAllowed)
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/runs/"), "/")
	if !strings.HasPrefix(r.URL.Path, "/v1/runs/") || len(parts) < 2 || !validName.MatchString(parts[0]) {
		http.NotFound(w, r)
		return
	}
	run := parts[0]
	token, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if token == "" {
		http.Error(w, "no token", http.StatusUnauthorized)
		return
	}
	ok, err := s.Auth.Allowed(r.Context(), token, run)
	if err != nil {
		klog.ErrorS(err, "PoolBench results: access check failed", "run", run)
		http.Error(w, "the access check failed", http.StatusServiceUnavailable)
		return
	}
	if !ok {
		http.Error(w, "you may not read this run", http.StatusForbidden)
		return
	}
	switch {
	case len(parts) == 2 && parts[1] == "raw.csv":
		s.rawCSV(w, run)
	case len(parts) == 4 && parts[1] == "vms" && parts[3] == "log" && validName.MatchString(parts[2]):
		data, err := s.Store.RawLog(run, parts[2])
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write(data)
	default:
		http.NotFound(w, r)
	}
}

func ms1(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

func (s *ResultsServer) rawCSV(w http.ResponseWriter, run string) {
	results := s.Store.Results(run)
	osOf := s.Store.OS(run)
	names := make([]string, 0, len(results))
	for vm := range results {
		names = append(names, vm)
	}
	sort.Strings(names)
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", run+"-raw.csv"))
	out := csv.NewWriter(w)
	_ = out.Write([]string{"vm", "os", "epoch", "phase", "read_iops", "write_iops", "p50_ms", "p95_ms", "p99_ms", "max_ms", "kib_per_s"})
	for _, vm := range names {
		for _, r := range results[vm] {
			_ = out.Write([]string{vm, osOf[vm], strconv.FormatInt(r.Epoch, 10), r.Phase,
				strconv.FormatInt(r.ReadIOPS, 10), strconv.FormatInt(r.WriteIOPS, 10),
				ms1(r.P50Ms), ms1(r.P95Ms), ms1(r.P99Ms), ms1(r.MaxMs), strconv.FormatInt(r.KBps, 10)})
		}
	}
	out.Flush()
}

// RawLog is the lines one VM of a run sent, as stored.
func (s *Store) RawLog(run, vm string) ([]byte, error) {
	if !validName.MatchString(run) || !validName.MatchString(vm) {
		return nil, errors.New("bad name")
	}
	return os.ReadFile(filepath.Join(s.dir, run, vm+".log")) //nolint:gosec // run and VM names are checked against validName
}

// OS is the OS of each VM of a run, as the collector learned it.
func (s *Store) OS(run string) map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]string{}
	for name, rec := range s.runs[run] {
		out[name] = rec.vm.OS
	}
	return out
}

// authCacheTTL is how long an access decision is kept, so a page that reads
// several files does not ask the API server each time.
const authCacheTTL = time.Minute

type authDecision struct {
	allowed bool
	until   time.Time
}

// KubeAuth checks a token with a TokenReview, then asks, as that person,
// whether they may get the PoolBench object of the run.
type KubeAuth struct {
	cs    kubernetes.Interface
	mu    sync.Mutex
	cache map[string]authDecision
}

// NewKubeAuth makes an Authorizer that asks the Kubernetes API.
func NewKubeAuth(cs kubernetes.Interface) *KubeAuth {
	return &KubeAuth{cs: cs, cache: map[string]authDecision{}}
}

func (a *KubeAuth) Allowed(ctx context.Context, token, run string) (bool, error) {
	key := fmt.Sprintf("%x/%s", sha256.Sum256([]byte(token)), run)
	a.mu.Lock()
	d, ok := a.cache[key]
	a.mu.Unlock()
	if ok && time.Now().Before(d.until) {
		return d.allowed, nil
	}
	tr, err := a.cs.AuthenticationV1().TokenReviews().Create(ctx, &authenticationv1.TokenReview{
		Spec: authenticationv1.TokenReviewSpec{Token: token},
	}, metav1.CreateOptions{})
	if err != nil {
		return false, fmt.Errorf("token review: %w", err)
	}
	allowed := false
	if tr.Status.Authenticated {
		extra := map[string]authorizationv1.ExtraValue{}
		for k, v := range tr.Status.User.Extra {
			extra[k] = authorizationv1.ExtraValue(v)
		}
		sar, err := a.cs.AuthorizationV1().SubjectAccessReviews().Create(ctx, &authorizationv1.SubjectAccessReview{
			Spec: authorizationv1.SubjectAccessReviewSpec{
				User: tr.Status.User.Username, Groups: tr.Status.User.Groups, UID: tr.Status.User.UID, Extra: extra,
				ResourceAttributes: &authorizationv1.ResourceAttributes{
					Group: "storage.ibmcloud.io", Resource: "poolbenches", Verb: "get", Name: run,
				},
			},
		}, metav1.CreateOptions{})
		if err != nil {
			return false, fmt.Errorf("access review: %w", err)
		}
		allowed = sar.Status.Allowed
	}
	a.mu.Lock()
	now := time.Now()
	for k, old := range a.cache {
		if now.After(old.until) {
			delete(a.cache, k)
		}
	}
	a.cache[key] = authDecision{allowed: allowed, until: now.Add(authCacheTTL)}
	a.mu.Unlock()
	return allowed, nil
}
