//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	"google.golang.org/grpc/codes"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
)

// ---------------------------------------------------------------------------
// Error Recovery Tests: VPC API failures, share creation failures,
// partial state recovery, K8s client errors.
// ---------------------------------------------------------------------------

func TestErrorRecovery_VPCCreateFailureDuringAutoExpand(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with 1 full share, auto-expand enabled
	pool := newTestPool("vpc-err-pool", "spread", 100,
		newStableShare("share-1", "s1", 100, 100, 10), // fully allocated
	)
	pool.Spec.AutoExpand = true
	pool.Spec.MaxShares = 5
	h.k8sClient.addPool(pool)

	// Inject VPC create error
	h.vpcClient.CreateErr = fmt.Errorf("VPC API unavailable: service temporarily unavailable")

	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "vpc-err-pool", 10))

	// Should fail with Internal error (auto-expand failed due to VPC error)
	assertGRPCCode(t, err, codes.Internal)

	// No SubVolume should have been created
	if h.k8sClient.subVolumeCount() != 0 {
		t.Error("expected no SubVolumes created when VPC API fails")
	}

	// No NFS directory should have been created
	if h.nfsOps.dirCount() != 0 {
		t.Error("expected no NFS directories when VPC API fails")
	}

	// Pool status should be unchanged
	p := h.k8sClient.getPool("vpc-err-pool")
	if p.Status.TotalAllocatedGB != 100 {
		t.Errorf("expected TotalAllocatedGB unchanged at 100, got %d", p.Status.TotalAllocatedGB)
	}
}

func TestErrorRecovery_VPCCreateFailureThenRecovery(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("recover-pool", "spread", 100,
		newStableShare("share-1", "s1", 100, 100, 10),
	)
	pool.Spec.AutoExpand = true
	pool.Spec.MaxShares = 5
	h.k8sClient.addPool(pool)

	// First attempt: VPC fails
	h.vpcClient.CreateErr = fmt.Errorf("VPC API unavailable")
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "recover-pool", 10))
	if err == nil {
		t.Fatal("expected error during VPC failure")
	}

	// Clear the error (VPC recovers)
	h.vpcClient.CreateErr = nil

	// Second attempt: should succeed with auto-expand
	_, err = h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "recover-pool", 10))
	if err != nil {
		t.Fatalf("CreateVolume should succeed after VPC recovery: %v", err)
	}

	// Verify SubVolume was created
	if h.k8sClient.subVolumeCount() != 1 {
		t.Errorf("expected 1 SubVolume after recovery, got %d", h.k8sClient.subVolumeCount())
	}
}

func TestErrorRecovery_NfsMkdirFailure(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("nfs-err-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Inject NFS mkdir error
	h.nfsOps.MkdirErr = fmt.Errorf("NFS: permission denied")

	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "nfs-err-pool", 10))

	// Should fail
	assertGRPCCode(t, err, codes.Internal)

	// SubVolume CR should NOT be created (mkdir happens before CR creation)
	if h.k8sClient.subVolumeCount() != 0 {
		t.Error("expected no SubVolume when NFS mkdir fails")
	}

	// Pool status should not have incremented allocations
	p := h.k8sClient.getPool("nfs-err-pool")
	if p.Status.TotalAllocatedGB != 0 {
		t.Errorf("expected TotalAllocatedGB=0 after NFS failure, got %d", p.Status.TotalAllocatedGB)
	}
}

func TestErrorRecovery_NfsMkdirFailureThenRecovery(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("nfs-recover", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// First: NFS fails
	h.nfsOps.MkdirErr = fmt.Errorf("NFS: connection reset")
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "nfs-recover", 10))
	if err == nil {
		t.Fatal("expected error during NFS failure")
	}

	// Clear NFS error
	h.nfsOps.MkdirErr = nil

	// Retry: should succeed
	_, err = h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "nfs-recover", 10))
	if err != nil {
		t.Fatalf("CreateVolume should succeed after NFS recovery: %v", err)
	}

	if h.k8sClient.subVolumeCount() != 1 {
		t.Errorf("expected 1 SubVolume after recovery, got %d", h.k8sClient.subVolumeCount())
	}
}

func TestErrorRecovery_NfsRemoveFailureDuringDeallocate(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("nfs-dealloc-err", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Allocate a volume
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "nfs-dealloc-err", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	// Inject NFS remove error
	h.nfsOps.RemoveErr = fmt.Errorf("NFS: I/O error")

	// DeleteVolume should fail
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(resp.GetVolume().VolumeId))
	assertGRPCCode(t, err, codes.Internal)

	// SubVolume CR should be retained (for tracking, fail-safe)
	if h.k8sClient.subVolumeCount() != 1 {
		t.Error("expected SubVolume CR to be retained when NFS remove fails")
	}

	// Clear error and retry
	h.nfsOps.RemoveErr = nil
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(resp.GetVolume().VolumeId))
	if err != nil {
		t.Fatalf("DeleteVolume should succeed after NFS recovery: %v", err)
	}

	// Now SubVolume should be deleted
	if h.k8sClient.subVolumeCount() != 0 {
		t.Error("expected SubVolume CR to be deleted after successful retry")
	}
}

func TestErrorRecovery_ReconcilerVPCCreateFailure(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with no shares yet
	pool := newTestPool("reconcile-err", "spread", 1000)
	pool.Spec.InitialShares = 2
	pool.Status.Phase = ""
	pool.Status.Shares = nil
	pool.Status.ShareCount = 0
	pool.Status.TotalCapacityGB = 0
	h.k8sClient.addPool(pool)

	// Inject VPC error
	h.vpcClient.CreateErr = fmt.Errorf("VPC API rate limit exceeded")

	reconciler := newPoolReconciler(h)
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "reconcile-err"},
	})
	// Reconcile should not return an error (soft failure), but requeue
	if err != nil {
		t.Fatalf("expected soft failure (nil error), got: %v", err)
	}
	if result.RequeueAfter == 0 {
		t.Error("expected requeue after reconciliation failure")
	}

	// Phase should be Initializing
	p := h.k8sClient.getPool("reconcile-err")
	if p.Status.Phase != "Initializing" {
		t.Errorf("expected phase 'Initializing', got %q", p.Status.Phase)
	}

	// No shares should exist
	if len(p.Status.Shares) != 0 {
		t.Errorf("expected 0 shares, got %d", len(p.Status.Shares))
	}

	// Clear error and reconcile again
	h.vpcClient.CreateErr = nil
	result2, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "reconcile-err"},
	})
	if err != nil {
		t.Fatalf("Reconcile after recovery failed: %v", err)
	}
	if result2.RequeueAfter == 0 {
		t.Error("expected requeue for normal reconciliation")
	}

	// Should now have 2 shares
	p2 := h.k8sClient.getPool("reconcile-err")
	if len(p2.Status.Shares) != 2 {
		t.Errorf("expected 2 shares after recovery, got %d", len(p2.Status.Shares))
	}
}

func TestErrorRecovery_ReconcilerShareHealthCheck_DegradedShare(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Create a share in VPC
	shareInfo, _ := h.vpcClient.CreateFileShare(ctx, ibmcloud.CreateShareInput{
		Name:    "s1",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  1000,
	})

	// Mark share as failed in VPC
	h.vpcClient.SetShareState(shareInfo.ID, "failed")

	pool := newTestPool("degraded-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID:       shareInfo.ID,
			ShareName:     shareInfo.Name,
			MountTargetIP: "10.240.0.1",
			MountTargetID: shareInfo.ID + "-mt",
			TotalGB:       1000,
			State:         "stable",
			Zone:          "us-south-1",
		},
	)
	pool.Spec.InitialShares = 1
	h.k8sClient.addPool(pool)

	reconciler := newPoolReconciler(h)
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "degraded-pool"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Share should be marked as draining
	p := h.k8sClient.getPool("degraded-pool")
	if p.Status.Shares[0].State != "draining" {
		t.Errorf("expected share state 'draining', got %q", p.Status.Shares[0].State)
	}

	// Pool should be Degraded
	if p.Status.Phase != "Degraded" {
		t.Errorf("expected pool phase 'Degraded', got %q", p.Status.Phase)
	}
}

func TestErrorRecovery_ReconcilerShareHealthCheck_CreatingBecomesStable(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Create a share in VPC (fake returns it as "stable")
	shareInfo, _ := h.vpcClient.CreateFileShare(ctx, ibmcloud.CreateShareInput{
		Name:    "s1",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  1000,
	})

	// Pool thinks share is still "creating"
	pool := newTestPool("creating-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID:   shareInfo.ID,
			ShareName: shareInfo.Name,
			TotalGB:   1000,
			State:     "creating",
			Zone:      "us-south-1",
		},
	)
	pool.Spec.InitialShares = 1
	h.k8sClient.addPool(pool)

	reconciler := newPoolReconciler(h)
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "creating-pool"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Share should transition to stable
	p := h.k8sClient.getPool("creating-pool")
	if p.Status.Shares[0].State != "stable" {
		t.Errorf("expected share state 'stable', got %q", p.Status.Shares[0].State)
	}

	// Mount target IP should be backfilled
	if p.Status.Shares[0].MountTargetIP == "" {
		t.Error("expected MountTargetIP to be backfilled")
	}

	// Now allocations should work
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	_, err = h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "creating-pool", 10))
	if err != nil {
		t.Fatalf("CreateVolume should succeed after share becomes stable: %v", err)
	}
}

func TestErrorRecovery_ReconcilerHealthCheckGetShareFails(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool has a share but VPC GetFileShare will fail
	pool := newTestPool("get-err-pool", "spread", 1000,
		newStableShare("share-missing", "s1", 1000, 0, 0),
	)
	pool.Spec.InitialShares = 1
	h.k8sClient.addPool(pool)

	// GetFileShare will fail since "share-missing" doesn't exist in VPC fake
	reconciler := newPoolReconciler(h)
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "get-err-pool"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Share state should remain unchanged (health check error is non-fatal)
	p := h.k8sClient.getPool("get-err-pool")
	if p.Status.Shares[0].State != "stable" {
		t.Errorf("expected share state to remain 'stable' on health check error, got %q", p.Status.Shares[0].State)
	}
}

func TestErrorRecovery_PartialAllocationCleanup(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("partial-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Allocate 5 volumes successfully
	for i := 0; i < 5; i++ {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", i)
		_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "partial-pool", 10))
		if err != nil {
			t.Fatalf("CreateVolume %d failed: %v", i, err)
		}
	}

	// Inject NFS error for the 6th allocation
	h.nfsOps.MkdirErr = fmt.Errorf("NFS: stale NFS file handle")

	pvNameFail := "pvc-a1b2c3d4-5678-90ab-cdef-000000000005"
	_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvNameFail, "partial-pool", 10))
	if err == nil {
		t.Fatal("expected error for 6th allocation")
	}

	// Should still have exactly 5 SubVolumes
	if h.k8sClient.subVolumeCount() != 5 {
		t.Errorf("expected 5 SubVolumes (failed one should not be created), got %d", h.k8sClient.subVolumeCount())
	}

	// Pool allocated should be 50 (5 * 10), not 60
	p := h.k8sClient.getPool("partial-pool")
	if p.Status.TotalAllocatedGB != 50 {
		t.Errorf("expected TotalAllocatedGB=50, got %d", p.Status.TotalAllocatedGB)
	}

	// Fix NFS and allocate the 6th
	h.nfsOps.MkdirErr = nil
	_, err = h.driver.CreateVolume(ctx, createVolumeRequest(pvNameFail, "partial-pool", 10))
	if err != nil {
		t.Fatalf("CreateVolume should succeed after NFS fix: %v", err)
	}

	if h.k8sClient.subVolumeCount() != 6 {
		t.Errorf("expected 6 SubVolumes after recovery, got %d", h.k8sClient.subVolumeCount())
	}
}

func TestErrorRecovery_DeletionWithVPCDeleteFailure(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Create a share in VPC
	shareInfo, _ := h.vpcClient.CreateFileShare(ctx, ibmcloud.CreateShareInput{
		Name:    "s1",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  1000,
	})

	// Pool marked for deletion, no SubVolumes
	now := metav1.Now()
	pool := newTestPool("del-err-pool", "spread", 1000,
		v1alpha1.PoolShareStatus{
			ShareID:   shareInfo.ID,
			ShareName: shareInfo.Name,
			TotalGB:   1000,
			State:     "stable",
			Zone:      "us-south-1",
		},
	)
	pool.DeletionTimestamp = &now
	pool.Finalizers = []string{"storage.ibmcloud.io/pool-protection"}
	pool.Spec.InitialShares = 1
	h.k8sClient.addPool(pool)

	// Inject VPC delete error
	h.vpcClient.DeleteErr = fmt.Errorf("VPC API: internal server error")

	reconciler := newPoolReconciler(h)
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "del-err-pool"},
	})

	// Reconcile should return the error (will be retried)
	if err == nil {
		t.Fatal("expected error when VPC delete fails during pool deletion")
	}

	// Finalizer should still be present (deletion not complete)
	p := h.k8sClient.getPool("del-err-pool")
	hasProtection := false
	for _, f := range p.Finalizers {
		if f == "storage.ibmcloud.io/pool-protection" {
			hasProtection = true
		}
	}
	if !hasProtection {
		t.Error("expected finalizer to be retained when VPC delete fails")
	}

	// Clear VPC error and retry
	h.vpcClient.DeleteErr = nil
	result, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "del-err-pool"},
	})
	if err != nil {
		t.Fatalf("Reconcile should succeed after VPC delete recovery: %v", err)
	}
	if result.RequeueAfter != 0 {
		t.Error("expected no requeue after successful deletion")
	}

	// Finalizer should be removed
	p2 := h.k8sClient.getPool("del-err-pool")
	for _, f := range p2.Finalizers {
		if f == "storage.ibmcloud.io/pool-protection" {
			t.Error("expected finalizer to be removed after successful deletion")
		}
	}
}

func TestErrorRecovery_AllocateFromPoolWithDrainingShares(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with one draining share and one stable share
	drainingShare := newStableShare("share-drain", "s1", 1000, 0, 0)
	drainingShare.State = "draining"
	stableShare := newStableShare("share-stable", "s2", 1000, 0, 0)

	pool := newTestPool("drain-pool", "spread", 1000, drainingShare, stableShare)
	pool.Spec.AutoExpand = false
	h.k8sClient.addPool(pool)

	// Allocation should skip the draining share and use the stable one
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "drain-pool", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	// Verify allocation went to the stable share
	volCtx := resp.GetVolume().VolumeContext
	if volCtx["shareID"] != "share-stable" {
		t.Errorf("expected allocation on share-stable, got %q", volCtx["shareID"])
	}
}

func TestErrorRecovery_AllocateFromPoolWithCreatingShares(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with one creating share and one stable share
	creatingShare := newStableShare("share-creating", "s1", 1000, 0, 0)
	creatingShare.State = "creating"
	stableShare := newStableShare("share-stable", "s2", 1000, 500, 3)

	pool := newTestPool("creating-pool-alloc", "spread", 1000, creatingShare, stableShare)
	h.k8sClient.addPool(pool)

	// Allocation should use only the stable share
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "creating-pool-alloc", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}

	volCtx := resp.GetVolume().VolumeContext
	if volCtx["shareID"] != "share-stable" {
		t.Errorf("expected allocation on share-stable, got %q", volCtx["shareID"])
	}
}

func TestErrorRecovery_SequentialFailuresDoNotCorruptState(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("seq-err-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Fail 5 times with NFS error, then succeed 5 times
	h.nfsOps.MkdirErr = fmt.Errorf("NFS: connection refused")
	for i := 0; i < 5; i++ {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", i)
		_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "seq-err-pool", 10))
		if err == nil {
			t.Fatalf("expected error on attempt %d", i)
		}
	}

	// State should be clean after failures
	if h.k8sClient.subVolumeCount() != 0 {
		t.Errorf("expected 0 SubVolumes after failures, got %d", h.k8sClient.subVolumeCount())
	}
	p := h.k8sClient.getPool("seq-err-pool")
	if p.Status.TotalAllocatedGB != 0 {
		t.Errorf("expected TotalAllocatedGB=0 after failures, got %d", p.Status.TotalAllocatedGB)
	}

	// Now succeed
	h.nfsOps.MkdirErr = nil
	for i := 0; i < 5; i++ {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", i)
		_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "seq-err-pool", 10))
		if err != nil {
			t.Fatalf("CreateVolume %d failed: %v", i, err)
		}
	}

	// State should be correct
	if h.k8sClient.subVolumeCount() != 5 {
		t.Errorf("expected 5 SubVolumes, got %d", h.k8sClient.subVolumeCount())
	}
	p = h.k8sClient.getPool("seq-err-pool")
	if p.Status.TotalAllocatedGB != 50 {
		t.Errorf("expected TotalAllocatedGB=50, got %d", p.Status.TotalAllocatedGB)
	}
}
