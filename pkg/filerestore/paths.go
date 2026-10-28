package filerestore

import (
	"fmt"
	"strings"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// Limits on the list of paths of one restore. The list is kept in a ConfigMap,
// which holds 1 MiB at most.
const (
	MaxPaths      = 1000
	MaxPathLength = 4096
	MaxPathsTotal = 256 << 10
)

// CheckPaths checks the paths of a restore request, which are in the escaped
// form, and returns them as lists of names. The error says which path is bad
// by its number, never by its text.
func CheckPaths(escaped []string) ([][][]byte, error) {
	if len(escaped) == 0 {
		return nil, fmt.Errorf("spec.paths needs at least one path")
	}
	if len(escaped) > MaxPaths {
		return nil, fmt.Errorf("spec.paths holds %d paths; at most %d are allowed", len(escaped), MaxPaths)
	}
	total := 0
	out := make([][][]byte, 0, len(escaped))
	for i, s := range escaped {
		total += len(s) + 1
		if len(s) > MaxPathLength {
			return nil, fmt.Errorf("spec.paths[%d] is longer than %d bytes", i, MaxPathLength)
		}
		if strings.ContainsAny(s, "\n\r\x00") {
			return nil, fmt.Errorf("spec.paths[%d] holds a line break or a NUL byte; write them as \\x0a, \\x0d and \\x00", i)
		}
		path, err := browse.SplitPath(s)
		if err != nil {
			return nil, fmt.Errorf("spec.paths[%d] is not a valid escaped path", i)
		}
		if err := browse.ValidatePath(path); err != nil {
			return nil, fmt.Errorf("spec.paths[%d] is not a valid path: %v", i, err)
		}
		out = append(out, path)
	}
	if total > MaxPathsTotal {
		return nil, fmt.Errorf("spec.paths hold %d bytes together; at most %d are allowed", total, MaxPathsTotal)
	}
	return out, nil
}
