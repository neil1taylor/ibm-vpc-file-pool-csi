package main

import (
	"errors"
	"fmt"
	"strings"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

// --from-cos on ls, get, browse and restore-files reads the backup from COS
// (issue #92, phase 4). It has two forms:
//
//   - With --policy and --pvc it reads the bucket that the policy's export block
//     names. It needs the same rights as the other browse commands. Without the
//     flag the gateway picks COS by itself when a moment is only there.
//   - With --bucket and --endpoint it names a bucket directly, for a backup that
//     no policy of this cluster describes. It takes the flags of
//     `restore --from-cos`. Only someone who may read the named Secrets in the
//     driver namespace may use it, because the helper mounts them.

// bucketFlags are the flags that name a bucket. They belong to the second form.
var bucketFlags = []string{"bucket", "endpoint", "prefix", "volume", "credentials-secret", "backup-key-secret",
	"kp-instance", "kp-region", "kp-root-key", "kp-api-key-secret", "emergency-key-secret", "emergency-key-only", "backup-key-file"}

// sourceOf reads --from-cos and the bucket flags. It returns the source for a
// request ("cos", or "" to let the gateway choose) and the bucket, if one is
// named directly.
func sourceOf(flags map[string]string) (source string, bucket *v1alpha1.BrowseFromCOS, err error) {
	given := func() []string {
		var names []string
		for _, f := range bucketFlags {
			if flags[f] != "" {
				names = append(names, "--"+f)
			}
		}
		return names
	}
	if flags["from-cos"] != "true" {
		if g := given(); len(g) > 0 {
			return "", nil, fmt.Errorf("%s name a bucket in COS; add --from-cos to read from it", strings.Join(g, ", "))
		}
		return "", nil, nil
	}
	if flags["bucket"] == "" && flags["endpoint"] == "" {
		if g := given(); len(g) > 0 {
			return "", nil, fmt.Errorf("%s name a bucket; with --from-cos alone the backup is read from the bucket of the policy's export block. Give --bucket and --endpoint to name one directly", strings.Join(g, ", "))
		}
		return v1alpha1.BackupSourceCOS, nil, nil
	}
	if flags["backup-key-file"] != "" {
		return "", nil, errors.New("--backup-key-file is not used here: a session lasts too long to keep the key in a Secret made for it. Make a Secret with the readable key (key name backupKey) and give --emergency-key-secret NAME --emergency-key-only")
	}
	for _, f := range []string{"bucket", "endpoint", "volume"} {
		if flags[f] == "" {
			return "", nil, fmt.Errorf("--%s is required to name a bucket", f)
		}
	}
	block := &v1alpha1.RestoreFromCOS{
		Bucket: flags["bucket"], Endpoint: flags["endpoint"], Prefix: flags["prefix"], Volume: flags["volume"],
		CredentialsSecret: orFlag(flags, "credentials-secret", "cos-credentials"),
	}
	switch {
	case flags["emergency-key-only"] == "true":
		if flags["emergency-key-secret"] == "" {
			return "", nil, errors.New("--emergency-key-only needs --emergency-key-secret NAME")
		}
		block.EmergencyKeyOnly, block.EmergencyKeySecret = true, flags["emergency-key-secret"]
	default:
		if err = keyProtectFlags(flags, block); err != nil {
			return "", nil, err
		}
		block.EmergencyKeySecret = flags["emergency-key-secret"]
	}
	out := &v1alpha1.BrowseFromCOS{
		Bucket: block.Bucket, Endpoint: block.Endpoint, Prefix: block.Prefix, CredentialsSecret: block.CredentialsSecret,
		Volume: block.Volume, BackupKeySecret: block.BackupKeySecret, KeyProtect: block.KeyProtect,
		EmergencyKeySecret: block.EmergencyKeySecret, EmergencyKeyOnly: block.EmergencyKeyOnly,
	}
	// The same check the gateway and the controller make, so a mistake is found here.
	if err = out.Validate(); err != nil {
		return "", nil, err
	}
	return v1alpha1.BackupSourceCOS, out, nil
}
