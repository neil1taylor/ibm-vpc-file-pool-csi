package pool

import (
	"context"
	"errors"
	"fmt"
	"path"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// A BackupRestore with spec.fromCOS reads its backup from a COS bucket. It
// needs no BackupPolicy and no snapshot pool in the cluster. It makes the PVC
// the same way a local restore does, then runs three Jobs on the share of the
// new volume: fetch one export from the Kopia repository into a scratch folder,
// copy the scratch folder into the volume with the restore Job of a local
// restore, and remove the scratch folder. The status remembers which steps
// have ended, because a Job is gone once it has been read. The controller never
// reads the keys: the Jobs do. See docs/specs/2026-10-21-backup-cos-export.md.

// COSRepoOf says where the Kopia repository of a policy is, for a Job that only
// reads it. The area of the policy is below the prefix of the block. It checks
// the block, because the webhook can be off.
func COSRepoOf(c *v1alpha1.RestoreFromCOS, policy string) (ExportRepo, error) {
	if err := c.ValidateSource(); err != nil {
		return ExportRepo{}, err
	}
	repo := ExportRepo{
		Endpoint: c.Endpoint, Bucket: c.Bucket, Prefix: exportPrefix(c.Prefix, policy),
		CredentialsSecret: c.CredentialsSecret, BackupKeySecret: c.BackupKeySecret,
		EmergencyKeySecret: c.EmergencyKeySecret, EmergencyOnly: c.EmergencyKeyOnly,
		LockMode: ExportLockNone, MaintenanceInterval: v1alpha1.MinExportMaintenanceInterval,
	}
	if c.KeyProtect != nil {
		repo.KeyProtect = *c.KeyProtect
	}
	if err := repo.validateRead(); err != nil {
		return ExportRepo{}, err
	}
	return repo, nil
}

// ExportAreaPrefix is the folder of one policy inside the bucket: the prefix of
// the export block, then the name of the policy. The delete marker clean-up of
// `kubectl backup restore --from-cos` works on this area only.
func ExportAreaPrefix(prefix, policy string) string { return exportPrefix(prefix, policy) }

// saveRestoreProgress records a step of a restore from COS.
func (bc *BackupController) saveRestoreProgress(ctx context.Context, br *v1alpha1.BackupRestore, phase, msg string) error {
	br.Status.Phase, br.Status.Message = phase, msg
	if err := bc.client.Status().Update(ctx, br); err != nil {
		klog.ErrorS(err, "Failed to update BackupRestore status", "restore", br.Name)
		return err
	}
	return nil
}

func (bc *BackupController) reconcileRestoreFromCOS(ctx context.Context, br *v1alpha1.BackupRestore) {
	sp := br.Spec
	err := sp.FromCOS.Validate()
	var repo ExportRepo
	if err == nil {
		repo, err = COSRepoOf(sp.FromCOS, sp.Policy)
	}
	if err != nil {
		bc.failRestore(ctx, br, err.Error())
		return
	}
	if bc.shareData == nil {
		bc.setRestoreStatus(ctx, br, restorePhasePending, "the controller cannot run Jobs on shares yet")
		return
	}

	// The class and size come from the request: COS does not keep them.
	pvc, err := bc.ensureRestoreTarget(ctx, br, &v1alpha1.BackupVolumeStatus{
		StorageClassName: sp.StorageClassName, RequestedGB: int64(sp.FromCOS.SizeGB),
	})
	if err != nil {
		var f restoreFailure
		if errors.As(err, &f) {
			bc.failRestore(ctx, br, f.Error())
			return
		}
		bc.setRestoreStatus(ctx, br, restorePhasePending, err.Error())
		return
	}
	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		bc.setRestoreStatus(ctx, br, restorePhaseProvisioning, "waiting for the driver to bind the new PVC")
		return
	}
	sv := &v1alpha1.SubVolume{}
	if err = bc.client.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, sv); err != nil {
		bc.setRestoreStatus(ctx, br, restorePhaseProvisioning, "waiting for the SubVolume of the new PVC")
		return
	}
	// Hold back pods until the restore has ended.
	if sv.Labels[RestorePendingLabel] != "true" {
		bc.setSubVolumeRestoreLabel(ctx, sv, true)
	}

	dst := subVolumeShareMount(sv)
	targetVolume := path.Base(sv.Spec.SubPath)
	scratch := COSScratchName(br.Name)
	var at time.Time
	if sp.Moment != nil {
		at = sp.Moment.Time
	}
	held := fmt.Sprintf("The new PVC %s/%s is held back; delete it and ask again", sp.TargetNamespace, sp.TargetName)

	if !br.Status.Fetched {
		done, _, ferr := bc.shareData.FetchFromCOS(ctx, "cosfetch-"+br.Name, dst, repo, COSRestoreRequest{
			SourceVolume: sp.FromCOS.Volume, TargetVolume: targetVolume, Scratch: scratch, Moment: at,
		})
		if errors.Is(ferr, ErrShareJobFailed) {
			bc.failRestore(ctx, br, fmt.Sprintf("fetching the backup from COS failed: %v. Check the bucket, the keys and the volume name. %s", ferr, held))
			return
		}
		if ferr != nil {
			bc.setRestoreStatus(ctx, br, restorePhaseRestoring, ferr.Error())
			return
		}
		if !done {
			bc.setRestoreStatus(ctx, br, restorePhaseRestoring, "fetching the backup from COS")
			return
		}
		br.Status.Fetched = true
		if bc.saveRestoreProgress(ctx, br, restorePhaseRestoring, "copying the backup into the new volume") != nil {
			return
		}
	}

	if !br.Status.Copied {
		backup, moments := COSScratchPaths(scratch, targetVolume)
		owner := &Ownership{UID: sv.Spec.UID, GID: sv.Spec.GID, Permissions: sv.Spec.Permissions, Recursive: true}
		done, outcome, cerr := bc.shareData.RestoreBackup(ctx, "restore-"+br.Name, dst, backup, moments, dst, sv.Spec.SubPath, at, owner)
		if errors.Is(cerr, ErrShareJobFailed) {
			bc.failRestore(ctx, br, fmt.Sprintf("the copy failed: %v. %s", cerr, held))
			return
		}
		if cerr != nil {
			bc.setRestoreStatus(ctx, br, restorePhaseRestoring, cerr.Error())
			return
		}
		if !done {
			bc.setRestoreStatus(ctx, br, restorePhaseRestoring, "copying the backup into the new volume")
			return
		}
		br.Status.Copied, br.Status.Latest = true, outcome.Latest
		if !outcome.Moment.IsZero() {
			m := metav1.NewTime(outcome.Moment)
			br.Status.RestoredMoment = &m
		}
		if bc.saveRestoreProgress(ctx, br, restorePhaseRestoring, "removing the scratch copy") != nil {
			return
		}
	}

	// The data is in the volume. A scratch folder that cannot be removed is
	// reported in the log and costs space; it does not fail the restore.
	cleaned, rerr := bc.shareData.RemoveCOSScratch(ctx, "coscleanup-"+br.Name, dst, scratch)
	switch {
	case errors.Is(rerr, ErrShareJobFailed):
		klog.ErrorS(rerr, "Could not remove the scratch folder of a restore from COS; remove it by hand",
			"restore", br.Name, "folder", "/backup/"+scratch)
	case rerr != nil:
		bc.setRestoreStatus(ctx, br, restorePhaseRestoring, rerr.Error())
		return
	case !cleaned:
		bc.setRestoreStatus(ctx, br, restorePhaseRestoring, "removing the scratch copy")
		return
	}
	// Save Done first. The volume opens only after that.
	if bc.saveRestoreProgress(ctx, br, restorePhaseDone, "") != nil {
		return
	}
	bc.setSubVolumeRestoreLabel(ctx, sv, false)
}
