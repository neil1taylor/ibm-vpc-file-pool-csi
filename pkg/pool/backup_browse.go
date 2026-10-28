package pool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A BackupBrowse is one open session on one backup. Everything made for a
// session (the helper Job, its share objects and its Secret) is named and
// labelled with the UID of the BackupBrowse, never its name, so a session that
// is deleted and made again with the same name can never get the old session's
// Secret or helper, which still mounts the old volume. The controller starts a
// read-only helper pod for it (the browse image), keeps its token and TLS
// certificate in a Secret that the BackupBrowse owns, and ends the session when
// the helper stops, when the session is closed, or at the hard limit. The
// design is in docs/specs/2026-10-22-backup-file-restore.md.

const (
	browsePhasePending  = "Pending"
	browsePhaseStarting = "Starting"
	browsePhaseReady    = "Ready"
	browsePhaseClosed   = "Closed"
	browsePhaseFailed   = "Failed"

	// Defaults for a BrowseConfig field that is not set.
	defaultBrowseIdle            = 15 * time.Minute
	defaultBrowseMaxLife         = 2 * time.Hour
	defaultBrowseMaxSessions     = 5
	defaultBrowseMaxPerNamespace = 2
	defaultBrowseClosedKeep      = 10 * time.Minute
)

// BrowseConfig sets the limits of browse sessions.
type BrowseConfig struct {
	// Namespace is the driver namespace: where helper pods and the session
	// Secrets live.
	Namespace string
	// Idle and MaxLife end a session that is not used, or that has lived too long.
	Idle, MaxLife time.Duration
	// MaxSessions and MaxPerNamespace limit open sessions in the cluster, and
	// for the claims of one namespace.
	MaxSessions, MaxPerNamespace int
	// ClosedKeep is how long a closed or failed session stays visible.
	ClosedKeep time.Duration
	// COSNamespace is the driver namespace, where the COS and Key Protect Secrets
	// are. A session that reads a backup in COS runs its helper there, with a copy
	// of the session Secret. Empty turns browsing of COS backups off.
	COSNamespace string
	// COSStartTimeout is how long a session that reads COS may wait for its
	// helper to be ready. Zero means five minutes.
	COSStartTimeout time.Duration
	// PullSecrets are Secrets in PullSecretNamespace (the driver namespace) that
	// are copied into Namespace, so the helper pods can pull their image. The
	// browse namespace holds nothing else of the driver's.
	PullSecretNamespace string
	PullSecrets         []string
}

// SetBrowse turns on browse sessions. A field that is zero gets its default.
func (bc *BackupController) SetBrowse(jobs BrowseJobs, cfg BrowseConfig) {
	if cfg.Namespace == "" && jobs != nil {
		cfg.Namespace = jobs.Namespace()
	}
	if cfg.Idle <= 0 {
		cfg.Idle = defaultBrowseIdle
	}
	if cfg.MaxLife <= 0 {
		cfg.MaxLife = defaultBrowseMaxLife
	}
	if cfg.MaxSessions <= 0 {
		cfg.MaxSessions = defaultBrowseMaxSessions
	}
	if cfg.MaxPerNamespace <= 0 {
		cfg.MaxPerNamespace = defaultBrowseMaxPerNamespace
	}
	if cfg.ClosedKeep <= 0 {
		cfg.ClosedKeep = defaultBrowseClosedKeep
	}
	bc.browseJobs, bc.browse = jobs, cfg
}

// browseCounts are the open sessions, in the cluster and for each namespace of
// the backed-up claims.
type browseCounts struct {
	total int
	perNS map[string]int
}

func (c *browseCounts) add(ns string)     { c.total++; c.perNS[ns]++ }
func (c *browseCounts) release(ns string) { c.total--; c.perNS[ns]-- }

func browseOpen(phase string) bool { return phase == browsePhaseStarting || phase == browsePhaseReady }

func (bc *BackupController) reconcileBrowses(ctx context.Context) {
	if bc.browseJobs == nil {
		return
	}
	list := &v1alpha1.BackupBrowseList{}
	if err := bc.client.List(ctx, list); err != nil {
		klog.ErrorS(err, "Failed to list BackupBrowses")
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
	known := map[string]bool{}
	counts := &browseCounts{perNS: map[string]int{}}
	for i := range items {
		known[string(items[i].UID)] = true
		if browseOpen(items[i].Status.Phase) {
			counts.add(items[i].Spec.PVCNamespace)
		}
	}
	bc.copyPullSecrets(ctx)
	helpers, listed := bc.sweepBrowseOrphans(ctx, known)
	bc.sweepCOSSessionCopies(ctx, known)
	for i := range items {
		bb := &items[i]
		if bb.Status.Phase == browsePhaseClosed || bb.Status.Phase == browsePhaseFailed {
			bc.tidyEndedBrowse(ctx, bb)
			continue
		}
		bc.reconcileBrowse(ctx, bb, counts)
	}
	if listed {
		bc.sweepBrowseHolds(ctx, items, helpers)
	}
}

// sweepBrowseOrphans removes the helper Jobs and Secrets of sessions that no
// longer exist, for example after the BackupBrowse was deleted.
func (bc *BackupController) sweepBrowseOrphans(ctx context.Context, known map[string]bool) (helpers map[string]bool, listed bool) {
	names, err := bc.browseJobs.BrowseSessions(ctx)
	if err != nil {
		klog.ErrorS(err, "Failed to list browse helper Jobs")
	}
	listed = err == nil
	helpers = map[string]bool{}
	for _, n := range names {
		helpers[n] = true
		if !known[n] {
			if _, err := bc.browseJobs.StopBrowse(ctx, n); err != nil {
				klog.ErrorS(err, "Failed to stop the helper of a vanished browse session", "session", n)
			}
		}
	}
	secrets := &corev1.SecretList{}
	if err := bc.client.List(ctx, secrets, client.InNamespace(bc.browse.Namespace), client.HasLabels{browseLabel}); err != nil {
		klog.ErrorS(err, "Failed to list browse Secrets")
		return helpers, listed
	}
	for i := range secrets.Items {
		s := &secrets.Items[i]
		if !known[s.Labels[browseLabel]] {
			bc.deleteBrowseSecret(ctx, s.Name)
		}
	}
	return helpers, listed
}

func browseSecretName(session string) string { return "browse-" + session }

func (bc *BackupController) deleteBrowseSecret(ctx context.Context, name string) {
	s := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Namespace: bc.browse.Namespace, Name: name}}
	if err := bc.client.Delete(ctx, s); err != nil && !apierrors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to delete a browse Secret", "secret", name)
	}
}

// endBrowse records that a session is over and removes its helper and Secret.
func (bc *BackupController) endBrowse(ctx context.Context, bb *v1alpha1.BackupBrowse, counts *browseCounts, phase, reason, msg string) {
	if browseOpen(bb.Status.Phase) {
		counts.release(bb.Spec.PVCNamespace)
	}
	now := metav1.NewTime(bc.nowFunc())
	bb.Status.Phase, bb.Status.EndReason, bb.Status.Message = phase, reason, msg
	bb.Status.PodIP, bb.Status.Port, bb.Status.EndedTime = "", 0, &now
	if err := bc.client.Status().Update(ctx, bb); err != nil {
		klog.ErrorS(err, "Failed to update BackupBrowse status", "session", bb.Name)
		return
	}
	klog.V(2).InfoS("Browse session ended", "session", bb.Name, "phase", phase, "reason", reason)
	bc.tidyEndedBrowse(ctx, bb)
}

// tidyEndedBrowse makes sure the helper and the Secret of an ended session are
// gone, and removes the record after ClosedKeep.
func (bc *BackupController) tidyEndedBrowse(ctx context.Context, bb *v1alpha1.BackupBrowse) {
	done, err := bc.browseJobs.StopBrowse(ctx, string(bb.UID))
	if err != nil {
		klog.ErrorS(err, "Failed to stop a browse helper", "session", bb.Name)
		return
	}
	if !done {
		return
	}
	bc.deleteBrowseSecret(ctx, browseSecretName(string(bb.UID)))
	bc.deleteCOSSessionCopy(ctx, browseSecretName(string(bb.UID)))
	if bb.Status.EndedTime != nil && bc.nowFunc().Sub(bb.Status.EndedTime.Time) >= bc.browse.ClosedKeep {
		if err := bc.client.Delete(ctx, bb); err != nil && !apierrors.IsNotFound(err) {
			klog.ErrorS(err, "Failed to delete an ended BackupBrowse", "session", bb.Name)
		}
	}
}

func (bc *BackupController) setBrowseStatus(ctx context.Context, bb *v1alpha1.BackupBrowse, phase, msg string) {
	if bb.Status.Phase == phase && bb.Status.Message == msg {
		return
	}
	bb.Status.Phase, bb.Status.Message = phase, msg
	if err := bc.client.Status().Update(ctx, bb); err != nil {
		klog.ErrorS(err, "Failed to update BackupBrowse status", "session", bb.Name)
	}
}

func (bc *BackupController) reconcileBrowse(ctx context.Context, bb *v1alpha1.BackupBrowse, counts *browseCounts) {
	age := bc.nowFunc().Sub(bb.CreationTimestamp.Time)
	if bb.Spec.Close || age >= bc.browse.MaxLife {
		reason := "closed"
		if !bb.Spec.Close {
			reason = "max-life"
		}
		done, err := bc.browseJobs.StopBrowse(ctx, string(bb.UID))
		if err != nil || !done {
			bc.setBrowseStatus(ctx, bb, bb.Status.Phase, "closing the session")
			return
		}
		bc.endBrowse(ctx, bb, counts, browsePhaseClosed, reason, "")
		return
	}
	switch bb.Status.Phase {
	case "", browsePhasePending:
		bc.startBrowse(ctx, bb, counts)
	default:
		bc.watchBrowse(ctx, bb, counts)
	}
}

func (bc *BackupController) startBrowse(ctx context.Context, bb *v1alpha1.BackupBrowse, counts *browseCounts) {
	if counts.total >= bc.browse.MaxSessions {
		bc.setBrowseStatus(ctx, bb, browsePhasePending, fmt.Sprintf("waiting: %d sessions are open and the limit is %d", counts.total, bc.browse.MaxSessions))
		return
	}
	if n := counts.perNS[bb.Spec.PVCNamespace]; n >= bc.browse.MaxPerNamespace {
		bc.setBrowseStatus(ctx, bb, browsePhasePending, fmt.Sprintf("waiting: %d sessions are open for namespace %s and the limit is %d", n, bb.Spec.PVCNamespace, bc.browse.MaxPerNamespace))
		return
	}
	req, svName, err := bc.browseRequest(ctx, bb)
	if err != nil {
		var f restoreFailure
		if errors.As(err, &f) {
			bc.endBrowse(ctx, bb, counts, browsePhaseFailed, "", f.Error())
			return
		}
		bc.setBrowseStatus(ctx, bb, browsePhasePending, err.Error())
		return
	}
	if req.Guest {
		// The disk is held before anything else: from now on no new backup copy
		// starts. A copy that is running is waited for.
		if msg := bc.holdForGuest(ctx, bb, svName); msg != "" {
			bc.setBrowseStatus(ctx, bb, browsePhasePending, msg)
			return
		}
	}
	if err := bc.ensureBrowseSecret(ctx, bb); err != nil {
		bc.setBrowseStatus(ctx, bb, browsePhasePending, fmt.Sprintf("make the session Secret: %v", err))
		return
	}
	if req.COS != nil && bc.browse.COSNamespace != bc.browse.Namespace {
		if err := bc.ensureCOSSessionCopy(ctx, bb); err != nil {
			bc.setBrowseStatus(ctx, bb, browsePhasePending, fmt.Sprintf("make the session Secret for the helper: %v", err))
			return
		}
	}
	if err := bc.browseJobs.StartBrowse(ctx, req); err != nil {
		if errors.Is(err, errNoBrowseCOS) {
			bc.endBrowse(ctx, bb, counts, browsePhaseFailed, "", "browsing a backup in COS is not turned on in this installation")
			return
		}
		bc.setBrowseStatus(ctx, bb, browsePhasePending, fmt.Sprintf("start the helper: %v", err))
		return
	}
	bb.Status.SecretName = req.SecretName
	now := metav1.NewTime(bc.nowFunc())
	bb.Status.StartedTime = &now
	counts.add(bb.Spec.PVCNamespace)
	bc.setBrowseStatus(ctx, bb, browsePhaseStarting, "starting the helper pod")
}

// watchBrowse follows the helper pod of a session that has started.
func (bc *BackupController) watchBrowse(ctx context.Context, bb *v1alpha1.BackupBrowse, counts *browseCounts) {
	st, err := bc.browseJobs.BrowseState(ctx, string(bb.UID))
	if err != nil {
		klog.ErrorS(err, "Failed to read the state of a browse helper", "session", bb.Name)
		return
	}
	switch {
	case !st.Exists && bb.Status.Phase == browsePhaseReady:
		bc.endBrowse(ctx, bb, counts, browsePhaseFailed, "", "the helper pod is gone")
	case !st.Exists:
		req, _, rerr := bc.browseRequest(ctx, bb)
		if rerr == nil {
			rerr = bc.browseJobs.StartBrowse(ctx, req)
		}
		if rerr != nil {
			bc.setBrowseStatus(ctx, bb, browsePhaseStarting, fmt.Sprintf("start the helper: %v", rerr))
		}
	case st.Finished && st.Succeeded:
		reason := st.Reason
		if reason == "" {
			reason = "stopped"
		}
		bc.endBrowse(ctx, bb, counts, browsePhaseClosed, reason, "")
	case st.Finished:
		// The helper writes the reason it could not start to its termination log,
		// in words, with no key in them.
		bc.endBrowse(ctx, bb, counts, browsePhaseFailed, "", helperFailure(st.Reason))
	case st.Ready && st.PodIP != "":
		if bb.Status.Phase == browsePhaseReady && bb.Status.PodIP == st.PodIP {
			return
		}
		bb.Status.PodIP, bb.Status.Port = st.PodIP, browsePort
		bc.setBrowseStatus(ctx, bb, browsePhaseReady, "")
	case bb.Status.Phase == browsePhaseStarting && browseIsCOS(&bb.Spec) && bb.Status.StartedTime != nil &&
		bc.nowFunc().Sub(bb.Status.StartedTime.Time) >= bc.cosStartTimeout():
		bc.endBrowse(ctx, bb, counts, browsePhaseFailed, "", fmt.Sprintf(
			"the helper pod was not ready after %s. Check that the Secrets named by the export block exist in namespace %s and hold the keys access_key_id, secret_access_key, wrappedKey and apikey, and that the image can be pulled",
			bc.cosStartTimeout(), bc.browse.COSNamespace))
	}
}

// browseRequest works out what the helper needs, and the SubVolume of the disk
// when the session reads a VM disk. An error that is a restoreFailure ends the
// session; any other error leaves it waiting.
func (bc *BackupController) browseRequest(ctx context.Context, bb *v1alpha1.BackupBrowse) (BrowseJobRequest, string, error) {
	sp := bb.Spec
	if sp.Guest && browseIsCOS(&sp) {
		return BrowseJobRequest{}, "", restoreFailure("the files inside a VM disk can be browsed only for a backup in the snapshot pool, not yet for a backup in COS")
	}
	if sp.Guest && !bc.browseJobs.GuestEnabled() {
		return BrowseJobRequest{}, "", restoreFailure("browsing the files inside a VM disk is not turned on in this installation (backupBrowse.guest.image is not set)")
	}
	if browseIsCOS(&sp) {
		req, err := bc.browseCOSRequest(ctx, bb)
		return req, "", err
	}
	policy := &v1alpha1.BackupPolicy{}
	if err := bc.client.Get(ctx, types.NamespacedName{Name: sp.Policy}, policy); err != nil {
		if apierrors.IsNotFound(err) {
			return BrowseJobRequest{}, "", restoreFailure(fmt.Sprintf("the backup policy %q does not exist", sp.Policy))
		}
		return BrowseJobRequest{}, "", fmt.Errorf("get the backup policy: %w", err)
	}
	var entry *v1alpha1.BackupVolumeStatus
	for i := range policy.Status.Volumes {
		v := &policy.Status.Volumes[i]
		if v.PVCNamespace == sp.PVCNamespace && v.PVCName == sp.PVCName {
			entry = v
		}
	}
	if entry == nil || entry.SubPath == "" {
		return BrowseJobRequest{}, "", restoreFailure(fmt.Sprintf("policy %s has no backup of %s/%s", sp.Policy, sp.PVCNamespace, sp.PVCName))
	}
	if sp.Guest && entry.VMName == "" {
		return BrowseJobRequest{}, "", restoreFailure(fmt.Sprintf("%s/%s is not a VM disk, so it has no guest file systems to browse", sp.PVCNamespace, sp.PVCName))
	}
	svs := &v1alpha1.SubVolumeList{}
	if err := bc.client.List(ctx, svs); err != nil {
		return BrowseJobRequest{}, "", fmt.Errorf("list SubVolumes: %w", err)
	}
	share, err := bc.backupShareOf(ctx, policy, svs.Items, entry)
	if err != nil {
		return BrowseJobRequest{}, "", err
	}
	policyBase := backupBasePath + "/" + policy.Name
	req := BrowseJobRequest{
		ID:         string(bb.UID),
		Share:      shareMountOf(share),
		LatestPath: policyBase + entry.SubPath,
		SecretName: browseSecretName(string(bb.UID)),
		Idle:       bc.browse.Idle,
		MaxLife:    bc.browse.MaxLife,
		Owner:      browseOwner(bb),
	}
	switch {
	case sp.Guest:
		// A VM disk keeps its earlier moments as undo files next to the disk,
		// not as a moments folder: the helper rebuilds the old view from them.
		req.Guest = true
		if sp.Moment != nil {
			req.At = sp.Moment.Time
		}
		return req, entry.SubVolumeName, nil
	case entry.RestorePoints > 0:
		req.MomentsPath = policyBase + "/.moments" + entry.SubPath
		if sp.Moment != nil {
			req.At = sp.Moment.Time
		}
	}
	return req, "", nil
}

func browseOwner(bb *v1alpha1.BackupBrowse) metav1.OwnerReference {
	return metav1.OwnerReference{APIVersion: v1alpha1.GroupVersion.String(), Kind: "BackupBrowse", Name: bb.Name, UID: bb.UID}
}

// ensureBrowseSecret makes the Secret with the session token, certificate and
// key. It leaves an existing one alone, so a restart of the controller does not
// change a running session's token. The Secret is owned by the BackupBrowse.
func (bc *BackupController) ensureBrowseSecret(ctx context.Context, bb *v1alpha1.BackupBrowse) error {
	name := browseSecretName(string(bb.UID))
	err := bc.client.Get(ctx, types.NamespacedName{Namespace: bc.browse.Namespace, Name: name}, &corev1.Secret{})
	if err == nil {
		return nil
	}
	if !apierrors.IsNotFound(err) {
		return err
	}
	raw := make([]byte, 32)
	if _, rerr := rand.Read(raw); rerr != nil {
		return rerr
	}
	cert, key, err := browse.NewSessionCert(bc.browse.MaxLife + 30*time.Minute)
	if err != nil {
		return err
	}
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: bc.browse.Namespace, Name: name,
			Labels:          map[string]string{browseLabel: string(bb.UID)},
			OwnerReferences: []metav1.OwnerReference{browseOwner(bb)},
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{"token": []byte(hex.EncodeToString(raw)), "tls.crt": cert, "tls.key": key},
	}
	if err := bc.client.Create(ctx, s); err != nil && !apierrors.IsAlreadyExists(err) {
		return err
	}
	return nil
}

// helperFailure is the status message of a helper that stopped with an error.
// The reason is the termination message the helper wrote; it is limited in size
// and never holds a key.
func helperFailure(reason string) string {
	if reason == "" {
		return "the helper pod failed"
	}
	if len(reason) > 600 {
		reason = reason[:600] + "..."
	}
	return "the helper pod failed: " + reason
}

// copyPullSecrets keeps copies of the image pull Secrets in the browse namespace.
// A Secret that does not exist is skipped.
func (bc *BackupController) copyPullSecrets(ctx context.Context) {
	if bc.browse.PullSecretNamespace == "" || bc.browse.PullSecretNamespace == bc.browse.Namespace {
		return
	}
	for _, name := range bc.browse.PullSecrets {
		src := &corev1.Secret{}
		if err := bc.client.Get(ctx, types.NamespacedName{Namespace: bc.browse.PullSecretNamespace, Name: name}, src); err != nil {
			if !apierrors.IsNotFound(err) {
				klog.ErrorS(err, "Failed to read a pull secret", "secret", name)
			}
			continue
		}
		dst := &corev1.Secret{}
		err := bc.client.Get(ctx, types.NamespacedName{Namespace: bc.browse.Namespace, Name: name}, dst)
		switch {
		case apierrors.IsNotFound(err):
			dst = &corev1.Secret{
				ObjectMeta: metav1.ObjectMeta{Namespace: bc.browse.Namespace, Name: name, Labels: map[string]string{browsePullSecretLabel: "true"}},
				Type:       src.Type, Data: src.Data,
			}
			if cerr := bc.client.Create(ctx, dst); cerr != nil {
				klog.ErrorS(cerr, "Failed to copy a pull secret", "secret", name)
			}
		case err != nil:
			klog.ErrorS(err, "Failed to read the copy of a pull secret", "secret", name)
		case !equalSecretData(dst.Data, src.Data):
			dst.Data = src.Data
			if err := bc.client.Update(ctx, dst); err != nil {
				klog.ErrorS(err, "Failed to update the copy of a pull secret", "secret", name)
			}
		}
	}
}

// browsePullSecretLabel marks a copy of a pull secret in the browse namespace.
const browsePullSecretLabel = "storage.ibmcloud.io/browse-pull-secret"

func equalSecretData(a, b map[string][]byte) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if string(b[k]) != string(v) {
			return false
		}
	}
	return true
}

// holdForGuest holds the disk of a session that reads a VM disk, and says why
// the session must wait, or returns "" if it may start. The hold comes first:
// once it is on the SubVolume, the replication controller starts no new copy of
// the disk (or of any disk of the same running VM). Only then is a copy that is
// already running waited for. The other order would leave a gap in which a copy
// could start between the check and the hold.
func (bc *BackupController) holdForGuest(ctx context.Context, bb *v1alpha1.BackupBrowse, svName string) string {
	sv := &v1alpha1.SubVolume{}
	if err := bc.client.Get(ctx, types.NamespacedName{Name: svName}, sv); err != nil {
		if apierrors.IsNotFound(err) {
			return "" // the volume is gone: nothing copies its disk any more
		}
		return fmt.Sprintf("waiting: cannot read the volume to hold it: %v", err)
	}
	session := string(bb.UID)
	if value := addBrowseHold(sv.Annotations[browseHoldAnnotation], session, bc.nowFunc()); value != sv.Annotations[browseHoldAnnotation] {
		patch := client.MergeFrom(sv.DeepCopy())
		if sv.Annotations == nil {
			sv.Annotations = map[string]string{}
		}
		sv.Annotations[browseHoldAnnotation] = value
		if err := bc.client.Patch(ctx, sv, patch); err != nil {
			klog.ErrorS(err, "Failed to hold a SubVolume for a browse session", "session", bb.Name, "volume", svName)
			return fmt.Sprintf("waiting: cannot hold the disk of the volume: %v", err)
		}
	}
	busy, known := bc.volumesBeingCopied(ctx, bb.Spec.Policy)
	if !known || busy[svName] {
		return "waiting: a backup copy of this disk is running; the session starts when it has ended"
	}
	return ""
}

// sweepBrowseHolds removes the holds that no session needs: of a session that no
// longer exists, of one that has ended and has no helper any more, and any that
// are older than the limit. A hold stays while the helper may still read the
// disk. helpers holds the sessions that have a helper Job.
func (bc *BackupController) sweepBrowseHolds(ctx context.Context, sessions []v1alpha1.BackupBrowse, helpers map[string]bool) {
	phase := map[string]string{}
	for i := range sessions {
		phase[string(sessions[i].UID)] = sessions[i].Status.Phase
	}
	svs := &v1alpha1.SubVolumeList{}
	if err := bc.client.List(ctx, svs); err != nil {
		klog.ErrorS(err, "Failed to list SubVolumes to release browse holds")
		return
	}
	now := bc.nowFunc()
	for i := range svs.Items {
		sv := &svs.Items[i]
		value, found := sv.Annotations[browseHoldAnnotation]
		if !found {
			continue
		}
		var keep []browseHold
		for _, h := range parseBrowseHolds(value) {
			ph, exists := phase[h.session]
			ended := ph == browsePhaseClosed || ph == browsePhaseFailed
			live := (exists && !ended) || helpers[h.session]
			if live && now.Sub(h.since) < browseHoldLimit {
				keep = append(keep, h)
			}
		}
		newValue := formatBrowseHolds(keep)
		if newValue == value {
			continue
		}
		patch := client.MergeFrom(sv.DeepCopy())
		if newValue == "" {
			delete(sv.Annotations, browseHoldAnnotation)
		} else {
			sv.Annotations[browseHoldAnnotation] = newValue
		}
		if err := bc.client.Patch(ctx, sv, patch); err != nil {
			klog.ErrorS(err, "Failed to release a browse hold", "volume", sv.Name)
		}
	}
}
