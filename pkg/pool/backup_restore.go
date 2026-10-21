package pool

import (
	"context"
	"errors"
	"fmt"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A BackupRestore makes a new PVC from a backup. The controller makes the PVC,
// waits for the driver to bind it, copies the backup into the volume with a
// Job, and until the copy has ended it holds back every pod that mounts the
// volume. It never writes over a volume that exists.

const (
	// backupRestoreAnnotation is on the PVC a restore made. Its value is the
	// name of the BackupRestore.
	backupRestoreAnnotation = "storage.ibmcloud.io/backup-restore"

	// RestorePendingLabel is on the SubVolume of a volume that a restore is
	// still filling. The node plugin refuses to mount such a volume.
	RestorePendingLabel = "storage.ibmcloud.io/restore-pending"

	restorePhasePending      = "Pending"
	restorePhaseProvisioning = "Provisioning"
	restorePhaseRestoring    = "Restoring"
	restorePhaseDone         = "Done"
	restorePhaseFailed       = "Failed"
)

// SetShareData sets how the controller runs Jobs on shares. Without it,
// restores wait.
func (bc *BackupController) SetShareData(sd ShareData) { bc.shareData = sd }

func (bc *BackupController) reconcileRestores(ctx context.Context) {
	bc.openFinishedRestores(ctx)
	list := &v1alpha1.BackupRestoreList{}
	if err := bc.client.List(ctx, list); err != nil {
		klog.ErrorS(err, "Failed to list BackupRestores")
		return
	}
	for i := range list.Items {
		br := &list.Items[i]
		if br.Status.Phase == restorePhaseDone || br.Status.Phase == restorePhaseFailed {
			continue
		}
		bc.reconcileRestore(ctx, br)
	}
}

// setRestoreStatus records the phase and the message of a restore.
func (bc *BackupController) setRestoreStatus(ctx context.Context, br *v1alpha1.BackupRestore, phase, msg string) {
	_ = bc.saveRestoreStatus(ctx, br, phase, msg) // a failed save is logged and tried again next pass
}

// saveRestoreStatus is setRestoreStatus for a caller that must know if the
// save worked.
func (bc *BackupController) saveRestoreStatus(ctx context.Context, br *v1alpha1.BackupRestore, phase, msg string) error {
	if br.Status.Phase == phase && br.Status.Message == msg {
		return nil
	}
	br.Status.Phase, br.Status.Message = phase, msg
	if err := bc.client.Status().Update(ctx, br); err != nil {
		klog.ErrorS(err, "Failed to update BackupRestore status", "restore", br.Name)
		return err
	}
	return nil
}

func (bc *BackupController) failRestore(ctx context.Context, br *v1alpha1.BackupRestore, msg string) {
	klog.ErrorS(nil, "Backup restore failed", "restore", br.Name, "reason", msg)
	bc.setRestoreStatus(ctx, br, restorePhaseFailed, msg)
}

func (bc *BackupController) reconcileRestore(ctx context.Context, br *v1alpha1.BackupRestore) {
	sp := br.Spec
	policy := &v1alpha1.BackupPolicy{}
	if err := bc.client.Get(ctx, types.NamespacedName{Name: sp.Policy}, policy); err != nil {
		if apierrors.IsNotFound(err) {
			bc.failRestore(ctx, br, fmt.Sprintf("the backup policy %q does not exist", sp.Policy))
			return
		}
		bc.setRestoreStatus(ctx, br, restorePhasePending, fmt.Sprintf("get the backup policy: %v", err))
		return
	}
	var entry *v1alpha1.BackupVolumeStatus
	for i := range policy.Status.Volumes {
		v := &policy.Status.Volumes[i]
		if v.PVCNamespace == sp.SourceNamespace && v.PVCName == sp.SourceName {
			entry = v
		}
	}
	if entry == nil || entry.SubPath == "" {
		bc.failRestore(ctx, br, fmt.Sprintf("policy %s has no backup of %s/%s", sp.Policy, sp.SourceNamespace, sp.SourceName))
		return
	}
	if bc.shareData == nil {
		bc.setRestoreStatus(ctx, br, restorePhasePending, "the controller cannot run Jobs on shares yet")
		return
	}

	// Check the snapshot pool before anything is made.
	svs := &v1alpha1.SubVolumeList{}
	if err := bc.client.List(ctx, svs); err != nil {
		bc.setRestoreStatus(ctx, br, restorePhasePending, fmt.Sprintf("list SubVolumes: %v", err))
		return
	}
	backupShare, err := bc.snapshotShare(ctx, policy, svs.Items)
	if err != nil {
		bc.setRestoreStatus(ctx, br, restorePhasePending, err.Error())
		return
	}

	pvc, err := bc.ensureRestoreTarget(ctx, br, entry)
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

	// Hold back pods until the copy has ended.
	if sv.Labels[RestorePendingLabel] != "true" {
		bc.setSubVolumeRestoreLabel(ctx, sv, true)
	}

	var at time.Time
	if sp.Moment != nil {
		at = sp.Moment.Time
	}
	policyBase := backupBasePath + "/" + policy.Name
	owner := &Ownership{UID: sv.Spec.UID, GID: sv.Spec.GID, Permissions: sv.Spec.Permissions, Recursive: true}
	done, outcome, err := bc.shareData.RestoreBackup(ctx, "restore-"+br.Name, shareMountOf(backupShare),
		policyBase+entry.SubPath, policyBase+"/.moments"+entry.SubPath,
		subVolumeShareMount(sv), sv.Spec.SubPath, at, owner)
	if errors.Is(err, ErrShareJobFailed) {
		// The volume stays held back: it holds a half-restored copy.
		bc.failRestore(ctx, br, fmt.Sprintf("the copy failed: %v. The new PVC %s/%s is held back; delete it and ask again",
			err, sp.TargetNamespace, sp.TargetName))
		return
	}
	if err != nil {
		bc.setRestoreStatus(ctx, br, restorePhaseRestoring, err.Error())
		return
	}
	if !done {
		bc.setRestoreStatus(ctx, br, restorePhaseRestoring, "copying the backup into the new volume")
		return
	}
	// Save Done first. The volume opens only after that, so a failed save
	// cannot leave a volume open that the next pass would empty and fill again.
	br.Status.Latest = outcome.Latest
	if !outcome.Moment.IsZero() {
		m := metav1.NewTime(outcome.Moment)
		br.Status.RestoredMoment = &m
	}
	if err := bc.saveRestoreStatus(ctx, br, restorePhaseDone, ""); err != nil {
		return
	}
	bc.setSubVolumeRestoreLabel(ctx, sv, false)
}

// openFinishedRestores removes the hold-back label from volumes whose restore
// has ended with Done. It covers a controller that stopped after it saved Done
// and before it removed the label.
func (bc *BackupController) openFinishedRestores(ctx context.Context) {
	svs := &v1alpha1.SubVolumeList{}
	if err := bc.client.List(ctx, svs, client.MatchingLabels{RestorePendingLabel: "true"}); err != nil {
		klog.ErrorS(err, "Failed to list held-back SubVolumes")
		return
	}
	for i := range svs.Items {
		sv := &svs.Items[i]
		pvc := &corev1.PersistentVolumeClaim{}
		if err := bc.client.Get(ctx, types.NamespacedName{Namespace: sv.Spec.PVCNamespace, Name: sv.Spec.PVCName}, pvc); err != nil {
			continue
		}
		name := pvc.Annotations[backupRestoreAnnotation]
		if name == "" {
			continue
		}
		br := &v1alpha1.BackupRestore{}
		if err := bc.client.Get(ctx, types.NamespacedName{Name: name}, br); err != nil {
			continue
		}
		if br.Status.Phase == restorePhaseDone {
			bc.setSubVolumeRestoreLabel(ctx, sv, false)
		}
	}
}

// shareMountOf turns the status of a pool's share into a mount.
func shareMountOf(sh v1alpha1.PoolShareStatus) ShareMount {
	return ShareMount{MountTargetIP: sh.MountTargetIP, ExportPath: sh.ExportPath}
}

// restoreFailure is an error that ends a restore: asking again changes nothing.
type restoreFailure string

func (f restoreFailure) Error() string { return string(f) }

// ensureRestoreTarget returns the new PVC. It makes it if it does not exist. A
// PVC that another restore or a person made is never used.
func (bc *BackupController) ensureRestoreTarget(ctx context.Context, br *v1alpha1.BackupRestore, entry *v1alpha1.BackupVolumeStatus) (*corev1.PersistentVolumeClaim, error) {
	sp := br.Spec
	pvc := &corev1.PersistentVolumeClaim{}
	err := bc.client.Get(ctx, types.NamespacedName{Namespace: sp.TargetNamespace, Name: sp.TargetName}, pvc)
	if err == nil {
		if pvc.Annotations[backupRestoreAnnotation] != br.Name {
			return nil, restoreFailure(fmt.Sprintf("%s/%s exists; a restore never writes over a volume that exists. Choose another name",
				sp.TargetNamespace, sp.TargetName))
		}
		return pvc, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get the target PVC: %w", err)
	}
	class := sp.StorageClassName
	if class == "" {
		class = entry.StorageClassName
	}
	if class == "" {
		return nil, restoreFailure("the storage class of the backed-up claim is not known; set spec.storageClassName")
	}
	size := entry.RequestedGB
	if size < 1 {
		size = 1
	}
	pvc = &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:   sp.TargetNamespace,
			Name:        sp.TargetName,
			Annotations: map[string]string{backupRestoreAnnotation: br.Name},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &class,
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: *resource.NewQuantity(size<<30, resource.BinarySI)},
			},
		},
	}
	if err := bc.client.Create(ctx, pvc); err != nil {
		return nil, fmt.Errorf("make the target PVC: %w", err)
	}
	klog.V(2).InfoS("Made the PVC for a backup restore", "restore", br.Name, "pvc", klog.KRef(sp.TargetNamespace, sp.TargetName))
	return pvc, nil
}

// setSubVolumeRestoreLabel holds a volume back from pods, or lets them have it.
func (bc *BackupController) setSubVolumeRestoreLabel(ctx context.Context, sv *v1alpha1.SubVolume, pending bool) {
	patch := client.MergeFrom(sv.DeepCopy())
	if pending {
		if sv.Labels == nil {
			sv.Labels = map[string]string{}
		}
		sv.Labels[RestorePendingLabel] = "true"
	} else {
		delete(sv.Labels, RestorePendingLabel)
	}
	if err := bc.client.Patch(ctx, sv, patch); err != nil {
		klog.ErrorS(err, "Failed to change the restore-pending label", "subVolume", sv.Name, "pending", pending)
	}
}
