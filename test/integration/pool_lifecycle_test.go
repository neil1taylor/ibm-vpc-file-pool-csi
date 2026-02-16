//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// ---------------------------------------------------------------------------
// Pool Lifecycle: end-to-end flow through CSI driver + pool manager +
// reconciler, with fake VPC and fake K8s clients.
// ---------------------------------------------------------------------------

func TestPoolLifecycle_CreateAllocateDeallocate(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Step 1: Set up a pool with one pre-existing stable share
	pool := newTestPool("lifecycle-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Step 2: CreateVolume via CSI driver (exercises driver -> pool manager -> K8s client)
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "lifecycle-pool", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	vol := resp.GetVolume()
	if vol == nil {
		t.Fatal("expected volume in response")
	}

	// Verify volume ID format: pool/shareID/pvName
	expectedVolumeID := fmt.Sprintf("lifecycle-pool/share-1/%s", pvName)
	if vol.VolumeId != expectedVolumeID {
		t.Errorf("expected volumeID %q, got %q", expectedVolumeID, vol.VolumeId)
	}

	// Verify volume context
	if vol.VolumeContext["server"] != "10.240.0.1" {
		t.Errorf("expected server '10.240.0.1', got %q", vol.VolumeContext["server"])
	}
	if vol.VolumeContext["subDir"] != fmt.Sprintf("/pvcs/%s", pvName) {
		t.Errorf("expected subDir '/pvcs/%s', got %q", pvName, vol.VolumeContext["subDir"])
	}

	// Step 3: Verify SubVolume CR was created
	sv := h.k8sClient.getSubVolume(pvName)
	if sv == nil {
		t.Fatal("SubVolume CR not created")
	}
	if sv.Spec.PoolName != "lifecycle-pool" {
		t.Errorf("expected poolName 'lifecycle-pool', got %q", sv.Spec.PoolName)
	}
	if sv.Spec.ShareID != "share-1" {
		t.Errorf("expected shareID 'share-1', got %q", sv.Spec.ShareID)
	}
	if sv.Spec.RequestedGB != 10 {
		t.Errorf("expected requestedGB 10, got %d", sv.Spec.RequestedGB)
	}

	// Step 4: Verify pool status was updated
	updatedPool := h.k8sClient.getPool("lifecycle-pool")
	if updatedPool.Status.TotalAllocatedGB != 10 {
		t.Errorf("expected TotalAllocatedGB=10, got %d", updatedPool.Status.TotalAllocatedGB)
	}
	if updatedPool.Status.TotalPVCCount != 1 {
		t.Errorf("expected TotalPVCCount=1, got %d", updatedPool.Status.TotalPVCCount)
	}

	// Step 5: Verify NFS directory was created
	expectedDir := fmt.Sprintf("/mnt/staging/pvcs/%s", pvName)
	if h.nfsOps.dirCount() != 1 {
		t.Errorf("expected 1 directory, got %d", h.nfsOps.dirCount())
	}

	// Step 6: DeleteVolume via CSI driver
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(vol.VolumeId))
	if err != nil {
		t.Fatalf("DeleteVolume failed: %v", err)
	}

	// Step 7: Verify SubVolume CR was deleted
	if h.k8sClient.subVolumeCount() != 0 {
		t.Error("expected SubVolume CR to be deleted")
	}

	// Step 8: Verify NFS directory was removed
	if h.nfsOps.dirCount() != 0 {
		t.Errorf("expected directory %s to be removed, %d dirs remain", expectedDir, h.nfsOps.dirCount())
	}

	// Step 9: Verify pool status after deallocation
	finalPool := h.k8sClient.getPool("lifecycle-pool")
	if finalPool.Status.TotalAllocatedGB != 0 {
		t.Errorf("expected TotalAllocatedGB=0 after deallocation, got %d", finalPool.Status.TotalAllocatedGB)
	}
	if finalPool.Status.TotalPVCCount != 0 {
		t.Errorf("expected TotalPVCCount=0 after deallocation, got %d", finalPool.Status.TotalPVCCount)
	}
}

func TestPoolLifecycle_MultipleAllocationsAndDeallocations(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with two shares, spread strategy
	pool := newTestPool("multi-pool", "spread", 500,
		newStableShare("share-1", "s1", 500, 0, 0),
		newStableShare("share-2", "s2", 500, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Allocate 5 volumes sequentially
	volumeIDs := make([]string, 5)
	for i := 0; i < 5; i++ {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", i)
		resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "multi-pool", 50))
		if err != nil {
			t.Fatalf("CreateVolume %d failed: %v", i, err)
		}
		volumeIDs[i] = resp.GetVolume().VolumeId
	}

	// Verify 5 SubVolumes exist
	if h.k8sClient.subVolumeCount() != 5 {
		t.Errorf("expected 5 SubVolumes, got %d", h.k8sClient.subVolumeCount())
	}

	// Verify 5 NFS directories
	if h.nfsOps.dirCount() != 5 {
		t.Errorf("expected 5 directories, got %d", h.nfsOps.dirCount())
	}

	// Verify pool status: total allocated = 5 * 50 = 250
	updatedPool := h.k8sClient.getPool("multi-pool")
	if updatedPool.Status.TotalAllocatedGB != 250 {
		t.Errorf("expected TotalAllocatedGB=250, got %d", updatedPool.Status.TotalAllocatedGB)
	}
	if updatedPool.Status.TotalPVCCount != 5 {
		t.Errorf("expected TotalPVCCount=5, got %d", updatedPool.Status.TotalPVCCount)
	}

	// Spread strategy: shares should have roughly equal allocation
	share1 := updatedPool.Status.Shares[0]
	share2 := updatedPool.Status.Shares[1]
	totalPVCs := share1.PVCCount + share2.PVCCount
	if totalPVCs != 5 {
		t.Errorf("expected total PVC count across shares to be 5, got %d", totalPVCs)
	}

	// Delete all volumes
	for i, volumeID := range volumeIDs {
		_, err := h.driver.DeleteVolume(ctx, deleteVolumeRequest(volumeID))
		if err != nil {
			t.Fatalf("DeleteVolume %d failed: %v", i, err)
		}
	}

	// Verify everything is cleaned up
	if h.k8sClient.subVolumeCount() != 0 {
		t.Errorf("expected 0 SubVolumes after cleanup, got %d", h.k8sClient.subVolumeCount())
	}
	if h.nfsOps.dirCount() != 0 {
		t.Errorf("expected 0 directories after cleanup, got %d", h.nfsOps.dirCount())
	}

	finalPool := h.k8sClient.getPool("multi-pool")
	if finalPool.Status.TotalAllocatedGB != 0 {
		t.Errorf("expected TotalAllocatedGB=0, got %d", finalPool.Status.TotalAllocatedGB)
	}
	if finalPool.Status.TotalPVCCount != 0 {
		t.Errorf("expected TotalPVCCount=0, got %d", finalPool.Status.TotalPVCCount)
	}
}

func TestPoolLifecycle_IdempotentCreateVolume(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("idem-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	req := createVolumeRequest(pvName, "idem-pool", 10)

	// First call
	resp1, err := h.driver.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("first CreateVolume failed: %v", err)
	}

	// Second call (idempotent)
	resp2, err := h.driver.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("second CreateVolume failed: %v", err)
	}

	// Should return the same volume ID
	if resp1.GetVolume().VolumeId != resp2.GetVolume().VolumeId {
		t.Errorf("idempotent calls returned different volume IDs: %q vs %q",
			resp1.GetVolume().VolumeId, resp2.GetVolume().VolumeId)
	}

	// Should still only have 1 SubVolume
	if h.k8sClient.subVolumeCount() != 1 {
		t.Errorf("expected 1 SubVolume (idempotent), got %d", h.k8sClient.subVolumeCount())
	}
}

func TestPoolLifecycle_IdempotentDeleteVolume(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("idem-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "idem-pool", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	volumeID := resp.GetVolume().VolumeId

	// First delete
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(volumeID))
	if err != nil {
		t.Fatalf("first DeleteVolume failed: %v", err)
	}

	// Second delete (idempotent - SubVolume already gone)
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(volumeID))
	if err != nil {
		t.Fatalf("second DeleteVolume should be idempotent, got: %v", err)
	}
}

func TestPoolLifecycle_PoolNotFound(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "nonexistent-pool", 10))

	assertGRPCCode(t, err, codes.NotFound)
}

func TestPoolLifecycle_ExpandVolume(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("expand-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "expand-pool", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	volumeID := resp.GetVolume().VolumeId

	// Expand from 10 GB to 50 GB
	expandReq := csiExpandRequest(volumeID, 50)
	expandResp, err := h.driver.ControllerExpandVolume(ctx, &expandReq)
	if err != nil {
		t.Fatalf("ControllerExpandVolume failed: %v", err)
	}

	if expandResp.CapacityBytes != gbToBytes(50) {
		t.Errorf("expected capacity %d, got %d", gbToBytes(50), expandResp.CapacityBytes)
	}

	// Verify SubVolume was updated
	sv := h.k8sClient.getSubVolume(pvName)
	if sv.Spec.RequestedGB != 50 {
		t.Errorf("expected RequestedGB=50, got %d", sv.Spec.RequestedGB)
	}

	// Verify pool status: delta of 40 GB
	updatedPool := h.k8sClient.getPool("expand-pool")
	if updatedPool.Status.TotalAllocatedGB != 50 {
		t.Errorf("expected TotalAllocatedGB=50, got %d", updatedPool.Status.TotalAllocatedGB)
	}
}

func TestPoolLifecycle_ReconcilerInitialProvisioning(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Create a pool with no shares yet (initial state)
	pool := newTestPool("reconciler-pool", "spread", 1000)
	pool.Spec.InitialShares = 2
	pool.Status.Phase = "" // triggers initial provisioning
	pool.Status.Shares = nil
	pool.Status.ShareCount = 0
	pool.Status.TotalCapacityGB = 0
	h.k8sClient.addPool(pool)

	// Run reconciler
	reconciler := newPoolReconciler(h)
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "reconciler-pool"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Should requeue
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after reconciliation")
	}

	// Should have created 2 VPC shares
	if h.vpcClient.CreateCalls != 2 {
		t.Errorf("expected 2 VPC create calls, got %d", h.vpcClient.CreateCalls)
	}

	// Pool status should have 2 shares
	updated := h.k8sClient.getPool("reconciler-pool")
	if len(updated.Status.Shares) != 2 {
		t.Errorf("expected 2 shares in status, got %d", len(updated.Status.Shares))
	}

	// Now allocate a volume using one of the newly created shares
	// The shares created by reconciler have "stable" state (fake VPC returns stable)
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	_, err = h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "reconciler-pool", 10))
	if err != nil {
		t.Fatalf("CreateVolume after reconciliation failed: %v", err)
	}

	if h.k8sClient.subVolumeCount() != 1 {
		t.Errorf("expected 1 SubVolume, got %d", h.k8sClient.subVolumeCount())
	}
}

func TestPoolLifecycle_ReconcilerThenAllocateThenReconcileMetrics(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Start with a pool that already has a share
	pool := newTestPool("metrics-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	pool.Spec.InitialShares = 1
	h.k8sClient.addPool(pool)

	// Also create the share in VPC so reconciler health check can find it
	h.vpcClient.CreateFileShare(ctx, ibmcloud.CreateShareInput{
		Name:    "s1",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  1000,
	})

	// Allocate 3 volumes (10, 20, 30 GB)
	sizes := []int64{10, 20, 30}
	for i, size := range sizes {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", i)
		_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "metrics-pool", size))
		if err != nil {
			t.Fatalf("CreateVolume %d failed: %v", i, err)
		}
	}

	// Now intentionally mess up pool status (simulate status drift)
	drifted := h.k8sClient.getPool("metrics-pool")
	drifted.Status.TotalAllocatedGB = 999 // wrong
	drifted.Status.TotalPVCCount = 99     // wrong
	h.k8sClient.addPool(drifted)

	// Run reconciler — metrics reconciliation should correct the drift
	reconciler := newPoolReconciler(h)
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "metrics-pool"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify metrics were corrected
	corrected := h.k8sClient.getPool("metrics-pool")
	if corrected.Status.TotalAllocatedGB != 60 { // 10+20+30
		t.Errorf("expected TotalAllocatedGB=60 after metrics reconciliation, got %d", corrected.Status.TotalAllocatedGB)
	}
	if corrected.Status.TotalPVCCount != 3 {
		t.Errorf("expected TotalPVCCount=3, got %d", corrected.Status.TotalPVCCount)
	}
}

func TestPoolLifecycle_DeletionBlockedBySubVolumes(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Create a pool with a share and allocate a volume
	fsp := newTestPool("delete-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(fsp)

	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "delete-pool", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	// Mark pool for deletion
	marked := h.k8sClient.getPool("delete-pool")
	now := metav1.Now()
	marked.DeletionTimestamp = &now
	marked.Finalizers = []string{pool.FinalizerName}
	h.k8sClient.addPool(marked)

	// Run reconciler — deletion should be blocked
	reconciler := newPoolReconciler(h)
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "delete-pool"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue when deletion is blocked by SubVolumes")
	}

	// Delete the volume
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(resp.GetVolume().VolumeId))
	if err != nil {
		t.Fatalf("DeleteVolume failed: %v", err)
	}

	// Create VPC share so reconciler can delete it
	h.vpcClient.CreateFileShare(ctx, ibmcloud.CreateShareInput{
		Name:    "s1",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  1000,
	})

	// Re-fetch pool with deletion timestamp
	marked2 := h.k8sClient.getPool("delete-pool")
	marked2.DeletionTimestamp = &now
	marked2.Finalizers = []string{pool.FinalizerName}
	// Update share ID to match the one in VPC
	if h.vpcClient.ShareCount() > 0 {
		shares, _ := h.vpcClient.ListFileShares(ctx, "", nil)
		if len(shares) > 0 {
			marked2.Status.Shares[0].ShareID = shares[0].ID
		}
	}
	h.k8sClient.addPool(marked2)

	// Run reconciler again — should succeed now
	result2, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "delete-pool"},
	})
	if err != nil {
		t.Fatalf("Reconcile after volume deletion failed: %v", err)
	}
	if result2.RequeueAfter != 0 {
		t.Error("expected no requeue after successful deletion")
	}
}

// ---------------------------------------------------------------------------
// CSI ExpandVolume helper
// ---------------------------------------------------------------------------

func csiExpandRequest(volumeID string, sizeGB int64) csi.ControllerExpandVolumeRequest {
	return csi.ControllerExpandVolumeRequest{
		VolumeId: volumeID,
		CapacityRange: &csi.CapacityRange{
			RequiredBytes: gbToBytes(sizeGB),
		},
	}
}

// ---------------------------------------------------------------------------
// Reconciler helper
// ---------------------------------------------------------------------------

func newPoolReconciler(h *testHarness) *pool.FileSharePoolReconciler {
	r := pool.NewFileSharePoolReconciler(h.k8sClient, h.vpcClient)
	r.SetVPCConfig("vpc-1", "subnet-1", "rg-1")
	return r
}
