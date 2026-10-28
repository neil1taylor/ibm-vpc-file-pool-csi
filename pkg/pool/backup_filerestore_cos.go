package pool

import (
	"context"
	"errors"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

// reconcileFileRestoreFromCOS is a file restore whose backup is in COS. It is
// the same restore as one from the snapshot pool: the same Job program, the same
// targets, the same checks. Only the place the backup is read from differs, and
// the Job runs in the driver namespace, where the keys are.
func (bc *BackupController) reconcileFileRestoreFromCOS(ctx context.Context, fr *v1alpha1.BackupFileRestore) {
	sp := fr.Spec
	loc, err := bc.cosLocationOf(ctx, sp.Policy, sp.SourceNamespace, sp.SourceName, sp.FromCOS)
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
	if sp.Moment != nil {
		req.At = sp.Moment.Time
	}
	if sp.Existing {
		bc.fileRestoreIntoClaim(ctx, fr, req)
		return
	}
	bc.fileRestoreIntoNewClaim(ctx, fr, entry, req)
}
