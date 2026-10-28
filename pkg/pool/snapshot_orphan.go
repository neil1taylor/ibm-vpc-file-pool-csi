package pool

import (
	"context"
	"errors"
	"regexp"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
)

// orphanGrace is how old an unfinished Snapshot record must be before it can
// be called an orphan. The VolumeSnapshotContent exists before the sidecar
// first asks for the snapshot, so this only covers a slow cache.
const orphanGrace = 2 * time.Minute

var volumeSnapshotContentListGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContentList"}

// sidecarSnapshotName matches the name that the external snapshotter gives a
// snapshot: <prefix>-<VolumeSnapshot UID>. The prefix is one word, so a group
// snapshot member (<group>-pvc-<uuid>) does not match.
var sidecarSnapshotName = regexp.MustCompile(`^[a-z0-9]+-([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$`)

// RemoveOrphanSnapshots deletes the Snapshot records that no VolumeSnapshot
// can finish or delete any more. When CreateSnapshot fails, the sidecar has no
// snapshot ID, so deleting the VolumeSnapshot never reaches DeleteSnapshot and
// the record would block the deletion of its pool for ever.
//
// A record is an orphan when it is not ready, it is named like a sidecar
// snapshot, and no VolumeSnapshotContent refers to that VolumeSnapshot. It is
// removed like any deleted snapshot: its Jobs stop, its files are merged away
// and its space is given back. If the contents cannot be listed, nothing is
// removed.
func (m *Manager) RemoveOrphanSnapshots(ctx context.Context) {
	m.mu.Lock()
	vm := m.vm
	m.mu.Unlock()
	if vm == nil {
		return
	}
	pools, err := m.k8sClient.ListFileSharePools(ctx)
	if err != nil {
		klog.ErrorS(err, "Failed to list pools for orphan snapshots")
		return
	}
	var candidates []v1alpha1.Snapshot
	for i := range pools {
		snaps, listErr := m.k8sClient.ListSnapshots(ctx, pools[i].Name)
		if listErr != nil {
			klog.ErrorS(listErr, "Failed to list snapshots for orphans", "pool", pools[i].Name)
			continue
		}
		for j := range snaps {
			if m.maybeOrphan(&snaps[j]) {
				candidates = append(candidates, snaps[j])
			}
		}
	}
	if len(candidates) == 0 {
		return
	}
	contents := &unstructured.UnstructuredList{}
	contents.SetGroupVersionKind(volumeSnapshotContentListGVK)
	if err = vm.kube.List(ctx, contents); err != nil {
		klog.ErrorS(err, "Failed to list VolumeSnapshotContents; keeping unfinished snapshots")
		return
	}
	owned := map[string]bool{}
	for i := range contents.Items {
		uid, _, _ := unstructured.NestedString(contents.Items[i].Object, "spec", "volumeSnapshotRef", "uid")
		owned[uid] = true
	}
	for i := range candidates {
		name := candidates[i].Name
		if owned[sidecarSnapshotName.FindStringSubmatch(name)[1]] {
			continue
		}
		klog.InfoS("Removing the record of a snapshot whose VolumeSnapshot is gone", "snapshot", name, "phase", candidates[i].Status.Phase)
		if delErr := m.DeleteSnapshot(ctx, name); delErr != nil && !errors.Is(delErr, ErrOperationInProgress) {
			klog.ErrorS(delErr, "Failed to remove an orphan snapshot record", "snapshot", name)
		}
	}
}

// maybeOrphan reports whether a record is old, unfinished and named by the sidecar.
func (m *Manager) maybeOrphan(s *v1alpha1.Snapshot) bool {
	if s.Status.ReadyToUse || !sidecarSnapshotName.MatchString(s.Name) {
		return false
	}
	return s.Status.CreationTime != nil && m.timeNow().Sub(s.Status.CreationTime.Time) >= orphanGrace
}
