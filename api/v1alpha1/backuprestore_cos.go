package v1alpha1

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var (
	// Only a COS S3 host: the caller's IAM token is sent to it.
	restoreEndpointPattern = regexp.MustCompile(`^(https://)?s3\.(direct\.|private\.)?[a-z0-9-]+\.cloud-object-storage\.appdomain\.cloud$`)
	restoreBucketPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	restorePrefixPattern   = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)
	restoreVolumePattern   = regexp.MustCompile(`^pvc-[a-f0-9-]{36}$`)
)

// RestoreFromCOS makes a BackupRestore read its backup from a COS bucket, not
// from a snapshot pool. It needs no BackupPolicy and no snapshot pool in this
// cluster: only the bucket, the keys and a storage class to restore into. See
// docs/specs/2026-10-21-backup-cos-export.md.
type RestoreFromCOS struct {
	// Bucket is the name of the COS bucket.
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Endpoint is the S3 host of the bucket.
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// Prefix is the folder in the bucket that was set on the policy's export
	// block. The area of the policy (spec.policy) is below it.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// CredentialsSecret names the Secret (in the driver's namespace) with the
	// COS key: access_key_id and secret_access_key.
	// +kubebuilder:validation:MinLength=1
	CredentialsSecret string `json:"credentialsSecret"`

	// Volume is the backed-up volume, as `kubectl backup cos-list` shows it
	// (pvc- and a UUID). The claim name is not kept in COS.
	// +kubebuilder:validation:MinLength=1
	Volume string `json:"volume"`

	// SizeGB is the size of the new PVC. COS holds the data, not the size of the
	// claim.
	// +kubebuilder:validation:Minimum=1
	SizeGB int32 `json:"sizeGB"`

	// BackupKeySecret names the Secret with the backup key wrapped by Key
	// Protect (wrappedKey). Not used with emergencyKeyOnly.
	// +optional
	BackupKeySecret string `json:"backupKeySecret,omitempty"`

	// KeyProtect says which root key unwraps the backup key. Not used with
	// emergencyKeyOnly.
	// +optional
	KeyProtect *KeyProtectRef `json:"keyProtect,omitempty"`

	// EmergencyKeySecret names a Secret (key: backupKey) with the readable
	// backup key.
	// +optional
	EmergencyKeySecret string `json:"emergencyKeySecret,omitempty"`

	// EmergencyKeyOnly makes the Jobs use the readable key in
	// emergencyKeySecret and never call Key Protect. It is for when Key Protect
	// or its root key is gone.
	// +optional
	EmergencyKeyOnly bool `json:"emergencyKeyOnly,omitempty"`
}

// Validate checks the block. The webhook and the controller both make it.
func (r *RestoreFromCOS) Validate() error {
	if !restoreVolumePattern.MatchString(r.Volume) {
		return fmt.Errorf("spec.fromCOS.volume %q is not a volume name such as pvc-<uuid>", r.Volume)
	}
	if r.SizeGB < 1 {
		return errors.New("spec.fromCOS.sizeGB must be at least 1")
	}
	return r.ValidateSource()
}

// ValidateSource checks where the repository is and how its key is reached:
// everything but the volume and the size. A listing needs only this.
func (r *RestoreFromCOS) ValidateSource() error {
	if r.Bucket == "" || !restoreBucketPattern.MatchString(r.Bucket) {
		return fmt.Errorf("spec.fromCOS.bucket %q is not a valid bucket name", r.Bucket)
	}
	if r.Endpoint == "" || !restoreEndpointPattern.MatchString(r.Endpoint) {
		return fmt.Errorf("spec.fromCOS.endpoint %q is not a COS S3 host such as s3.eu-de.cloud-object-storage.appdomain.cloud", r.Endpoint)
	}
	if !restorePrefixPattern.MatchString(r.Prefix) || strings.Contains(r.Prefix, "..") {
		return fmt.Errorf("spec.fromCOS.prefix %q has characters that are not allowed", r.Prefix)
	}
	if !secretNamePattern.MatchString(r.CredentialsSecret) {
		return fmt.Errorf("spec.fromCOS.credentialsSecret %q is not a valid Secret name", r.CredentialsSecret)
	}
	if r.EmergencyKeySecret != "" && !secretNamePattern.MatchString(r.EmergencyKeySecret) {
		return fmt.Errorf("spec.fromCOS.emergencyKeySecret %q is not a valid Secret name", r.EmergencyKeySecret)
	}
	if r.EmergencyKeyOnly {
		if r.EmergencyKeySecret == "" {
			return errors.New("spec.fromCOS.emergencyKeySecret is required with emergencyKeyOnly")
		}
		return nil
	}
	if !secretNamePattern.MatchString(r.BackupKeySecret) {
		return fmt.Errorf("spec.fromCOS.backupKeySecret %q is not a valid Secret name", r.BackupKeySecret)
	}
	if r.KeyProtect == nil {
		return errors.New("spec.fromCOS.keyProtect is required (or set emergencyKeyOnly)")
	}
	return r.KeyProtect.validateAt("spec.fromCOS.keyProtect")
}
