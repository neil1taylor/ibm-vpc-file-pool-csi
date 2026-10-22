package browse

import (
	"fmt"
	"math/rand"
	"sort"
	"strings"
	"testing"
)

func TestCollectorKeepsOnlyOnePage(t *testing.T) {
	const total, limit = 30000, 10
	long := strings.Repeat("x", 200)
	names := make([]string, total)
	for i := range names {
		names[i] = fmt.Sprintf("%s%07d", long, i)
	}
	rand.New(rand.NewSource(1)).Shuffle(total, func(i, j int) { names[i], names[j] = names[j], names[i] })
	c := newPageCollector([]byte(long+"0000100"), limit)
	for _, n := range names {
		if err := c.add(n); err != nil {
			t.Fatal(err)
		}
	}
	if c.maxHeld > limit+1 {
		t.Errorf("held %d names at once, want at most %d", c.maxHeld, limit+1)
	}
	got, more := c.result()
	if !more || len(got) != limit || !sort.StringsAreSorted(got) {
		t.Fatalf("got %d names, more=%v, sorted=%v", len(got), more, sort.StringsAreSorted(got))
	}
	if got[0] != fmt.Sprintf("%s%07d", long, 101) || got[limit-1] != fmt.Sprintf("%s%07d", long, 110) {
		t.Errorf("wrong page: first %q last %q", got[0][200:], got[limit-1][200:])
	}
}

func TestCollectorLastPage(t *testing.T) {
	c := newPageCollector(nil, 5)
	for _, n := range []string{"c", "a", "b"} {
		_ = c.add(n)
	}
	got, more := c.result()
	if more || len(got) != 3 || got[0] != "a" || got[2] != "c" {
		t.Errorf("got %v more=%v", got, more)
	}
}

func TestCollectorRefusesHugeFolders(t *testing.T) {
	c := newPageCollector(nil, 1)
	var err error
	for i := 0; i <= MaxDirEntries && err == nil; i++ {
		err = c.add("n")
	}
	if err == nil {
		t.Error("a folder over MaxDirEntries must be refused")
	}
}
