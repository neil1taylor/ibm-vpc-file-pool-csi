package pool

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"k8s.io/klog/v2"
)

type fakeTotals struct {
	ops, secs float64
	err       error
	calls     int
}

func (f *fakeTotals) Totals(_ context.Context, _ string) (float64, float64, error) {
	f.calls++
	return f.ops, f.secs, f.err
}

func newMeter(f *fakeTotals, now *time.Time) *LatencyMeter {
	return &LatencyMeter{Source: f, MinOps: 50, Window: 3, MinGap: 10 * time.Second, Now: func() time.Time { return *now }}
}

// read adds ops operations that took ms milliseconds each, waits 30 s, and reads.
func read(m *LatencyMeter, f *fakeTotals, now *time.Time, ops, ms float64) (float64, bool) {
	f.ops += ops
	f.secs += ops * ms / 1000
	*now = now.Add(30 * time.Second)
	return m.Latency(context.Background(), "p")
}

func TestLatencyMeter_NeedsTwoReadings(t *testing.T) {
	now := time.Now()
	f := &fakeTotals{}
	m := newMeter(f, &now)
	if _, ok := m.Latency(context.Background(), "p"); ok {
		t.Fatal("the first reading has nothing to compare with")
	}
	if ms, ok := read(m, f, &now, 3000, 2); !ok || ms < 1.99 || ms > 2.01 {
		t.Fatalf("got %v %v, want 2 ms", ms, ok)
	}
}

func TestLatencyMeter_MedianOfLastThree(t *testing.T) {
	now := time.Now()
	f := &fakeTotals{}
	m := newMeter(f, &now)
	_, _ = m.Latency(context.Background(), "p")
	read(m, f, &now, 3000, 2)
	read(m, f, &now, 3000, 2)
	ms, ok := read(m, f, &now, 3000, 200) // one bad interval
	if !ok || ms > 3 {
		t.Fatalf("one spike must not decide: got %v", ms)
	}
	ms, _ = read(m, f, &now, 3000, 200) // two bad ones in the last three
	if ms < 100 {
		t.Fatalf("two bad intervals in three must show: got %v", ms)
	}
}

func TestLatencyMeter_TooFewOperationsIsNoReading(t *testing.T) {
	now := time.Now()
	f := &fakeTotals{}
	m := newMeter(f, &now)
	_, _ = m.Latency(context.Background(), "p")
	if _, ok := read(m, f, &now, 10, 500); ok {
		t.Fatal("10 operations say nothing about the share")
	}
}

func TestLatencyMeter_ErrorIsNoReading(t *testing.T) {
	now := time.Now()
	f := &fakeTotals{}
	m := newMeter(f, &now)
	_, _ = m.Latency(context.Background(), "p")
	read(m, f, &now, 3000, 2)
	f.err = errors.New("node agent unreachable")
	if _, ok := read(m, f, &now, 3000, 2); ok {
		t.Fatal("a scrape that fails must give no reading, not the old one")
	}
}

func TestLatencyMeter_CounterResetIsIgnored(t *testing.T) {
	now := time.Now()
	f := &fakeTotals{}
	m := newMeter(f, &now)
	_, _ = m.Latency(context.Background(), "p")
	read(m, f, &now, 3000, 2)
	f.ops, f.secs = 100, 0.1 // a node agent restarted
	now = now.Add(30 * time.Second)
	if ms, ok := m.Latency(context.Background(), "p"); ok && ms > 5 {
		t.Fatalf("a reset must not give a wild number: %v", ms)
	}
}

func TestLatencyMeter_DoesNotScrapeTwiceInAShortTime(t *testing.T) {
	now := time.Now()
	f := &fakeTotals{}
	m := newMeter(f, &now)
	_, _ = m.Latency(context.Background(), "p")
	read(m, f, &now, 3000, 2)
	calls := f.calls
	now = now.Add(2 * time.Second)
	_, _ = m.Latency(context.Background(), "p")
	if f.calls != calls {
		t.Fatal("two copies on one share in one pass must share one scrape")
	}
}

func TestLatencyMeter_OldReadingsExpire(t *testing.T) {
	now := time.Now()
	f := &fakeTotals{}
	m := newMeter(f, &now)
	_, _ = m.Latency(context.Background(), "p")
	read(m, f, &now, 3000, 2)
	f.err = errors.New("down")
	now = now.Add(5 * time.Minute)
	if _, ok := m.Latency(context.Background(), "p"); ok {
		t.Fatal("a reading from five minutes ago is not a reading")
	}
}

func TestLatencyMeter_LogsWhyItCannotRead(t *testing.T) {
	var out bytes.Buffer
	klog.LogToStderr(false)
	klog.SetOutput(&out)
	t.Cleanup(func() { klog.LogToStderr(true); klog.SetOutput(nil) })
	now := time.Now()
	f := &fakeTotals{err: errors.New("node agent 10.0.0.1: HTTP 401")}
	m := newMeter(f, &now)
	_, _ = m.Latency(context.Background(), "p")
	klog.Flush()
	if !strings.Contains(out.String(), "HTTP 401") {
		t.Fatalf("the failure was not logged: %q", out.String())
	}
}
