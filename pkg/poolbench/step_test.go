package poolbench

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// readLogs reads every *.log file of a folder as the results of one VM.
func readLogs(t *testing.T, dir string) map[string][]Result {
	t.Helper()
	files, err := filepath.Glob(filepath.Join(dir, "*.log"))
	if err != nil {
		t.Fatal(err)
	}
	out := map[string][]Result{}
	for _, f := range files {
		fh, err := os.Open(f)
		if err != nil {
			t.Fatal(err)
		}
		vm := strings.TrimSuffix(filepath.Base(f), ".log")
		out[vm] = nil
		sc := bufio.NewScanner(fh)
		for sc.Scan() {
			if r, ok := ParseLine(sc.Text()); ok {
				out[vm] = append(out[vm], r)
			}
		}
		_ = fh.Close()
	}
	return out
}

func defaultRules(limitMs float64) Rules {
	return Rules{SteadySecs: 555, BurstSecs: 45, LatencyLimitMs: limitMs, StallMs: 5000, TargetIOPS: 30}
}

// TestStep_MatchesBash reads the same VM results as print-golden.sh and must
// give the same numbers as the Bash tooling.
func TestStep_MatchesBash(t *testing.T) {
	logs := readLogs(t, "testdata/step")
	var got []string
	for _, ms := range []float64{10, 20, 30} {
		s := defaultRules(ms).Step(logs, 1700000000, 1700002000)
		got = append(got, fmt.Sprintf("slow%.0f=%d", ms, s.SlowPercent))
	}
	s := defaultRules(20).Step(logs, 1700000000, 1700002000)
	line := fmt.Sprintf("%s burstslow=%d stalls=%d short=%d guestiops=%d",
		strings.Join(got, " "), s.BurstSlowPercent, s.Stalls, s.ShortPercent, s.GuestIOPS)
	want, err := os.ReadFile("testdata/step.golden")
	if err != nil {
		t.Fatal(err)
	}
	if line != strings.TrimSpace(string(want)) {
		t.Fatalf("got  %s\nwant %s", line, strings.TrimSpace(string(want)))
	}
}

func TestInWindow(t *testing.T) {
	r := defaultRules(20)
	cases := []struct {
		res  Result
		want bool
	}{
		{Result{Epoch: 1700000555, Phase: "steady"}, true},  // started at the window's start
		{Result{Epoch: 1700000554, Phase: "steady"}, false}, // started a second before
		{Result{Epoch: 1700000045, Phase: "burst"}, true},
		{Result{Epoch: 1700002000, Phase: "burst"}, true},
		{Result{Epoch: 1700002001, Phase: "burst"}, false},
	}
	for _, tc := range cases {
		if got := r.InWindow(tc.res, 1700000000, 1700002000); got != tc.want {
			t.Errorf("%+v: got %v, want %v", tc.res, got, tc.want)
		}
	}
}

// TestStep_Latency checks the step's latency figures: the median over the
// VMs of each VM's worst steady p50, p95 and p99, and the summed throughput.
func TestStep_Latency(t *testing.T) {
	steady := func(epoch int64, p50, p95, p99 float64) Result {
		return Result{Epoch: epoch, Phase: "steady", ReadIOPS: 20, WriteIOPS: 10, P50Ms: p50, P95Ms: p95, P99Ms: p99, KBps: 480}
	}
	logs := map[string][]Result{
		"a": {steady(1700000600, 1, 2, 3), steady(1700001200, 1, 4, 5)},
		"b": {steady(1700000600, 2, 6, 8)},
		"c": {steady(1700000600, 3, 10, 12)},
	}
	s := defaultRules(20).Step(logs, 1700000000, 1700002000)
	if s.VMs != 3 || s.P50Ms != 2 || s.P95Ms != 6 || s.P99Ms != 8 {
		t.Fatalf("got %+v", s)
	}
	if s.KBps != 3*480 {
		t.Fatalf("throughput: got %d, want %d", s.KBps, 3*480)
	}
}

func TestStep_NoVMs(t *testing.T) {
	s := defaultRules(20).Step(nil, 1700000000, 1700002000)
	if s.VMs != 0 || s.SlowPercent != 0 || s.GuestIOPS != 0 {
		t.Fatalf("got %+v", s)
	}
}

func TestKnee(t *testing.T) {
	row := func(vms, slow int32) StepRow { return StepRow{VMs: vms, SlowPercent: slow} }
	cases := []struct {
		name     string
		rows     []StepRow
		knee     int32
		recommed int32
	}{
		{"first step over a quarter slow", []StepRow{row(15, 0), row(30, 10), row(45, 30), row(60, 60)}, 45, 30},
		{"exactly a quarter is not a knee", []StepRow{row(15, 25), row(30, 26)}, 30, 15},
		{"no knee recommends the largest step", []StepRow{row(15, 0), row(30, 0)}, 0, 30},
		{"knee at the first step recommends nothing", []StepRow{row(15, 30)}, 15, 0},
		{"a stall is a knee", []StepRow{row(15, 0), {VMs: 30, Stalls: 1}}, 30, 15},
		{"a slow burst on over a quarter is a knee", []StepRow{{VMs: 15, BurstSlowPercent: 30}}, 15, 0},
		{"a share at its limit is a knee", []StepRow{row(15, 0), row(30, 0), {VMs: 45, AtLimit: true}}, 45, 30},
		{"boot storm twice as slow per VM", []StepRow{{VMs: 30, BootStormSecs: 150}, {VMs: 45, BootStormSecs: 680}}, 45, 30},
		{"boot storm exactly twice per VM is not", []StepRow{{VMs: 30, BootStormSecs: 150}, {VMs: 45, BootStormSecs: 450}}, 0, 45},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			knee := Knee(tc.rows, 25)
			if knee != tc.knee {
				t.Fatalf("knee: got %d, want %d", knee, tc.knee)
			}
			if got := Recommended(tc.rows, knee); got != tc.recommed {
				t.Fatalf("recommended: got %d, want %d", got, tc.recommed)
			}
		})
	}
}
