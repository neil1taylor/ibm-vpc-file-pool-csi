package nodemetrics

import (
	"context"
	"errors"
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
