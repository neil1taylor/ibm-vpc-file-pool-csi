package webhook

import (
	"context"
	"fmt"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// FileSharePoolValidator validates FileSharePool create and update requests.
type FileSharePoolValidator struct{}

var _ admission.Validator[*v1alpha1.FileSharePool] = &FileSharePoolValidator{}

func (v *FileSharePoolValidator) ValidateCreate(_ context.Context, pool *v1alpha1.FileSharePool) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating FileSharePool create", "name", pool.Name)
	return nil, validateFileSharePoolSpec(&pool.Spec)
}

func (v *FileSharePoolValidator) ValidateUpdate(_ context.Context, oldPool, newPool *v1alpha1.FileSharePool) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating FileSharePool update", "name", newPool.Name)

	if err := validateFileSharePoolSpec(&newPool.Spec); err != nil {
		return nil, err
	}

	// Immutable fields
	if oldPool.Spec.Zone != newPool.Spec.Zone {
		return nil, fmt.Errorf("spec.zone is immutable (was %q)", oldPool.Spec.Zone)
	}
	if oldPool.Spec.Profile != newPool.Spec.Profile {
		return nil, fmt.Errorf("spec.profile is immutable (was %q)", oldPool.Spec.Profile)
	}

	return nil, nil
}

func (v *FileSharePoolValidator) ValidateDelete(_ context.Context, _ *v1alpha1.FileSharePool) (admission.Warnings, error) {
	return nil, nil
}

func validateFileSharePoolSpec(spec *v1alpha1.FileSharePoolSpec) error {
	if spec.Zone == "" {
		return fmt.Errorf("spec.zone is required")
	}
	if spec.Profile == "" {
		return fmt.Errorf("spec.profile is required")
	}
	if spec.ShareSizeGB < 10 || spec.ShareSizeGB > 32000 {
		return fmt.Errorf("spec.shareSizeGB must be between 10 and 32000, got %d", spec.ShareSizeGB)
	}
	if spec.MaxShares < 1 || spec.MaxShares > 1000 {
		return fmt.Errorf("spec.maxShares must be between 1 and 1000, got %d", spec.MaxShares)
	}
	if spec.InitialShares > spec.MaxShares {
		return fmt.Errorf("spec.initialShares (%d) must not exceed spec.maxShares (%d)", spec.InitialShares, spec.MaxShares)
	}
	if spec.ExpandThresholdPercent < 1 || spec.ExpandThresholdPercent > 99 {
		return fmt.Errorf("spec.expandThresholdPercent must be between 1 and 99, got %d", spec.ExpandThresholdPercent)
	}
	if spec.AllocationStrategy != "" && spec.AllocationStrategy != "spread" && spec.AllocationStrategy != "binpack" {
		return fmt.Errorf("spec.allocationStrategy must be 'spread' or 'binpack', got %q", spec.AllocationStrategy)
	}
	return nil
}
