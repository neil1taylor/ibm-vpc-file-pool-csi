package filerestore

import (
	"strings"
	"testing"
	"time"
)

func TestFolderName(t *testing.T) {
	m := time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC)
	cases := []struct {
		policy string
		moment time.Time
		want   string
	}{
		{"nightly", m, "restored-nightly-20261022T091500Z"},
		{"nightly", time.Time{}, "restored-nightly-latest"},
		{"Night_ly/../x", m, "restored-night-ly----x-20261022T091500Z"},
	}
	for _, c := range cases {
		if got := FolderName(c.policy, c.moment); got != c.want {
			t.Errorf("FolderName(%q) = %q, want %q", c.policy, got, c.want)
		}
	}
}

func TestFolderNameIsShortAndStaysDistinctForLongPolicies(t *testing.T) {
	m := time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC)
	a := FolderName(strings.Repeat("a", 250)+"1", m)
	b := FolderName(strings.Repeat("a", 250)+"2", m)
	if len(a) > 200 || len(b) > 200 {
		t.Fatalf("too long: %d %d", len(a), len(b))
	}
	if a == b {
		t.Fatal("two policies that differ at the end got the same folder name")
	}
	if !strings.HasPrefix(a, "restored-aaa") || !strings.HasSuffix(a, "-20261022T091500Z") {
		t.Fatalf("name %q", a)
	}
}

func TestStagingNameUsesOnlyTheRestoresID(t *testing.T) {
	if got := StagingName("3f2c-A/../b"); got != ".restoring-3f2c-a----b" {
		t.Fatalf("staging name %q", got)
	}
}
