package pool

import (
	"fmt"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

// selectShare picks a share from the pool based on the allocation strategy.
// Only shares in "stable" state with a matching tier are considered.
// Spread: pick the share with the most free space.
// Binpack: pick the share with the least free space that still fits.
func selectShare(strategy string, shares []v1alpha1.PoolShareStatus, requestedGB int64, tier string) (*v1alpha1.PoolShareStatus, error) {
	var bestIdx = -1
	var bestFree int64

	for i := range shares {
		if shares[i].State != "stable" {
			continue
		}
		if shares[i].Tier != tier {
			continue
		}
		freeGB := shares[i].TotalGB - shares[i].AllocatedGB
		if freeGB < requestedGB {
			continue
		}

		switch strategy {
		case "spread":
			if bestIdx == -1 || freeGB > bestFree {
				bestIdx = i
				bestFree = freeGB
			}
		case "binpack":
			if bestIdx == -1 || freeGB < bestFree {
				bestIdx = i
				bestFree = freeGB
			}
		default:
			return nil, fmt.Errorf("unknown allocation strategy: %q", strategy)
		}
	}

	if bestIdx == -1 {
		return nil, ErrPoolExhausted
	}

	return &shares[bestIdx], nil
}

// selectShareForClone picks a share for a clone operation.
// It prefers the source's share when it has sufficient capacity; otherwise
// it falls back to the normal selectShare algorithm.
func selectShareForClone(strategy string, shares []v1alpha1.PoolShareStatus, requestedGB int64, tier string, sourceShareID string) (*v1alpha1.PoolShareStatus, error) {
	// 1. Check if source share has capacity
	for i := range shares {
		if shares[i].ShareID == sourceShareID && shares[i].State == "stable" && shares[i].Tier == tier {
			freeGB := shares[i].TotalGB - shares[i].AllocatedGB
			if freeGB >= requestedGB {
				return &shares[i], nil
			}
			break
		}
	}

	// 2. Fall back to normal selection
	return selectShare(strategy, shares, requestedGB, tier)
}
