package util

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// FlagAlias is a flag that was renamed. The old name still works, is hidden
// from the usage text, and prints one line on stderr when it is used.
//
// Bool is true for a flag that takes no value.
type FlagAlias struct {
	Old, New string
	Bool     bool
}

func deprecationLine(a FlagAlias) string {
	return fmt.Sprintf("--%s is deprecated; use --%s\n", a.Old, a.New)
}

func conflict(a FlagAlias) error {
	return fmt.Errorf("--%s and --%s are both set and differ; use only --%s (--%s is deprecated)", a.Old, a.New, a.New, a.Old)
}

// RewriteAliasedArgs renames old flags to their new names in an argument list,
// for the standard flag package, which cannot hide a flag. Every aliased flag
// must take a value. Arguments after "--" are left alone.
func RewriteAliasedArgs(args []string, aliases []FlagAlias, stderr io.Writer) ([]string, error) {
	byOld, byNew := map[string]FlagAlias{}, map[string]FlagAlias{}
	for _, a := range aliases {
		byOld[a.Old], byNew[a.New] = a, a
	}
	out := append([]string(nil), args...)
	oldVal, newVal := map[string]string{}, map[string]string{}
	oldSeen := map[string]bool{}
	for i := 0; i < len(out); i++ {
		arg := out[i]
		if arg == "--" {
			break
		}
		if !strings.HasPrefix(arg, "-") {
			continue
		}
		dashes := arg[:len(arg)-len(strings.TrimLeft(arg, "-"))]
		name, val, hasVal := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		rest := arg[len(dashes)+len(name):] // "=value", or empty
		a, isOld := byOld[name]
		isNew := false
		if !isOld {
			a, isNew = byNew[name]
		}
		if !isOld && !isNew {
			continue
		}
		if !hasVal && !a.Bool && i+1 < len(out) {
			val = out[i+1]
		}
		if isOld {
			if !oldSeen[a.New] {
				_, _ = io.WriteString(stderr, deprecationLine(a))
			}
			oldSeen[a.New] = true
			oldVal[a.New] = val
			out[i] = dashes + a.New + rest
		} else {
			newVal[name] = val
		}
	}
	var news []string
	for n := range oldVal {
		news = append(news, n)
	}
	sort.Strings(news)
	for _, n := range news {
		if v, both := newVal[n]; both && v != oldVal[n] {
			return nil, conflict(byNew[n])
		}
	}
	return out, nil
}
