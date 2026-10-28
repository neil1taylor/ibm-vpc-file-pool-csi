package v1alpha1

import (
	"fmt"
	"regexp"
	"time"
)

// Lock modes of an export. See docs/specs/2026-10-21-backup-cos-export.md.
const (
	// ExportLockCompliance sets an object lock that nobody can shorten.
	ExportLockCompliance = "Compliance"
	// ExportLockNone sets no object lock.
	ExportLockNone = "None"
)

const (
	// DefaultExportKeepFor is how long exported backups are kept when a policy
	// sets nothing.
	DefaultExportKeepFor = 30 * 24 * time.Hour

	// MinExportMaintenanceInterval is the shortest time between Kopia
	// maintenance runs.
	MinExportMaintenanceInterval = time.Hour

	// ExportLockMargin is how much longer than the maintenance interval a
	// Compliance keep time must be: Kopia extends the locks of chunks still in
	// use at each maintenance run (tested 20 Oct 2026).
	ExportLockMargin = 24 * time.Hour

	// maxExportMaintenanceInterval is Kopia's own default of one day.
	maxExportMaintenanceInterval = 24 * time.Hour

	minExportKeep     = time.Hour
	minExportSchedule = time.Minute
)

var (
	keyProtectRegionPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	keyProtectIDPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
	secretNamePattern       = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]{0,251}[a-z0-9])?$`)
)

// KeyProtectRef says which Key Protect root key unlocks the backup key, and
// which Secret holds the API key that may use it. The Jobs, not the controller,
// call Key Protect.
type KeyProtectRef struct {
	// InstanceID is the GUID of the Key Protect instance.
	// +kubebuilder:validation:MinLength=1
	InstanceID string `json:"instanceID"`

	// Region is the region of the Key Protect instance, such as eu-de.
	// +kubebuilder:validation:MinLength=1
	Region string `json:"region"`

	// RootKeyID is the ID of the root key that wrapped the backup key.
	// +kubebuilder:validation:MinLength=1
	RootKeyID string `json:"rootKeyID"`

	// APIKeySecret names the Secret (key: apikey) with the IBM Cloud API key that
	// has the Reader role on the root key.
	// +kubebuilder:validation:MinLength=1
	APIKeySecret string `json:"apiKeySecret"`
}

// BackupExport sends the backups of a policy to an IBM Cloud Object Storage
// bucket, encrypted before they leave the cluster. The Secrets are in the
// driver's namespace. The controller passes only their names to the Jobs.
type BackupExport struct {
	// Bucket is the name of the COS bucket.
	// +kubebuilder:validation:MinLength=1
	Bucket string `json:"bucket"`

	// Endpoint is the S3 host of the bucket, such as
	// s3.eu-de.cloud-object-storage.appdomain.cloud.
	// +kubebuilder:validation:MinLength=1
	Endpoint string `json:"endpoint"`

	// Prefix is a folder in the bucket. The policy's own area is below it.
	// +optional
	Prefix string `json:"prefix,omitempty"`

	// CredentialsSecret names the Secret with the driver's COS key (an HMAC key:
	// access_key_id and secret_access_key).
	// +kubebuilder:validation:MinLength=1
	CredentialsSecret string `json:"credentialsSecret"`

	// BackupKeySecret names the Secret with the backup key, wrapped by Key
	// Protect (wrappedKey). The readable key is never stored. The controller
	// never reads the Secret: only the Jobs do, and they unwrap the key in memory.
	// +kubebuilder:validation:MinLength=1
	BackupKeySecret string `json:"backupKeySecret"`

	// KeyProtect says which root key unwraps the backup key and how to log in.
	KeyProtect KeyProtectRef `json:"keyProtect"`

	// EmergencyKeySecret names an optional Secret (key: backupKey) with the
	// readable backup key. The Jobs use it only when Key Protect cannot be
	// reached. Storing it in the cluster weakens the Key Protect protection; most
	// users keep the emergency copy outside the cluster and leave this empty.
	// +optional
	EmergencyKeySecret string `json:"emergencyKeySecret,omitempty"`

	// KeepFor is how long exported backups are kept: a Go duration, or a number
	// of days such as "30d". With a Compliance lock it is at least 25 hours.
	// +kubebuilder:default="30d"
	// +optional
	KeepFor string `json:"keepFor,omitempty"`

	// Lock is Compliance (nobody can delete or shorten the keep time) or None.
	// +kubebuilder:validation:Enum=Compliance;None
	// +kubebuilder:default=Compliance
	// +optional
	Lock string `json:"lock,omitempty"`

	// Schedule is a Go duration such as "6h": the most often a volume is sent.
	// Empty means after each local backup.
	// +optional
	Schedule string `json:"schedule,omitempty"`
}

// EffectiveLock is the lock mode, with the default applied.
func (e *BackupExport) EffectiveLock() string {
	if e.Lock == "" {
		return ExportLockCompliance
	}
	return e.Lock
}

// EffectiveKeepFor is the keep time, with the default applied.
func (e *BackupExport) EffectiveKeepFor() (time.Duration, error) {
	if e.KeepFor == "" {
		return DefaultExportKeepFor, nil
	}
	return ParseKeepFor(e.KeepFor)
}

// ExportMaintenanceInterval is the time between Kopia maintenance runs for a
// keep time: a day, or less when the keep time is short, and an hour at the
// least. For any keep time of 25 hours or more it satisfies MinComplianceKeepFor.
func ExportMaintenanceInterval(keepFor time.Duration) time.Duration {
	interval := keepFor - ExportLockMargin
	if interval > maxExportMaintenanceInterval {
		interval = maxExportMaintenanceInterval
	}
	if interval < MinExportMaintenanceInterval {
		interval = MinExportMaintenanceInterval
	}
	return interval
}

// MinComplianceKeepFor is the shortest keep time a Compliance lock may have
// when maintenance runs every maintenanceInterval. This is the one place that
// holds the rule: the webhook and the controller both use it.
func MinComplianceKeepFor(maintenanceInterval time.Duration) time.Duration {
	return maintenanceInterval + ExportLockMargin
}

// Validate checks the block. It is the check of the webhook; the controller
// makes it again before it starts a Job.
func (e *BackupExport) Validate() error {
	for field, v := range map[string]string{
		"bucket": e.Bucket, "endpoint": e.Endpoint,
		"credentialsSecret": e.CredentialsSecret, "backupKeySecret": e.BackupKeySecret,
	} {
		if v == "" {
			return fmt.Errorf("spec.export.%s is required", field)
		}
	}
	if err := e.KeyProtect.validate(); err != nil {
		return err
	}
	if e.EmergencyKeySecret != "" && !secretNamePattern.MatchString(e.EmergencyKeySecret) {
		return fmt.Errorf("spec.export.emergencyKeySecret %q is not a valid Secret name", e.EmergencyKeySecret)
	}
	lock := e.EffectiveLock()
	if lock != ExportLockCompliance && lock != ExportLockNone {
		return fmt.Errorf("spec.export.lock must be %s or %s, got %q", ExportLockCompliance, ExportLockNone, e.Lock)
	}
	keep, err := e.EffectiveKeepFor()
	if err != nil {
		return fmt.Errorf("spec.export.keepFor: %v", err)
	}
	if keep < minExportKeep {
		return fmt.Errorf("spec.export.keepFor must be at least %s, got %s", minExportKeep, e.KeepFor)
	}
	if min := MinComplianceKeepFor(ExportMaintenanceInterval(keep)); lock == ExportLockCompliance && keep < min {
		return fmt.Errorf("spec.export.keepFor must be at least %s with a Compliance lock (the maintenance interval plus 24 hours), got %s", min, e.KeepFor)
	}
	if e.Schedule != "" {
		d, err := time.ParseDuration(e.Schedule)
		if err != nil {
			return fmt.Errorf("spec.export.schedule is not a valid duration: %v", err)
		}
		if d < minExportSchedule {
			return fmt.Errorf("spec.export.schedule must be at least %s, got %s", minExportSchedule, e.Schedule)
		}
	}
	return nil
}

func (k KeyProtectRef) validate() error { return k.validateAt("spec.export.keyProtect") }

// validateAt checks the block; path names it in messages.
func (k KeyProtectRef) validateAt(path string) error {
	for field, v := range map[string]string{
		"instanceID": k.InstanceID, "region": k.Region, "rootKeyID": k.RootKeyID, "apiKeySecret": k.APIKeySecret,
	} {
		if v == "" {
			return fmt.Errorf("%s.%s is required", path, field)
		}
	}
	if !keyProtectRegionPattern.MatchString(k.Region) {
		return fmt.Errorf("%s.region %q is not a region name such as eu-de", path, k.Region)
	}
	if !keyProtectIDPattern.MatchString(k.InstanceID) {
		return fmt.Errorf("%s.instanceID %q is not valid", path, k.InstanceID)
	}
	if !keyProtectIDPattern.MatchString(k.RootKeyID) {
		return fmt.Errorf("%s.rootKeyID %q is not valid", path, k.RootKeyID)
	}
	if !secretNamePattern.MatchString(k.APIKeySecret) {
		return fmt.Errorf("%s.apiKeySecret %q is not a valid Secret name", path, k.APIKeySecret)
	}
	return nil
}
