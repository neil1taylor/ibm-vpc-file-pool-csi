package v1alpha1

import (
	"strings"
	"testing"
)

func validBrowseFromCOS() *BrowseFromCOS {
	r := validFromCOS()
	return &BrowseFromCOS{
		Bucket: r.Bucket, Endpoint: r.Endpoint, Prefix: r.Prefix, CredentialsSecret: r.CredentialsSecret,
		Volume: r.Volume, BackupKeySecret: r.BackupKeySecret, KeyProtect: r.KeyProtect,
	}
}

func TestBrowseFromCOS_Validate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*BrowseFromCOS)
		wantErr string
	}{
		{"valid", func(*BrowseFromCOS) {}, ""},
		{"valid with emergency key only", func(b *BrowseFromCOS) {
			b.KeyProtect, b.BackupKeySecret, b.EmergencyKeyOnly, b.EmergencyKeySecret = nil, "", true, "emergency"
		}, ""},
		{"no volume", func(b *BrowseFromCOS) { b.Volume = "" }, "fromCOS.volume"},
		{"volume that is a path", func(b *BrowseFromCOS) { b.Volume = "../pvc-11111111-2222-3333-4444-555555555555" }, "fromCOS.volume"},
		// The key is sent to the endpoint, so only a COS S3 host is accepted.
		{"endpoint that is not COS", func(b *BrowseFromCOS) { b.Endpoint = "evil.example" }, "fromCOS.endpoint"},
		{"endpoint with http", func(b *BrowseFromCOS) { b.Endpoint = "http://s3.eu-de.cloud-object-storage.appdomain.cloud" }, "fromCOS.endpoint"},
		{"bad bucket", func(b *BrowseFromCOS) { b.Bucket = "Bad_Bucket" }, "fromCOS.bucket"},
		{"prefix with ..", func(b *BrowseFromCOS) { b.Prefix = "a/../b" }, "fromCOS.prefix"},
		{"no credentials secret", func(b *BrowseFromCOS) { b.CredentialsSecret = "" }, "fromCOS.credentialsSecret"},
		{"no key protect", func(b *BrowseFromCOS) { b.KeyProtect = nil }, "fromCOS.keyProtect"},
		{"bad secret name", func(b *BrowseFromCOS) { b.CredentialsSecret = "../../etc" }, "fromCOS.credentialsSecret"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := validBrowseFromCOS()
			tt.mutate(b)
			err := b.Validate()
			switch {
			case tt.wantErr == "" && err != nil:
				t.Errorf("unexpected error: %v", err)
			case tt.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tt.wantErr)):
				t.Errorf("error = %v, want one that mentions %q", err, tt.wantErr)
			}
		})
	}
	var none *BrowseFromCOS
	if err := none.Validate(); err == nil {
		t.Error("a nil block was accepted")
	}
}

func TestBrowseFromCOS_AsRestoreFromCOSKeepsEveryField(t *testing.T) {
	b := validBrowseFromCOS()
	b.EmergencyKeySecret = "emergency"
	r := b.AsRestoreFromCOS()
	if r.Bucket != b.Bucket || r.Endpoint != b.Endpoint || r.Prefix != b.Prefix || r.CredentialsSecret != b.CredentialsSecret ||
		r.Volume != b.Volume || r.BackupKeySecret != b.BackupKeySecret || r.KeyProtect != b.KeyProtect || r.EmergencyKeySecret != "emergency" {
		t.Errorf("restore block = %+v", r)
	}
}
