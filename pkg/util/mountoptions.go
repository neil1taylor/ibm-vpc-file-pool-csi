package util

import "slices"

// DefaultNFSMountOptions are the NFS mount options every volume of the driver
// is mounted with. The node agent applies them, then the pool's own options.
// "sec=sys" is required: without it the share negotiates anonymous access and
// every file belongs to UID 99.
func DefaultNFSMountOptions() []string {
	return []string{"nfsvers=4.1", "soft", "timeo=600", "retrans=3", "sec=sys"}
}

// PoolMountOptions are the options a volume of the pool is mounted with: the
// defaults, changed by the pool's own mountOptions. A PersistentVolume that is
// not made by the driver but points at a pool's share (as failover makes) must
// carry them too.
func PoolMountOptions(poolOptions []string) []string {
	return MergeNFSMountOptions(DefaultNFSMountOptions(), slices.Clone(poolOptions))
}

// MergeNFSMountOptions merges custom mount flags with safe NFS defaults.
// Custom flags override defaults with the same key (e.g. "timeo=300" overrides
// "timeo=600"). The "soft" default is always preserved unless the custom flags
// explicitly include "hard".
func MergeNFSMountOptions(defaults, custom []string) []string {
	if len(custom) == 0 {
		return defaults
	}

	// optKey extracts the key portion of a mount option (before '=').
	optKey := func(opt string) string {
		for i, c := range opt {
			if c == '=' {
				return opt[:i]
			}
		}
		return opt
	}

	// Build a map of default options keyed by their prefix.
	merged := make(map[string]string, len(defaults)+len(custom))
	order := make([]string, 0, len(defaults)+len(custom))
	for _, opt := range defaults {
		key := optKey(opt)
		merged[key] = opt
		order = append(order, key)
	}

	// Apply custom options, overriding defaults with the same key.
	hasHard := false
	for _, opt := range custom {
		key := optKey(opt)
		if key == "hard" {
			hasHard = true
		}
		if _, exists := merged[key]; !exists {
			order = append(order, key)
		}
		merged[key] = opt
	}

	// If "hard" was explicitly specified, remove "soft" (they're mutually exclusive).
	if hasHard {
		delete(merged, "soft")
	}

	result := make([]string, 0, len(merged))
	for _, key := range order {
		if opt, ok := merged[key]; ok {
			result = append(result, opt)
		}
	}
	return result
}
