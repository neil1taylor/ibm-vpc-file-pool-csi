package browse

import (
	"container/heap"
	"fmt"
	"sort"
)

type maxHeap []string

func (h maxHeap) Len() int           { return len(h) }
func (h maxHeap) Less(i, j int) bool { return h[i] > h[j] }
func (h maxHeap) Swap(i, j int)      { h[i], h[j] = h[j], h[i] }
func (h *maxHeap) Push(x any) {
	if s, ok := x.(string); ok {
		*h = append(*h, s)
	}
}
func (h *maxHeap) Pop() any {
	old := *h
	n := len(old)
	x := old[n-1]
	*h = old[:n-1]
	return x
}

// pageCollector keeps the smallest limit+1 names above a cursor while a folder
// is scanned, so memory stays bounded however many names, or how long names,
// the folder holds.
type pageCollector struct {
	cursor  string
	hasCur  bool
	limit   int
	h       maxHeap
	scanned int
	maxHeld int
}

func newPageCollector(cursor []byte, limit int) *pageCollector {
	return &pageCollector{cursor: string(cursor), hasCur: len(cursor) > 0, limit: limit}
}

// add offers one name. It fails once more than MaxDirEntries were offered.
func (c *pageCollector) add(name string) error {
	c.scanned++
	if c.scanned > MaxDirEntries {
		return &Error{Code: CodeInternal, Msg: fmt.Sprintf("the folder has more than %d names", MaxDirEntries)}
	}
	if c.hasCur && name <= c.cursor {
		return nil
	}
	switch {
	case len(c.h) < c.limit+1:
		heap.Push(&c.h, name)
	case name < c.h[0]:
		c.h[0] = name
		heap.Fix(&c.h, 0)
	}
	c.maxHeld = max(c.maxHeld, len(c.h))
	return nil
}

// result returns up to limit names in order, and whether more names follow.
func (c *pageCollector) result() (names []string, more bool) {
	names = append([]string(nil), c.h...)
	sort.Strings(names)
	if len(names) > c.limit {
		return names[:c.limit], true
	}
	return names, false
}
