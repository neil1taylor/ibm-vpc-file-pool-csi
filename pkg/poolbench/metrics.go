package poolbench

import (
	"github.com/prometheus/client_golang/prometheus"
)

// Metrics are the collector's per-VM metrics. The console's live graphs read them.
type Metrics struct {
	iops       *prometheus.GaugeVec
	latency    *prometheus.GaugeVec
	throughput *prometheus.GaugeVec
	stalls     *prometheus.CounterVec
	results    *prometheus.CounterVec
	lastResult *prometheus.GaugeVec
}

// NewMetrics makes the metrics and registers them.
func NewMetrics(reg prometheus.Registerer) *Metrics {
	vm := []string{"run", "vm", "os"}
	phase := []string{"run", "vm", "os", "phase"}
	m := &Metrics{
		iops: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "poolbench_vm_iops",
			Help: "Reads plus writes a second of a test VM's last phase.",
		}, phase),
		latency: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "poolbench_vm_latency_seconds",
			Help: "Latency percentile (the worse of read and write) of a test VM's last phase.",
		}, append(phase, "quantile")),
		throughput: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "poolbench_vm_throughput_bytes_per_second",
			Help: "Read plus write throughput of a test VM's last phase.",
		}, phase),
		stalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "poolbench_vm_stalls_total",
			Help: "Phases of a test VM whose longest I/O was over 5 seconds.",
		}, vm),
		results: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "poolbench_vm_results_total",
			Help: "Result lines received from a test VM.",
		}, vm),
		lastResult: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "poolbench_vm_last_result_timestamp_seconds",
			Help: "Time of the last result line from a test VM.",
		}, vm),
	}
	reg.MustRegister(m.iops, m.latency, m.throughput, m.stalls, m.results, m.lastResult)
	return m
}

// stallMs is the I/O time the stall metric counts. The step's own stall time
// comes from the run's knee rule.
const stallMs = 5000

func (m *Metrics) observe(run string, vm VM, r Result) {
	l := prometheus.Labels{"run": run, "vm": vm.Name, "os": vm.OS}
	pl := prometheus.Labels{"run": run, "vm": vm.Name, "os": vm.OS, "phase": r.Phase}
	m.iops.With(pl).Set(float64(r.ReadIOPS + r.WriteIOPS))
	m.throughput.With(pl).Set(float64(r.KBps * 1024))
	for q, v := range map[string]float64{"0.5": r.P50Ms, "0.95": r.P95Ms, "0.99": r.P99Ms} {
		ql := prometheus.Labels{"quantile": q}
		for k, val := range pl {
			ql[k] = val
		}
		m.latency.With(ql).Set(v / 1000)
	}
	if r.MaxMs > stallMs {
		m.stalls.With(l).Inc()
	}
	m.results.With(l).Inc()
	m.lastResult.With(l).Set(float64(r.Epoch))
}

func (m *Metrics) forget(run string) {
	l := prometheus.Labels{"run": run}
	m.iops.DeletePartialMatch(l)
	m.latency.DeletePartialMatch(l)
	m.throughput.DeletePartialMatch(l)
	m.stalls.DeletePartialMatch(l)
	m.results.DeletePartialMatch(l)
	m.lastResult.DeletePartialMatch(l)
}
