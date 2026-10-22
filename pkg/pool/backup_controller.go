package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
)

// A BackupPolicy backs up the PVCs it selects, and the VMs whose disks it
// selects, into a pool that holds only backups. The controller labels the
// SubVolumes it selects and makes one ReplicationPolicy for each source pool,
// aimed at the snapshot pool. Replication does the copying: QEMU for running
// VM disks at one moment, changed blocks only, with restore points. The design
// is in docs/specs/2026-10-21-backup-to-snapshot-pool.md.

const (
	// backupPolicyLabel is on a SubVolume that a BackupPolicy backs up, and on
	// the ReplicationPolicies the controller makes. Its value is the policy name.
	backupPolicyLabel = "storage.ibmcloud.io/backup-policy"

	// backupBasePath is the folder on the snapshot pool's share that holds the
	// backups. Each policy has its own folder below it.
	backupBasePath = "/backup"

	// DefaultBackupInterval is how often the controller looks at all policies.
	DefaultBackupInterval = 30 * time.Second

	backupMaxRetries       = 3
	backupMaxParallelSyncs = 2

	backupPhaseActive    = "Active"
	backupPhaseSuspended = "Suspended"
	backupPhaseError     = "Error"

	// consistencyFileCopy is reported for a volume that is not a VM disk: the
	// backup is a copy of files and is as consistent as the files are.
	consistencyFileCopy = "FileCopy"
)

var vmListGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineList"}

// BackupController keeps the objects that BackupPolicies need.
type BackupController struct {
	client   client.Client
	interval time.Duration
	nowFunc  func() time.Time
	// shareData runs Jobs on shares, for restores.
	shareData ShareData
	// reported holds the policies that have metric series.
	reported map[string]bool
	// usage holds what the usage Jobs found, by share ID.
	usage map[string]*poolUsage
	// exports holds what the controller remembers about the COS exports of each
	// policy; exportReported holds the policies that have export metric series.
	exports        map[string]*exportState
	exportReported map[string]bool
	// browseJobs and browse run browse sessions; nil browseJobs means off.
	browseJobs BrowseJobs
	browse     BrowseConfig
}

// NewBackupController returns a controller that reads and writes with c. It
// must read without a cache of pods or PVCs for every namespace, as the
// replication controller does.
func NewBackupController(c client.Client) *BackupController {
	return &BackupController{client: c, interval: DefaultBackupInterval, nowFunc: time.Now, reported: map[string]bool{}, usage: map[string]*poolUsage{},
		exports: map[string]*exportState{}, exportReported: map[string]bool{}}
}

// SetNowFunc replaces the clock (for tests).
func (bc *BackupController) SetNowFunc(f func() time.Time) { bc.nowFunc = f }

// Run looks at all BackupPolicies every interval until ctx ends.
func (bc *BackupController) Run(ctx context.Context) {
	t := time.NewTicker(bc.interval)
	defer t.Stop()
	for {
		bc.ReconcileAll(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ReconcileAll makes every BackupPolicy right, and tidies what a deleted
// policy left behind.
func (bc *BackupController) ReconcileAll(ctx context.Context) {
	list := &v1alpha1.BackupPolicyList{}
	if err := bc.client.List(ctx, list); err != nil {
		klog.ErrorS(err, "Failed to list BackupPolicies")
		return
	}
	known := map[string]bool{}
	for i := range list.Items {
		known[list.Items[i].Name] = true
	}
	bc.tidyDeleted(ctx, known)
	for name := range bc.reported {
		if !known[name] {
			metrics.BackupVolumes.DeleteLabelValues(name)
			metrics.BackupStalestAgeSeconds.DeleteLabelValues(name)
			metrics.BackupOldestAgeSeconds.DeleteLabelValues(name)
			delete(bc.reported, name)
		}
	}
	for name := range bc.exportReported {
		if !known[name] {
			bc.deleteExportMetrics(name)
		}
	}
	for name := range bc.exports {
		if !known[name] {
			delete(bc.exports, name)
		}
	}
	sort.Slice(list.Items, func(i, j int) bool {
		a, b := list.Items[i].CreationTimestamp, list.Items[j].CreationTimestamp
		if !a.Equal(&b) {
			return a.Before(&b)
		}
		return list.Items[i].Name < list.Items[j].Name
	})
	for i := range list.Items {
		bc.reconcile(ctx, &list.Items[i])
	}
	bc.reconcileRestores(ctx)
	bc.reconcileBrowses(ctx)
}

// tidyDeleted removes the labels and the ReplicationPolicies of policies that
// no longer exist. Kubernetes removes owned objects itself; this also covers a
// cluster where it has not yet.
func (bc *BackupController) tidyDeleted(ctx context.Context, known map[string]bool) {
	rps := &v1alpha1.ReplicationPolicyList{}
	if err := bc.client.List(ctx, rps, client.HasLabels{backupPolicyLabel}); err != nil {
		klog.ErrorS(err, "Failed to list backup ReplicationPolicies")
	} else {
		for i := range rps.Items {
			if bc.policyIsGone(ctx, rps.Items[i].Labels[backupPolicyLabel], known) {
				bc.deleteReplicationPolicy(ctx, &rps.Items[i])
			}
		}
	}
	svs := &v1alpha1.SubVolumeList{}
	if err := bc.client.List(ctx, svs, client.HasLabels{backupPolicyLabel}); err != nil {
		klog.ErrorS(err, "Failed to list backed-up SubVolumes")
		return
	}
	for i := range svs.Items {
		if bc.policyIsGone(ctx, svs.Items[i].Labels[backupPolicyLabel], known) {
			bc.setSubVolumeLabel(ctx, &svs.Items[i], "", "")
		}
	}
}

// policyIsGone is true only when the API says the policy does not exist. A
// list that misses a policy (a cache that has not filled) is not enough.
func (bc *BackupController) policyIsGone(ctx context.Context, name string, known map[string]bool) bool {
	if known[name] {
		return false
	}
	err := bc.client.Get(ctx, types.NamespacedName{Name: name}, &v1alpha1.BackupPolicy{})
	return apierrors.IsNotFound(err)
}

func (bc *BackupController) deleteReplicationPolicy(ctx context.Context, rp *v1alpha1.ReplicationPolicy) {
	if err := bc.client.Delete(ctx, rp); err != nil && !apierrors.IsNotFound(err) {
		klog.ErrorS(err, "Failed to delete a backup ReplicationPolicy", "policy", rp.Name)
	}
}

// setSubVolumeLabel sets the backup label of a SubVolume, or removes it when
// policy is empty.
func (bc *BackupController) setSubVolumeLabel(ctx context.Context, sv *v1alpha1.SubVolume, policy, shareID string) {
	if sv.Labels[backupPolicyLabel] == policy && sv.Labels[backupShareLabel] == shareID {
		return
	}
	patch := client.MergeFrom(sv.DeepCopy())
	delete(sv.Labels, backupPolicyLabel)
	delete(sv.Labels, backupShareLabel)
	if policy != "" {
		if sv.Labels == nil {
			sv.Labels = map[string]string{}
		}
		sv.Labels[backupPolicyLabel] = policy
		if shareID != "" {
			sv.Labels[backupShareLabel] = shareID
		}
	}
	if err := bc.client.Patch(ctx, sv, patch); err != nil {
		klog.ErrorS(err, "Failed to label a SubVolume for backup", "subVolume", sv.Name, "policy", policy)
	}
}

// backupTarget is a volume a policy backs up.
type backupTarget struct {
	sv           *v1alpha1.SubVolume
	vmName       string
	storageClass string
}

func claimKey(ns, name string) string { return ns + "/" + name }

func (bc *BackupController) reconcile(ctx context.Context, bp *v1alpha1.BackupPolicy) {
	status := v1alpha1.BackupPolicyStatus{}
	defer func() {
		bc.saveStatus(ctx, bp, status)
		bc.recordMetrics(bp, status)
		bc.recordExportMetrics(bp, status)
	}()

	svs := &v1alpha1.SubVolumeList{}
	if err := bc.client.List(ctx, svs); err != nil {
		status = bc.keepStatus(bp, fmt.Sprintf("list SubVolumes: %v", err))
		return
	}

	shares, err := bc.snapshotShares(ctx, bp, svs.Items)
	if err != nil {
		// Do not copy into a pool that may be wrong. Pause what exists; do not
		// delete it, so a short outage does not throw away the change lists.
		bc.pauseOwned(ctx, bp, true)
		// Keep the list of volumes: a restore needs it when the pool is back.
		status = bc.keepStatus(bp, err.Error())
		status.Phase = backupPhaseError
		return
	}

	// A share above the limit gets no new backups. Its copies are paused like
	// a suspended policy's, and the status says why. The other shares go on.
	full, usedPercent := map[string]bool{}, map[string]int{}
	for _, sh := range shares {
		if shareReady(sh) {
			usedPercent[sh.ShareID], full[sh.ShareID] = bc.shareFull(ctx, bp.Spec.SnapshotPool, sh)
		}
	}

	targets, conflicts, err := bc.resolveTargets(ctx, bp, svs.Items)
	if err != nil {
		status = bc.keepStatus(bp, err.Error())
		return
	}

	// Each volume has one share for good: its backups stay where they are.
	placement, waiting := placeVolumes(bp.Status.Volumes, targets, shares, full, bc.shareLoad(ctx, bp.Spec.SnapshotPool, shares))

	// Labels: the selected volumes get this policy's name and their share, others lose them.
	chosen := map[string]bool{}
	for _, t := range targets {
		chosen[t.sv.Name] = true
		bc.setSubVolumeLabel(ctx, t.sv, bp.Name, placement[t.sv.Name])
	}
	for i := range svs.Items {
		sv := &svs.Items[i]
		if sv.Labels[backupPolicyLabel] == bp.Name && !chosen[sv.Name] {
			bc.setSubVolumeLabel(ctx, sv, "", "")
		}
	}

	// One ReplicationPolicy for each source pool and share that has volumes.
	type group struct{ pool, shareID string }
	groups := map[group]bool{}
	for _, t := range targets {
		if id := placement[t.sv.Name]; id != "" {
			groups[group{t.sv.Spec.PoolName, id}] = true
		}
	}
	var names []string
	desired := map[string]bool{}
	for g := range groups {
		sh, found := shareByID(shares, g.shareID)
		if !found || !shareReady(sh) {
			// The share of these volumes is gone or not ready: nothing to copy
			// to, and their entries stay for a restore.
			continue
		}
		name := backupReplicationName(bp.Name, g.pool, g.shareID, shares[0].ShareID)
		desired[name] = true
		names = append(names, name)
		if err := bc.ensureReplicationPolicy(ctx, bp, g.pool, name, sh, bp.Spec.Suspend || full[g.shareID]); err != nil {
			status = bc.keepStatus(bp, fmt.Sprintf("make the replication policy for pool %s: %v", g.pool, err))
			return
		}
	}
	sort.Strings(names)
	owned := &v1alpha1.ReplicationPolicyList{}
	if err := bc.client.List(ctx, owned, client.MatchingLabels{backupPolicyLabel: bp.Name}); err != nil {
		status = bc.keepStatus(bp, fmt.Sprintf("list the replication policies: %v", err))
		return
	}
	for i := range owned.Items {
		if !desired[owned.Items[i].Name] {
			bc.deleteReplicationPolicy(ctx, &owned.Items[i])
		}
	}

	status = bc.buildStatus(ctx, bp, targets, names, conflicts, placement, waiting)
	if msg := bc.shareProblems(bp, status, shares, full, usedPercent, waiting); msg != "" {
		status.Phase, status.Message = backupPhaseError, msg
	}
	bc.reconcileExport(ctx, bp, &status, shares)
}

// shareProblems says what is wrong with the shares of the snapshot pool, for
// the status: a share that holds volumes of the policy and is too full, a
// volume whose share is gone, and volumes that wait for room.
func (bc *BackupController) shareProblems(bp *v1alpha1.BackupPolicy, st v1alpha1.BackupPolicyStatus, shares []v1alpha1.PoolShareStatus,
	full map[string]bool, usedPercent map[string]int, waiting map[string]string) string {
	var msgs []string
	seen := map[string]bool{}
	for _, v := range st.Volumes {
		id := v.ShareID
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		if _, found := shareByID(shares, id); !found {
			msgs = append(msgs, fmt.Sprintf("share %s of the snapshot pool %q, which holds backups, is not in the pool", id, bp.Spec.SnapshotPool))
		} else if full[id] {
			msgs = append(msgs, snapshotPoolFullMessage(bp.Spec.SnapshotPool, id, usedPercent[id]))
		}
	}
	if n := len(waiting); n > 0 {
		msgs = append(msgs, fmt.Sprintf("%d new volume(s) wait: %s", n, noRoomMessage))
	}
	sort.Strings(msgs)
	return strings.Join(msgs, "; ")
}

// keepStatus keeps the last status and adds why this pass did not finish.
func (bc *BackupController) keepStatus(bp *v1alpha1.BackupPolicy, msg string) v1alpha1.BackupPolicyStatus {
	st := *bp.Status.DeepCopy()
	st.Message = msg
	return st
}

func (bc *BackupController) saveStatus(ctx context.Context, bp *v1alpha1.BackupPolicy, st v1alpha1.BackupPolicyStatus) {
	if equalBackupStatus(bp.Status, st) {
		return
	}
	bp.Status = st
	if err := bc.client.Status().Update(ctx, bp); err != nil {
		klog.ErrorS(err, "Failed to update BackupPolicy status", "policy", bp.Name)
	}
}

func equalBackupStatus(a, b v1alpha1.BackupPolicyStatus) bool {
	return a.Phase == b.Phase && a.Message == b.Message && a.VolumeCount == b.VolumeCount &&
		strings.Join(a.ReplicationPolicies, ",") == strings.Join(b.ReplicationPolicies, ",") &&
		equality.Semantic.DeepEqual(a.Volumes, b.Volumes)
}

// resolveTargets finds the volumes the policy backs up: the claims that match
// the selector, and all disks of every VM that uses one of them. Volumes that
// another policy has are reported as conflicts.
func (bc *BackupController) resolveTargets(ctx context.Context, bp *v1alpha1.BackupPolicy, svs []v1alpha1.SubVolume) ([]backupTarget, []string, error) {
	sel, err := metav1.LabelSelectorAsSelector(bp.Spec.Selector)
	if err != nil {
		return nil, nil, fmt.Errorf("the selector is not valid: %w", err)
	}
	selected := map[string]bool{}
	classes := map[string]string{}
	list := func(ns string) error {
		pvcs := &corev1.PersistentVolumeClaimList{}
		opts := []client.ListOption{client.MatchingLabelsSelector{Selector: sel}}
		if ns != "" {
			opts = append(opts, client.InNamespace(ns))
		}
		if err := bc.client.List(ctx, pvcs, opts...); err != nil {
			return fmt.Errorf("list claims: %w", err)
		}
		for i := range pvcs.Items {
			if pvcs.Items[i].DeletionTimestamp == nil {
				key := claimKey(pvcs.Items[i].Namespace, pvcs.Items[i].Name)
				selected[key] = true
				if sc := pvcs.Items[i].Spec.StorageClassName; sc != nil {
					classes[key] = *sc
				}
			}
		}
		return nil
	}
	if len(bp.Spec.Namespaces) == 0 {
		if err := list(""); err != nil {
			return nil, nil, err
		}
	}
	for _, ns := range bp.Spec.Namespaces {
		if err := list(ns); err != nil {
			return nil, nil, err
		}
	}

	// A VM is backed up whole: its other disks join.
	vmOf := map[string]string{}
	namespaces := map[string]bool{}
	for k := range selected {
		namespaces[strings.SplitN(k, "/", 2)[0]] = true
	}
	for ns := range namespaces {
		vms := &unstructured.UnstructuredList{}
		vms.SetGroupVersionKind(vmListGVK)
		if err := bc.client.List(ctx, vms, client.InNamespace(ns)); err != nil {
			if meta.IsNoMatchError(err) || apierrors.IsNotFound(err) {
				continue // KubeVirt is not installed
			}
			return nil, nil, fmt.Errorf("list VMs in %s: %w", ns, err)
		}
		for _, vm := range vms.Items {
			claims := vmClaims(&vm)
			hit := false
			for _, c := range claims {
				if selected[claimKey(ns, c)] {
					hit = true
				}
			}
			if !hit {
				continue
			}
			for _, c := range claims {
				selected[claimKey(ns, c)] = true
				vmOf[claimKey(ns, c)] = vm.GetName()
			}
		}
	}

	var targets []backupTarget
	var conflicts []string
	for i := range svs {
		sv := &svs[i]
		key := claimKey(sv.Spec.PVCNamespace, sv.Spec.PVCName)
		if !selected[key] || sv.DeletionTimestamp != nil {
			continue
		}
		if owner := sv.Labels[backupPolicyLabel]; owner != "" && owner != bp.Name {
			conflicts = append(conflicts, fmt.Sprintf("%s is already backed up by policy %s", key, owner))
			continue
		}
		targets = append(targets, backupTarget{sv: sv, vmName: vmOf[key], storageClass: classes[key]})
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].sv.Name < targets[j].sv.Name })
	sort.Strings(conflicts)
	return targets, conflicts, nil
}

// vmClaims returns the claim names that a VirtualMachine's volumes use: a
// PersistentVolumeClaim volume by its claim, and a DataVolume volume by the
// name of the DataVolume, which is also the name of its claim.
func vmClaims(vm *unstructured.Unstructured) []string {
	vols, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	var out []string
	for _, v := range vols {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if c, found, _ := unstructured.NestedString(m, "persistentVolumeClaim", "claimName"); found && c != "" {
			out = append(out, c)
		} else if d, found, _ := unstructured.NestedString(m, "dataVolume", "name"); found && d != "" {
			out = append(out, d)
		}
	}
	return out
}

// backupReplicationName names the ReplicationPolicy of a backup policy for one
// source pool and one share of the snapshot pool. The policy for the first
// share of the pool keeps the name it had when a snapshot pool had one share,
// so an upgrade keeps its change lists.
// The short hash keeps "a-b" + "c" apart from "a" + "b-c", and keeps long names apart.
func backupReplicationName(policy, pool, shareID, firstShareID string) string {
	key := policy + "\x00" + pool
	if shareID != firstShareID {
		key += "\x00" + shareID
	}
	sum := sha256.Sum256([]byte(key))
	suffix := "-" + hex.EncodeToString(sum[:3])
	// The name is a label value on the temporary PV of a copy: 63 characters at
	// most. A long name loses the end of its readable part; the hash keeps it unique.
	readable := strings.TrimRight(truncate("backup-"+policy+"-"+pool, 63-len(suffix)), "-")
	return readable + suffix
}

// backupKeepHours is the keep time rounded up to whole hours.
func backupKeepHours(bp *v1alpha1.BackupPolicy) int32 {
	d, err := v1alpha1.ParseKeepFor(bp.Spec.KeepFor)
	if err != nil || d <= 0 {
		d = v1alpha1.DefaultBackupKeepFor
	}
	return int32(math.Ceil(d.Hours())) // #nosec G115 -- a keep time is far below 2^31 hours
}

// desiredReplicationSpec is what the ReplicationPolicy for one source pool and
// one share of the snapshot pool must say.
func desiredReplicationSpec(bp *v1alpha1.BackupPolicy, pool string, share v1alpha1.PoolShareStatus) v1alpha1.ReplicationPolicySpec {
	speed := int32(defaultVMCopySpeedLimitMBps)
	if bp.Spec.VMCopySpeedLimitMBps != nil {
		speed = *bp.Spec.VMCopySpeedLimitMBps
	}
	keep := backupKeepHours(bp)
	export := share.ExportPath
	if export == "" {
		export = "/"
	}
	return v1alpha1.ReplicationPolicySpec{
		SourcePoolName:        pool,
		DestinationNFSServer:  share.MountTargetIP,
		DestinationExportPath: export,
		DestinationBasePath:   backupBasePath + "/" + bp.Name,
		Schedule:              bp.Spec.Schedule,
		SubVolumeSelector:     &metav1.LabelSelector{MatchLabels: map[string]string{backupPolicyLabel: bp.Name, backupShareLabel: share.ShareID}},
		MaxRetries:            backupMaxRetries,
		MaxParallelSyncs:      backupMaxParallelSyncs,
		VMCopySpeedLimitMBps:  &speed,
		RestorePointKeepHours: &keep,
		MomentKeepHours:       &keep,
	}
}

func sameReplicationSpec(a, b v1alpha1.ReplicationPolicySpec) bool {
	return a.SourcePoolName == b.SourcePoolName && a.DestinationNFSServer == b.DestinationNFSServer &&
		a.DestinationExportPath == b.DestinationExportPath && a.DestinationBasePath == b.DestinationBasePath &&
		a.Schedule == b.Schedule && a.MaxRetries == b.MaxRetries && a.MaxParallelSyncs == b.MaxParallelSyncs &&
		ptrEq(a.VMCopySpeedLimitMBps, b.VMCopySpeedLimitMBps) && ptrEq(a.RestorePointKeepHours, b.RestorePointKeepHours) &&
		ptrEq(a.MomentKeepHours, b.MomentKeepHours) &&
		equality.Semantic.DeepEqual(a.SubVolumeSelector, b.SubVolumeSelector)
}

func ptrEq(a, b *int32) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// ensureReplicationPolicy makes the ReplicationPolicy for one source pool and
// one share, or brings it in line with the BackupPolicy.
func (bc *BackupController) ensureReplicationPolicy(ctx context.Context, bp *v1alpha1.BackupPolicy, pool, name string, share v1alpha1.PoolShareStatus, paused bool) error {
	want := desiredReplicationSpec(bp, pool, share)
	rp := &v1alpha1.ReplicationPolicy{}
	err := bc.client.Get(ctx, types.NamespacedName{Name: name}, rp)
	if apierrors.IsNotFound(err) {
		rp = &v1alpha1.ReplicationPolicy{ObjectMeta: metav1.ObjectMeta{
			Name:            name,
			Labels:          map[string]string{backupPolicyLabel: bp.Name},
			OwnerReferences: []metav1.OwnerReference{ownerOf(bp)},
		}, Spec: want}
		setPaused(rp, paused)
		if createErr := bc.client.Create(ctx, rp); createErr != nil {
			return createErr
		}
		klog.V(2).InfoS("Made the replication policy of a backup policy", "backupPolicy", bp.Name, "policy", name, "sourcePool", pool)
		return nil
	}
	if err != nil {
		return err
	}
	if sameReplicationSpec(rp.Spec, want) && (rp.Annotations[pausedAnnotation] == "true") == paused {
		return nil
	}
	rp.Spec.SourcePoolName, rp.Spec.DestinationNFSServer, rp.Spec.DestinationExportPath = want.SourcePoolName, want.DestinationNFSServer, want.DestinationExportPath
	rp.Spec.DestinationBasePath, rp.Spec.Schedule, rp.Spec.SubVolumeSelector = want.DestinationBasePath, want.Schedule, want.SubVolumeSelector
	rp.Spec.MaxRetries, rp.Spec.MaxParallelSyncs = want.MaxRetries, want.MaxParallelSyncs
	rp.Spec.VMCopySpeedLimitMBps, rp.Spec.RestorePointKeepHours = want.VMCopySpeedLimitMBps, want.RestorePointKeepHours
	rp.Spec.MomentKeepHours = want.MomentKeepHours
	setPaused(rp, paused)
	return bc.client.Update(ctx, rp)
}

func setPaused(rp *v1alpha1.ReplicationPolicy, paused bool) {
	if paused {
		if rp.Annotations == nil {
			rp.Annotations = map[string]string{}
		}
		rp.Annotations[pausedAnnotation] = "true"
		return
	}
	delete(rp.Annotations, pausedAnnotation)
}

// pauseOwned pauses or resumes the ReplicationPolicies of a policy.
func (bc *BackupController) pauseOwned(ctx context.Context, bp *v1alpha1.BackupPolicy, paused bool) {
	owned := &v1alpha1.ReplicationPolicyList{}
	if err := bc.client.List(ctx, owned, client.MatchingLabels{backupPolicyLabel: bp.Name}); err != nil {
		klog.ErrorS(err, "Failed to list the replication policies of a backup policy", "policy", bp.Name)
		return
	}
	for i := range owned.Items {
		rp := &owned.Items[i]
		if (rp.Annotations[pausedAnnotation] == "true") == paused {
			continue
		}
		setPaused(rp, paused)
		if err := bc.client.Update(ctx, rp); err != nil {
			klog.ErrorS(err, "Failed to pause or resume a backup replication policy", "policy", rp.Name)
		}
	}
}

func ownerOf(bp *v1alpha1.BackupPolicy) metav1.OwnerReference {
	yes := true
	return metav1.OwnerReference{
		APIVersion: v1alpha1.GroupVersion.String(), Kind: "BackupPolicy", Name: bp.Name, UID: bp.UID,
		Controller: &yes, BlockOwnerDeletion: &yes,
	}
}

// buildStatus makes the policy's status from the targets and from what the
// owned ReplicationPolicies report.
func (bc *BackupController) buildStatus(ctx context.Context, bp *v1alpha1.BackupPolicy, targets []backupTarget, rpNames, conflicts []string, placement, waiting map[string]string) v1alpha1.BackupPolicyStatus {
	byName := map[string]v1alpha1.SubVolumeReplicationStatus{}
	for _, n := range rpNames {
		rp := &v1alpha1.ReplicationPolicy{}
		if err := bc.client.Get(ctx, types.NamespacedName{Name: n}, rp); err != nil {
			continue
		}
		for _, s := range rp.Status.SubVolumeStatuses {
			byName[s.SubVolumeName] = s
		}
	}
	st := v1alpha1.BackupPolicyStatus{Phase: backupPhaseActive, VolumeCount: int32(len(targets)), ReplicationPolicies: rpNames} // #nosec G115 -- far below 2^31 volumes
	for _, t := range targets {
		v := v1alpha1.BackupVolumeStatus{
			PVCNamespace: t.sv.Spec.PVCNamespace, PVCName: t.sv.Spec.PVCName, SubVolumeName: t.sv.Name, VMName: t.vmName,
			SubPath: t.sv.Spec.SubPath, RequestedGB: t.sv.Spec.RequestedGB, StorageClassName: t.storageClass,
			ShareID: placement[t.sv.Name],
		}
		if msg, ok := waiting[t.sv.Name]; ok {
			v.LastError = msg
			st.Volumes = append(st.Volumes, v)
			continue
		}
		if v.StorageClassName == "" {
			// The claim is not listed this pass (a VM disk found through its VM):
			// keep what the last pass knew.
			for _, old := range bp.Status.Volumes {
				if old.SubVolumeName == t.sv.Name {
					v.StorageClassName = old.StorageClassName
				}
			}
		}
		if bp.Spec.Export != nil {
			// The export fields are the controller's own: keep what the last pass knew.
			for _, old := range bp.Status.Volumes {
				if old.SubVolumeName == t.sv.Name {
					v.ExportedTime, v.ExportedBytes, v.ExportError = old.ExportedTime, old.ExportedBytes, old.ExportError
				}
			}
		}
		if rs, ok := byName[t.sv.Name]; !ok {
			// No report this pass (the replication policy is new, or cannot be
			// read): keep what the last pass knew, or an alert would be false.
			for _, old := range bp.Status.Volumes {
				if old.SubVolumeName == t.sv.Name && !old.Retained {
					v.LastBackupTime, v.OldestBackupTime, v.RestorePoints = old.LastBackupTime, old.OldestBackupTime, old.RestorePoints
					v.BytesWritten, v.Consistency, v.LastError = old.BytesWritten, old.Consistency, old.LastError
				}
			}
		} else {
			v.LastBackupTime = rs.LastSyncTime
			v.OldestBackupTime = rs.OldestRestorePoint
			v.RestorePoints = rs.RestorePoints
			if rs.BytesSynced != nil {
				v.BytesWritten = *rs.BytesSynced
			}
			v.Consistency = rs.Consistency
			if v.Consistency == "" && rs.LastSyncTime != nil {
				v.Consistency = consistencyFileCopy
			}
			v.LastError = rs.LastError
		}
		st.Volumes = append(st.Volumes, v)
	}
	st.Volumes = append(st.Volumes, bc.retainedVolumes(bp, targets)...)
	if bp.Spec.Suspend {
		st.Phase, st.Message = backupPhaseSuspended, "backups are suspended; existing backups stay"
	}
	if len(conflicts) > 0 {
		msg := fmt.Sprintf("%d volume(s) skipped: %s", len(conflicts), strings.Join(conflicts, "; "))
		if st.Message != "" {
			msg = st.Message + "; " + msg
		}
		st.Message = msg
	}
	return st
}

// retainedVolumes lists the volumes that were backed up before and are not any
// more (deleted, or no longer matching the selector). Their backups are still
// on the share, and a restore needs their entry. An entry goes once the keep
// time has passed since its last backup.
func (bc *BackupController) retainedVolumes(bp *v1alpha1.BackupPolicy, targets []backupTarget) []v1alpha1.BackupVolumeStatus {
	keep := time.Duration(backupKeepHours(bp)) * time.Hour
	current := map[string]bool{}
	for _, t := range targets {
		current[t.sv.Name] = true
	}
	now := bc.nowFunc()
	var out []v1alpha1.BackupVolumeStatus
	for _, old := range bp.Status.Volumes {
		if current[old.SubVolumeName] || old.LastBackupTime == nil || now.Sub(old.LastBackupTime.Time) > keep {
			continue
		}
		old.Retained = true
		out = append(out, old)
	}
	return out
}

// recordMetrics reports how many volumes a policy backs up and how old its
// backups are.
func (bc *BackupController) recordMetrics(bp *v1alpha1.BackupPolicy, st v1alpha1.BackupPolicyStatus) {
	now := bc.nowFunc()
	var stalest, oldest float64
	for _, v := range st.Volumes {
		if v.LastBackupTime != nil {
			if !v.Retained {
				if age := now.Sub(v.LastBackupTime.Time).Seconds(); age > stalest {
					stalest = age
				}
			}
			first := v.LastBackupTime.Time
			if v.OldestBackupTime != nil && v.OldestBackupTime.Before(&metav1.Time{Time: first}) {
				first = v.OldestBackupTime.Time
			}
			if age := now.Sub(first).Seconds(); age > oldest {
				oldest = age
			}
		} else if !v.Retained && !bp.CreationTimestamp.IsZero() {
			if age := now.Sub(bp.CreationTimestamp.Time).Seconds(); age > stalest {
				stalest = age
			}
		}
	}
	metrics.BackupVolumes.WithLabelValues(bp.Name).Set(float64(st.VolumeCount))
	metrics.BackupStalestAgeSeconds.WithLabelValues(bp.Name).Set(stalest)
	metrics.BackupOldestAgeSeconds.WithLabelValues(bp.Name).Set(oldest)
	bc.reported[bp.Name] = true
}
