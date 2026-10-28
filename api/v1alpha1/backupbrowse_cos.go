package v1alpha1

import "errors"

// Where a browse session or a file restore reads its backup from.
const (
	// BackupSourceLocal is the snapshot pool. It is the default.
	BackupSourceLocal = "local"
	// BackupSourceCOS is the Kopia repository that the policy's export block
	// names (or, for a cluster administrator, the one that spec.fromCOS names).
	BackupSourceCOS = "cos"
)

// NoNamespace is what the namespace of a claim holds when it is not known: a
// backup that is read straight from a bucket (spec.fromCOS) has no claim in this
// cluster. It is not a valid namespace name, so it cannot clash with one.
const NoNamespace = "-"

// BrowseFromCOS names a COS bucket directly, for a backup that no policy of this
// cluster describes (a new cluster after a loss). It needs the Secrets in the
// driver namespace, so only a cluster administrator may ask for it: the backup
// gateway checks that. The fields are those of RestoreFromCOS, without the size
// of a new claim. See docs/specs/2026-10-21-backup-cos-export.md.
type BrowseFromCOS struct {
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
	// (pvc- and a UUID).
	// +kubebuilder:validation:MinLength=1
	Volume string `json:"volume"`

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

	// EmergencyKeyOnly makes the helper use the readable key in
	// emergencyKeySecret and never call Key Protect.
	// +optional
	EmergencyKeyOnly bool `json:"emergencyKeyOnly,omitempty"`
}

// asRestore is the same block as a RestoreFromCOS, so that the two are checked
// by the same code. The size is not used.
func (b *BrowseFromCOS) asRestore() *RestoreFromCOS {
	return &RestoreFromCOS{
		Bucket: b.Bucket, Endpoint: b.Endpoint, Prefix: b.Prefix, CredentialsSecret: b.CredentialsSecret,
		Volume: b.Volume, SizeGB: 1, BackupKeySecret: b.BackupKeySecret, KeyProtect: b.KeyProtect,
		EmergencyKeySecret: b.EmergencyKeySecret, EmergencyKeyOnly: b.EmergencyKeyOnly,
	}
}

// AsRestoreFromCOS returns the block as a RestoreFromCOS, with a size of one,
// for the code that builds the repository from it.
func (b *BrowseFromCOS) AsRestoreFromCOS() *RestoreFromCOS { return b.asRestore() }

// Validate checks the block: the endpoint must be a COS S3 host (the key is
// sent to it), and the names must be valid. The gateway, the controller and the
// webhook all make this check.
func (b *BrowseFromCOS) Validate() error {
	if b == nil {
		return errors.New("spec.fromCOS is not set")
	}
	if !restoreVolumePattern.MatchString(b.Volume) {
		return errors.New("spec.fromCOS.volume is not a volume name such as pvc-<uuid>")
	}
	return b.asRestore().ValidateSource()
}
