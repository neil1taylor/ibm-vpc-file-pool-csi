package pool

import (
	"context"
	"fmt"
	"sort"
	"strings"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

// backupShareLabel is on a SubVolume that a BackupPolicy backs up. Its value is
// the ID of the share of the snapshot pool that holds the backups of the
// volume. The ReplicationPolicy of that share selects on it.
const backupShareLabel = "storage.ibmcloud.io/backup-share"

// noRoomMessage starts the error of a new volume that no share of the snapshot
// pool can take. Such an entry has no ShareID and is not an old entry.
const noRoomMessage = "no share of the snapshot pool has room for new backups"

// shareByID finds a share of a pool.
func shareByID(shares []v1alpha1.PoolShareStatus, id string) (v1alpha1.PoolShareStatus, bool) {
	for _, sh := range shares {
		if sh.ShareID == id {
			return sh, true
		}
	}
	return v1alpha1.PoolShareStatus{}, false
}

// shareOfEntry is the share that holds the backups of a status entry. An entry
// with no ShareID was made when the pool had one share: the first one.
func shareOfEntry(e v1alpha1.BackupVolumeStatus, shares []v1alpha1.PoolShareStatus) string {
	if e.ShareID != "" {
		return e.ShareID
	}
	if strings.HasPrefix(e.LastError, noRoomMessage) || len(shares) == 0 {
		return ""
	}
	return shares[0].ShareID
}

// shareLoad counts the volumes each share holds, over every BackupPolicy that
// uses the pool. Retained volumes count: their backups are still on the share.
func (bc *BackupController) shareLoad(ctx context.Context, pool string, shares []v1alpha1.PoolShareStatus) map[string]int {
	load := map[string]int{}
	list := &v1alpha1.BackupPolicyList{}
	if err := bc.client.List(ctx, list); err != nil {
		klog.ErrorS(err, "Failed to list BackupPolicies to count the volumes on each share", "pool", pool)
		return load
	}
	for i := range list.Items {
		if list.Items[i].Spec.SnapshotPool != pool {
			continue
		}
		for _, v := range list.Items[i].Status.Volumes {
			if id := shareOfEntry(v, shares); id != "" {
				load[id]++
			}
		}
	}
	return load
}

// placeVolumes says which share holds the backups of each volume. A volume
// that has a share keeps it: its backups stay where they are. A new volume goes
// to the ready share with room that holds the fewest volumes; if there is none
// it waits, and the reason is in waiting.
func placeVolumes(old []v1alpha1.BackupVolumeStatus, targets []backupTarget, shares []v1alpha1.PoolShareStatus,
	full map[string]bool, load map[string]int) (placement, waiting map[string]string) {
	placement, waiting = map[string]string{}, map[string]string{}
	oldByVolume := map[string]v1alpha1.BackupVolumeStatus{}
	for _, v := range old {
		oldByVolume[v.SubVolumeName] = v
	}
	sorted := append([]backupTarget(nil), targets...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].sv.Name < sorted[j].sv.Name })
	for _, t := range sorted {
		if e, ok := oldByVolume[t.sv.Name]; ok {
			if id := shareOfEntry(e, shares); id != "" {
				placement[t.sv.Name] = id
				continue
			}
		}
		best := ""
		for _, sh := range shares {
			if !shareReady(sh) || full[sh.ShareID] {
				continue
			}
			if best == "" || load[sh.ShareID] < load[best] {
				best = sh.ShareID
			}
		}
		if best == "" {
			waiting[t.sv.Name] = noRoomMessage + "; the volume is backed up when one has"
			continue
		}
		placement[t.sv.Name] = best
		load[best]++
	}
	return placement, waiting
}

// snapshotShares checks the snapshot pool and returns its shares. The pool must
// exist, hold no workload claims, and have a ready share.
func (bc *BackupController) snapshotShares(ctx context.Context, bp *v1alpha1.BackupPolicy, svs []v1alpha1.SubVolume) ([]v1alpha1.PoolShareStatus, error) {
	pool := &v1alpha1.FileSharePool{}
	if err := bc.client.Get(ctx, types.NamespacedName{Name: bp.Spec.SnapshotPool}, pool); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("the snapshot pool %q does not exist", bp.Spec.SnapshotPool)
		}
		return nil, fmt.Errorf("get the snapshot pool %q: %w", bp.Spec.SnapshotPool, err)
	}
	claims := 0
	for i := range svs {
		if svs[i].Spec.PoolName == pool.Name {
			claims++
		}
	}
	if claims > 0 {
		return nil, fmt.Errorf("the snapshot pool %q holds %d workload claim(s); a snapshot pool must hold only backups", pool.Name, claims)
	}
	ready := 0
	for _, sh := range pool.Status.Shares {
		if shareReady(sh) {
			ready++
		}
	}
	if ready == 0 {
		return nil, fmt.Errorf("the snapshot pool %q has no ready share (it has %d share(s))", pool.Name, len(pool.Status.Shares))
	}
	return pool.Status.Shares, nil
}

// backupShareOf returns the share that holds the backups of a volume. It must
// be ready: a restore reads from it.
func (bc *BackupController) backupShareOf(ctx context.Context, bp *v1alpha1.BackupPolicy, svs []v1alpha1.SubVolume, entry *v1alpha1.BackupVolumeStatus) (v1alpha1.PoolShareStatus, error) {
	shares, err := bc.snapshotShares(ctx, bp, svs)
	if err != nil {
		return v1alpha1.PoolShareStatus{}, err
	}
	id := shareOfEntry(*entry, shares)
	sh, found := shareByID(shares, id)
	if !found {
		return v1alpha1.PoolShareStatus{}, fmt.Errorf("share %q of the snapshot pool %q, which holds the backups of %s/%s, is not in the pool", id, bp.Spec.SnapshotPool, entry.PVCNamespace, entry.PVCName)
	}
	if !shareReady(sh) {
		return v1alpha1.PoolShareStatus{}, fmt.Errorf("share %s of the snapshot pool %q, which holds the backups of %s/%s, is not ready (state %q)", id, bp.Spec.SnapshotPool, entry.PVCNamespace, entry.PVCName, sh.State)
	}
	return sh, nil
}
