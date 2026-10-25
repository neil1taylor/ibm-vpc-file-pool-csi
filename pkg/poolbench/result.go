// Package poolbench runs the PoolBench performance test: test VMs on a new
// pool send one result line after each load phase, and the service turns
// them into the numbers of each step. The rules match the Bash tooling in
// test/perf, which the golden files in testdata tie it to. See
// docs/specs/2026-10-25-poolbench.md.
package poolbench

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
)

// Result is one load phase of one VM, from the line the VM sends:
//
//	PERF <epoch seconds> <steady|burst> <fio result as one line of JSON>
//
// Times are in milliseconds, rounded to 0.1 ms. Latency is the total latency
// (lat_ns, which includes waiting before submission) when fio reported it, and
// the completion latency (clat_ns) otherwise.
type Result struct {
	Epoch     int64
	Phase     string
	ReadIOPS  int64
	WriteIOPS int64
	// The fields the Bash tooling reads: read and write p95, read p99, and
	// the longest single I/O.
	ReadP95Ms  float64
	WriteP95Ms float64
	ReadP99Ms  float64
	MaxMs      float64
	// The worse of read and write, for the step table.
	P50Ms float64
	P95Ms float64
	P99Ms float64
	// KBps is the read plus write throughput in KiB a second.
	KBps int64
}

type fioLatency struct {
	Max        float64            `json:"max"`
	Percentile map[string]float64 `json:"percentile"`
}

type fioDirection struct {
	IOPS float64     `json:"iops"`
	BW   int64       `json:"bw"`
	Lat  *fioLatency `json:"lat_ns"`
	CLat *fioLatency `json:"clat_ns"`
}

func (d fioDirection) latency() fioLatency {
	if d.Lat != nil {
		return *d.Lat
	}
	if d.CLat != nil {
		return *d.CLat
	}
	return fioLatency{}
}

// ms turns nanoseconds into milliseconds rounded to 0.1, as jq's round does.
func ms(ns float64) float64 {
	return math.Floor(ns/1e6*10+0.5) / 10
}

// ParseLine reads one PERF line. ok is false for any other line, or a line
// whose JSON is broken.
func ParseLine(line string) (Result, bool) {
	fields := strings.SplitN(line, " ", 4)
	if len(fields) != 4 || fields[0] != "PERF" {
		return Result{}, false
	}
	epoch, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return Result{}, false
	}
	var out struct {
		Jobs []struct {
			Read  fioDirection `json:"read"`
			Write fioDirection `json:"write"`
		} `json:"jobs"`
	}
	if json.Unmarshal([]byte(fields[3]), &out) != nil || len(out.Jobs) == 0 {
		return Result{}, false
	}
	j := out.Jobs[0]
	r, w := j.Read.latency(), j.Write.latency()
	return Result{
		Epoch:      epoch,
		Phase:      fields[2],
		ReadIOPS:   int64(math.Floor(j.Read.IOPS)),
		WriteIOPS:  int64(math.Floor(j.Write.IOPS)),
		ReadP95Ms:  ms(r.Percentile["95.000000"]),
		WriteP95Ms: ms(w.Percentile["95.000000"]),
		ReadP99Ms:  ms(r.Percentile["99.000000"]),
		MaxMs:      ms(math.Max(r.Max, w.Max)),
		P50Ms:      ms(math.Max(r.Percentile["50.000000"], w.Percentile["50.000000"])),
		P95Ms:      ms(math.Max(r.Percentile["95.000000"], w.Percentile["95.000000"])),
		P99Ms:      ms(math.Max(r.Percentile["99.000000"], w.Percentile["99.000000"])),
		KBps:       j.Read.BW + j.Write.BW,
	}, true
}
