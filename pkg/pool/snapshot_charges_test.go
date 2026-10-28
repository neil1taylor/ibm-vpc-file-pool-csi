package pool

import (
	"context"
	"errors"
	"fmt"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud/fake"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// A snapshot uses space on its source's share, so the share is charged for it
// for as long as the snapshot exists (issue #97).

func chargedSnapshot(name, shareID string, sizeGB int64, method string) *v1alpha1.Snapshot {
	return &v1alpha1.Snapshot{
		ObjectMeta: metav1.ObjectMeta{Name: name, Annotations: map[string]string{snapshotMethodAnnotation: method}},
		Spec: v1alpha1.SnapshotSpec{
			PoolName: "test-pool", ShareID: shareID, SourceSubVolume: testPVName,
			SnapshotPath: "/snapshots/" + name, SizeGB: sizeGB,
		},
	}
}

func TestReconcile_RecountKeepsSnapshotCharges(t *testing.T) {
	k := newFakeK8sClient()
	vpc := fake.NewFakeVPCClient()
	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))
	k.addPool(newTestPool("test-pool", "spread", 1000, v1alpha1.PoolShareStatus{
		ShareID: shareInfo.ID, ShareName: shareInfo.Name, MountTargetIP: "10.240.0.1",
		TotalGB: 1000, State: "stable", Zone: "us-south-1",
	}))
	k.addSubVolume(newTestSubVolume(testPVName, "test-pool", shareInfo.ID, 100))
	k.addSnapshot(chargedSnapshot("copy-snap", shareInfo.ID, 100, snapshotMethodFile))
	k.addSnapshot(chargedSnapshot("instant-snap", shareInfo.ID, 100, snapshotMethodInstant))

	r := newReconciler(k, vpc)
	if _, err := reconcilePool(r, "test-pool"); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	p := k.getPool("test-pool")
	// 100 for the volume, 100 for the copy, a tenth of 100 for the instant snapshot.
	if got := p.Status.Shares[0].AllocatedGB; got != 210 {
		t.Errorf("share AllocatedGB = %d, want 210: the recount must keep the snapshots' charges", got)
	}
	if p.Status.TotalAllocatedGB != 210 {
		t.Errorf("TotalAllocatedGB = %d, want 210", p.Status.TotalAllocatedGB)
	}
	if p.Status.Shares[0].PVCCount != 1 || p.Status.TotalPVCCount != 1 {
		t.Errorf("PVC counts = %d/%d, want 1: a snapshot is not a PVC", p.Status.Shares[0].PVCCount, p.Status.TotalPVCCount)
	}
}

func TestCreateSnapshot_IsChargedButNotCountedAsAPVC(t *testing.T) {
	k, _, mgr := newShareDataTestManager(t)
	before := k.getPool("test-pool").Status
	if _, err := mgr.CreateSnapshot(context.Background(), "snap-1", "test-pool/share-1/"+testPVName, nil); err != nil {
		t.Fatalf("CreateSnapshot: %v", err)
	}
	after := k.getPool("test-pool").Status
	if after.TotalAllocatedGB != before.TotalAllocatedGB+10 {
		t.Errorf("TotalAllocatedGB %d -> %d, want +10", before.TotalAllocatedGB, after.TotalAllocatedGB)
	}
	if after.TotalPVCCount != before.TotalPVCCount || after.Shares[0].PVCCount != before.Shares[0].PVCCount {
		t.Errorf("PVC count %d -> %d: a snapshot is not a PVC", before.TotalPVCCount, after.TotalPVCCount)
	}
	if err := mgr.DeleteSnapshot(context.Background(), "snap-1"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	final := k.getPool("test-pool").Status
	if final.TotalAllocatedGB != before.TotalAllocatedGB || final.TotalPVCCount != before.TotalPVCCount {
		t.Errorf("after the delete: allocated %d, PVCs %d; want %d, %d", final.TotalAllocatedGB, final.TotalPVCCount,
			before.TotalAllocatedGB, before.TotalPVCCount)
	}
}

func TestCreateSnapshot_RefusesWhenTheShareIsFull(t *testing.T) {
	k := newFakeK8sClient()
	// 100 GB share, 95 GB allocated: a 10 GB volume's snapshot does not fit.
	k.addPool(newTestPool("test-pool", "spread", 100, newStableShare("share-1", "s1", 100, 95, 1)))
	k.addSubVolume(newTestSubVolume(testPVName, "test-pool", "share-1", 10))
	mgr := newManagerForTest(k, fake.NewFakeVPCClient(), newFakeNFSOperations())

	_, err := mgr.CreateSnapshot(context.Background(), "snap-1", fmt.Sprintf("test-pool/share-1/%s", testPVName), nil)
	if !errors.Is(err, ErrInsufficientShareCapacity) {
		t.Fatalf("err = %v, want ErrInsufficientShareCapacity", err)
	}
	if k.getSnapshot("snap-1") != nil {
		t.Error("no Snapshot may be made when the share has no room")
	}
	if got := k.getPool("test-pool").Status.Shares[0].AllocatedGB; got != 95 {
		t.Errorf("AllocatedGB = %d, want 95 unchanged", got)
	}
}
