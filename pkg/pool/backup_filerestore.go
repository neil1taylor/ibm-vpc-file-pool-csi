package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filerestore"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// A BackupFileRestore copies chosen files out of a backup into a claim. The
// controller works out where the backup and the claim are, runs one Job that
// does the copy, and writes the result into the status. Into a new claim it
// first sizes the claim with a Job that only reads, makes the claim, and holds
// pods back from it until the copy has ended. Into an existing claim it writes
// a new folder and nothing else of the claim changes. It never writes over a
// file that exists. The design is in docs/specs/2026-10-22-backup-file-restore.md.

const (
	// backupFileRestoreAnnotation and backupFileRestoreUIDAnnotation are on the
	// new claim a file restore made. They hold the name and the UID of the
	// restore. Both must match: a person can write the name on a claim of their own.
	backupFileRestoreAnnotation    = "storage.ibmcloud.io/backup-file-restore"
	backupFileRestoreUIDAnnotation = "storage.ibmcloud.io/backup-file-restore-uid"

	restorePhaseMeasuring = "Measuring"

	// DefaultFileRestoreKeep is how long an ended file restore stays visible.
	DefaultFileRestoreKeep = 24 * time.Hour

	// DefaultFileRestoreMaxRunning is how many file restores run at once.
	DefaultFileRestoreMaxRunning = 5

	// fileRestoreMargin is the room a new claim gets over the chosen files, in
	// percent. The minimum is one GiB.
	fileRestoreMargin = 20
)

// FileRestoreConfig sets the limits of file restores. A field that is zero gets
// its default.
type FileRestoreConfig struct {
	// MaxBytes is the most one restore writes.
	MaxBytes int64
	// Keep is how long an ended restore stays visible.
	Keep time.Duration
	// MaxRunning is how many restores may run at once in the cluster. More wait.
	MaxRunning int
}

// FileRestoreRequest asks for the Job of one file restore.
type FileRestoreRequest struct {
	// Key names the Job. It holds the UID of the BackupFileRestore, never its
	// name, so a restore deleted and made again with the same name cannot get
	// the earlier restore's Job or paths.
	Key string
	// Owner owns the Job, its share objects and the ConfigMap of paths.
	Owner metav1.OwnerReference
	// Mode is filerestore.ModeMeasure, ModeNewClaim or ModeFolder.
	Mode string
	// Backup is the snapshot pool's share. LatestPath and MomentsPath are the
	// backup folder of the volume and its earlier moments. At picks a moment;
	// the zero time reads the latest copy.
	Backup      ShareMount
	LatestPath  string
	MomentsPath string
	At          time.Time
	// Dest is the share that holds the target claim, and Volume the claim's
	// folder on it, like /pvcs/pvc-.... Not used for ModeMeasure.
	Dest   ShareMount
	Volume string
	// Paths are the paths to restore, in escaped form.
	Paths []string
	// Policy names the restored folder.
	Policy string
	// CapacityBytes is the size of the target claim.
	CapacityBytes int64
	// MaxBytes limits one restore.
	MaxBytes int64
	// UID, GID and Permissions are the owner and mode the claim is configured
	// with. Nil and empty keep the backup's own.
	UID, GID    *int64
	Permissions string
}

// FileRestoreJobs runs the Jobs of file restores.
type FileRestoreJobs interface {
	// RunFileRestore starts the Job if it does not exist, and reports done once
	// it has succeeded, with its outcome. A Job that failed gives an error that
	// is ErrShareJobFailed and holds the reason the Job wrote.
	RunFileRestore(ctx context.Context, req FileRestoreRequest) (bool, filerestore.Outcome, error)
	// Namespace is where the Jobs run.
	Namespace() string
}

// SetFileRestore turns on file restores.
func (bc *BackupController) SetFileRestore(jobs FileRestoreJobs, cfg FileRestoreConfig) {
	if cfg.MaxBytes <= 0 {
		cfg.MaxBytes = filerestore.DefaultMaxBytes
	}
	if cfg.Keep <= 0 {
		cfg.Keep = DefaultFileRestoreKeep
	}
	if cfg.MaxRunning <= 0 {
		cfg.MaxRunning = DefaultFileRestoreMaxRunning
	}
	bc.fileJobs, bc.fileCfg = jobs, cfg
}

func fileRestoreEnded(phase string) bool {
	return phase == restorePhaseDone || phase == restorePhaseFailed
}

// fileRestoreActive says if a restore has started its work: it has a Job, or is
// making the claim for one.
func fileRestoreActive(phase string) bool {
	return phase == restorePhaseMeasuring || phase == restorePhaseProvisioning || phase == restorePhaseRestoring
}

func (bc *BackupController) reconcileFileRestores(ctx context.Context) {
	if bc.fileJobs == nil {
		return
	}
	list := &v1alpha1.BackupFileRestoreList{}
	if err := bc.client.List(ctx, list); err != nil {
		klog.ErrorS(err, "Failed to list BackupFileRestores")
		return
	}
	items := list.Items
	sort.Slice(items, func(i, j int) bool {
		a, b := items[i].CreationTimestamp, items[j].CreationTimestamp
		if !a.Equal(&b) {
			return a.Before(&b)
		}
		return items[i].Name < items[j].Name
	})
	// Only one restore at a time writes into one existing claim: the first that
	// has not ended holds it.
	held := map[string]bool{}
	running := 0
	for i := range items {
		if fileRestoreActive(items[i].Status.Phase) {
			running++
		}
	}
	for i := range items {
		fr := &items[i]
		if fileRestoreEnded(fr.Status.Phase) {
			bc.tidyEndedFileRestore(ctx, fr)
			continue
		}
		if fr.Spec.Existing {
			k := fr.Spec.TargetNamespace + "/" + fr.Spec.TargetName
			if held[k] {
				bc.setFileRestoreStatus(ctx, fr, restorePhasePending, "waiting: another restore into this claim has not ended")
				continue
			}
			held[k] = true
		}
		// A restore that has not started waits for a free place. Each one holds a
		// Job, and two share mounts, while it runs.
		wasActive := fileRestoreActive(fr.Status.Phase)
		if !wasActive && running >= bc.fileCfg.MaxRunning {
			bc.setFileRestoreStatus(ctx, fr, restorePhasePending,
				fmt.Sprintf("waiting: %d restores are running and the limit is %d", running, bc.fileCfg.MaxRunning))
			continue
		}
		bc.reconcileFileRestore(ctx, fr)
		if !wasActive && fileRestoreActive(fr.Status.Phase) {
			running++
		}
	}
}

func (bc *BackupController) tidyEndedFileRestore(ctx context.Context, fr *v1alpha1.BackupFileRestore) {
	if fr.Status.EndedTime == nil || bc.nowFunc().Sub(fr.Status.EndedTime.Time) < bc.fileCfg.Keep {
		return
	}
	if err := bc.client.Delete(ctx, fr); err != nil && !apierrors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to delete an ended BackupFileRestore", "restore", fr.Name)
	}
}

// saveFileRestoreStatus writes the status of fr.
func (bc *BackupController) saveFileRestoreStatus(ctx context.Context, fr *v1alpha1.BackupFileRestore) error {
	if err := bc.client.Status().Update(ctx, fr); err != nil {
		klog.ErrorS(err, "Failed to update BackupFileRestore status", "restore", fr.Name)
		return err
	}
	return nil
}

func (bc *BackupController) setFileRestoreStatus(ctx context.Context, fr *v1alpha1.BackupFileRestore, phase, msg string) {
	if fr.Status.Phase == phase && fr.Status.Message == msg {
		return
	}
	fr.Status.Phase, fr.Status.Message = phase, msg
	_ = bc.saveFileRestoreStatus(ctx, fr) // a failed save is logged and tried again next pass
}

func (bc *BackupController) failFileRestore(ctx context.Context, fr *v1alpha1.BackupFileRestore, msg string) {
	klog.ErrorS(nil, "File restore failed", "restore", fr.Name, "reason", msg)
	now := metav1.NewTime(bc.nowFunc())
	fr.Status.Phase, fr.Status.Message, fr.Status.EndedTime = restorePhaseFailed, msg, &now
	if bc.saveFileRestoreStatus(ctx, fr) == nil {
		bc.fileRestoreEvent(ctx, fr, "FileRestoreFailed", msg, corev1.EventTypeWarning)
	}
}

// fileRestoreEvent writes an Event on the restore. It holds counts and reasons,
// never paths or contents. The gateway writes the Event that names the user and
// the paths.
func (bc *BackupController) fileRestoreEvent(ctx context.Context, fr *v1alpha1.BackupFileRestore, reason, msg, typ string) {
	if len(msg) > 1000 {
		msg = msg[:1000] + "..."
	}
	now := metav1.NewTime(bc.nowFunc())
	ev := &corev1.Event{
		ObjectMeta: metav1.ObjectMeta{GenerateName: "file-restore-", Namespace: bc.fileJobs.Namespace()},
		InvolvedObject: corev1.ObjectReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "BackupFileRestore",
			Name: fr.Name, UID: fr.UID},
		Reason: reason, Message: msg, Type: typ,
		Source:         corev1.EventSource{Component: "backup-controller"},
		FirstTimestamp: now, LastTimestamp: now, Count: 1,
	}
	if err := bc.client.Create(ctx, ev); err != nil {
		klog.ErrorS(err, "Could not write a file restore Event", "restore", fr.Name)
	}
}

func fileRestoreKey(fr *v1alpha1.BackupFileRestore) string { return "filerestore-" + string(fr.UID) }

func fileRestoreOwner(fr *v1alpha1.BackupFileRestore) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "BackupFileRestore", Name: fr.Name, UID: fr.UID}
}

func (bc *BackupController) reconcileFileRestore(ctx context.Context, fr *v1alpha1.BackupFileRestore) {
	sp := fr.Spec
	policy := &v1alpha1.BackupPolicy{}
	if err := bc.client.Get(ctx, types.NamespacedName{Name: sp.Policy}, policy); err != nil {
		if apierrors.IsNotFound(err) {
			bc.failFileRestore(ctx, fr, fmt.Sprintf("the backup policy %q does not exist", sp.Policy))
			return
		}
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, fmt.Sprintf("get the backup policy: %v", err))
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
		bc.failFileRestore(ctx, fr, fmt.Sprintf("policy %s has no backup of %s/%s", sp.Policy, sp.SourceNamespace, sp.SourceName))
		return
	}
	svs := &v1alpha1.SubVolumeList{}
	if err := bc.client.List(ctx, svs); err != nil {
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, fmt.Sprintf("list SubVolumes: %v", err))
		return
	}
	backupShare, err := bc.backupShareOf(ctx, policy, svs.Items, entry)
	if err != nil {
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, err.Error())
		return
	}
	policyBase := backupBasePath + "/" + policy.Name
	req := FileRestoreRequest{
		Key: fileRestoreKey(fr), Owner: fileRestoreOwner(fr),
		Backup: shareMountOf(backupShare), LatestPath: policyBase + entry.SubPath,
		Paths: sp.Paths, Policy: policy.Name, MaxBytes: bc.fileCfg.MaxBytes,
	}
	// A volume without moments has no moments folder, and a folder that is not
	// there cannot be mounted read-only.
	if entry.RestorePoints > 0 {
		req.MomentsPath = policyBase + "/.moments" + entry.SubPath
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

// fileRestoreFailureReason works out why a Job failed from what it wrote.
func fileRestoreFailureReason(err error) string {
	var f *shareJobFailure
	if !errors.As(err, &f) || f.msg == "" {
		return "the copy Job failed or ran out of time"
	}
	var out filerestore.Outcome
	if json.Unmarshal([]byte(f.msg), &out) == nil && out.Error != "" {
		return out.Error
	}
	return "the copy Job failed"
}

// runFileRestoreJob runs the Job. It returns the outcome once the Job is done.
// A Job that failed ends the restore; any other error is tried again next pass.
func (bc *BackupController) runFileRestoreJob(ctx context.Context, fr *v1alpha1.BackupFileRestore, phase, working, onFail string, req FileRestoreRequest) (filerestore.Outcome, bool) {
	done, out, err := bc.fileJobs.RunFileRestore(ctx, req)
	if errors.Is(err, ErrShareJobFailed) {
		bc.failFileRestore(ctx, fr, fmt.Sprintf("%s%s", fileRestoreFailureReason(err), onFail))
		return out, false
	}
	if err != nil {
		bc.setFileRestoreStatus(ctx, fr, phase, err.Error())
		return out, false
	}
	if !done {
		bc.setFileRestoreStatus(ctx, fr, phase, working)
		return out, false
	}
	if out.Error != "" {
		// The Job ended with exit code 0 and a reason: it decided the restore
		// cannot go on (no room, a VM disk claim, a missing path, an owner it
		// cannot set). It removed what it wrote.
		bc.failFileRestore(ctx, fr, out.Error+onFail)
		return out, false
	}
	return out, true
}

// finishFileRestore records the result of a copy that worked.
func (bc *BackupController) finishFileRestore(ctx context.Context, fr *v1alpha1.BackupFileRestore, out filerestore.Outcome) bool {
	now := metav1.NewTime(bc.nowFunc())
	st := &fr.Status
	st.Phase, st.Message, st.EndedTime = restorePhaseDone, fileRestoreSummary(out), &now
	st.Folder, st.Latest = out.Folder, out.Latest
	st.Files, st.Dirs, st.Links, st.Skipped, st.Unreadable, st.Bytes = out.Files, out.Dirs, out.Links, out.Skipped, out.Unreadable, out.Bytes
	st.RestoredMoment = nil
	if t, err := time.Parse(time.RFC3339, out.Moment); err == nil {
		m := metav1.NewTime(t)
		st.RestoredMoment = &m
	}
	if err := bc.saveFileRestoreStatus(ctx, fr); err != nil {
		return false
	}
	bc.fileRestoreEvent(ctx, fr, "FileRestoreDone", st.Message, corev1.EventTypeNormal)
	return true
}

func fileRestoreSummary(out filerestore.Outcome) string {
	msg := fmt.Sprintf("restored %d files, %d folders and %d links (%d bytes)", out.Files, out.Dirs, out.Links, out.Bytes)
	if out.Unreadable > 0 {
		msg += fmt.Sprintf("; %d could not be read", out.Unreadable)
	}
	if out.Skipped > 0 {
		msg += fmt.Sprintf("; %d devices, pipes or sockets were left out", out.Skipped)
	}
	return msg
}

// fileRestoreIntoClaim writes a new folder into a claim that exists.
func (bc *BackupController) fileRestoreIntoClaim(ctx context.Context, fr *v1alpha1.BackupFileRestore, req FileRestoreRequest) {
	sp := fr.Spec
	pvc := &corev1.PersistentVolumeClaim{}
	if err := bc.client.Get(ctx, types.NamespacedName{Namespace: sp.TargetNamespace, Name: sp.TargetName}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			bc.failFileRestore(ctx, fr, fmt.Sprintf("the claim %s/%s does not exist", sp.TargetNamespace, sp.TargetName))
			return
		}
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, fmt.Sprintf("get the claim: %v", err))
		return
	}
	// The claim is the one the user's rights were checked for, and stays so.
	want := sp.TargetUID
	if want == "" {
		want = fr.Status.TargetUID
	}
	if want == "" {
		want = pvc.UID
	}
	if pvc.UID != want {
		bc.failFileRestore(ctx, fr, fmt.Sprintf("the claim %s/%s was deleted and made again since the restore was asked for; ask again", sp.TargetNamespace, sp.TargetName))
		return
	}
	if fr.Status.TargetUID != want {
		fr.Status.TargetUID = want
		if err := bc.saveFileRestoreStatus(ctx, fr); err != nil {
			return
		}
	}
	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, "waiting for the claim to be bound")
		return
	}
	sv := &v1alpha1.SubVolume{}
	if err := bc.client.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, sv); err != nil {
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, "waiting for the SubVolume of the claim")
		return
	}
	if sv.Spec.PVCNamespace != pvc.Namespace || sv.Spec.PVCName != pvc.Name {
		bc.failFileRestore(ctx, fr, fmt.Sprintf("the volume of the claim %s/%s does not belong to it", sp.TargetNamespace, sp.TargetName))
		return
	}
	if sv.Labels[RestorePendingLabel] == "true" {
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, "the claim is being restored; it takes files when that restore has ended")
		return
	}
	req.Mode = filerestore.ModeFolder
	req.Dest, req.Volume = subVolumeShareMount(sv), sv.Spec.SubPath
	req.CapacityBytes = pvc.Spec.Resources.Requests.Storage().Value()
	req.UID, req.GID, req.Permissions = sv.Spec.UID, sv.Spec.GID, sv.Spec.Permissions
	out, done := bc.runFileRestoreJob(ctx, fr, restorePhaseRestoring, "copying the files into a new folder of the claim", "", req)
	if !done {
		return
	}
	bc.finishFileRestore(ctx, fr, out)
}

// fileRestoreSizeGB sizes a new claim: the chosen files plus the margin, rounded
// up to a whole GiB, at least one.
func fileRestoreSizeGB(bytes int64) int64 {
	const gib = int64(1) << 30
	need := bytes + bytes*fileRestoreMargin/100
	gb := (need + gib - 1) / gib
	if gb < 1 {
		gb = 1
	}
	return gb
}

// fileRestoreIntoNewClaim makes a claim for the files, sized to them unless the
// request gives a size.
func (bc *BackupController) fileRestoreIntoNewClaim(ctx context.Context, fr *v1alpha1.BackupFileRestore, entry *v1alpha1.BackupVolumeStatus, req FileRestoreRequest) {
	sp := fr.Spec
	size := sp.SizeGB
	if size == 0 {
		size = fr.Status.SizeGB
	}
	if size == 0 {
		mreq := req
		mreq.Key, mreq.Mode = "measure-"+req.Key, filerestore.ModeMeasure
		out, done := bc.runFileRestoreJob(ctx, fr, restorePhaseMeasuring, "measuring the chosen files to size the new claim", "", mreq)
		if !done {
			return
		}
		if out.Bytes > bc.fileCfg.MaxBytes {
			bc.failFileRestore(ctx, fr, fmt.Sprintf("the chosen files are %d bytes; the limit for one restore is %d bytes", out.Bytes, bc.fileCfg.MaxBytes))
			return
		}
		size = fileRestoreSizeGB(out.Bytes)
	}
	if fr.Status.SizeGB != size || fr.Status.Phase == restorePhaseMeasuring || fr.Status.Phase == "" {
		fr.Status.SizeGB = size
		fr.Status.Phase, fr.Status.Message = restorePhaseProvisioning, "making the new claim"
		if err := bc.saveFileRestoreStatus(ctx, fr); err != nil {
			return
		}
	}
	pvc, err := bc.ensureFileRestoreClaim(ctx, fr, entry, size)
	if err != nil {
		var f restoreFailure
		if errors.As(err, &f) {
			bc.failFileRestore(ctx, fr, f.Error())
			return
		}
		bc.setFileRestoreStatus(ctx, fr, restorePhasePending, err.Error())
		return
	}
	if pvc.Status.Phase != corev1.ClaimBound || pvc.Spec.VolumeName == "" {
		bc.setFileRestoreStatus(ctx, fr, restorePhaseProvisioning, "waiting for the driver to bind the new claim")
		return
	}
	sv := &v1alpha1.SubVolume{}
	if err := bc.client.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, sv); err != nil {
		bc.setFileRestoreStatus(ctx, fr, restorePhaseProvisioning, "waiting for the SubVolume of the new claim")
		return
	}
	if sv.Spec.PVCNamespace != pvc.Namespace || sv.Spec.PVCName != pvc.Name {
		bc.failFileRestore(ctx, fr, fmt.Sprintf("the volume of the claim %s/%s does not belong to it", sp.TargetNamespace, sp.TargetName))
		return
	}
	// Hold back pods until the copy has ended.
	if sv.Labels[RestorePendingLabel] != "true" {
		bc.setSubVolumeRestoreLabel(ctx, sv, true)
	}
	req.Mode = filerestore.ModeNewClaim
	req.Dest, req.Volume = subVolumeShareMount(sv), sv.Spec.SubPath
	req.CapacityBytes = size << 30
	req.UID, req.GID, req.Permissions = sv.Spec.UID, sv.Spec.GID, sv.Spec.Permissions
	held := fmt.Sprintf(". The new claim %s/%s is held back; delete it and ask again", sp.TargetNamespace, sp.TargetName)
	out, done := bc.runFileRestoreJob(ctx, fr, restorePhaseRestoring, "copying the files into the new claim", held, req)
	if !done {
		return
	}
	// Save Done first. The claim opens only after that, so a failed save cannot
	// leave a claim open that the next pass would empty and fill again.
	if !bc.finishFileRestore(ctx, fr, out) {
		return
	}
	bc.setSubVolumeRestoreLabel(ctx, sv, false)
}

// ensureFileRestoreClaim returns the new claim. It makes it if it does not
// exist. A claim that another restore or a person made is never used.
func (bc *BackupController) ensureFileRestoreClaim(ctx context.Context, fr *v1alpha1.BackupFileRestore, entry *v1alpha1.BackupVolumeStatus, sizeGB int64) (*corev1.PersistentVolumeClaim, error) {
	sp := fr.Spec
	pvc := &corev1.PersistentVolumeClaim{}
	err := bc.client.Get(ctx, types.NamespacedName{Namespace: sp.TargetNamespace, Name: sp.TargetName}, pvc)
	if err == nil {
		if pvc.Annotations[backupFileRestoreAnnotation] != fr.Name || pvc.Annotations[backupFileRestoreUIDAnnotation] != string(fr.UID) {
			return nil, restoreFailure(fmt.Sprintf("%s/%s exists; a restore never writes over a volume that exists. Choose another name",
				sp.TargetNamespace, sp.TargetName))
		}
		return pvc, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, fmt.Errorf("get the new claim: %w", err)
	}
	class := sp.StorageClassName
	if class == "" {
		class = entry.StorageClassName
	}
	if class == "" {
		return nil, restoreFailure("the storage class of the backed-up claim is not known; set spec.storageClassName")
	}
	annotations := map[string]string{backupFileRestoreAnnotation: fr.Name, backupFileRestoreUIDAnnotation: string(fr.UID)}
	if err := bc.createRestorePVC(ctx, sp.TargetNamespace, sp.TargetName, class, sizeGB, annotations); err != nil {
		return nil, err
	}
	klog.V(2).InfoS("Made the claim for a file restore", "restore", fr.Name, "pvc", klog.KRef(sp.TargetNamespace, sp.TargetName))
	return bc.getRestorePVC(ctx, sp.TargetNamespace, sp.TargetName)
}
