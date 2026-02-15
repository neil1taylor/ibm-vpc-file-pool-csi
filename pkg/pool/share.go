package pool

import (
	"fmt"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

// selectShare picks a share from the pool based on the allocation strategy.
// Only shares in "stable" state are considered.
// Spread: pick the share with the most free space.
// Binpack: pick the share with the least free space that still fits.
func selectShare(strategy string, shares []v1alpha1.PoolShareStatus, requestedGB int64) (*v1alpha1.PoolShareStatus, error) {
	var bestIdx int = -1
	var bestFree int64

	for i := range shares {
		if shares[i].State != "stable" {
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
