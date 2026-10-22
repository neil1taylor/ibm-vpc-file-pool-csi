package v1alpha1

import (
	"strings"
	"testing"
)

func validFromCOS() *RestoreFromCOS {
	return &RestoreFromCOS{
		Bucket: "pool-backups", Endpoint: "s3.eu-de.cloud-object-storage.appdomain.cloud", Prefix: "team-a",
		CredentialsSecret: "cos-credentials", BackupKeySecret: "cos-backup-key",
		KeyProtect: &KeyProtectRef{InstanceID: "11111111-2222-3333-4444-555555555555", Region: "eu-de",
			RootKeyID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", APIKeySecret: "kp-apikey"},
		Volume: "pvc-11111111-2222-3333-4444-555555555555", SizeGB: 40,
	}
}

func TestRestoreFromCOS_Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*RestoreFromCOS)
		wantErr string
	}{
		{"valid", func(*RestoreFromCOS) {}, ""},
		{"valid with emergency key only", func(r *RestoreFromCOS) {
			r.KeyProtect, r.BackupKeySecret, r.EmergencyKeyOnly, r.EmergencyKeySecret = nil, "", true, "emergency"
		}, ""},
		{"no bucket", func(r *RestoreFromCOS) { r.Bucket = "" }, "fromCOS.bucket"},
		{"no endpoint", func(r *RestoreFromCOS) { r.Endpoint = "" }, "fromCOS.endpoint"},
		{"endpoint with a path", func(r *RestoreFromCOS) { r.Endpoint = "evil.example/x" }, "fromCOS.endpoint"},
		{"bad bucket", func(r *RestoreFromCOS) { r.Bucket = "Bad_Bucket" }, "fromCOS.bucket"},
		{"prefix with ..", func(r *RestoreFromCOS) { r.Prefix = "a/../b" }, "fromCOS.prefix"},
		{"no credentials secret", func(r *RestoreFromCOS) { r.CredentialsSecret = "" }, "fromCOS.credentialsSecret"},
		{"no backup key secret", func(r *RestoreFromCOS) { r.BackupKeySecret = "" }, "fromCOS.backupKeySecret"},
		{"no key protect", func(r *RestoreFromCOS) { r.KeyProtect = nil }, "fromCOS.keyProtect"},
		{"bad key protect region", func(r *RestoreFromCOS) { r.KeyProtect.Region = "EU DE" }, "fromCOS.keyProtect.region"},
		{"emergency only without a secret", func(r *RestoreFromCOS) {
			r.KeyProtect, r.BackupKeySecret, r.EmergencyKeyOnly = nil, "", true
		}, "fromCOS.emergencyKeySecret"},
		{"bad emergency secret name", func(r *RestoreFromCOS) { r.EmergencyKeySecret = "Bad Name" }, "fromCOS.emergencyKeySecret"},
		{"volume is not a driver volume", func(r *RestoreFromCOS) { r.Volume = "../../etc" }, "fromCOS.volume"},
		{"no volume", func(r *RestoreFromCOS) { r.Volume = "" }, "fromCOS.volume"},
		{"no size", func(r *RestoreFromCOS) { r.SizeGB = 0 }, "fromCOS.sizeGB"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := validFromCOS()
			tt.mutate(r)
			err := r.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Fatalf("Validate() = %v, want nil", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Fatalf("Validate() = %v, want an error naming %q", err, tt.wantErr)
			}
		})
	}
}
