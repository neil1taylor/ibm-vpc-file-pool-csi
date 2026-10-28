package v1alpha1

import (
	"strings"
	"testing"
	"time"
)

func validExport() *BackupExport {
	return &BackupExport{
		Bucket:            "pool-backups",
		Endpoint:          "s3.eu-de.cloud-object-storage.appdomain.cloud",
		CredentialsSecret: "cos-credentials",
		BackupKeySecret:   "cos-backup-key",
		KeyProtect: KeyProtectRef{
			InstanceID:   "11111111-2222-3333-4444-555555555555",
			Region:       "eu-de",
			RootKeyID:    "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee",
			APIKeySecret: "keyprotect-apikey",
		},
	}
}

func TestExportMaintenanceInterval(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		25 * time.Hour:      time.Hour,                    // the shortest lock: the shortest interval
		36 * time.Hour:      12 * time.Hour,               // keep time minus 24 hours
		48 * time.Hour:      24 * time.Hour,               // Kopia's own default
		30 * 24 * time.Hour: 24 * time.Hour,               // never longer than a day
		time.Hour:           MinExportMaintenanceInterval, // too short to matter: validation refuses it
	}
	for keep, want := range cases {
		if got := ExportMaintenanceInterval(keep); got != want {
			t.Errorf("ExportMaintenanceInterval(%s) = %s, want %s", keep, got, want)
		}
	}
}

func TestMinComplianceKeepFor_IsTheIntervalPlus24Hours(t *testing.T) {
	if got := MinComplianceKeepFor(time.Hour); got != 25*time.Hour {
		t.Errorf("MinComplianceKeepFor(1h) = %s, want 25h", got)
	}
	if got := MinComplianceKeepFor(24 * time.Hour); got != 48*time.Hour {
		t.Errorf("MinComplianceKeepFor(24h) = %s, want 48h", got)
	}
}

// Whatever keep time passes, the interval the driver picks must satisfy the
// rule, or the controller would start Jobs its own check refuses.
func TestExportMaintenanceInterval_AlwaysSatisfiesTheRule(t *testing.T) {
	for h := 25; h <= 24*400; h += 7 {
		keep := time.Duration(h) * time.Hour
		if keep < MinComplianceKeepFor(ExportMaintenanceInterval(keep)) {
			t.Fatalf("keep %s is below the minimum for its own interval %s", keep, ExportMaintenanceInterval(keep))
		}
	}
}

func TestBackupExport_Defaults(t *testing.T) {
	e := validExport()
	if e.EffectiveLock() != ExportLockCompliance {
		t.Errorf("lock = %q, want Compliance", e.EffectiveLock())
	}
	keep, err := e.EffectiveKeepFor()
	if err != nil || keep != 30*24*time.Hour {
		t.Errorf("keepFor = %s, %v; want 30d", keep, err)
	}
	e.Lock, e.KeepFor = ExportLockNone, "36h"
	keep, _ = e.EffectiveKeepFor()
	if e.EffectiveLock() != ExportLockNone || keep != 36*time.Hour {
		t.Errorf("explicit values were changed: %q %s", e.EffectiveLock(), keep)
	}
}

func TestBackupExport_Validate(t *testing.T) {
	if err := validExport().Validate(); err != nil {
		t.Fatalf("a valid export: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*BackupExport)
		want   string
	}{
		{"no bucket", func(e *BackupExport) { e.Bucket = "" }, "bucket"},
		{"no endpoint", func(e *BackupExport) { e.Endpoint = "" }, "endpoint"},
		{"no credentials secret", func(e *BackupExport) { e.CredentialsSecret = "" }, "credentialsSecret"},
		{"no backup key secret", func(e *BackupExport) { e.BackupKeySecret = "" }, "backupKeySecret"},
		{"no key protect instance", func(e *BackupExport) { e.KeyProtect.InstanceID = "" }, "keyProtect.instanceID"},
		{"no key protect region", func(e *BackupExport) { e.KeyProtect.Region = "" }, "keyProtect.region"},
		{"bad key protect region", func(e *BackupExport) { e.KeyProtect.Region = "eu-de/x" }, "keyProtect.region"},
		{"no root key", func(e *BackupExport) { e.KeyProtect.RootKeyID = "" }, "keyProtect.rootKeyID"},
		{"no api key secret", func(e *BackupExport) { e.KeyProtect.APIKeySecret = "" }, "keyProtect.apiKeySecret"},
		{"bad emergency secret name", func(e *BackupExport) { e.EmergencyKeySecret = "Not A Name" }, "emergencyKeySecret"},
		{"bad lock", func(e *BackupExport) { e.Lock = "Governance" }, "lock"},
		{"bad keep", func(e *BackupExport) { e.KeepFor = "soon" }, "keepFor"},
		{"compliance under 25h", func(e *BackupExport) { e.KeepFor = "24h" }, "25h"},
		{"compliance one day", func(e *BackupExport) { e.KeepFor = "1d" }, "25h"},
		{"bad schedule", func(e *BackupExport) { e.Schedule = "often" }, "schedule"},
		{"schedule too frequent", func(e *BackupExport) { e.Schedule = "30s" }, "schedule"},
	}
	for _, tc := range cases {
		e := validExport()
		tc.mutate(e)
		err := e.Validate()
		if err == nil {
			t.Errorf("%s: want an error", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: error %q should name %q", tc.name, err, tc.want)
		}
	}
}

func TestBackupExport_Validate_AcceptsTheShortestLockAndAnyKeepWithNone(t *testing.T) {
	e := validExport()
	e.KeepFor = "25h"
	if err := e.Validate(); err != nil {
		t.Errorf("25h with Compliance is the shortest allowed: %v", err)
	}
	e.Lock, e.KeepFor = ExportLockNone, "2h"
	if err := e.Validate(); err != nil {
		t.Errorf("lock None needs no minimum above the usual hour: %v", err)
	}
	e.KeepFor = "30m"
	if err := e.Validate(); err == nil {
		t.Errorf("a keep time under an hour is never valid")
	}
}

func TestBackupExport_Validate_LongKeepNeedsNoExtraCheck(t *testing.T) {
	e := validExport()
	e.KeepFor = "3650d" // the typed confirmation lives in the CLI, not here
	if err := e.Validate(); err != nil {
		t.Errorf("a long keep time: %v", err)
	}
}

func TestBackupExport_Validate_EmergencyKeySecretIsOptional(t *testing.T) {
	e := validExport()
	e.EmergencyKeySecret = "cos-emergency-key"
	if err := e.Validate(); err != nil {
		t.Errorf("an emergency key Secret: %v", err)
	}
}
