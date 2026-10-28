package pool

import (
	"context"
	"sort"
	"sync"
	"time"

	"k8s.io/klog/v2"
)

// ShareTotals reads the NFS read and write operations of a pool's shares, and
// the seconds they took, added up over every node agent. The numbers only grow.
type ShareTotals interface {
	Totals(ctx context.Context, pool string) (ops, seconds float64, err error)
}

// maxReadingAge is how long an interval reading stays valid.
const maxReadingAge = 2 * time.Minute

// logGap is the shortest time between two logged read failures of one pool.
const logGap = 5 * time.Minute

// LatencyMeter turns the growing totals of a pool into the average latency of
// its shares over the last intervals. The speed of a VM disk copy follows it.
type LatencyMeter struct {
	Source ShareTotals
	// MinOps is the fewest operations an interval needs to count as a reading.
	MinOps float64
	// Window is how many interval readings the median is taken over, so that
	// one bad interval does not decide.
	Window int
	// MinGap is the shortest time between two reads of the node agents for one
	// pool. Copies that ask within it share the last answer.
	MinGap time.Duration
	Now    func() time.Time

	mu    sync.Mutex
	pools map[string]*poolReading
}

type poolReading struct {
	loggedAt time.Time
	have     bool
	at       time.Time
	ops      float64
	secs     float64
	samples  []latencySample
}

type latencySample struct {
	ms float64
	at time.Time
}

// Latency is the median of the latest interval readings of the average share
// latency in milliseconds. ok is false when there is no reading: this is the
// first call, the node agents could not be read, or the share did no work.
func (m *LatencyMeter) Latency(ctx context.Context, pool string) (float64, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pools == nil {
		m.pools = map[string]*poolReading{}
	}
	p := m.pools[pool]
	if p == nil {
		p = &poolReading{}
		m.pools[pool] = p
	}
	now := m.Now()
	if p.have && now.Sub(p.at) < m.MinGap {
		return p.median(now)
	}
	ops, secs, err := m.Source.Totals(ctx, pool)
	if err != nil {
		// A copy keeps its speed without a reading, so say why there is none.
		if now.Sub(p.loggedAt) >= logGap {
			klog.ErrorS(err, "Cannot read the latency of the pool's shares; VM disk copies keep their speed", "pool", pool)
			p.loggedAt = now
		}
		return 0, false
	}
	if p.have {
		dOps, dSecs := ops-p.ops, secs-p.secs
		if dOps >= m.MinOps && dSecs >= 0 {
			p.samples = append(p.samples, latencySample{ms: 1000 * dSecs / dOps, at: now})
			if len(p.samples) > m.Window {
				p.samples = p.samples[len(p.samples)-m.Window:]
			}
		}
	}
	p.have, p.at, p.ops, p.secs = true, now, ops, secs
	return p.median(now)
}

func (p *poolReading) median(now time.Time) (float64, bool) {
	var ms []float64
	for _, s := range p.samples {
		if now.Sub(s.at) <= maxReadingAge {
			ms = append(ms, s.ms)
		}
	}
	if len(ms) == 0 {
		return 0, false
	}
	sort.Float64s(ms)
	return ms[len(ms)/2], true
}
