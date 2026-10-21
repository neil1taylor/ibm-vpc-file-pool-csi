package v1alpha1

import (
	"fmt"
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

	// BackupKeySecret names the Secret with the key that encrypts the backups
	// (backupKey). The controller never reads it: only the Jobs do.
	// +kubebuilder:validation:MinLength=1
	BackupKeySecret string `json:"backupKeySecret"`

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
