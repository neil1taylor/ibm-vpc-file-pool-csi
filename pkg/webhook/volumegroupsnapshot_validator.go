package webhook

import (
	"context"
	"fmt"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// VolumeGroupSnapshotValidator validates VolumeGroupSnapshot create and update requests.
type VolumeGroupSnapshotValidator struct{}

var _ admission.Validator[*v1alpha1.VolumeGroupSnapshot] = &VolumeGroupSnapshotValidator{}

func (v *VolumeGroupSnapshotValidator) ValidateCreate(_ context.Context, vgs *v1alpha1.VolumeGroupSnapshot) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating VolumeGroupSnapshot create", "name", vgs.Name)
	return nil, validateVolumeGroupSnapshotSpec(&vgs.Spec)
}

func (v *VolumeGroupSnapshotValidator) ValidateUpdate(_ context.Context, _, vgs *v1alpha1.VolumeGroupSnapshot) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating VolumeGroupSnapshot update", "name", vgs.Name)
	return nil, validateVolumeGroupSnapshotSpec(&vgs.Spec)
}

func (v *VolumeGroupSnapshotValidator) ValidateDelete(_ context.Context, _ *v1alpha1.VolumeGroupSnapshot) (admission.Warnings, error) {
	return nil, nil
}

func validateVolumeGroupSnapshotSpec(spec *v1alpha1.VolumeGroupSnapshotSpec) error {
	if spec.PoolName == "" {
		return fmt.Errorf("spec.poolName is required")
	}
	if len(spec.SourcePVCs) == 0 {
		return fmt.Errorf("spec.sourcePVCs must not be empty")
	}
	if spec.FailurePolicy != "" && spec.FailurePolicy != "Abort" && spec.FailurePolicy != "Continue" {
		return fmt.Errorf("spec.failurePolicy must be 'Abort' or 'Continue', got %q", spec.FailurePolicy)
	}
	return nil
}
