package vmcopy

import "testing"

func TestAdaptNext(t *testing.T) {
	const mb = 1 << 20
	cfg := AdaptConfig{Start: 5 * mb, Step: 5 * mb, Max: 30 * mb, LimitMs: 15}
	tests := []struct {
		name    string
		cur     int64
		latency float64
		ok      bool
		want    int64
	}{
		{"quiet share: one step up", 10 * mb, 2, true, 15 * mb},
		{"never above the highest speed", 28 * mb, 2, true, 30 * mb},
		{"over the limit: halve", 20 * mb, 40, true, 10 * mb},
		{"never below the start speed", 6 * mb, 40, true, 5 * mb},
		{"exactly at the limit counts as quiet", 10 * mb, 15, true, 15 * mb},
		{"no reading: keep the speed", 10 * mb, 0, false, 10 * mb},
		{"no reading never speeds up from a low speed", 3 * mb, 0, false, 5 * mb},
		{"below the start speed goes to the start speed", 1 * mb, 2, true, 10 * mb},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := cfg.Next(tc.cur, tc.latency, tc.ok); got != tc.want {
				t.Fatalf("Next(%d, %v, %v) = %d, want %d", tc.cur, tc.latency, tc.ok, got, tc.want)
			}
		})
	}
}

func TestAdaptNoHighestSpeed(t *testing.T) {
	const mb = 1 << 20
	cfg := AdaptConfig{Start: 5 * mb, Step: 5 * mb, LimitMs: 15}
	if got := cfg.Next(500*mb, 2, true); got != 505*mb {
		t.Fatalf("with no highest speed the step still adds: got %d", got)
	}
}
