package poolbench

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	authenticationv1 "k8s.io/api/authentication/v1"
	authorizationv1 "k8s.io/api/authorization/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
)

type fakeAuth struct{ allowed map[string]bool }

func (f fakeAuth) Allowed(_ context.Context, token, run string) (bool, error) {
	if token == "broken" {
		return false, errors.New("token review failed")
	}
	return f.allowed[token+"/"+run], nil
}

func newResultsServer(t *testing.T) *ResultsServer {
	t.Helper()
	s, err := NewStore(t.TempDir(), NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(1700000000, 0)
	_ = s.Add("t10k", VM{Name: "vm-001", OS: "linux"}, "PERF-READY", now)
	_ = s.Add("t10k", VM{Name: "vm-001", OS: "linux"}, steadyLine(1700000600), now)
	_ = s.Add("t10k", VM{Name: "vm-003", OS: "windows"}, steadyLine(1700000700), now)
	return &ResultsServer{Store: s, Auth: fakeAuth{allowed: map[string]bool{"alice/t10k": true}}}
}

func getAs(srv http.Handler, path, token string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestResultsServer_RawCSV(t *testing.T) {
	rec := getAs(newResultsServer(t), "/v1/runs/t10k/raw.csv", "alice")
	if rec.Code != http.StatusOK || !strings.HasPrefix(rec.Header().Get("Content-Type"), "text/csv") {
		t.Fatalf("got %d %s", rec.Code, rec.Header().Get("Content-Type"))
	}
	lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n")
	if lines[0] != "vm,os,epoch,phase,read_iops,write_iops,p50_ms,p95_ms,p99_ms,max_ms,kib_per_s" {
		t.Fatalf("header: %s", lines[0])
	}
	if len(lines) != 3 || !strings.HasPrefix(lines[1], "vm-001,linux,1700000600,steady,20,9,1,4,9,12,") ||
		!strings.HasPrefix(lines[2], "vm-003,windows,1700000700,steady,") {
		t.Fatalf("rows:\n%s", rec.Body.String())
	}
}

func TestResultsServer_VMLog(t *testing.T) {
	rec := getAs(newResultsServer(t), "/v1/runs/t10k/vms/vm-001/log", "alice")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "PERF-READY\nPERF 1700000600 steady") {
		t.Fatalf("got %d:\n%s", rec.Code, rec.Body.String())
	}
	if rec := getAs(newResultsServer(t), "/v1/runs/t10k/vms/vm-999/log", "alice"); rec.Code != http.StatusNotFound {
		t.Fatalf("an unknown VM: got %d", rec.Code)
	}
}

func TestResultsServer_Refuses(t *testing.T) {
	srv := newResultsServer(t)
	cases := []struct {
		name, path, token string
		want              int
	}{
		{"no token", "/v1/runs/t10k/raw.csv", "", http.StatusUnauthorized},
		{"not allowed to read the run", "/v1/runs/t10k/raw.csv", "bob", http.StatusForbidden},
		{"the check failed", "/v1/runs/t10k/raw.csv", "broken", http.StatusServiceUnavailable},
		{"a path out of the store", "/v1/runs/t10k/vms/..%2f..%2fetc/log", "alice", http.StatusNotFound},
		{"an unknown path", "/v1/runs/t10k/other", "alice", http.StatusNotFound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if rec := getAs(srv, tc.path, tc.token); rec.Code != tc.want {
				t.Fatalf("got %d, want %d", rec.Code, tc.want)
			}
		})
	}
}

// TestKubeAuth checks the token with a TokenReview and then asks, as that
// user, whether they may get the PoolBench object.
func TestKubeAuth(t *testing.T) {
	cs := kubefake.NewClientset()
	var sar *authorizationv1.SubjectAccessReview
	cs.PrependReactor("create", "tokenreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ca, _ := a.(k8stesting.CreateAction)
		tr, ok := ca.GetObject().(*authenticationv1.TokenReview)
		if !ok {
			return true, nil, errors.New("not a TokenReview")
		}
		tr.Status = authenticationv1.TokenReviewStatus{Authenticated: tr.Spec.Token == "good",
			User: authenticationv1.UserInfo{Username: "alice", Groups: []string{"admins"}}}
		return true, tr, nil
	})
	cs.PrependReactor("create", "subjectaccessreviews", func(a k8stesting.Action) (bool, runtime.Object, error) {
		ca, _ := a.(k8stesting.CreateAction)
		got, ok := ca.GetObject().(*authorizationv1.SubjectAccessReview)
		if !ok {
			return true, nil, errors.New("not a SubjectAccessReview")
		}
		sar = got
		sar.Status.Allowed = sar.Spec.ResourceAttributes.Name == "t10k"
		return true, sar, nil
	})
	auth := NewKubeAuth(cs)
	ok, err := auth.Allowed(context.Background(), "good", "t10k")
	if err != nil || !ok {
		t.Fatalf("want allowed, got %v %v", ok, err)
	}
	ra := sar.Spec.ResourceAttributes
	if sar.Spec.User != "alice" || sar.Spec.Groups[0] != "admins" || ra.Group != "storage.ibmcloud.io" ||
		ra.Resource != "poolbenches" || ra.Verb != "get" {
		t.Fatalf("access review: %+v %+v", sar.Spec, ra)
	}
	if ok, _ := auth.Allowed(context.Background(), "bad", "t10k"); ok {
		t.Fatal("an unknown token must not be allowed")
	}
	if ok, _ := auth.Allowed(context.Background(), "good", "other"); ok {
		t.Fatal("a run the user may not get must not be allowed")
	}
}
