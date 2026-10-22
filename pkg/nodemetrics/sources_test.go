package nodemetrics

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

func TestResolverFromPools(t *testing.T) {
	list := func(context.Context) ([]v1alpha1.FileSharePool, error) {
		p1 := v1alpha1.FileSharePool{}
		p1.Name = "gp"
		p1.Status.Shares = []v1alpha1.PoolShareStatus{
			{ShareName: "gp-1", ExportPath: "/share_a"},
			{ShareName: "gp-2"}, // no export path yet
		}
		p2 := v1alpha1.FileSharePool{}
		p2.Name = "fast"
		p2.Status.Shares = []v1alpha1.PoolShareStatus{{ShareName: "fast-1", ExportPath: "/share_b"}}
		return []v1alpha1.FileSharePool{p1, p2}, nil
	}
	got, err := ResolverFromPools(list)(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["/share_a"] != (ShareRef{"gp", "gp-1"}) || got["/share_b"] != (ShareRef{"fast", "fast-1"}) {
		t.Errorf("got %+v", got)
	}
}

func TestResolverFromPools_Error(t *testing.T) {
	_, err := ResolverFromPools(func(context.Context) ([]v1alpha1.FileSharePool, error) {
		return nil, errors.New("forbidden")
	})(context.Background())
	if err == nil {
		t.Fatal("want error")
	}
}

func TestStatfs_RealDirectory(t *testing.T) {
	total, free, err := Statfs(context.Background(), t.TempDir())
	if err != nil || total == 0 || free > total {
		t.Fatalf("total=%d free=%d err=%v", total, free, err)
	}
}

func TestStatfs_GivesUpOnAHungMount(t *testing.T) {
	block := make(chan struct{})
	defer close(block)
	start := time.Now()
	_, _, err := statfsWith(context.Background(), "/x", 50*time.Millisecond,
		func(string) (uint64, uint64, error) { <-block; return 0, 0, nil })
	if err == nil || time.Since(start) > 2*time.Second {
		t.Fatalf("want a timeout error quickly, got %v after %v", err, time.Since(start))
	}
}

func TestHandler_ServesMetrics(t *testing.T) {
	c := NewCollector(baseConfig())
	c.Refresh(context.Background())
	rec := httptest.NewRecorder()
	Handler(c).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, `vpc_file_pool_share_used_bytes{pool="gp",share="gp-share-1"}`) {
		t.Fatalf("code %d body:\n%s", rec.Code, body)
	}
}

func TestServe_AnswersUntilContextEnds(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Serve(ctx, l, Handler(NewCollector(baseConfig()))) }()

	resp, err := http.Get("http://" + l.Addr().String() + "/metrics")
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("get: %v %v", resp, err)
	}
	_ = resp.Body.Close()

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Serve returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve did not stop")
	}
}
