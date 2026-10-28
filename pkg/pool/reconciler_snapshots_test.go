package pool

import (
	"context"
	"errors"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A snapshot's data lives on a share of its pool, so the pool and the share
// must stay while the snapshot does (issue #96).

func newPoolSnapshot(name, poolName, shareID string) *v1alpha1.Snapshot {
	return &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.SnapshotSpec{
			PoolName: poolName, ShareID: shareID, SourceSubVolume: "pvc-gone",
			SnapshotPath: "/snapshots/" + name, SizeGB: 10,
		},
	}
}

func TestReconcile_DeletionBlocked_SnapshotsExist(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))
	now := metav1.Now()
	pool := newTestPool("test-pool", "spread", 1000, v1alpha1.PoolShareStatus{
		ShareID: shareInfo.ID, ShareName: shareInfo.Name, TotalGB: 1000, State: "stable", Zone: "us-south-1",
	})
	pool.DeletionTimestamp = &now
	pool.Finalizers = []string{FinalizerName}
	k.addPool(pool)
	k.addSnapshot(newPoolSnapshot("snapshot-1", "test-pool", shareInfo.ID))

	r := newReconciler(k, vpc)
	result, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if vpc.DeleteCalls != 0 {
		t.Fatalf("the share holds a snapshot's data and must not be deleted, %d deletes", vpc.DeleteCalls)
	}
	updated := k.getPool("test-pool")
	if result.RequeueAfter == 0 || !hasFinalizer(updated) {
		t.Error("expected the deletion to wait while snapshots exist")
	}
	c := findCondition(updated.Status.Conditions, "DeletionBlocked")
	if c == nil || c.Status != metav1.ConditionTrue || c.Reason != "SnapshotsExist" {
		t.Errorf("DeletionBlocked condition = %+v, want True with reason SnapshotsExist", c)
	}

	k.mu.Lock()
	delete(k.snapshots, "snapshot-1")
	k.mu.Unlock()
	if _, err := reconcilePool(r, "test-pool"); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if vpc.DeleteCalls != 1 {
		t.Errorf("expected the share to be deleted once the snapshots are gone, got %d", vpc.DeleteCalls)
	}
}

func TestReconcile_DrainRequests_SnapshotsKeepTheShareNotDrained(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))
	pool := newTestPool("test-pool", "spread", 1000, v1alpha1.PoolShareStatus{
		ShareID: shareInfo.ID, ShareName: shareInfo.Name, MountTargetIP: "10.240.0.1",
		TotalGB: 1000, State: "stable", Zone: "us-south-1",
	})
	pool.Spec.InitialShares = 1
	pool.Spec.DrainShares = []string{shareInfo.ID}
	k.addPool(pool)
	k.addSnapshot(newPoolSnapshot("snapshot-1", "test-pool", shareInfo.ID))

	r := newReconciler(k, vpc)
	if _, err := reconcilePool(r, "test-pool"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	updated := k.getPool("test-pool")
	if len(updated.Status.DrainStatus) != 1 {
		t.Fatalf("expected 1 drain status entry, got %d", len(updated.Status.DrainStatus))
	}
	ds := updated.Status.DrainStatus[0]
	if ds.Drained {
		t.Error("a share with a snapshot on it is not drained: deleting it would delete the snapshot")
	}
	if ds.RemainingSnapshots != 1 {
		t.Errorf("RemainingSnapshots = %d, want 1", ds.RemainingSnapshots)
	}
	if c := findCondition(updated.Status.Conditions, "DrainComplete"); c == nil || c.Status != metav1.ConditionFalse {
		t.Errorf("DrainComplete = %+v, want False", c)
	}
}

func TestDeleteSnapshot_PoolAlreadyGoneRemovesTheRecord(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	// A pool deleted before deletion waited for snapshots took their data with it.
	k.addSnapshot(newPoolSnapshot("snapshot-orphan", "deleted-pool", "share-x"))
	if err := mgr.DeleteSnapshot(context.Background(), "snapshot-orphan"); err != nil {
		t.Fatalf("DeleteSnapshot: %v: the data is gone with the pool, so the delete must not stay stuck", err)
	}
	if k.getSnapshot("snapshot-orphan") != nil {
		t.Error("the Snapshot record must be removed")
	}
}

func TestDeleteSnapshot_PoolReadErrorKeepsTheRecord(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	k.addSnapshot(newPoolSnapshot("snapshot-1", "test-pool", "share-1"))
	k.GetPoolErr = errors.New("etcdserver: request timed out")
	if err := mgr.DeleteSnapshot(context.Background(), "snapshot-1"); err == nil {
		t.Fatal("expected an error to retry: the pool may still exist")
	}
	k.GetPoolErr = nil
	if k.getSnapshot("snapshot-1") == nil {
		t.Error("the Snapshot record must stay")
	}
}
