package pool

import (
	"context"
	"fmt"
	"path"
	"regexp"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Sessions and file restores that read a backup in COS (issue #92, phase 4).
// The helper and the restore Job run in the driver namespace, where the COS and
// Key Protect Secrets are. The controller never reads those Secrets: it only
// checks that they exist, from their metadata, so that a wrong name is said in
// words and not as a pod that never starts.

const defaultBrowseCOSStartTimeout = 5 * time.Minute

var cosVolumeNamePattern = regexp.MustCompile(`^pvc-[a-f0-9-]{36}$`)

// cosLocation says which backup in COS a session or a restore reads.
type cosLocation struct {
	repo   ExportRepo
	policy string
	volume string
	// entry is the policy's record of the volume. It is nil when the bucket is
	// named directly.
	entry *v1alpha1.BackupVolumeStatus
}

// cosLocationOf works out where a backup in COS is. With fromCOS it is the
// bucket the block names, and policyName is the area of the bucket. Without it
// the policy's export block names the bucket, and the volume is the one the
// policy keeps for the claim. An error that is a restoreFailure ends the session
// or restore; any other error is tried again.
func (bc *BackupController) cosLocationOf(ctx context.Context, policyName, ns, name string, fromCOS *v1alpha1.BrowseFromCOS) (cosLocation, error) {
	if fromCOS != nil {
		if !policyNamePattern.MatchString(policyName) {
			return cosLocation{}, restoreFailure(fmt.Sprintf("%q is not a policy name", policyName))
		}
		if err := fromCOS.Validate(); err != nil {
			return cosLocation{}, restoreFailure(err.Error())
		}
		repo, err := COSRepoOf(fromCOS.AsRestoreFromCOS(), policyName)
		if err != nil {
			return cosLocation{}, restoreFailure(err.Error())
		}
		return cosLocation{repo: repo, policy: policyName, volume: fromCOS.Volume}, nil
	}
	policy := &v1alpha1.BackupPolicy{}
	if err := bc.client.Get(ctx, types.NamespacedName{Name: policyName}, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return cosLocation{}, restoreFailure(fmt.Sprintf("the backup policy %q does not exist", policyName))
		}
		return cosLocation{}, fmt.Errorf("get the backup policy: %w", err)
	}
	if policy.Spec.Export == nil {
		return cosLocation{}, restoreFailure(fmt.Sprintf("policy %s has no export block, so it has no backups in COS", policyName))
	}
	var entry *v1alpha1.BackupVolumeStatus
	for i := range policy.Status.Volumes {
		v := &policy.Status.Volumes[i]
		if v.PVCNamespace == ns && v.PVCName == name {
			entry = v
		}
	}
	if entry == nil || entry.SubPath == "" {
		return cosLocation{}, restoreFailure(fmt.Sprintf("policy %s has no backup of %s/%s", policyName, ns, name))
	}
	repo, _, err := exportRepoOf(policy)
	if err != nil {
		return cosLocation{}, restoreFailure(fmt.Sprintf("the export block of policy %s is not valid: %v", policyName, err))
	}
	volume := path.Base(entry.SubPath)
	if !cosVolumeNamePattern.MatchString(volume) {
		return cosLocation{}, restoreFailure(fmt.Sprintf("the backup folder %q of %s/%s is not a volume folder", entry.SubPath, ns, name))
	}
	return cosLocation{repo: repo, policy: policy.Name, volume: volume, entry: entry}, nil
}

// checkCOSSecrets confirms that the Secrets the repository needs exist in the
// driver namespace. It reads metadata only: the controller does not read the
// keys. A key that is missing from a Secret is found by the pod.
func (bc *BackupController) checkCOSSecrets(ctx context.Context, repo ExportRepo, namespace string) error {
	names := []string{repo.CredentialsSecret}
	if !repo.EmergencyOnly {
		names = append(names, repo.BackupKeySecret, repo.KeyProtect.APIKeySecret)
	}
	if repo.EmergencyKeySecret != "" {
		names = append(names, repo.EmergencyKeySecret)
	}
	for _, n := range names {
		meta := &metav1.PartialObjectMetadata{}
		meta.SetGroupVersionKind(corev1.SchemeGroupVersion.WithKind("Secret"))
		err := bc.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: n}, meta)
		switch {
		case apierrors.IsNotFound(err):
			return restoreFailure(fmt.Sprintf("the Secret %q that the export block names does not exist in namespace %s", n, namespace))
		case err != nil:
			return fmt.Errorf("check the Secret %s: %w", n, err)
		}
	}
	return nil
}

// browseCOSRequest works out the helper of a session that reads COS.
func (bc *BackupController) browseCOSRequest(ctx context.Context, bb *v1alpha1.BackupBrowse) (BrowseJobRequest, error) {
	sp := bb.Spec
	if bc.browse.COSNamespace == "" {
		return BrowseJobRequest{}, restoreFailure("browsing a backup in COS is not turned on in this installation")
	}
	loc, err := bc.cosLocationOf(ctx, sp.Policy, sp.PVCNamespace, sp.PVCName, sp.FromCOS)
	if err != nil {
		return BrowseJobRequest{}, err
	}
	if err = bc.checkCOSSecrets(ctx, loc.repo, bc.browse.COSNamespace); err != nil {
		return BrowseJobRequest{}, err
	}
	req := BrowseJobRequest{
		ID:         string(bb.UID),
		SecretName: browseSecretName(string(bb.UID)),
		Idle:       bc.browse.Idle,
		MaxLife:    bc.browse.MaxLife,
		Owner:      browseOwner(bb),
		COS:        &BrowseCOSRequest{Repo: loc.repo, Policy: loc.policy, Volume: loc.volume},
	}
	if sp.Moment != nil {
		req.At = sp.Moment.Time
	}
	return req, nil
}

func browseIsCOS(sp *v1alpha1.BackupBrowseSpec) bool {
	return sp.Source == v1alpha1.BackupSourceCOS || sp.FromCOS != nil
}

// ensureCOSSessionCopy keeps a copy of the session Secret (token, certificate
// and key) in the driver namespace, next to the COS Secrets, for the helper to
// mount. The gateway reads the one in the browse namespace and cannot read this
// one. The copy is owned by the BackupBrowse and named by its UID.
func (bc *BackupController) ensureCOSSessionCopy(ctx context.Context, bb *v1alpha1.BackupBrowse) error {
	name := browseSecretName(string(bb.UID))
	err := bc.client.Get(ctx, types.NamespacedName{Namespace: bc.browse.COSNamespace, Name: name}, &corev1.Secret{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	src := &corev1.Secret{}
	if err := bc.client.Get(ctx, types.NamespacedName{Namespace: bc.browse.Namespace, Name: name}, src); err != nil {
		return fmt.Errorf("read the session Secret: %w", err)
	}
	cp := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: bc.browse.COSNamespace, Name: name,
			Labels:          map[string]string{browseLabel: string(bb.UID)},
			OwnerReferences: []metav1.OwnerReference{browseOwner(bb)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: src.Data,
	}
	if err := bc.client.Create(ctx, cp); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// deleteCOSSessionCopy removes the copy of a session Secret in the driver
// namespace.
func (bc *BackupController) deleteCOSSessionCopy(ctx context.Context, name string) {
	if bc.browse.COSNamespace == "" || bc.browse.COSNamespace == bc.browse.Namespace {
		return
	}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: bc.browse.COSNamespace, Name: name}}
	if err := bc.client.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to delete the copy of a browse Secret", "secret", name)
	}
}

// sweepCOSSessionCopies removes the copies of session Secrets whose session is
// gone.
func (bc *BackupController) sweepCOSSessionCopies(ctx context.Context, known map[string]bool) {
	if bc.browse.COSNamespace == "" || bc.browse.COSNamespace == bc.browse.Namespace {
		return
	}
	secrets := &corev1.SecretList{}
	if err := bc.client.List(ctx, secrets, client.InNamespace(bc.browse.COSNamespace), client.HasLabels{browseLabel}); err != nil {
		klog.ErrorS(err, "Failed to list the copies of browse Secrets")
		return
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if !known[s.Labels[browseLabel]] {
			bc.deleteCOSSessionCopy(ctx, s.Name)
		}
	}
}

// cosStartTimeout is how long a session that reads COS may be Starting.
func (bc *BackupController) cosStartTimeout() time.Duration {
	if bc.browse.COSStartTimeout > 0 {
		return bc.browse.COSStartTimeout
	}
	return defaultBrowseCOSStartTimeout
}
