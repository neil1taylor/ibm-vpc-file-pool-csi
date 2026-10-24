package nodemetrics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

const stage = "/var/data/kubelet/plugins/kubernetes.io/csi/vpc-file-pool.csi.ibm.io/h1/globalmount"

func baseConfig() Config {
	return Config{
		DriverName: "vpc-file-pool.csi.ibm.io",
		ReadMountStats: func() ([]NFSMount, error) {
			return []NFSMount{
				{Server: "10.0.0.5", Export: "/share_abc", MountPoint: stage, Ops: map[string]OpStats{
					"READ":    {Ops: 100, BytesRecv: 4000, RTTMillis: 300, ExecMillis: 400},
					"WRITE":   {Ops: 50, BytesSent: 2000, RTTMillis: 150, ExecMillis: 250},
					"GETATTR": {Ops: 10, RTTMillis: 20, ExecMillis: 30},
					"LOOKUP":  {Ops: 5, RTTMillis: 10, ExecMillis: 10},
				}},
				// a bind mount into a pod: must not be counted twice
				{Server: "10.0.0.5", Export: "/share_abc", MountPoint: "/pods/p1/mnt", Ops: map[string]OpStats{"READ": {Ops: 100}}},
				// some other NFS mount on the node: not ours
				{Server: "x", Export: "/y", MountPoint: "/mnt/other", Ops: map[string]OpStats{"READ": {Ops: 1}}},
			}, nil
		},
		Statfs: func(_ context.Context, path string) (uint64, uint64, error) {
			if path != stage {
				return 0, 0, errors.New("unexpected path " + path)
			}
			return 100 << 30, 60 << 30, nil
		},
		Resolve: func(context.Context) (map[string]ShareRef, error) {
			return map[string]ShareRef{"/share_abc": {Pool: "gp", Share: "gp-share-1"}}, nil
		},
	}
}

// gather returns every series as one line: name{label="v",...} value, labels sorted.
func gather(t *testing.T, c *Collector) string {
	t.Helper()
	reg := prometheus.NewPedanticRegistry()
	reg.MustRegister(c)
	mfs, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var b strings.Builder
	for _, mf := range mfs {
		for _, m := range mf.Metric {
			var ls []string
			for _, l := range m.Label {
				ls = append(ls, l.GetName()+"=\""+l.GetValue()+"\"")
			}
			v := 0.0
			switch {
			case m.Gauge != nil:
				v = m.Gauge.GetValue()
			case m.Counter != nil:
				v = m.Counter.GetValue()
			}
			fmt.Fprintf(&b, "%s{%s} %g\n", mf.GetName(), strings.Join(ls, ","), v)
		}
	}
	return b.String()
}

func wantLines(t *testing.T, got string, lines ...string) {
	t.Helper()
	for _, l := range lines {
		if !strings.Contains(got, l+"\n") {
			t.Errorf("missing %q in:\n%s", l, got)
		}
	}
}

func TestCollector_UsedAndSizeBytes(t *testing.T) {
	c := NewCollector(baseConfig())
	c.Refresh(context.Background())
	wantLines(t, gather(t, c),
		`vpc_file_pool_share_used_bytes{pool="gp",share="gp-share-1"} 4.294967296e+10`,
		`vpc_file_pool_share_size_bytes{pool="gp",share="gp-share-1"} 1.073741824e+11`,
	)
}

func TestCollector_NFSCountersGroupedByOp(t *testing.T) {
	c := NewCollector(baseConfig())
	c.Refresh(context.Background())
	wantLines(t, gather(t, c),
		`vpc_file_pool_share_nfs_operations_total{op="read",pool="gp",share="gp-share-1"} 100`,
		`vpc_file_pool_share_nfs_operations_total{op="write",pool="gp",share="gp-share-1"} 50`,
		`vpc_file_pool_share_nfs_operations_total{op="other",pool="gp",share="gp-share-1"} 15`,
		`vpc_file_pool_share_nfs_rtt_seconds_total{op="read",pool="gp",share="gp-share-1"} 0.3`,
		`vpc_file_pool_share_nfs_rtt_seconds_total{op="write",pool="gp",share="gp-share-1"} 0.15`,
		`vpc_file_pool_share_nfs_rtt_seconds_total{op="other",pool="gp",share="gp-share-1"} 0.03`,
		`vpc_file_pool_share_nfs_execute_seconds_total{op="read",pool="gp",share="gp-share-1"} 0.4`,
		`vpc_file_pool_share_nfs_execute_seconds_total{op="write",pool="gp",share="gp-share-1"} 0.25`,
		`vpc_file_pool_share_nfs_execute_seconds_total{op="other",pool="gp",share="gp-share-1"} 0.04`,
		`vpc_file_pool_share_nfs_bytes_total{direction="read",pool="gp",share="gp-share-1"} 4000`,
		`vpc_file_pool_share_nfs_bytes_total{direction="write",pool="gp",share="gp-share-1"} 2000`,
	)
}

func TestCollector_UnresolvedShareStillReported(t *testing.T) {
	cfg := baseConfig()
	cfg.Resolve = func(context.Context) (map[string]ShareRef, error) { return nil, errors.New("api down") }
	c := NewCollector(cfg)
	c.Refresh(context.Background())

	got := gather(t, c)
	if !strings.Contains(got, `vpc_file_pool_share_used_bytes{pool="unknown",share="share_abc"}`) {
		t.Errorf("want unresolved labels, got:\n%s", got)
	}
}

func TestCollector_KeepsLastNamesWhenResolveFails(t *testing.T) {
	cfg := baseConfig()
	fail := false
	good := cfg.Resolve
	cfg.Resolve = func(ctx context.Context) (map[string]ShareRef, error) {
		if fail {
			return nil, errors.New("api down")
		}
		return good(ctx)
	}
	c := NewCollector(cfg)
	c.Refresh(context.Background())
	fail = true
	c.Refresh(context.Background())
	if got := gather(t, c); !strings.Contains(got, `share="gp-share-1"`) {
		t.Errorf("lost names after a failed lookup:\n%s", got)
	}
}

func TestCollector_StatfsFailureDropsOnlySpaceMetrics(t *testing.T) {
	cfg := baseConfig()
	cfg.Statfs = func(context.Context, string) (uint64, uint64, error) { return 0, 0, errors.New("stale") }
	c := NewCollector(cfg)
	c.Refresh(context.Background())

	got := gather(t, c)
	if strings.Contains(got, "used_bytes") || strings.Contains(got, "size_bytes") {
		t.Errorf("space metrics must be absent when statfs fails:\n%s", got)
	}
	if !strings.Contains(got, "nfs_operations_total") {
		t.Errorf("NFS counters should remain:\n%s", got)
	}
}

func TestCollector_UnmountedShareDisappears(t *testing.T) {
	cfg := baseConfig()
	read := cfg.ReadMountStats
	empty := false
	cfg.ReadMountStats = func() ([]NFSMount, error) {
		if empty {
			return nil, nil
		}
		return read()
	}
	c := NewCollector(cfg)
	c.Refresh(context.Background())
	empty = true
	c.Refresh(context.Background())
	if got := gather(t, c); got != "" {
		t.Errorf("want no series, got:\n%s", got)
	}
}

func TestCollector_ReadErrorKeepsPreviousSnapshot(t *testing.T) {
	cfg := baseConfig()
	read := cfg.ReadMountStats
	broken := false
	cfg.ReadMountStats = func() ([]NFSMount, error) {
		if broken {
			return nil, errors.New("no /proc")
		}
		return read()
	}
	c := NewCollector(cfg)
	c.Refresh(context.Background())
	broken = true
	c.Refresh(context.Background())
	if got := gather(t, c); !strings.Contains(got, "used_bytes") {
		t.Errorf("snapshot lost after read error:\n%s", got)
	}
}

// Kubelet gives each volume its own staging path, so one node can have several
// staging mounts of the same share. Prometheus refuses a scrape that holds the
// same series twice, so the endpoint answered HTTP 500 and showed nothing.
func TestCollector_SameShareStagedTwiceIsReportedOnce(t *testing.T) {
	stage2 := "/var/data/kubelet/plugins/kubernetes.io/csi/vpc-file-pool.csi.ibm.io/h2/globalmount"
	cfg := baseConfig()
	cfg.ReadMountStats = func() ([]NFSMount, error) {
		return []NFSMount{
			{Server: "10.0.0.5", Export: "/share_abc", MountPoint: stage, Ops: map[string]OpStats{
				"READ":  {Ops: 100, BytesRecv: 4000, RTTMillis: 300, ExecMillis: 400},
				"WRITE": {Ops: 50, BytesSent: 2000, RTTMillis: 150, ExecMillis: 250},
			}},
			// the same share through a second volume's staging path, read a moment later
			{Server: "10.0.0.5", Export: "/share_abc", MountPoint: stage2, Ops: map[string]OpStats{
				"READ":  {Ops: 120, BytesRecv: 4800, RTTMillis: 360, ExecMillis: 480},
				"WRITE": {Ops: 50, BytesSent: 2000, RTTMillis: 150, ExecMillis: 250},
			}},
		}, nil
	}
	cfg.Statfs = func(context.Context, string) (uint64, uint64, error) { return 100 << 30, 60 << 30, nil }
	c := NewCollector(cfg)
	c.Refresh(context.Background())

	// gather uses a pedantic registry, which fails the test on a repeated series.
	got := gather(t, c)
	wantLines(t, got,
		// the larger counter wins: counters only grow, so it is the newer reading of the same client
		`vpc_file_pool_share_nfs_operations_total{op="read",pool="gp",share="gp-share-1"} 120`,
		`vpc_file_pool_share_nfs_operations_total{op="write",pool="gp",share="gp-share-1"} 50`,
		`vpc_file_pool_share_nfs_bytes_total{direction="read",pool="gp",share="gp-share-1"} 4800`,
		`vpc_file_pool_share_used_bytes{pool="gp",share="gp-share-1"} 4.294967296e+10`,
	)
	if n := strings.Count(got, "vpc_file_pool_share_used_bytes{"); n != 1 {
		t.Errorf("want one used_bytes series for the share, got %d:\n%s", n, got)
	}
}

// A share that is not resolved is labelled "unknown"; two such shares must stay apart.
func TestCollector_TwoUnresolvedSharesStayApart(t *testing.T) {
	cfg := baseConfig()
	cfg.Resolve = func(context.Context) (map[string]ShareRef, error) { return nil, errors.New("api down") }
	stage2 := "/var/data/kubelet/plugins/kubernetes.io/csi/vpc-file-pool.csi.ibm.io/h2/globalmount"
	cfg.ReadMountStats = func() ([]NFSMount, error) {
		return []NFSMount{
			{Export: "/share_aaa", MountPoint: stage, Ops: map[string]OpStats{"READ": {Ops: 1}}},
			{Export: "/share_bbb", MountPoint: stage2, Ops: map[string]OpStats{"READ": {Ops: 2}}},
		}, nil
	}
	cfg.Statfs = func(context.Context, string) (uint64, uint64, error) { return 100 << 30, 60 << 30, nil }
	c := NewCollector(cfg)
	c.Refresh(context.Background())
	wantLines(t, gather(t, c),
		`vpc_file_pool_share_nfs_operations_total{op="read",pool="unknown",share="share_aaa"} 1`,
		`vpc_file_pool_share_nfs_operations_total{op="read",pool="unknown",share="share_bbb"} 2`,
	)
}

func stagePath(i int) string {
	return fmt.Sprintf("/var/data/kubelet/plugins/kubernetes.io/csi/vpc-file-pool.csi.ibm.io/h%d/globalmount", i)
}

// A node can hold many stale staging mounts of deleted shares, and statfs on
// each one waits for its full timeout. Asked one after the other, 25 such
// mounts made one refresh take about four minutes.
func TestCollector_StatfsCallsRunAtTheSameTime(t *testing.T) {
	const mounts = 20
	cfg := baseConfig()
	cfg.ReadMountStats = func() ([]NFSMount, error) {
		var ms []NFSMount
		for i := range mounts {
			ms = append(ms, NFSMount{Export: fmt.Sprintf("/share_%d", i), MountPoint: stagePath(i), Ops: map[string]OpStats{}})
		}
		return ms, nil
	}
	var running, most atomic.Int32
	cfg.Statfs = func(context.Context, string) (uint64, uint64, error) {
		n := running.Add(1)
		defer running.Add(-1)
		for {
			m := most.Load()
			if n <= m || most.CompareAndSwap(m, n) {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
		return 100 << 30, 60 << 30, nil
	}
	c := NewCollector(cfg)
	c.Refresh(context.Background())

	if m := most.Load(); m < 2 || m > statfsParallel {
		t.Errorf("want between 2 and %d statfs calls at once, got %d", statfsParallel, m)
	}
	if n := strings.Count(gather(t, c), "vpc_file_pool_share_used_bytes{"); n != mounts {
		t.Errorf("want %d used_bytes series, got %d", mounts, n)
	}
}

// A mount whose statfs failed is not asked again until the back-off ends, so a
// dead mount does not cost a timeout on every refresh.
func TestCollector_FailedMountWaitsForBackoff(t *testing.T) {
	stage2 := stagePath(2)
	cfg := baseConfig()
	cfg.ReadMountStats = func() ([]NFSMount, error) {
		return []NFSMount{
			{Export: "/share_abc", MountPoint: stage, Ops: map[string]OpStats{}},
			{Export: "/share_dead", MountPoint: stage2, Ops: map[string]OpStats{"READ": {Ops: 7}}},
		}, nil
	}
	var mu sync.Mutex
	calls := map[string]int{}
	cfg.Statfs = func(_ context.Context, path string) (uint64, uint64, error) {
		mu.Lock()
		calls[path]++
		mu.Unlock()
		if path == stage2 {
			return 0, 0, errors.New("no answer in 10s")
		}
		return 100 << 30, 60 << 30, nil
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	cfg.Now = func() time.Time { return now }
	c := NewCollector(cfg)

	c.Refresh(context.Background())
	now = now.Add(statfsBackoff - time.Second)
	c.Refresh(context.Background())
	if calls[stage2] != 1 {
		t.Errorf("dead mount asked %d times inside the back-off, want 1", calls[stage2])
	}
	if calls[stage] != 2 {
		t.Errorf("good mount asked %d times, want 2", calls[stage])
	}
	got := gather(t, c)
	wantLines(t, got, `vpc_file_pool_share_nfs_operations_total{op="read",pool="unknown",share="share_dead"} 7`)
	if strings.Contains(got, `used_bytes{pool="unknown",share="share_dead"}`) {
		t.Errorf("a mount in back-off must have no space metrics:\n%s", got)
	}

	now = now.Add(2 * time.Second)
	c.Refresh(context.Background())
	if calls[stage2] != 2 {
		t.Errorf("dead mount asked %d times after the back-off, want 2", calls[stage2])
	}
}

// A mount that goes away is forgotten, so a new mount at the same path is asked at once.
func TestCollector_BackoffEndsWhenMountGoes(t *testing.T) {
	cfg := baseConfig()
	read := cfg.ReadMountStats
	gone := false
	cfg.ReadMountStats = func() ([]NFSMount, error) {
		if gone {
			return nil, nil
		}
		return read()
	}
	fail := true
	calls := 0
	cfg.Statfs = func(context.Context, string) (uint64, uint64, error) {
		calls++
		if fail {
			return 0, 0, errors.New("stale")
		}
		return 100 << 30, 60 << 30, nil
	}
	now := time.Date(2026, 10, 24, 12, 0, 0, 0, time.UTC)
	cfg.Now = func() time.Time { return now }
	c := NewCollector(cfg)

	c.Refresh(context.Background())
	gone = true
	c.Refresh(context.Background())
	gone, fail = false, false
	c.Refresh(context.Background())
	if calls != 2 {
		t.Errorf("statfs asked %d times, want 2", calls)
	}
	if got := gather(t, c); !strings.Contains(got, "used_bytes") {
		t.Errorf("want space metrics for the new mount:\n%s", got)
	}
}
