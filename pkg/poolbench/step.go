package poolbench

import (
	"math"
	"sort"
)

// BurstSlowMs is the burst p95 over which a VM's burst counts as slow. Bursts
// are short and expected to be slower, so the limit is looser than the steady one.
const BurstSlowMs = 50

// ShortPercent is the share of its target rate under which a VM's steady
// phase is short: a load generator that cannot keep its rate has been held
// back, which hides latency.
const ShortPercent = 90

// Rules are what a step is measured against.
type Rules struct {
	// SteadySecs and BurstSecs are the lengths of the two phases. A result is
	// sent when its phase ends, so the phase started that much earlier.
	SteadySecs int64
	BurstSecs  int64
	// LatencyLimitMs is the steady p95 over which a VM is slow.
	LatencyLimitMs float64
	// StallMs is the time of a single I/O that counts as a stall.
	StallMs float64
	// TargetIOPS is the steady rate of a VM; 0 turns off the short check.
	TargetIOPS int64
}

func (r Rules) phaseSecs(phase string) int64 {
	switch phase {
	case "steady":
		return r.SteadySecs
	case "burst":
		return r.BurstSecs
	}
	return 0
}

// InWindow is true when the phase of res ran wholly between start and end. A
// phase that started before the window would carry what happened before it,
// such as the disk copies of the step, into the measurement.
func (r Rules) InWindow(res Result, start, end int64) bool {
	return res.Epoch-r.phaseSecs(res.Phase) >= start && res.Epoch <= end
}

// vmStats is what one VM did in a window. A worst value of -1 means the VM
// had no phase of that kind in the window.
type vmStats struct {
	steadyP95, burstP95 float64
	p50, p95, p99       float64
	stalls, short       int
	iops, kbps          float64
	hasIOPS             bool
}

func (r Rules) vm(results []Result, start, end int64) vmStats {
	s := vmStats{steadyP95: -1, burstP95: -1, p50: -1, p95: -1, p99: -1}
	var iopsSum, kbpsSum, weight float64
	for _, res := range results {
		if !r.InWindow(res, start, end) {
			continue
		}
		if res.MaxMs > r.StallMs {
			s.stalls++
		}
		worst := math.Max(res.ReadP95Ms, res.WriteP95Ms)
		switch res.Phase {
		case "steady":
			s.steadyP95 = math.Max(s.steadyP95, worst)
			s.p50 = math.Max(s.p50, res.P50Ms)
			s.p95 = math.Max(s.p95, res.P95Ms)
			s.p99 = math.Max(s.p99, res.P99Ms)
			if r.TargetIOPS > 0 && float64(res.ReadIOPS+res.WriteIOPS) < float64(r.TargetIOPS)*ShortPercent/100 {
				s.short++
			}
		case "burst":
			s.burstP95 = math.Max(s.burstP95, worst)
		}
		w := float64(r.phaseSecs(res.Phase))
		if w == 0 {
			w = 1
		}
		iopsSum += float64(res.ReadIOPS+res.WriteIOPS) * w
		kbpsSum += float64(res.KBps) * w
		weight += w
	}
	if weight > 0 {
		s.iops, s.kbps, s.hasIOPS = iopsSum/weight, kbpsSum/weight, true
	}
	return s
}

// StepStats are the numbers of one step, over all its VMs.
type StepStats struct {
	VMs int
	// SlowPercent is the percent of VMs whose worst steady p95 was over the
	// limit. A VM with no steady phase in the window counts as slow: it could
	// not be shown to be fast.
	SlowPercent int32
	// BurstSlowPercent is the percent of VMs with a burst p95 over BurstSlowMs.
	BurstSlowPercent int32
	// ShortPercent is the percent of VMs that had a steady phase far under
	// their target rate.
	ShortPercent int32
	Stalls       int32
	// GuestIOPS is the VMs' own IOPS, added up: for each VM the mean of its
	// phases, weighted by the length of the phase.
	GuestIOPS int64
	// P50Ms, P95Ms and P99Ms are the median over the VMs of each VM's worst
	// steady value.
	P50Ms, P95Ms, P99Ms float64
	// KBps is the VMs' throughput, added up like GuestIOPS.
	KBps int64
}

// percentOf rounds as the Bash tooling does: (s*100 + t/2) / t, cut to a whole number.
func percentOf(s, t int) int32 {
	if t == 0 {
		return 0
	}
	return int32((float64(s)*100 + float64(t)/2) / float64(t))
}

// Step measures one step over the results of each VM, by VM name.
func (r Rules) Step(results map[string][]Result, start, end int64) StepStats {
	var out StepStats
	var slow, burstSlow, short int
	var iops, kbps float64
	var p50s, p95s, p99s []float64
	for _, vmResults := range results {
		out.VMs++
		s := r.vm(vmResults, start, end)
		out.Stalls += int32(min(s.stalls, maxResultsPerVM)) // #nosec G115 -- capped at maxResultsPerVM
		if s.steadyP95 < 0 || s.steadyP95 > r.LatencyLimitMs {
			slow++
		}
		if s.burstP95 > BurstSlowMs {
			burstSlow++
		}
		if s.short > 0 {
			short++
		}
		if s.hasIOPS {
			iops += s.iops
			kbps += s.kbps
		}
		if s.p95 >= 0 {
			p50s, p95s, p99s = append(p50s, s.p50), append(p95s, s.p95), append(p99s, s.p99)
		}
	}
	out.SlowPercent = percentOf(slow, out.VMs)
	out.BurstSlowPercent = percentOf(burstSlow, out.VMs)
	out.ShortPercent = percentOf(short, out.VMs)
	out.GuestIOPS = int64(math.RoundToEven(iops))
	out.KBps = int64(math.RoundToEven(kbps))
	out.P50Ms, out.P95Ms, out.P99Ms = median(p50s), median(p95s), median(p99s)
	return out
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	sort.Float64s(v)
	m := len(v) / 2
	if len(v)%2 == 1 {
		return v[m]
	}
	return (v[m-1] + v[m]) / 2
}

// StepRow is what the knee rule reads of one step.
type StepRow struct {
	VMs              int32
	SlowPercent      int32
	BurstSlowPercent int32
	Stalls           int32
	// BootStormSecs is the time of a boot storm at this step; 0 if none ran.
	BootStormSecs int64
	// AtLimit is true when the share ran at its IOPS limit.
	AtLimit bool
}

// Fails is true when the step is a knee on its own: more than slowPercent of
// the VMs slow, or with a slow burst, any stall, or the share at its limit.
func (s StepRow) Fails(slowPercent int32) bool {
	return s.SlowPercent > slowPercent || s.BurstSlowPercent > slowPercent || s.Stalls > 0 || s.AtLimit
}

// Knee is the VM count of the first step that fails, or whose boot storm
// takes more than twice as long per VM as the first boot storm. Boot storms
// are compared per VM because a storm of more VMs is longer by itself. 0
// means no step is a knee.
func Knee(rows []StepRow, slowPercent int32) int32 {
	base := -1.0
	for _, s := range rows {
		knee := s.Fails(slowPercent)
		if s.BootStormSecs > 0 && s.VMs > 0 {
			per := float64(s.BootStormSecs) / float64(s.VMs)
			if base < 0 {
				base = per
			} else if per > 2*base {
				knee = true
			}
		}
		if knee {
			return s.VMs
		}
	}
	return 0
}

// Recommended is the largest step at or below 80 percent of the knee. With no
// knee it is the largest step; with a knee at the first step it is 0.
func Recommended(rows []StepRow, knee int32) int32 {
	var best int32
	for _, s := range rows {
		if knee == 0 || s.VMs <= knee*80/100 {
			best = max(best, s.VMs)
		}
	}
	return best
}
