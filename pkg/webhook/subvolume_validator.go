package webhook

import (
	"context"
	"fmt"
	"regexp"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var subPathPattern = regexp.MustCompile(`^/pvcs/pvc-[a-f0-9-]{36}$`)

// SubVolumeValidator validates SubVolume create and update requests.
type SubVolumeValidator struct{}

var _ admission.Validator[*v1alpha1.SubVolume] = &SubVolumeValidator{}

func (v *SubVolumeValidator) ValidateCreate(_ context.Context, sv *v1alpha1.SubVolume) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating SubVolume create", "name", sv.Name)
	return nil, validateSubVolumeSpec(&sv.Spec)
}

func (v *SubVolumeValidator) ValidateUpdate(_ context.Context, _, sv *v1alpha1.SubVolume) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating SubVolume update", "name", sv.Name)
	return nil, validateSubVolumeSpec(&sv.Spec)
}

func (v *SubVolumeValidator) ValidateDelete(_ context.Context, _ *v1alpha1.SubVolume) (admission.Warnings, error) {
	return nil, nil
}

func validateSubVolumeSpec(spec *v1alpha1.SubVolumeSpec) error {
	if spec.PoolName == "" {
		return fmt.Errorf("spec.poolName is required")
	}
	if spec.PVCName == "" {
		return fmt.Errorf("spec.pvcName is required")
	}
	if spec.PVCNamespace == "" {
		return fmt.Errorf("spec.pvcNamespace is required")
	}
	if spec.RequestedGB <= 0 {
		return fmt.Errorf("spec.requestedGB must be > 0, got %d", spec.RequestedGB)
	}
	if !subPathPattern.MatchString(spec.SubPath) {
		return fmt.Errorf("spec.subPath must match %s, got %q", subPathPattern.String(), spec.SubPath)
	}
	return nil
}
