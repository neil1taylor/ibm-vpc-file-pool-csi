package browse

import (
	"testing"
	"time"
)

func TestPickMoment(t *testing.T) {
	names := []string{"20261022T090000Z", "20261022T100000Z", "20261022T110000Z", "notes.txt", "20261022T1100", "2026-10-22T11:00:00Z"}
	at := func(s string) time.Time {
		v, err := time.Parse(time.RFC3339, s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	tests := []struct {
		name       string
		names      []string
		at         time.Time
		want       string
		wantNewest bool
		wantErr    bool
	}{
		{"zero time takes the newest", names, time.Time{}, "20261022T110000Z", true, false},
		{"after the newest", names, at("2026-10-23T00:00:00Z"), "20261022T110000Z", true, false},
		{"exactly a moment", names, at("2026-10-22T10:00:00Z"), "20261022T100000Z", false, false},
		{"between two moments takes the earlier", names, at("2026-10-22T10:30:00Z"), "20261022T100000Z", false, false},
		{"before the first", names, at("2026-10-22T08:00:00Z"), "", false, true},
		{"no moments with a time", []string{"x"}, at("2026-10-22T10:00:00Z"), "", false, false},
		{"no moments, zero time", nil, time.Time{}, "", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, newest, err := PickMoment(tt.names, tt.at)
			if (err != nil) != tt.wantErr || got != tt.want || newest != tt.wantNewest {
				t.Errorf("PickMoment = %q, %v, %v; want %q, %v, err=%v", got, newest, err, tt.want, tt.wantNewest, tt.wantErr)
			}
		})
	}
}
