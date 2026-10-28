package pool

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A BackupPolicy with spec.export also sends its backups to a COS bucket. When
// the local backup of a volume has a moment that is newer than the last one
// exported, the controller starts an export Job for it. The Job reads the
// snapshot pool's share read-only, so a running VM is never slowed. The
// design is in docs/specs/2026-10-21-backup-cos-export.md.
//
// An export problem only ever sets exportError on a volume. It never changes
// the phase of the policy and never pauses the backups to the snapshot pool.
// The controller never reads the keys: it passes the names of Secrets to the
// Jobs.

const (
	// exportRetryDelay is how long the controller waits before it tries a failed
	// export or maintenance Job again.
	exportRetryDelay = 5 * time.Minute

	exportKeyPrefix      = "export-"
	exportMaintenanceKey = "exportmaint-"
)

// exportState is what the controller remembers about the exports of a policy.
// It is only a help: after a restart the controller works it out again from the
// status, at the cost of one more Job.
type exportState struct {
	// active is the SubVolume whose export Job the controller is waiting for.
	// It keeps the rule of one Job at a time even when an earlier volume leaves
	// its retry delay while a later one runs.
	active string
	// failedAt is when the last export of each SubVolume failed.
	failedAt map[string]time.Time
	// maintDone and maintFailed are the end of the last maintenance Job and the
	// time of its last failure.
	maintDone, maintFailed time.Time
}

func (bc *BackupController) exportStateOf(policy string) *exportState {
	s := bc.exports[policy]
	if s == nil {
		s = &exportState{failedAt: map[string]time.Time{}}
		bc.exports[policy] = s
	}
	return s
}

// exportPrefix is the area of one policy inside the bucket: the prefix of the
// block, then the name of the policy.
func exportPrefix(prefix, policy string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return policy + "/"
	}
	return prefix + "/" + policy + "/"
}

// exportRepoOf says where the Kopia repository of a policy is. It checks the
// block again, because the webhook can be off.
func exportRepoOf(bp *v1alpha1.BackupPolicy) (ExportRepo, time.Duration, error) {
	ex := bp.Spec.Export
	if err := ex.Validate(); err != nil {
		return ExportRepo{}, 0, err
	}
	keep, _ := ex.EffectiveKeepFor() // Validate has read it
	repo := ExportRepo{
		Endpoint: ex.Endpoint, Bucket: ex.Bucket, Prefix: exportPrefix(ex.Prefix, bp.Name),
		CredentialsSecret: ex.CredentialsSecret, BackupKeySecret: ex.BackupKeySecret,
		KeyProtect: ex.KeyProtect, EmergencyKeySecret: ex.EmergencyKeySecret,
		LockMode: ExportLockMode(ex.EffectiveLock()), KeepFor: keep,
		MaintenanceInterval: v1alpha1.ExportMaintenanceInterval(keep),
	}
	if err := repo.validate(); err != nil {
		return ExportRepo{}, 0, err
	}
	return repo, keep, nil
}

func clearExportStatus(st *v1alpha1.BackupPolicyStatus) {
	for i := range st.Volumes {
		st.Volumes[i].ExportedTime, st.Volumes[i].ExportedBytes, st.Volumes[i].ExportError = nil, 0, ""
	}
}

// exportDue says whether a volume has a local moment to export now.
func exportDue(v v1alpha1.BackupVolumeStatus, now time.Time, schedule time.Duration) bool {
	if v.LastBackupTime == nil || v.SubPath == "" {
		return false
	}
	if v.ExportedTime == nil {
		return true
	}
	if !v.LastBackupTime.After(v.ExportedTime.Time) {
		return false
	}
	return schedule == 0 || now.Sub(v.ExportedTime.Time) >= schedule
}

// reconcileExport starts, follows and records the export Jobs of a policy, and
// runs its maintenance. It changes only the export fields of st.
func (bc *BackupController) reconcileExport(ctx context.Context, bp *v1alpha1.BackupPolicy, st *v1alpha1.BackupPolicyStatus, shares []v1alpha1.PoolShareStatus) {
	if bp.Spec.Export == nil {
		clearExportStatus(st)
		delete(bc.exports, bp.Name)
		bc.releaseStaleExportHolds(ctx, bp.Name, "")
		return
	}
	repo, keep, err := exportRepoOf(bp)
	if err != nil {
		for i := range st.Volumes {
			if st.Volumes[i].LastBackupTime != nil {
				st.Volumes[i].ExportError = err.Error()
			}
		}
		return
	}
	if bc.shareData == nil {
		return
	}
	var schedule time.Duration
	if bp.Spec.Export.Schedule != "" {
		schedule, _ = time.ParseDuration(bp.Spec.Export.Schedule) // Validate has read it
	}
	now := bc.nowFunc()
	state := bc.exportStateOf(bp.Name)

	var due []int
	for i, v := range st.Volumes {
		if exportDue(v, now, schedule) {
			due = append(due, i)
		}
	}
	sort.Slice(due, func(a, b int) bool {
		va, vb := st.Volumes[due[a]], st.Volumes[due[b]]
		switch {
		case (va.ExportedTime == nil) != (vb.ExportedTime == nil):
			return va.ExportedTime == nil
		case va.ExportedTime != nil && !va.ExportedTime.Equal(vb.ExportedTime):
			return va.ExportedTime.Before(vb.ExportedTime)
		}
		return va.SubVolumeName < vb.SubVolumeName
	})
	if state.active != "" {
		still := false
		for _, i := range due {
			still = still || st.Volumes[i].SubVolumeName == state.active
		}
		if !still {
			state.active = ""
		}
	}

	copying, copiesKnown := bc.volumesBeingCopied(ctx, bp.Name)

export:
	for _, i := range due {
		v := &st.Volumes[i]
		if state.active != "" && state.active != v.SubVolumeName {
			continue
		}
		if at, failed := state.failedAt[v.SubVolumeName]; failed && now.Sub(at) < exportRetryDelay {
			continue
		}
		sh, found := shareByID(shares, shareOfEntry(*v, shares))
		if !found || !shareReady(sh) {
			continue
		}
		if !copiesKnown && state.active != v.SubVolumeName {
			continue
		}
		switch bc.takeExportHold(ctx, bp.Name, v.SubVolumeName, now, state.active == v.SubVolumeName, copying) {
		case holdWait:
			continue
		case holdExpired:
			key := exportKeyPrefix + bp.Name + "-" + v.SubVolumeName
			if _, stopErr := bc.shareData.Stop(ctx, key); stopErr != nil {
				klog.ErrorS(stopErr, "Failed to stop an export Job that passed the hold limit", "policy", bp.Name, "volume", v.SubVolumeName)
			}
			bc.releaseExportHold(ctx, bp.Name, v.SubVolumeName)
			v.ExportError = fmt.Sprintf("the export took longer than %d hours, so it was stopped; it is tried again", int(exportHoldLimit.Hours()))
			state.failedAt[v.SubVolumeName] = now
			state.active = ""
			continue
		}
		base := backupBasePath + "/" + bp.Name
		done, out, jobErr := bc.shareData.ExportToCOS(ctx, exportKeyPrefix+bp.Name+"-"+v.SubVolumeName, shareMountOf(sh), repo,
			base+v.SubPath, base+"/.moments"+v.SubPath, v.LastBackupTime.Time)
		if jobErr == nil && done && out.Moment.IsZero() {
			jobErr = errors.New("the export Job finished but did not report which moment it sent")
		}
		switch {
		case jobErr != nil:
			if errors.Is(jobErr, ErrShareJobFailed) {
				v.ExportError = fmt.Sprintf("the export Job failed (%v); it is tried again", jobErr)
			} else {
				v.ExportError = fmt.Sprintf("export to COS: %v", jobErr)
			}
			klog.ErrorS(jobErr, "Export of a backup to COS failed", "policy", bp.Name, "volume", v.SubVolumeName)
			state.failedAt[v.SubVolumeName] = now
			state.active = ""
			bc.releaseExportHold(ctx, bp.Name, v.SubVolumeName)
		case !done:
			state.active = v.SubVolumeName
			break export
		default:
			m := metav1.NewTime(out.Moment)
			v.ExportedTime, v.ExportedBytes, v.ExportError = &m, out.Bytes, ""
			delete(state.failedAt, v.SubVolumeName)
			state.active = ""
			bc.releaseExportHold(ctx, bp.Name, v.SubVolumeName)
			klog.V(2).InfoS("Exported a backup to COS", "policy", bp.Name, "volume", v.SubVolumeName, "moment", out.Moment, "bytes", out.Bytes)
		}
	}
	bc.releaseStaleExportHolds(ctx, bp.Name, state.active)
	bc.maintainExport(ctx, bp, st, repo, keep, state, now)
}

// maintainExport runs Kopia's maintenance on the repository of a policy once
// every maintenance interval, after the first export has made the repository.
// It extends the locks of chunks still in use and deletes the snapshots that
// are older than the keep time. A failure is logged and tried again later.
func (bc *BackupController) maintainExport(ctx context.Context, bp *v1alpha1.BackupPolicy, st *v1alpha1.BackupPolicyStatus,
	repo ExportRepo, keep time.Duration, state *exportState, now time.Time) {
	exported := false
	for _, v := range st.Volumes {
		exported = exported || v.ExportedTime != nil
	}
	if !exported {
		return
	}
	if !state.maintDone.IsZero() && now.Sub(state.maintDone) < repo.MaintenanceInterval {
		return
	}
	if !state.maintFailed.IsZero() && now.Sub(state.maintFailed) < exportRetryDelay {
		return
	}
	repo.ExpireBefore = now.Add(-keep)
	done, err := bc.shareData.MaintainCOSRepo(ctx, exportMaintenanceKey+bp.Name, repo)
	switch {
	case err != nil:
		klog.ErrorS(err, "Maintenance of the COS repository failed; it is tried again", "policy", bp.Name)
		state.maintFailed = now
	case done:
		state.maintDone, state.maintFailed = now, time.Time{}
		klog.V(2).InfoS("Ran maintenance on the COS repository", "policy", bp.Name)
	}
}

// recordExportMetrics reports how late the exports of a policy are.
func (bc *BackupController) recordExportMetrics(bp *v1alpha1.BackupPolicy, st v1alpha1.BackupPolicyStatus) {
	if bp.Spec.Export == nil {
		bc.deleteExportMetrics(bp.Name)
		return
	}
	now := bc.nowFunc()
	var oldestUnexported float64
	newest, haveNewest, exportable := 0.0, false, false
	for _, v := range st.Volumes {
		if v.LastBackupTime == nil {
			continue
		}
		exportable = true
		if v.ExportedTime == nil || v.LastBackupTime.After(v.ExportedTime.Time) {
			if age := now.Sub(v.LastBackupTime.Time).Seconds(); age > oldestUnexported {
				oldestUnexported = age
			}
		}
		if v.ExportedTime != nil {
			if age := now.Sub(v.ExportedTime.Time).Seconds(); !haveNewest || age < newest {
				newest, haveNewest = age, true
			}
		}
	}
	if !haveNewest && exportable && !bp.CreationTimestamp.IsZero() {
		newest = now.Sub(bp.CreationTimestamp.Time).Seconds()
	}
	metrics.BackupExportOldestUnexportedAgeSeconds.WithLabelValues(bp.Name).Set(oldestUnexported)
	metrics.BackupExportNewestAgeSeconds.WithLabelValues(bp.Name).Set(newest)
	bc.exportReported[bp.Name] = true
}

func (bc *BackupController) deleteExportMetrics(policy string) {
	if !bc.exportReported[policy] {
		return
	}
	metrics.BackupExportOldestUnexportedAgeSeconds.DeleteLabelValues(policy)
	metrics.BackupExportNewestAgeSeconds.DeleteLabelValues(policy)
	delete(bc.exportReported, policy)
}

const (
	// exportHoldAnnotation is set on a SubVolume while the export of its backup
	// runs. Its value is "<policy>/<time the hold began>". The replication
	// controller makes no new local backup of a held volume.
	exportHoldAnnotation = "storage.ibmcloud.io/export-hold"
	// exportHoldLimit is the longest a hold lasts. After it the export is
	// stopped and tried again later, so a stuck Job cannot stop backups.
	exportHoldLimit = 6 * time.Hour
)

// holdResult tells the export loop what to do with a volume.
type holdResult int

const (
	holdTaken   holdResult = iota // the volume is held by this policy: export it
	holdWait                      // do not export it now
	holdExpired                   // the hold passed its limit
)

// volumesBeingCopied lists the volumes of a policy whose local backup is being
// copied now. An export must not start on one of them, because the copy changes
// the folder the export reads.
// It reports known=false when it cannot tell, and the caller starts no export.
func (bc *BackupController) volumesBeingCopied(ctx context.Context, policy string) (busy map[string]bool, known bool) {
	busy = map[string]bool{}
	rps := &v1alpha1.ReplicationPolicyList{}
	if err := bc.client.List(ctx, rps, client.MatchingLabels{backupPolicyLabel: policy}); err != nil {
		klog.ErrorS(err, "Failed to list the replication policies of a backup policy", "policy", policy)
		return nil, false
	}
	for _, rp := range rps.Items {
		for _, s := range rp.Status.SubVolumeStatuses {
			if s.LastError == "sync in progress" || s.VMCopy != nil {
				busy[s.SubVolumeName] = true
			}
		}
	}
	return busy, true
}

// takeExportHold marks the volume as held by the export of this policy, so the
// replication controller makes no new backup of it while the export reads it.
// A running export keeps the hold it has. An export that has not started waits
// if a copy of the volume runs or another policy holds the volume.
func (bc *BackupController) takeExportHold(ctx context.Context, policy, svName string, now time.Time, running bool, copying map[string]bool) holdResult {
	sv := &v1alpha1.SubVolume{}
	if err := bc.client.Get(ctx, types.NamespacedName{Name: svName}, sv); err != nil {
		klog.ErrorS(err, "Failed to read a SubVolume to hold it for an export", "policy", policy, "volume", svName)
		return holdWait
	}
	owner, since, held := parseExportHold(sv.Annotations[exportHoldAnnotation])
	switch {
	case held && owner != policy:
		return holdWait
	case held && now.Sub(since) > exportHoldLimit:
		return holdExpired
	case held:
		return holdTaken
	case !running && copying[svName]:
		return holdWait
	}
	patch := client.MergeFrom(sv.DeepCopy())
	if sv.Annotations == nil {
		sv.Annotations = map[string]string{}
	}
	sv.Annotations[exportHoldAnnotation] = policy + "/" + now.UTC().Format(time.RFC3339)
	if err := bc.client.Patch(ctx, sv, patch); err != nil {
		klog.ErrorS(err, "Failed to hold a SubVolume for an export", "policy", policy, "volume", svName)
		return holdWait
	}
	return holdTaken
}

// releaseExportHold removes the hold of this policy from a volume.
func (bc *BackupController) releaseExportHold(ctx context.Context, policy, svName string) {
	sv := &v1alpha1.SubVolume{}
	if err := bc.client.Get(ctx, types.NamespacedName{Name: svName}, sv); err != nil {
		if !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to read a SubVolume to release its export hold", "policy", policy, "volume", svName)
		}
		return
	}
	if owner, _, held := parseExportHold(sv.Annotations[exportHoldAnnotation]); !held || owner != policy {
		return
	}
	patch := client.MergeFrom(sv.DeepCopy())
	delete(sv.Annotations, exportHoldAnnotation)
	if err := bc.client.Patch(ctx, sv, patch); err != nil {
		klog.ErrorS(err, "Failed to release the export hold of a SubVolume", "policy", policy, "volume", svName)
	}
}

// releaseStaleExportHolds removes the holds of this policy that have no export
// Job behind them: a Job that ended while the controller was down, or a policy
// that lost its export block. keep is the volume whose export is running.
func (bc *BackupController) releaseStaleExportHolds(ctx context.Context, policy, keep string) {
	svs := &v1alpha1.SubVolumeList{}
	if err := bc.client.List(ctx, svs); err != nil {
		klog.ErrorS(err, "Failed to list SubVolumes to release export holds", "policy", policy)
		return
	}
	for i := range svs.Items {
		sv := &svs.Items[i]
		if owner, _, held := parseExportHold(sv.Annotations[exportHoldAnnotation]); held && owner == policy && sv.Name != keep {
			bc.releaseExportHold(ctx, policy, sv.Name)
		}
	}
}

// parseExportHold reads the value of the hold annotation.
func parseExportHold(value string) (policy string, since time.Time, ok bool) {
	policy, stamp, found := strings.Cut(value, "/")
	if !found || policy == "" {
		return "", time.Time{}, false
	}
	since, err := time.Parse(time.RFC3339, stamp)
	if err != nil {
		return "", time.Time{}, false
	}
	return policy, since, true
}
