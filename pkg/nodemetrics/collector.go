package nodemetrics

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/klog/v2"
)

// unknownPool labels a share whose pool the node could not look up.
const unknownPool = "unknown"

// ShareRef names a share the way the controller's metrics do.
type ShareRef struct {
	Pool  string
	Share string
}

// Config holds what the collector reads from. Each field can be replaced in tests.
type Config struct {
	// DriverName picks out this driver's staging mounts.
	DriverName string
	// ReadMountStats lists the NFS mounts with their counters.
	ReadMountStats func() ([]NFSMount, error)
	// Statfs returns the size and the free bytes of the file system at path.
	Statfs func(ctx context.Context, path string) (total, free uint64, err error)
	// Resolve maps an NFS export path to the pool and share it belongs to.
	Resolve func(ctx context.Context) (map[string]ShareRef, error)
}

type shareSample struct {
	ref       ShareRef
	hasSpace  bool
	total     uint64
	used      uint64
	ops       map[string]OpStats // grouped: read, write, other
	bytesRead uint64
	bytesWrit uint64
}

// Collector is a Prometheus collector for the NFS shares staged on this node.
// Refresh does the reading; Collect only reports the last snapshot, so a scrape
// never waits on an NFS server.
type Collector struct {
	cfg Config

	mu      sync.RWMutex
	samples []shareSample
	names   map[string]ShareRef // last good export path -> share lookup

	usedDesc, sizeDesc, opsDesc, rttDesc, execDesc, bytesDesc *prometheus.Desc
}

// NewCollector creates a collector.
func NewCollector(cfg Config) *Collector {
	l := []string{"pool", "share"}
	lo := []string{"pool", "share", "op"}
	return &Collector{
		cfg:   cfg,
		names: map[string]ShareRef{},
		usedDesc: prometheus.NewDesc("vpc_file_pool_share_used_bytes",
			"Bytes written on one share of the pool, from statfs on the share mount of this node", l, nil),
		sizeDesc: prometheus.NewDesc("vpc_file_pool_share_size_bytes",
			"Size of one share of the pool as the NFS client sees it, from statfs on the share mount of this node", l, nil),
		opsDesc: prometheus.NewDesc("vpc_file_pool_share_nfs_operations_total",
			"NFS operations done on one share by this node, by kind (read, write, other)", lo, nil),
		rttDesc: prometheus.NewDesc("vpc_file_pool_share_nfs_rtt_seconds_total",
			"Seconds NFS requests spent between send and reply on one share, by kind", lo, nil),
		execDesc: prometheus.NewDesc("vpc_file_pool_share_nfs_execute_seconds_total",
			"Seconds NFS requests took from queueing to done on one share, by kind", lo, nil),
		bytesDesc: prometheus.NewDesc("vpc_file_pool_share_nfs_bytes_total",
			"Bytes read from and written to one share by this node", []string{"pool", "share", "direction"}, nil),
	}
}

// isStaging reports whether a mount point is a staging mount of this driver.
// The bind mounts into pods are the same file system and are left out so that
// nothing is counted twice.
func (c *Collector) isStaging(mountPoint string) bool {
	return filepath.Base(mountPoint) == "globalmount" &&
		strings.Contains(mountPoint, "/plugins/kubernetes.io/csi/"+c.cfg.DriverName+"/")
}

// kind groups an NFS operation name as read, write or other.
func kind(op string) string {
	switch op {
	case "READ":
		return "read"
	case "WRITE":
		return "write"
	}
	return "other"
}

// Refresh reads the mounts again and replaces the snapshot. If the mount list
// cannot be read, the old snapshot stays.
func (c *Collector) Refresh(ctx context.Context) {
	mounts, err := c.cfg.ReadMountStats()
	if err != nil {
		klog.ErrorS(err, "Cannot read NFS mount statistics")
		return
	}

	if resolved, err := c.cfg.Resolve(ctx); err != nil {
		klog.V(2).InfoS("Cannot look up pool shares; using the last names", "err", err)
	} else {
		c.mu.Lock()
		c.names = resolved
		c.mu.Unlock()
	}
	c.mu.RLock()
	names := c.names
	c.mu.RUnlock()

	var samples []shareSample
	for _, m := range mounts {
		if !c.isStaging(m.MountPoint) {
			continue
		}
		ref, ok := names[m.Export]
		if !ok {
			ref = ShareRef{Pool: unknownPool, Share: strings.TrimPrefix(m.Export, "/")}
		}
		s := shareSample{ref: ref, ops: map[string]OpStats{}}
		for op, st := range m.Ops {
			k := kind(op)
			agg := s.ops[k]
			agg.Ops += st.Ops
			agg.RTTMillis += st.RTTMillis
			agg.ExecMillis += st.ExecMillis
			s.ops[k] = agg
		}
		s.bytesRead = m.Ops["READ"].BytesRecv
		s.bytesWrit = m.Ops["WRITE"].BytesSent

		total, free, err := c.cfg.Statfs(ctx, m.MountPoint)
		if err != nil {
			klog.V(2).InfoS("statfs failed on share mount", "share", ref.Share, "err", err)
		} else if total >= free {
			s.hasSpace, s.total, s.used = true, total, total-free
		}
		samples = append(samples, s)
	}

	c.mu.Lock()
	c.samples = mergeByShare(samples)
	c.mu.Unlock()
}

// mergeByShare keeps one sample for each share. Kubelet gives each volume its
// own staging path, so a node can hold several staging mounts of the same
// share, and some of them can be stale mounts of a deleted pool. Prometheus
// refuses a scrape that holds the same series twice, which made the whole
// endpoint answer HTTP 500. The mounts of one share on one node go to the same
// NFS server, and every counter only grows, so the larger value is the newer
// reading and is never counted twice.
func mergeByShare(samples []shareSample) []shareSample {
	var merged []shareSample
	index := map[ShareRef]int{}
	for _, s := range samples {
		i, seen := index[s.ref]
		if !seen {
			index[s.ref] = len(merged)
			merged = append(merged, s)
			continue
		}
		m := &merged[i]
		if !m.hasSpace && s.hasSpace {
			m.hasSpace, m.total, m.used = true, s.total, s.used
		}
		for k, st := range s.ops {
			cur := m.ops[k]
			cur.Ops = max(cur.Ops, st.Ops)
			cur.RTTMillis = max(cur.RTTMillis, st.RTTMillis)
			cur.ExecMillis = max(cur.ExecMillis, st.ExecMillis)
			m.ops[k] = cur
		}
		m.bytesRead = max(m.bytesRead, s.bytesRead)
		m.bytesWrit = max(m.bytesWrit, s.bytesWrit)
	}
	return merged
}

// Run refreshes now and then every interval, until ctx ends.
func (c *Collector) Run(ctx context.Context, interval time.Duration) {
	c.Refresh(ctx)
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.Refresh(ctx)
		}
	}
}

// Describe implements prometheus.Collector.
func (c *Collector) Describe(ch chan<- *prometheus.Desc) {
	for _, d := range []*prometheus.Desc{c.usedDesc, c.sizeDesc, c.opsDesc, c.rttDesc, c.execDesc, c.bytesDesc} {
		ch <- d
	}
}

// Collect implements prometheus.Collector.
func (c *Collector) Collect(ch chan<- prometheus.Metric) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, s := range c.samples {
		p, n := s.ref.Pool, s.ref.Share
		if s.hasSpace {
			ch <- prometheus.MustNewConstMetric(c.usedDesc, prometheus.GaugeValue, float64(s.used), p, n)
			ch <- prometheus.MustNewConstMetric(c.sizeDesc, prometheus.GaugeValue, float64(s.total), p, n)
		}
		for k, st := range s.ops {
			ch <- prometheus.MustNewConstMetric(c.opsDesc, prometheus.CounterValue, float64(st.Ops), p, n, k)
			ch <- prometheus.MustNewConstMetric(c.rttDesc, prometheus.CounterValue, float64(st.RTTMillis)/1000, p, n, k)
			ch <- prometheus.MustNewConstMetric(c.execDesc, prometheus.CounterValue, float64(st.ExecMillis)/1000, p, n, k)
		}
		ch <- prometheus.MustNewConstMetric(c.bytesDesc, prometheus.CounterValue, float64(s.bytesRead), p, n, "read")
		ch <- prometheus.MustNewConstMetric(c.bytesDesc, prometheus.CounterValue, float64(s.bytesWrit), p, n, "write")
	}
}
