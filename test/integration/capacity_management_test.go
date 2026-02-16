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
// Capacity Management: auto-expansion, capacity tracking across allocate/
// deallocate cycles, proactive expansion triggered by the reconciler.
// ---------------------------------------------------------------------------

func TestCapacity_TrackingAcrossAllocateDeallocateCycles(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("cap-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Phase 1: Allocate 5 volumes of varying sizes
	sizes := []int64{10, 20, 30, 40, 50}
	volumeIDs := make([]string, len(sizes))
	for i, size := range sizes {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", i)
		resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "cap-pool", size))
		if err != nil {
			t.Fatalf("CreateVolume %d failed: %v", i, err)
		}
		volumeIDs[i] = resp.GetVolume().VolumeId
	}

	// Check capacity: total allocated = 10+20+30+40+50 = 150
	p := h.k8sClient.getPool("cap-pool")
	if p.Status.TotalAllocatedGB != 150 {
		t.Errorf("after allocation: expected TotalAllocatedGB=150, got %d", p.Status.TotalAllocatedGB)
	}
	if p.Status.TotalPVCCount != 5 {
		t.Errorf("after allocation: expected TotalPVCCount=5, got %d", p.Status.TotalPVCCount)
	}
	if p.Status.Shares[0].AllocatedGB != 150 {
		t.Errorf("after allocation: expected share AllocatedGB=150, got %d", p.Status.Shares[0].AllocatedGB)
	}

	// Phase 2: Deallocate volumes 0 and 2 (10 and 30 GB)
	_, err := h.driver.DeleteVolume(ctx, deleteVolumeRequest(volumeIDs[0]))
	if err != nil {
		t.Fatalf("DeleteVolume 0 failed: %v", err)
	}
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(volumeIDs[2]))
	if err != nil {
		t.Fatalf("DeleteVolume 2 failed: %v", err)
	}

	// Check capacity: 150 - 10 - 30 = 110
	p = h.k8sClient.getPool("cap-pool")
	if p.Status.TotalAllocatedGB != 110 {
		t.Errorf("after partial dealloc: expected TotalAllocatedGB=110, got %d", p.Status.TotalAllocatedGB)
	}
	if p.Status.TotalPVCCount != 3 {
		t.Errorf("after partial dealloc: expected TotalPVCCount=3, got %d", p.Status.TotalPVCCount)
	}
	if p.Status.Shares[0].AllocatedGB != 110 {
		t.Errorf("after partial dealloc: expected share AllocatedGB=110, got %d", p.Status.Shares[0].AllocatedGB)
	}

	// Phase 3: Allocate 2 new volumes (60 and 70 GB)
	for i, size := range []int64{60, 70} {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-aaa%09d", i)
		_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "cap-pool", size))
		if err != nil {
			t.Fatalf("CreateVolume new %d failed: %v", i, err)
		}
	}

	// Check: 110 + 60 + 70 = 240
	p = h.k8sClient.getPool("cap-pool")
	if p.Status.TotalAllocatedGB != 240 {
		t.Errorf("after realloc: expected TotalAllocatedGB=240, got %d", p.Status.TotalAllocatedGB)
	}
	if p.Status.TotalPVCCount != 5 {
		t.Errorf("after realloc: expected TotalPVCCount=5, got %d", p.Status.TotalPVCCount)
	}

	// Phase 4: Deallocate remaining
	remainingIDs := []string{volumeIDs[1], volumeIDs[3], volumeIDs[4]}
	for i, id := range remainingIDs {
		_, err := h.driver.DeleteVolume(ctx, deleteVolumeRequest(id))
		if err != nil {
			t.Fatalf("DeleteVolume remaining %d failed: %v", i, err)
		}
	}
	// Also delete the newly allocated
	for _, name := range h.k8sClient.subVolumeNames() {
		sv := h.k8sClient.getSubVolume(name)
		vid := fmt.Sprintf("cap-pool/%s/%s", sv.Spec.ShareID, sv.Spec.PVName)
		_, err := h.driver.DeleteVolume(ctx, deleteVolumeRequest(vid))
		if err != nil {
			t.Fatalf("DeleteVolume %s failed: %v", name, err)
		}
	}

	p = h.k8sClient.getPool("cap-pool")
	if p.Status.TotalAllocatedGB != 0 {
		t.Errorf("after full cleanup: expected TotalAllocatedGB=0, got %d", p.Status.TotalAllocatedGB)
	}
	if p.Status.TotalPVCCount != 0 {
		t.Errorf("after full cleanup: expected TotalPVCCount=0, got %d", p.Status.TotalPVCCount)
	}
}

func TestCapacity_AutoExpandTriggeredByAllocation(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with 100 GB, auto-expand enabled
	pool := newTestPool("auto-expand", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	pool.Spec.AutoExpand = true
	pool.Spec.MaxShares = 5
	h.k8sClient.addPool(pool)

	// Allocate 10 volumes of 10 GB each (fills share-1 exactly)
	for i := 0; i < 10; i++ {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", i)
		_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "auto-expand", 10))
		if err != nil {
			t.Fatalf("CreateVolume %d failed: %v", i, err)
		}
	}

	// Next allocation should trigger auto-expand
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-a00000000000"
	_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "auto-expand", 10))
	if err != nil {
		t.Fatalf("CreateVolume (trigger expand) failed: %v", err)
	}

	// VPC should have created a new share
	if h.vpcClient.CreateCalls != 1 {
		t.Errorf("expected 1 VPC create call for auto-expand, got %d", h.vpcClient.CreateCalls)
	}

	// Pool should now have 2 shares
	p := h.k8sClient.getPool("auto-expand")
	if len(p.Status.Shares) != 2 {
		t.Errorf("expected 2 shares after auto-expand, got %d", len(p.Status.Shares))
	}

	// Total capacity should be 200 GB (100 original + 100 new)
	if p.Status.TotalCapacityGB != 200 {
		t.Errorf("expected TotalCapacityGB=200, got %d", p.Status.TotalCapacityGB)
	}
}

func TestCapacity_AutoExpandMaxSharesReached(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with maxShares=1, fully allocated
	pool := newTestPool("max-shares", "spread", 100,
		newStableShare("share-1", "s1", 100, 100, 10),
	)
	pool.Spec.AutoExpand = true
	pool.Spec.MaxShares = 1
	h.k8sClient.addPool(pool)

	// Should fail with ResourceExhausted
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "max-shares", 10))

	assertGRPCCode(t, err, codes.ResourceExhausted)
}

func TestCapacity_ProactiveExpansionViaReconciler(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with one share at 90% utilization
	pool := newTestPool("proactive-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	pool.Spec.ExpandThresholdPercent = 80
	pool.Spec.AutoExpand = true
	pool.Spec.MaxShares = 5
	pool.Spec.InitialShares = 1
	h.k8sClient.addPool(pool)

	// Create the share in VPC so health check works
	h.vpcClient.CreateFileShare(ctx, ibmcloud.CreateShareInput{
		Name:    "s1",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  1000,
	})

	// Allocate 900 GB worth of volumes (9 x 100 GB)
	for i := 0; i < 9; i++ {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", i)
		_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "proactive-pool", 100))
		if err != nil {
			t.Fatalf("CreateVolume %d failed: %v", i, err)
		}
	}

	// Reset VPC create counter (we want to count only reconciler-triggered creates)
	vpcCreatesBefore := h.vpcClient.CreateCalls

	// Run reconciler — metrics reconciliation will compute 90% utilization,
	// and proactive expansion should trigger
	reconciler := newPoolReconciler(h)
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "proactive-pool"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	newCreates := h.vpcClient.CreateCalls - vpcCreatesBefore
	if newCreates != 1 {
		t.Errorf("expected 1 new VPC create call for proactive expansion, got %d", newCreates)
	}

	// Pool should now have 2 shares
	p := h.k8sClient.getPool("proactive-pool")
	if len(p.Status.Shares) != 2 {
		t.Errorf("expected 2 shares after proactive expansion, got %d", len(p.Status.Shares))
	}
}

func TestCapacity_ProactiveExpansionNotTriggeredBelowThreshold(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool at 50% utilization (below 80% threshold)
	pool := newTestPool("below-threshold", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	pool.Spec.ExpandThresholdPercent = 80
	pool.Spec.AutoExpand = true
	pool.Spec.MaxShares = 5
	pool.Spec.InitialShares = 1
	h.k8sClient.addPool(pool)

	h.vpcClient.CreateFileShare(ctx, ibmcloud.CreateShareInput{
		Name:    "s1",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  1000,
	})

	// Allocate 500 GB (50%)
	for i := 0; i < 5; i++ {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", i)
		_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "below-threshold", 100))
		if err != nil {
			t.Fatalf("CreateVolume %d failed: %v", i, err)
		}
	}

	vpcCreatesBefore := h.vpcClient.CreateCalls

	reconciler := newPoolReconciler(h)
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "below-threshold"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	newCreates := h.vpcClient.CreateCalls - vpcCreatesBefore
	if newCreates != 0 {
		t.Errorf("expected 0 new VPC create calls (below threshold), got %d", newCreates)
	}
}

func TestCapacity_ExpandVolumeWithinShareCapacity(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("expand-cap", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Allocate a 10 GB volume
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "expand-cap", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	volumeID := resp.GetVolume().VolumeId

	// Expand to 100 GB (within share capacity)
	expandReq := csiExpandRequest(volumeID, 100)
	expandResp, err := h.driver.ControllerExpandVolume(ctx, &expandReq)
	if err != nil {
		t.Fatalf("ControllerExpandVolume failed: %v", err)
	}
	if expandResp.CapacityBytes != gbToBytes(100) {
		t.Errorf("expected capacity %d, got %d", gbToBytes(100), expandResp.CapacityBytes)
	}

	// Verify SubVolume updated
	sv := h.k8sClient.getSubVolume(pvName)
	if sv.Spec.RequestedGB != 100 {
		t.Errorf("expected RequestedGB=100, got %d", sv.Spec.RequestedGB)
	}

	// Verify pool capacity tracked correctly
	p := h.k8sClient.getPool("expand-cap")
	if p.Status.TotalAllocatedGB != 100 {
		t.Errorf("expected TotalAllocatedGB=100, got %d", p.Status.TotalAllocatedGB)
	}
}

func TestCapacity_ExpandVolumeExceedsShareCapacity(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("exceed-cap", "spread", 100,
		newStableShare("share-1", "s1", 100, 50, 5),
	)
	h.k8sClient.addPool(pool)

	// Pre-existing SubVolume
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           "exceed-cap",
			ShareID:            "share-1",
			ShareMountTargetIP: "10.240.0.1",
			SubPath:            "/pvcs/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab",
			RequestedGB:        10,
			PVName:             "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab",
			PVCName:            "my-pvc",
			PVCNamespace:       "default",
		},
	}
	h.k8sClient.CreateSubVolume(ctx, sv)

	volumeID := "exceed-cap/share-1/pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	expandReq := csiExpandRequest(volumeID, 500) // 490 GB delta, only 50 GB free
	_, err := h.driver.ControllerExpandVolume(ctx, &expandReq)

	assertGRPCCode(t, err, codes.ResourceExhausted)
}

func TestCapacity_MultiShareCapacityTracking(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with 3 shares of different sizes
	pool := newTestPool("multi-share-cap", "spread", 500,
		newStableShare("share-1", "s1", 500, 0, 0),
		newStableShare("share-2", "s2", 300, 0, 0),
		newStableShare("share-3", "s3", 200, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Allocate volumes targeting different shares via spread
	for i := 0; i < 6; i++ {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", i)
		_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "multi-share-cap", 50))
		if err != nil {
			t.Fatalf("CreateVolume %d failed: %v", i, err)
		}
	}

	// Verify total tracking: 6 * 50 = 300
	p := h.k8sClient.getPool("multi-share-cap")
	if p.Status.TotalAllocatedGB != 300 {
		t.Errorf("expected TotalAllocatedGB=300, got %d", p.Status.TotalAllocatedGB)
	}
	if p.Status.TotalPVCCount != 6 {
		t.Errorf("expected TotalPVCCount=6, got %d", p.Status.TotalPVCCount)
	}

	// Per-share tracking: sum of per-share allocated should equal total
	var sumAllocated int64
	var sumPVCs int32
	for _, share := range p.Status.Shares {
		sumAllocated += share.AllocatedGB
		sumPVCs += share.PVCCount
	}
	if sumAllocated != 300 {
		t.Errorf("expected sum of share allocations = 300, got %d", sumAllocated)
	}
	if sumPVCs != 6 {
		t.Errorf("expected sum of share PVC counts = 6, got %d", sumPVCs)
	}
}

func TestCapacity_MetricsReconciliationCorrectsDrift(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with deliberately incorrect status
	pool := newTestPool("drift-pool", "spread", 1000,
		newStableShare("share-1", "s1", 1000, 999, 99), // wrong values
	)
	pool.Spec.InitialShares = 1
	h.k8sClient.addPool(pool)

	// Create VPC share for health check
	h.vpcClient.CreateFileShare(ctx, ibmcloud.CreateShareInput{
		Name:    "s1",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  1000,
	})

	// Allocate through the driver to create real SubVolumes
	for i := 0; i < 3; i++ {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", i)
		_, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "drift-pool", 10))
		if err != nil {
			t.Fatalf("CreateVolume %d failed: %v", i, err)
		}
	}

	// Intentionally corrupt pool status again to simulate drift
	drifted := h.k8sClient.getPool("drift-pool")
	drifted.Status.TotalAllocatedGB = 555
	drifted.Status.TotalPVCCount = 55
	drifted.Status.Shares[0].AllocatedGB = 555
	drifted.Status.Shares[0].PVCCount = 55
	h.k8sClient.addPool(drifted)

	// Run reconciler
	reconciler := newPoolReconciler(h)
	_, err := reconciler.Reconcile(ctx, ctrl.Request{
		NamespacedName: types.NamespacedName{Name: "drift-pool"},
	})
	if err != nil {
		t.Fatalf("Reconcile failed: %v", err)
	}

	// Verify metrics were corrected from SubVolume CRs (3 * 10 = 30)
	corrected := h.k8sClient.getPool("drift-pool")
	if corrected.Status.TotalAllocatedGB != 30 {
		t.Errorf("expected TotalAllocatedGB=30 after reconciliation, got %d", corrected.Status.TotalAllocatedGB)
	}
	if corrected.Status.TotalPVCCount != 3 {
		t.Errorf("expected TotalPVCCount=3 after reconciliation, got %d", corrected.Status.TotalPVCCount)
	}
}

func TestCapacity_TierBasedAutoExpand(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with two tiers
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "tier-cap-pool"},
		Spec: v1alpha1.FileSharePoolSpec{
			Zone:               "us-south-1",
			Profile:            "dp2",
			ShareSizeGB:        1000,
			MaxShares:          10,
			InitialShares:      1,
			AutoExpand:         true,
			AllocationStrategy: "spread",
			DefaultPermissions: "0755",
			Tiers: []v1alpha1.ShareTier{
				{Name: "standard", Profile: "dp2", ShareSizeGB: 100, MaxShares: 5, InitialShares: 1},
				{Name: "premium", Profile: "custom", ShareSizeGB: 200, MaxShares: 3, InitialShares: 1},
			},
		},
		Status: v1alpha1.FileSharePoolStatus{
			Phase: "Ready",
			Shares: []v1alpha1.PoolShareStatus{
				{ShareID: "share-std-1", ShareName: "std1", MountTargetIP: "10.0.0.1", MountTargetID: "mt-1",
					TotalGB: 100, AllocatedGB: 0, PVCCount: 0, State: "stable", Tier: "standard", Zone: "us-south-1"},
				{ShareID: "share-prem-1", ShareName: "prem1", MountTargetIP: "10.0.0.2", MountTargetID: "mt-2",
					TotalGB: 200, AllocatedGB: 0, PVCCount: 0, State: "stable", Tier: "premium", Zone: "us-south-1"},
			},
			ShareCount:      2,
			TotalCapacityGB: 300,
		},
	}
	h.k8sClient.addPool(pool)

	// Fill the standard tier share completely
	for i := 0; i < 10; i++ {
		pvName := fmt.Sprintf("pvc-a1b2c3d4-5678-90ab-cdef-%012d", i)
		req := createVolumeRequest(pvName, "tier-cap-pool", 10)
		req.Parameters["tier"] = "standard"
		_, err := h.driver.CreateVolume(ctx, req)
		if err != nil {
			t.Fatalf("CreateVolume %d failed: %v", i, err)
		}
	}

	// The next standard tier allocation should auto-expand
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-a00000000000"
	req := createVolumeRequest(pvName, "tier-cap-pool", 10)
	req.Parameters["tier"] = "standard"
	_, err := h.driver.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("CreateVolume (trigger expand) failed: %v", err)
	}

	// VPC should have created 1 new share for the standard tier
	if h.vpcClient.CreateCalls != 1 {
		t.Errorf("expected 1 VPC create call for standard tier auto-expand, got %d", h.vpcClient.CreateCalls)
	}

	// The premium tier should be unaffected
	p := h.k8sClient.getPool("tier-cap-pool")
	premiumShareCount := 0
	for _, s := range p.Status.Shares {
		if s.Tier == "premium" {
			premiumShareCount++
		}
	}
	if premiumShareCount != 1 {
		t.Errorf("expected 1 premium share (unchanged), got %d", premiumShareCount)
	}
}

