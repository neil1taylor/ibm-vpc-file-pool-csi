// Package cossetup makes the IBM Cloud side of the COS export: the COS
// instance, a locked and versioned bucket, a role that can write but not
// delete or unlock, a service ID and HMAC key limited to the one bucket, a
// context-based restriction (CBR) rule, and the wrapped backup key.
//
// It runs only on a person's machine, with that person's own IBM Cloud login.
// The driver never calls it. It follows docs/TUTORIAL-09-COS-BACKUP-BUCKET.md step
// by step, and it deletes only the instance-wide policy that IBM Cloud adds
// for the HMAC key. Design: docs/specs/2026-10-21-backup-cos-export.md.
package cossetup

const (
	// RoleName is the custom IAM role for the driver's key.
	RoleName = "PoolCsiBackupWriter"
	// RoleDisplayName is how a policy names the role in the console and the CLI.
	RoleDisplayName = "Pool CSI backup writer"
	// COSServiceName is the IAM service name of IBM Cloud Object Storage.
	COSServiceName = "cloud-object-storage"
	// WrappedKeyObject is where the wrapped backup key is stored in the
	// bucket. It is outside the Kopia repository's prefix on purpose.
	WrappedKeyObject = "_pool-csi-setup/backup-key.wrapped.json"
	// MarkerObject says the bucket was made by this command.
	MarkerObject = "_pool-csi-setup/created-by-cos-setup"
)

// writerActions is the one place that holds the role's action list. It is the
// list of step 3 in docs/TUTORIAL-09-COS-BACKUP-BUCKET.md. It leaves out
// object.delete_version, object.bypass_governance_retention and every
// bucket.put_* and bucket.delete_* action, on purpose.
var writerActions = []string{
	"account.get_account_buckets",
	"bucket.get", "bucket.get_basic", "bucket.head", "bucket.get_location",
	"bucket.get_versioning", "bucket.get_versions", "bucket.get_object_lock_configuration", "bucket.get_uploads",
	"object.get", "object.get_version", "object.head", "object.head_version",
	"object.put", "object.post", "object.post_initiate_upload", "object.put_part", "object.post_complete_upload",
	"object.get_uploads", "object.delete_upload", "object.delete",
	"object.get_object_lock_retention", "object.get_object_lock_retention_version",
	"object.put_object_lock_retention", "object.put_object_lock_retention_version",
	"object.post_extend_retention",
}

// WriterActions returns the IAM actions of the custom role, each with the
// service name in front (cloud-object-storage.object.put).
func WriterActions() []string {
	out := make([]string, len(writerActions))
	for i, a := range writerActions {
		out[i] = COSServiceName + "." + a
	}
	return out
}
