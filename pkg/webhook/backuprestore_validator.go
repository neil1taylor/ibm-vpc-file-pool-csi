package webhook

import (
	"context"
	"fmt"
	"reflect"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// BackupRestoreValidator validates BackupRestore create and update requests.
// Whether the policy and the backup exist is for the controller to say, in the
// status of the restore.
type BackupRestoreValidator struct{}

var _ admission.Validator[*v1alpha1.BackupRestore] = &BackupRestoreValidator{}

func (v *BackupRestoreValidator) ValidateCreate(_ context.Context, br *v1alpha1.BackupRestore) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating BackupRestore create", "name", br.Name)
	return nil, validateBackupRestoreSpec(&br.Spec)
}

// ValidateUpdate refuses a change to the spec: the restore may have made a PVC
// from the first request already.
func (v *BackupRestoreValidator) ValidateUpdate(_ context.Context, old, br *v1alpha1.BackupRestore) (admission.Warnings, error) {
	if !reflect.DeepEqual(old.Spec, br.Spec) {
		return nil, fmt.Errorf("the spec of a BackupRestore cannot change; make a new one")
	}
	return nil, nil
}

func (v *BackupRestoreValidator) ValidateDelete(_ context.Context, _ *v1alpha1.BackupRestore) (admission.Warnings, error) {
	return nil, nil
}

func validateBackupRestoreSpec(spec *v1alpha1.BackupRestoreSpec) error {
	required := []struct{ field, value string }{
		{"policy", spec.Policy},
		{"targetNamespace", spec.TargetNamespace},
		{"targetName", spec.TargetName},
	}
	if spec.FromCOS == nil {
		required = append(required,
			struct{ field, value string }{"sourceNamespace", spec.SourceNamespace},
			struct{ field, value string }{"sourceName", spec.SourceName})
	}
	for _, r := range required {
		if r.value == "" {
			return fmt.Errorf("spec.%s is required", r.field)
		}
	}
	if errs := validation.IsDNS1123Label(spec.TargetNamespace); len(errs) > 0 {
		return fmt.Errorf("spec.targetNamespace is not a namespace name: %v", errs)
	}
	if errs := validation.IsDNS1123Subdomain(spec.TargetName); len(errs) > 0 {
		return fmt.Errorf("spec.targetName is not a PVC name: %v", errs)
	}
	if spec.FromCOS != nil {
		return validateRestoreFromCOS(spec)
	}
	return nil
}

// validateRestoreFromCOS checks a restore that reads from a COS bucket. The
// policy name is the folder of the policy in the bucket, so it must be a name a
// BackupPolicy could have. The storage class is required because COS does not
// keep it.
func validateRestoreFromCOS(spec *v1alpha1.BackupRestoreSpec) error {
	if errs := validation.IsDNS1123Subdomain(spec.Policy); len(errs) > 0 || len(spec.Policy) > 63 {
		return fmt.Errorf("spec.policy %q is not a policy name (up to 63 characters)", spec.Policy)
	}
	if spec.StorageClassName == "" {
		return fmt.Errorf("spec.storageClassName is required with spec.fromCOS: COS does not keep the class")
	}
	return spec.FromCOS.Validate()
}
