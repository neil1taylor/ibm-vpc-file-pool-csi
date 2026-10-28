// Package cosbrowse reads a backup that was exported to IBM Cloud Object
// Storage (COS), for the browse helper and for the file restore Job (issue
// #92, phase 4). It shows the backup of one volume as a browse.Tree, read-only.
//
// The backup is a Kopia repository. Kopia runs as a child process (the pinned
// program of the image); it is never linked in. Everything a tenant wrote is
// untrusted: a folder is read from Kopia as JSON, a name is only ever compared
// with the names in that folder, and the only things handed to Kopia are object
// IDs that came out of the repository, after a check of their shape. A user's
// path is never an argument. See docs/specs/2026-10-22-backup-file-restore.md,
// section 4.
package cosbrowse

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Where the Secrets of a COS source are mounted. They are files, read by this
// package and never put in the environment of the process or of Kopia, except
// for the COS key and the backup key in the environment of the Kopia child.
const (
	CredentialsDir   = "/var/run/pool-csi/cos-credentials" //nolint:gosec // a folder path, not a credential
	WrappedKeyDir    = "/var/run/pool-csi/backup-key-wrapped"
	APIKeyDir        = "/var/run/pool-csi/keyprotect-apikey" //nolint:gosec // a folder path, not a credential
	EmergencyKeyDir  = "/var/run/pool-csi/backup-key-emergency"
	AccessKeyFile    = CredentialsDir + "/access_key_id"
	SecretKeyFile    = CredentialsDir + "/secret_access_key"
	WrappedKeyFile   = WrappedKeyDir + "/wrappedKey"
	APIKeyFile       = APIKeyDir + "/apikey"
	EmergencyKeyFile = EmergencyKeyDir + "/backupKey"

	// DefaultStateDir is Kopia's private folder: its settings, cache and logs. It
	// is a memory-backed or scratch volume, because the root file system of the
	// pod is read-only.
	DefaultStateDir = "/tmp/kopia"
)

// KeyProtect says which Key Protect root key unwraps the backup key.
type KeyProtect struct {
	Region     string `json:"region"`
	InstanceID string `json:"instanceID"`
	RootKeyID  string `json:"rootKeyID"`
}

// Source says which backup in COS to read. It holds no secret: the keys are
// files at the fixed places above.
type Source struct {
	// Endpoint is the S3 host of the bucket. A leading http:// turns TLS off
	// (tests only).
	Endpoint string `json:"endpoint"`
	Bucket   string `json:"bucket"`
	// Prefix is the folder of the policy's area inside the bucket, including the
	// policy's name. Nothing outside it is read.
	Prefix string `json:"prefix"`
	// Policy is the name of the backup policy: the area of the bucket, and the
	// folder name in the source path of every snapshot that belongs to it.
	Policy string `json:"policy"`
	// Volume is the backed-up volume (pvc- and a UUID).
	Volume string `json:"volume"`
	// KeyProtect is unset with EmergencyOnly.
	KeyProtect *KeyProtect `json:"keyProtect,omitempty"`
	// EmergencyKey says that the emergency key file is mounted. It is used only
	// when Key Protect cannot be reached, unless EmergencyOnly is set.
	EmergencyKey  bool `json:"emergencyKey,omitempty"`
	EmergencyOnly bool `json:"emergencyOnly,omitempty"`
}

var (
	endpointPattern = regexp.MustCompile(`^(https?://)?[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)
	bucketPattern   = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)
	prefixPattern   = regexp.MustCompile(`^[A-Za-z0-9._/-]*$`)
	policyPattern   = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,251}[a-z0-9])?$`)
	volumePattern   = regexp.MustCompile(`^pvc-[a-f0-9-]{36}$`)
	regionPattern   = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	idPattern       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
)

// Validate checks the source. The controller makes the same checks before it
// starts a Job, and the program makes them again when it starts, because its
// arguments are the last thing between a bad value and Kopia's command line.
func (s Source) Validate() error {
	switch {
	case !endpointPattern.MatchString(s.Endpoint):
		return fmt.Errorf("the COS endpoint %q is not a host name with an optional port", s.Endpoint)
	case !bucketPattern.MatchString(s.Bucket):
		return fmt.Errorf("the COS bucket %q is not a valid bucket name", s.Bucket)
	case !prefixPattern.MatchString(s.Prefix) || strings.Contains(s.Prefix, ".."):
		return fmt.Errorf("the COS prefix %q has characters that are not allowed", s.Prefix)
	case !policyPattern.MatchString(s.Policy):
		return fmt.Errorf("the policy name %q is not valid", s.Policy)
	case !volumePattern.MatchString(s.Volume):
		return fmt.Errorf("the volume %q is not a volume name such as pvc-<uuid>", s.Volume)
	}
	if s.EmergencyOnly {
		if !s.EmergencyKey {
			return errors.New("an emergency-only source needs the emergency key")
		}
		return nil
	}
	if s.KeyProtect == nil {
		return errors.New("the Key Protect root key is not set (or use only the emergency key)")
	}
	if !regionPattern.MatchString(s.KeyProtect.Region) {
		return fmt.Errorf("the Key Protect region %q is not a region name such as eu-de", s.KeyProtect.Region)
	}
	if !idPattern.MatchString(s.KeyProtect.InstanceID) || !idPattern.MatchString(s.KeyProtect.RootKeyID) {
		return errors.New("the Key Protect instance ID and root key ID are required and must be IDs")
	}
	return nil
}
