package poolbench

import (
	"fmt"
	"testing"
)

// fioJSON is a fio result as a VM prints it, with times in nanoseconds.
func fioJSON(key string, rIOPS, wIOPS, rP50, rP95, rP99, rMax, wP95, wMax int64) string {
	return fmt.Sprintf(`{"jobs":[{"read":{"iops":%d.6,"bw":%d,"%s":{"max":%d,"percentile":{"50.000000":%d,"95.000000":%d,"99.000000":%d}}},`+
		`"write":{"iops":%d.2,"bw":%d,"%s":{"max":%d,"percentile":{"50.000000":%d,"95.000000":%d,"99.000000":%d}}}}]}`,
		rIOPS, rIOPS*16, key, rMax, rP50, rP95, rP99, wIOPS, wIOPS*16, key, wMax, wP95/4, wP95, wMax)
}

func TestParseLine(t *testing.T) {
	line := "PERF 1700000100 steady " + fioJSON("lat_ns", 20, 9, 1000000, 4000000, 9000000, 12000000, 2000000, 3000000)
	r, ok := ParseLine(line)
	if !ok {
		t.Fatal("a PERF line should parse")
	}
	want := Result{
		Epoch: 1700000100, Phase: "steady", ReadIOPS: 20, WriteIOPS: 9,
		ReadP95Ms: 4, WriteP95Ms: 2, ReadP99Ms: 9, MaxMs: 12,
		P50Ms: 1, P95Ms: 4, P99Ms: 9, KBps: 20*16 + 9*16,
	}
	if r != want {
		t.Fatalf("got %+v, want %+v", r, want)
	}
}

func TestParseLine_CompletionLatencyWhenNoTotal(t *testing.T) {
	r, ok := ParseLine("PERF 1700000100 burst " + fioJSON("clat_ns", 20, 9, 1000000, 4000000, 9000000, 12000000, 2000000, 3000000))
	if !ok || r.ReadP95Ms != 4 || r.Phase != "burst" {
		t.Fatalf("got %+v, %v", r, ok)
	}
}

func TestParseLine_TotalLatencyWins(t *testing.T) {
	both := `{"jobs":[{"read":{"iops":20,"lat_ns":{"max":9000000,"percentile":{"95.000000":7000000,"99.000000":8000000}},` +
		`"clat_ns":{"max":1000000,"percentile":{"95.000000":1000000,"99.000000":1000000}}},` +
		`"write":{"iops":9,"lat_ns":{"max":3000000,"percentile":{"95.000000":2000000,"99.000000":3000000}},` +
		`"clat_ns":{"max":1000000,"percentile":{"95.000000":1000000,"99.000000":1000000}}}}]}`
	r, ok := ParseLine("PERF 1700000100 steady " + both)
	if !ok || r.ReadP95Ms != 7 || r.WriteP95Ms != 2 || r.ReadP99Ms != 8 || r.MaxMs != 9 {
		t.Fatalf("got %+v, %v", r, ok)
	}
}

func TestParseLine_RoundsToATenth(t *testing.T) {
	r, ok := ParseLine("PERF 1700000100 steady " + fioJSON("lat_ns", 20, 9, 1000000, 4260000, 9000000, 12000000, 2000000, 3000000))
	if !ok || r.ReadP95Ms != 4.3 {
		t.Fatalf("got %+v", r)
	}
}

func TestParseLine_Refuses(t *testing.T) {
	for _, line := range []string{
		"[  OK  ] Started cloud-init",
		"PERF-READY",
		"PERF 1700000100 steady {oops",
		"PERF 1700000100 steady",
		"PERF soon steady {}",
		`PERF 1700000100 steady {"jobs":[]}`,
		// The phase becomes a metric label, so only the two phases are taken.
		"PERF 1700000100 other " + fioJSON("lat_ns", 20, 9, 1000000, 4000000, 9000000, 12000000, 2000000, 3000000),
	} {
		if r, ok := ParseLine(line); ok {
			t.Errorf("%q: want no result, got %+v", line, r)
		}
	}
}
