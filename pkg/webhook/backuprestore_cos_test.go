package webhook

import (
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

func validCOSRestore() *v1alpha1.BackupRestore {
	r := validBackupRestore()
	r.Spec.SourceNamespace, r.Spec.SourceName = "", ""
	r.Spec.StorageClassName = "fast"
	r.Spec.FromCOS = &v1alpha1.RestoreFromCOS{
		Bucket: "pool-backups", Endpoint: "s3.eu-de.cloud-object-storage.appdomain.cloud",
		CredentialsSecret: "cos-credentials", BackupKeySecret: "cos-backup-key",
		KeyProtect: &v1alpha1.KeyProtectRef{InstanceID: "11111111-2222-3333-4444-555555555555", Region: "eu-de",
			RootKeyID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", APIKeySecret: "kp-apikey"},
		Volume: "pvc-11111111-2222-3333-4444-555555555555", SizeGB: 40,
	}
	return r
}

func TestBackupRestoreValidator_FromCOS(t *testing.T) {
	if _, err := (&BackupRestoreValidator{}).ValidateCreate(context.Background(), validCOSRestore()); err != nil {
		t.Fatalf("a restore from COS needs no source claim: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*v1alpha1.BackupRestore)
		want   string
	}{
		{"no storage class", func(r *v1alpha1.BackupRestore) { r.Spec.StorageClassName = "" }, "storageClassName"},
		{"bad volume", func(r *v1alpha1.BackupRestore) { r.Spec.FromCOS.Volume = "x" }, "fromCOS.volume"},
		{"no bucket", func(r *v1alpha1.BackupRestore) { r.Spec.FromCOS.Bucket = "" }, "fromCOS.bucket"},
		{"policy name that is not a folder name", func(r *v1alpha1.BackupRestore) { r.Spec.Policy = "A/B" }, "policy"},
	}
	for _, tc := range cases {
		r := validCOSRestore()
		tc.mutate(r)
		_, err := (&BackupRestoreValidator{}).ValidateCreate(context.Background(), r)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one that names %q", tc.name, err, tc.want)
		}
	}
}

// A local restore still needs the source claim.
func TestBackupRestoreValidator_LocalStillNeedsSource(t *testing.T) {
	r := validBackupRestore()
	r.Spec.SourceName = ""
	if _, err := (&BackupRestoreValidator{}).ValidateCreate(context.Background(), r); err == nil {
		t.Fatal("want an error for a local restore with no source name")
	}
}
