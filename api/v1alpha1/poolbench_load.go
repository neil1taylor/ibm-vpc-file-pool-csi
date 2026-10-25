package v1alpha1

import (
	"fmt"
	"strconv"
	"strings"
)

// BlockShare is one block size of a load and its share of the I/O.
type BlockShare struct {
	KiB     int64
	Percent int32
}

// ParseBlockMix reads a block mix such as "4k:50 16k:30 64k:20". Sizes take a
// k or m suffix; the shares must add up to 100.
func ParseBlockMix(mix string) ([]BlockShare, error) {
	var out []BlockShare
	var total int32
	for _, entry := range strings.Fields(mix) {
		size, weight, ok := strings.Cut(entry, ":")
		if !ok {
			return nil, fmt.Errorf("%q is not size:percent", entry)
		}
		kib, err := parseBlockSize(size)
		if err != nil {
			return nil, err
		}
		pct, err := strconv.ParseInt(weight, 10, 32)
		if err != nil || pct < 1 {
			return nil, fmt.Errorf("%q: the share must be a whole number from 1 to 100", entry)
		}
		out = append(out, BlockShare{KiB: kib, Percent: int32(pct)})
		total += int32(pct)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no block sizes")
	}
	if total != 100 {
		return nil, fmt.Errorf("the shares add up to %d, not 100", total)
	}
	return out, nil
}

func parseBlockSize(s string) (int64, error) {
	unit := int64(1)
	switch {
	case strings.HasSuffix(strings.ToLower(s), "k"):
		s = s[:len(s)-1]
	case strings.HasSuffix(strings.ToLower(s), "m"):
		s, unit = s[:len(s)-1], 1024
	default:
		return 0, fmt.Errorf("block size %q needs a k or m suffix", s)
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("block size %q is not a whole number", s)
	}
	return n * unit, nil
}
