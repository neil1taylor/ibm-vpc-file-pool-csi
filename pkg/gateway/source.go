package gateway

import (
	"context"
	"fmt"
	"net/http"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"k8s.io/apimachinery/pkg/util/validation"
)

// Where a backup is read from (issue #92, phase 4). A user never names a bucket:
// a backup in COS is the one the policy's export block names, and the volume is
// the one the policy keeps for the claim. Only a person who may read the named
// Secrets in the driver namespace may name a bucket directly, because the
// helper would mount those Secrets for them.

// sourceError is a refusal with the status and words the caller gets.
type sourceError struct {
	status int
	code   string
	msg    string
}

func (e *sourceError) Error() string { return e.msg }

func findVolume(p *v1alpha1.BackupPolicy, ns, pvc string) *v1alpha1.BackupVolumeStatus {
	for i := range p.Status.Volumes {
		v := &p.Status.Volumes[i]
		if v.PVCNamespace == ns && v.PVCName == pvc && v.SubPath != "" {
			return v
		}
	}
	return nil
}

// chooseSource says where to read: "" for the snapshot pool, "cos" for the
// bucket of the policy's export block. If the caller asked, the answer is what
// was asked, or an error. If not, it is COS only when the backup is only there:
// the moment is older than the oldest local restore point, or the volume has no
// local backup left, and an export of it exists.
func chooseSource(p *v1alpha1.BackupPolicy, v *v1alpha1.BackupVolumeStatus, moment *time.Time, asked string) (string, *sourceError) {
	exported := p.Spec.Export != nil && v.ExportedTime != nil
	switch asked {
	case v1alpha1.BackupSourceLocal:
		return "", nil
	case v1alpha1.BackupSourceCOS:
		switch {
		case p.Spec.Export == nil:
			return "", &sourceError{http.StatusConflict, "no_export", "that policy does not send its backups to COS"}
		case v.ExportedTime == nil:
			return "", &sourceError{http.StatusConflict, "not_exported", "that volume has no copy in COS yet"}
		}
		return v1alpha1.BackupSourceCOS, nil
	case "":
	default:
		return "", &sourceError{http.StatusBadRequest, "bad_request", `source must be "local" or "cos"`}
	}
	if !exported {
		return "", nil
	}
	if v.LastBackupTime == nil {
		return v1alpha1.BackupSourceCOS, nil
	}
	if moment != nil && v.OldestBackupTime != nil && moment.Before(v.OldestBackupTime.Time) {
		return v1alpha1.BackupSourceCOS, nil
	}
	return "", nil
}

// vmDiskInCOS is the refusal for a VM disk that would be read from COS: the
// files inside a VM disk can be read from the snapshot pool only (phase 6 adds COS).
func vmDiskInCOS() *sourceError {
	return &sourceError{http.StatusConflict, "vm_disk_cos",
		"the files inside a VM disk can be browsed and restored only from the snapshot pool, for the moments that are still there; a VM disk in COS can only be restored whole"}
}

// sourceName is the source as a session reports it.
func sourceName(src string) string {
	if src == v1alpha1.BackupSourceCOS {
		return v1alpha1.BackupSourceCOS
	}
	return v1alpha1.BackupSourceLocal
}

func sourceText(spec *v1alpha1.BackupBrowseSpec) string {
	switch {
	case spec.FromCOS != nil:
		return "from the COS bucket " + spec.FromCOS.Bucket
	case spec.Source == v1alpha1.BackupSourceCOS:
		return "from COS"
	}
	return "from the snapshot pool"
}

// checkFromCOS checks a bucket named directly: the area (the policy name) and
// the block. The endpoint must be a COS host, because the COS key is sent to it.
func checkFromCOS(policy string, b *v1alpha1.BrowseFromCOS) error {
	if policy == "" || len(validation.IsDNS1123Subdomain(policy)) > 0 || len(policy) > 63 {
		return fmt.Errorf("policy must be the name of the policy's area in the bucket")
	}
	return b.Validate()
}

// secretsOf lists the Secrets in the driver namespace that a bucket block names.
func secretsOf(b *v1alpha1.BrowseFromCOS) []string {
	names := []string{b.CredentialsSecret}
	if !b.EmergencyKeyOnly {
		names = append(names, b.BackupKeySecret)
		if b.KeyProtect != nil {
			names = append(names, b.KeyProtect.APIKeySecret)
		}
	}
	if b.EmergencyKeySecret != "" {
		names = append(names, b.EmergencyKeySecret)
	}
	return names
}

// mayUseBucket says whether the user may read every Secret that a bucket block
// names, in the driver namespace. The helper mounts them, so a person who could
// not read them must not be able to name them. It answers false on an error.
func (g *Gateway) mayUseBucket(ctx context.Context, u User, b *v1alpha1.BrowseFromCOS) bool {
	for _, name := range secretsOf(b) {
		if !g.canDo(ctx, u, Access{Namespace: g.driverNamespace(), Resource: "secrets", Verb: "get", Name: name}) {
			return false
		}
	}
	return true
}

func (g *Gateway) driverNamespace() string {
	if g.cfg.DriverNamespace != "" {
		return g.cfg.DriverNamespace
	}
	return g.cfg.EventNamespace
}

// mayRead says whether the user may read the backup that a session or a restore
// names: the backups of the claim's namespace, or, for a bucket named directly,
// the Secrets of that bucket.
func (g *Gateway) mayRead(ctx context.Context, u User, namespace string, fromCOS *v1alpha1.BrowseFromCOS) bool {
	if fromCOS != nil {
		return g.mayUseBucket(ctx, u, fromCOS)
	}
	return g.allowed(ctx, u, namespace)
}
