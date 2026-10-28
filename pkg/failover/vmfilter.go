package failover

import (
	"fmt"
	"sort"
	"strings"

	"k8s.io/apimachinery/pkg/util/validation"
)

// NormalizeVMs returns the VM filter as a sorted list without duplicates or
// empty names. An empty filter (all VMs) is nil.
func NormalizeVMs(in []string) []string {
	set := map[string]bool{}
	for _, v := range in {
		if v != "" {
			set[v] = true
		}
	}
	if len(set) == 0 {
		return nil
	}
	out := make([]string, 0, len(set))
	for v := range set {
		out = append(out, v)
	}
	sort.Strings(out)
	return out
}

// SameVMs reports whether two VM filters name the same VMs. The order and
// repeats do not matter.
func SameVMs(a, b []string) bool {
	x, y := NormalizeVMs(a), NormalizeVMs(b)
	if len(x) != len(y) {
		return false
	}
	for i := range x {
		if x[i] != y[i] {
			return false
		}
	}
	return true
}

// vmSet is the filter as a set, or nil if the filter is empty.
func vmSet(in []string) map[string]bool {
	n := NormalizeVMs(in)
	if n == nil {
		return nil
	}
	set := make(map[string]bool, len(n))
	for _, v := range n {
		set[v] = true
	}
	return set
}

// ValidateVMName checks one entry of the VM filter: "namespace/name".
func ValidateVMName(v string) error {
	ns, name, ok := strings.Cut(v, "/")
	if !ok || ns == "" || name == "" {
		return fmt.Errorf("%q is not a VM name; use namespace/name", v)
	}
	if errs := validation.IsDNS1123Label(ns); len(errs) > 0 {
		return fmt.Errorf("%q is not a VM name: the namespace is not valid: %s", v, strings.Join(errs, "; "))
	}
	if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
		return fmt.Errorf("%q is not a VM name: the name is not valid: %s", v, strings.Join(errs, "; "))
	}
	return nil
}

// ParseVMs reads a comma-separated VM filter, as the Job and the CLI take it.
// Spaces around a name are ignored. An empty text is no filter (nil).
func ParseVMs(csv string) ([]string, error) {
	var names []string
	for _, v := range strings.Split(csv, ",") {
		v = strings.TrimSpace(v)
		if v == "" {
			continue
		}
		if err := ValidateVMName(v); err != nil {
			return nil, err
		}
		names = append(names, v)
	}
	return NormalizeVMs(names), nil
}
