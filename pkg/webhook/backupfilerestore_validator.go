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
	if err := validateBackupSource(spec); err != nil {
		return err
	}
	if spec.Guest && (spec.Source == v1alpha1.BackupSourceCOS || spec.FromCOS != nil) {
		return fmt.Errorf("spec.guest (files inside a VM disk) is for a backup in the snapshot pool; a VM disk in COS is not supported yet")
	}
	if errs := validation.IsDNS1123Label(spec.TargetNamespace); len(errs) > 0 {
		return fmt.Errorf("spec.targetNamespace is not a namespace name: %v", errs)
	}
	if errs := validation.IsDNS1123Subdomain(spec.TargetName); len(errs) > 0 {
		return fmt.Errorf("spec.targetName is not a PVC name: %v", errs)
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

// validateBackupSource checks where the backup is read from. A backup in the
// snapshot pool, or in the COS bucket of a policy, is named by the claim that
// was backed up. A bucket named directly (spec.fromCOS) has no claim in this
// cluster: the policy is only the area in the bucket, the source namespace holds
// "-" and the source name is the volume. The webhook guards objects made by hand
// too, so the block gets the same checks the gateway and the controller make.
func validateBackupSource(spec *v1alpha1.BackupFileRestoreSpec) error {
	if spec.FromCOS == nil {
		if errs := validation.IsDNS1123Label(spec.SourceNamespace); len(errs) > 0 {
			return fmt.Errorf("spec.sourceNamespace is not a namespace name: %v", errs)
		}
		if errs := validation.IsDNS1123Subdomain(spec.SourceName); len(errs) > 0 {
			return fmt.Errorf("spec.sourceName is not a PVC name: %v", errs)
		}
		return nil
	}
	if spec.Source != v1alpha1.BackupSourceCOS {
		return fmt.Errorf("spec.source must be %q when spec.fromCOS is set", v1alpha1.BackupSourceCOS)
	}
	if spec.SourceNamespace != v1alpha1.NoNamespace {
		return fmt.Errorf("spec.sourceNamespace must be %q when spec.fromCOS is set: the bucket names no claim of this cluster", v1alpha1.NoNamespace)
	}
	if spec.SourceName != spec.FromCOS.Volume {
		return fmt.Errorf("spec.sourceName must be the volume of spec.fromCOS when it is set")
	}
	if err := spec.FromCOS.Validate(); err != nil {
		return err
	}
	if !spec.Existing && spec.StorageClassName == "" {
		return fmt.Errorf("spec.storageClassName is required for a new claim when spec.fromCOS is set: COS does not keep the class of the backed-up claim")
	}
	return nil
}
