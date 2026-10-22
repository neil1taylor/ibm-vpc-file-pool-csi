package nodemetrics

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

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
