package webhook

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// minBackupSchedule is the shortest time between backups. A shorter one would
// start a copy before the last has had time to end.
const minBackupSchedule = time.Minute

// minBackupKeep is the shortest keep time.
const minBackupKeep = time.Hour

// BackupPolicyValidator validates BackupPolicy create and update requests.
// Checks that need other objects (the pool, its claims, other policies) are
// made by the controller, which reports them in the policy's status.
type BackupPolicyValidator struct{}

var _ admission.Validator[*v1alpha1.BackupPolicy] = &BackupPolicyValidator{}

func (v *BackupPolicyValidator) ValidateCreate(_ context.Context, bp *v1alpha1.BackupPolicy) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating BackupPolicy create", "name", bp.Name)
	// The name is the value of a label on every volume the policy backs up.
	if len(bp.Name) > 63 {
		return nil, fmt.Errorf("the name must be at most 63 characters, got %d", len(bp.Name))
	}
	return nil, validateBackupPolicySpec(&bp.Spec)
}

func (v *BackupPolicyValidator) ValidateUpdate(_ context.Context, _, bp *v1alpha1.BackupPolicy) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating BackupPolicy update", "name", bp.Name)
	return nil, validateBackupPolicySpec(&bp.Spec)
}

func (v *BackupPolicyValidator) ValidateDelete(_ context.Context, _ *v1alpha1.BackupPolicy) (admission.Warnings, error) {
	return nil, nil
}

func validateBackupPolicySpec(spec *v1alpha1.BackupPolicySpec) error {
	if spec.SnapshotPool == "" {
		return fmt.Errorf("spec.snapshotPool is required")
	}
	if spec.Schedule == "" {
		return fmt.Errorf("spec.schedule is required")
	}
	d, err := time.ParseDuration(spec.Schedule)
	if err != nil {
		return fmt.Errorf("spec.schedule is not a valid duration: %v", err)
	}
	if d < minBackupSchedule {
		return fmt.Errorf("spec.schedule must be at least %s, got %s", minBackupSchedule, spec.Schedule)
	}
	if spec.Selector == nil || (len(spec.Selector.MatchLabels) == 0 && len(spec.Selector.MatchExpressions) == 0) {
		return fmt.Errorf("spec.selector must select by label: an empty selector would back up every volume")
	}
	if _, selErr := metav1.LabelSelectorAsSelector(spec.Selector); selErr != nil {
		return fmt.Errorf("spec.selector is not valid: %v", selErr)
	}
	keep, err := v1alpha1.ParseKeepFor(spec.KeepFor)
	if err != nil {
		return fmt.Errorf("spec.keepFor: %v", err)
	}
	if keep < minBackupKeep {
		return fmt.Errorf("spec.keepFor must be at least %s, got %s", minBackupKeep, spec.KeepFor)
	}
	if spec.VMCopySpeedLimitMBps != nil && *spec.VMCopySpeedLimitMBps < 0 {
		return fmt.Errorf("spec.vmCopySpeedLimitMBps must be >= 0, got %d", *spec.VMCopySpeedLimitMBps)
	}
	for _, ns := range spec.Namespaces {
		if ns == "" {
			return fmt.Errorf("spec.namespaces must not contain an empty name")
		}
	}
	return nil
}
