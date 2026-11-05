package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// reconcileFileRestoreFromCOS is a file restore whose backup is in COS. It is
// the same restore as one from the snapshot pool: the same Job program, the same
// targets, the same checks. Only the place the backup is read from differs, and
// the Job runs in the driver namespace, where the keys are.
func (bc *BackupController) reconcileFileRestoreFromCOS(ctx context.Context, fr *v1alpha1.BackupFileRestore) {
	sp := fr.Spec
	loc, err := bc.cosLocationOf(ctx, sp.Policy, sp.SourceNamespace, sp.SourceName, sp.FromCOS)
	if err == nil && sp.Guest {
		err = bc.checkGuestCOSRestore(loc)
	}
	if err == nil {
		err = bc.checkCOSSecrets(ctx, loc.repo, bc.fileJobs.Namespace())
	}
	if err != nil {
		var f restoreFailure
		if errors.As(err, &f) {
			bc.failFileRestore(ctx, fr, f.Error())
			return
		}
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, err.Error())
		return
	}
	entry := loc.entry
	if entry == nil {
		// The bucket is named directly: COS does not keep the class of the claim,
		// so a new claim needs spec.storageClassName.
		entry = &v1alpha1.BackupVolumeStatus{}
	}
	req := FileRestoreRequest{
		Key: fileRestoreKey(fr), Owner: fileRestoreOwner(fr),
		COS:   &FileRestoreCOS{Repo: loc.repo, Policy: loc.policy, Volume: loc.volume},
		Paths: sp.Paths, Policy: loc.policy, MaxBytes: bc.fileCfg.MaxBytes,
	}
	if rp := sp.EffectiveRestorePoint(); rp != nil {
		req.At = rp.Time
	}
	if sp.Guest {
		// A VM disk in COS is read through a disk server, which holds the keys.
		// The restore Job gets no key; it reads the disk from the disk server.
		disk, ready := bc.guestCOSDisk(ctx, fr, loc, req.At)
		if !ready {
			return
		}
		req.Guest = &FileRestoreGuest{Disk: disk}
		if c := entry.Consistency; c == "FileSystemConsistent" || c == "CrashConsistent" {
			req.Guest.Consistency = c
		}
	}
	if sp.Existing {
		bc.fileRestoreIntoClaim(ctx, fr, req)
		return
	}
	bc.fileRestoreIntoNewClaim(ctx, fr, entry, req)
}

// FileRestoreDiskJobs is the part of the runner that starts and follows the
// disk server of a restore from a VM disk in COS, and stops it.
type FileRestoreDiskJobs interface {
	BrowseDiskJobs
	// StopDisk ends the disk server of the restore with this UID.
	StopDisk(ctx context.Context, id string) (bool, error)
}

// restoreDiskIdle is how long the disk server of a restore may go unasked
// before it ends by itself. A running restore asks all the time.
const restoreDiskIdle = 30 * time.Minute

func restoreDiskSecretName(id string) string { return "restoredisk-" + id }

// checkGuestCOSRestore refuses, in words, what cannot be restored from a VM
// disk in COS, before anything is started.
func (bc *BackupController) checkGuestCOSRestore(loc cosLocation) error {
	switch {
	case !bc.fileJobs.GuestEnabled():
		return restoreFailure("restoring files from inside a VM disk is not turned on in this installation (backupBrowse.guest.image is not set)")
	case loc.entry == nil:
		return restoreFailure("a VM disk in a bucket named directly cannot be restored from: nothing there says it is a VM disk. Restore it through its policy")
	case loc.entry.VMName == "":
		return restoreFailure(fmt.Sprintf("%s/%s is not a VM disk, so it has no guest file systems to restore from", loc.entry.PVCNamespace, loc.entry.PVCName))
	}
	if _, ok := bc.fileJobs.(FileRestoreDiskJobs); !ok {
		return restoreFailure("restoring files from a VM disk in COS is not set up in this installation")
	}
	return bc.checkKeepFor(loc, guestFileRestoreDeadline, "a restore may last", "Restore from a newer backup")
}

// guestCOSDisk makes sure the disk server of the restore runs, and returns where
// it is once it is ready. Until then the restore waits (and the caller returns).
func (bc *BackupController) guestCOSDisk(ctx context.Context, fr *v1alpha1.BackupFileRestore, loc cosLocation, at time.Time) (*BrowseDisk, bool) {
	disks, _ := bc.fileJobs.(FileRestoreDiskJobs)
	id := string(fr.UID)
	secret := restoreDiskSecretName(id)
	if err := bc.ensureRestoreDiskSecret(ctx, fr, secret); err != nil {
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, fmt.Sprintf("make the Secret of the disk server: %v", err))
		return nil, false
	}
	req := BrowseJobRequest{
		ID: id, Restore: true, DiskSecretName: secret, At: at,
		Idle: restoreDiskIdle, MaxLife: guestFileRestoreDeadline + 15*time.Minute,
		Owner: fileRestoreOwner(fr),
		COS:   &BrowseCOSRequest{Repo: loc.repo, Policy: loc.policy, Volume: loc.volume},
	}
	if err := disks.StartBrowseDisk(ctx, req); err != nil {
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, fmt.Sprintf("start the disk server: %v", err))
		return nil, false
	}
	st, err := disks.DiskState(ctx, id)
	switch {
	case err != nil:
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, fmt.Sprintf("look at the disk server: %v", err))
	case st.Finished:
		reason := st.Reason
		if reason == "" {
			reason = "the disk server ended before the restore could read the disk"
		}
		bc.failFileRestore(ctx, fr, reason)
	case !st.Ready || st.PodIP == "":
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, "starting the disk server for the VM disk in COS")
	default:
		return &BrowseDisk{IP: st.PodIP, SecretName: secret}, true
	}
	return nil, false
}

// ensureRestoreDiskSecret makes the Secret of the disk server (token,
// certificate and key) in the driver namespace, owned by the restore. An
// existing Secret is left alone. The restore Job mounts only the token and the
// certificate from it.
func (bc *BackupController) ensureRestoreDiskSecret(ctx context.Context, fr *v1alpha1.BackupFileRestore, name string) error {
	ns := bc.fileJobs.Namespace()
	err := bc.client.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &corev1.Secret{})
	if err == nil || !apierrors.IsNotFound(err) {
		return err
	}
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return err
	}
	cert, key, err := browse.NewSessionCert(guestFileRestoreDeadline + time.Hour)
	if err != nil {
		return err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name,
			Labels:          map[string]string{restoreDiskLabel: string(fr.UID)},
			OwnerReferences: []metav1.OwnerReference{fileRestoreOwner(fr)}},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"token": []byte(hex.EncodeToString(raw)), "tls.crt": cert, "tls.key": key},
	}
	if err = bc.client.Create(ctx, s); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// stopRestoreDisk ends the disk server of a restore that has ended, and removes
// its Secret. It does nothing for a restore that had none.
func (bc *BackupController) stopRestoreDisk(ctx context.Context, fr *v1alpha1.BackupFileRestore) {
	sp := fr.Spec
	if !sp.Guest || (sp.Source != v1alpha1.BackupSourceCOS && sp.FromCOS == nil) {
		return
	}
	disks, ok := bc.fileJobs.(FileRestoreDiskJobs)
	if !ok {
		return
	}
	id := string(fr.UID)
	if _, err := disks.StopDisk(ctx, id); err != nil {
		klog.ErrorS(err, "Could not stop the disk server of a file restore", "restore", fr.Name)
	}
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: bc.fileJobs.Namespace(), Name: restoreDiskSecretName(id)}}
	if err := bc.client.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
		klog.ErrorS(err, "Could not delete the Secret of a disk server", "restore", fr.Name)
	}
}
