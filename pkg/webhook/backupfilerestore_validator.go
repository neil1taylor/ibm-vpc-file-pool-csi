package webhook

import (
	"context"
	"fmt"
	"reflect"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// maxFileRestoreSizeGB is the largest claim a file restore may make. It is the
// largest size of a VPC file share.
const maxFileRestoreSizeGB = 32000

// BackupFileRestoreValidator validates BackupFileRestore create and update
// requests. Whether the policy, the backup and the claims exist is for the
// controller to say, in the status of the restore.
type BackupFileRestoreValidator struct{}

var _ admission.Validator[*v1alpha1.BackupFileRestore] = &BackupFileRestoreValidator{}

func (v *BackupFileRestoreValidator) ValidateCreate(_ context.Context, br *v1alpha1.BackupFileRestore) (admission.Warnings, error) {
	klog.V(4).InfoS("Validating BackupFileRestore create", "name", br.Name)
	return nil, validateBackupFileRestoreSpec(&br.Spec)
}

// ValidateUpdate refuses a change to the spec: the restore may have made a
// claim or started a copy from the first request already.
func (v *BackupFileRestoreValidator) ValidateUpdate(_ context.Context, old, br *v1alpha1.BackupFileRestore) (admission.Warnings, error) {
	if !reflect.DeepEqual(old.Spec, br.Spec) {
		return nil, fmt.Errorf("the spec of a BackupFileRestore cannot change; make a new one")
	}
	return nil, nil
}

func (v *BackupFileRestoreValidator) ValidateDelete(_ context.Context, _ *v1alpha1.BackupFileRestore) (admission.Warnings, error) {
	return nil, nil
}

func validateBackupFileRestoreSpec(spec *v1alpha1.BackupFileRestoreSpec) error {
	for _, r := range []struct{ field, value string }{
		{"policy", spec.Policy},
		{"sourceNamespace", spec.SourceNamespace},
		{"sourceName", spec.SourceName},
		{"targetNamespace", spec.TargetNamespace},
		{"targetName", spec.TargetName},
	} {
		if r.value == "" {
			return fmt.Errorf("spec.%s is required", r.field)
		}
	}
	if errs := validation.IsDNS1123Subdomain(spec.Policy); len(errs) > 0 || len(spec.Policy) > 63 {
		return fmt.Errorf("spec.policy %q is not a policy name (up to 63 characters)", spec.Policy)
	}
	for _, ns := range []struct{ field, value string }{{"sourceNamespace", spec.SourceNamespace}, {"targetNamespace", spec.TargetNamespace}} {
		if errs := validation.IsDNS1123Label(ns.value); len(errs) > 0 {
			return fmt.Errorf("spec.%s is not a namespace name: %v", ns.field, errs)
		}
	}
	for _, n := range []struct{ field, value string }{{"sourceName", spec.SourceName}, {"targetName", spec.TargetName}} {
		if errs := validation.IsDNS1123Subdomain(n.value); len(errs) > 0 {
			return fmt.Errorf("spec.%s is not a PVC name: %v", n.field, errs)
		}
	}
	if _, err := filerestore.CheckPaths(spec.Paths); err != nil {
		return err
	}
	if spec.SizeGB < 0 || spec.SizeGB > maxFileRestoreSizeGB {
		return fmt.Errorf("spec.sizeGB must be between 0 and %d", maxFileRestoreSizeGB)
	}
	if spec.Existing {
		if spec.SizeGB != 0 {
			return fmt.Errorf("spec.sizeGB is for a new claim; an existing claim keeps its size")
		}
		if spec.StorageClassName != "" {
			return fmt.Errorf("spec.storageClassName is for a new claim; an existing claim keeps its class")
		}
		return nil
	}
	if spec.TargetUID != "" {
		return fmt.Errorf("spec.targetUID is for an existing claim; a new claim does not exist yet")
	}
	return nil
}
