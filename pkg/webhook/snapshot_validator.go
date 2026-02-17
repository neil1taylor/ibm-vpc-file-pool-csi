package webhook

import (
	"context"
	"fmt"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// SnapshotValidator validates Snapshot create and update requests.
type SnapshotValidator struct{}

var _ admission.Validator[*v1alpha1.Snapshot] = &SnapshotValidator{}

func (v *SnapshotValidator) ValidateCreate(_ context.Context, snap *v1alpha1.Snapshot) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating Snapshot create", "name", snap.Name)
	return nil, validateSnapshotSpec(&snap.Spec)
}

func (v *SnapshotValidator) ValidateUpdate(_ context.Context, _, snap *v1alpha1.Snapshot) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating Snapshot update", "name", snap.Name)
	return nil, validateSnapshotSpec(&snap.Spec)
}

func (v *SnapshotValidator) ValidateDelete(_ context.Context, _ *v1alpha1.Snapshot) (admission.Warnings, error) {
	return nil, nil
}

func validateSnapshotSpec(spec *v1alpha1.SnapshotSpec) error {
	if spec.SourceSubVolume == "" {
		return fmt.Errorf("spec.sourceSubVolume is required")
	}
	if spec.PoolName == "" {
		return fmt.Errorf("spec.poolName is required")
	}
	return nil
}
