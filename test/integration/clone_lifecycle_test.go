//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
)

// ---------------------------------------------------------------------------
// Clone Lifecycle — Synchronous (same-share, small volume)
// ---------------------------------------------------------------------------

func TestClone_SyncCloneHappyPath(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("clone-sync", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Create source volume (5 GB — under default 10 GB sync threshold)
	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-sync", 5))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	// Clone to new volume
	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	cloneResp, err := h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clonePV, "clone-sync", 5, sourceVolumeID))
	if err != nil {
		t.Fatalf("CreateVolume (clone) failed: %v", err)
	}

	cloneVol := cloneResp.GetVolume()
	if cloneVol == nil {
		t.Fatal("expected volume in clone response")
	}

	// Verify ContentSource references source volume
	if cloneVol.ContentSource == nil {
		t.Fatal("expected ContentSource to be set")
	}
	volSource := cloneVol.ContentSource.GetVolume()
	if volSource == nil {
		t.Fatal("expected Volume content source")
	}
	if volSource.VolumeId != sourceVolumeID {
		t.Errorf("expected source volume ID %q, got %q", sourceVolumeID, volSource.VolumeId)
	}

	// Verify SubVolume CR exists with clone metadata
	sv := h.k8sClient.getSubVolume(clonePV)
	if sv == nil {
		t.Fatal("expected cloned SubVolume CR to exist")
	}
	if sv.Spec.SourceVolume != srcPV {
		t.Errorf("expected SourceVolume %q, got %q", srcPV, sv.Spec.SourceVolume)
	}
	if sv.Spec.SourceShareID != "share-1" {
		t.Errorf("expected SourceShareID 'share-1', got %q", sv.Spec.SourceShareID)
	}

	// Sync clone: CloneStatus should be Complete
	if sv.Status.CloneStatus != "Complete" {
		t.Errorf("expected CloneStatus 'Complete' for sync clone, got %q", sv.Status.CloneStatus)
	}
	if sv.Status.CloneProgress == nil {
		t.Fatal("expected CloneProgress to be set")
	}
	if sv.Status.CloneProgress.CompletedAt == nil {
		t.Error("expected CompletedAt to be set for sync clone")
	}

	// Verify NFS copy was performed
	if h.nfsOps.copyCount() != 1 {
		t.Errorf("expected 1 NFS copy for sync clone, got %d", h.nfsOps.copyCount())
	}

	// Verify pool capacity: 5 (source) + 5 (clone) = 10
	p := h.k8sClient.getPool("clone-sync")
	if p.Status.TotalAllocatedGB != 10 {
		t.Errorf("expected TotalAllocatedGB=10, got %d", p.Status.TotalAllocatedGB)
	}
}

func TestClone_SyncCloneSameShare(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with 2 shares — verify sync clone stays on source share
	pool := newTestPool("clone-same", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
		newStableShare("share-2", "s2", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Create source on share-1 (spread picks the first empty share)
	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-same", 5))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	// Verify source landed on a specific share
	srcSV := h.k8sClient.getSubVolume(srcPV)
	sourceShareID := srcSV.Spec.ShareID

	// Clone — sync path prefers source share
	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	_, err = h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clonePV, "clone-same", 5, sourceVolumeID))
	if err != nil {
		t.Fatalf("CreateVolume (clone) failed: %v", err)
	}

	cloneSV := h.k8sClient.getSubVolume(clonePV)
	if cloneSV.Spec.ShareID != sourceShareID {
		t.Errorf("expected clone on same share %q, got %q", sourceShareID, cloneSV.Spec.ShareID)
	}
	if cloneSV.Status.CloneStatus != "Complete" {
		t.Errorf("expected sync clone (Complete), got %q", cloneSV.Status.CloneStatus)
	}
}

// ---------------------------------------------------------------------------
// Clone Lifecycle — Asynchronous (large volume or cross-share)
// ---------------------------------------------------------------------------

func TestClone_AsyncCloneLargeVolume(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("clone-async", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Create source volume (20 GB — over default 10 GB threshold)
	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-async", 20))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	// Clone with default threshold (10 GB) — 20 GB source triggers async
	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	_, err = h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clonePV, "clone-async", 20, sourceVolumeID))
	if err != nil {
		t.Fatalf("CreateVolume (async clone) failed: %v", err)
	}

	sv := h.k8sClient.getSubVolume(clonePV)
	if sv == nil {
		t.Fatal("expected cloned SubVolume CR to exist")
	}

	// Async clone: CloneStatus should be Pending
	if sv.Status.CloneStatus != "Pending" {
		t.Errorf("expected CloneStatus 'Pending' for async clone, got %q", sv.Status.CloneStatus)
	}
	if sv.Status.Phase != "Cloning" {
		t.Errorf("expected Phase 'Cloning', got %q", sv.Status.Phase)
	}
	if sv.Status.CloneProgress == nil {
		t.Fatal("expected CloneProgress to be set")
	}
	if sv.Status.CloneProgress.TotalBytes != gbToBytes(20) {
		t.Errorf("expected TotalBytes %d, got %d", gbToBytes(20), sv.Status.CloneProgress.TotalBytes)
	}

	// NFS copy should NOT have happened (async defers the copy)
	if h.nfsOps.copyCount() != 0 {
		t.Errorf("expected 0 NFS copies for async clone, got %d", h.nfsOps.copyCount())
	}

	// Pool capacity still includes the clone allocation
	p := h.k8sClient.getPool("clone-async")
	if p.Status.TotalAllocatedGB != 40 { // 20 (source) + 20 (clone)
		t.Errorf("expected TotalAllocatedGB=40, got %d", p.Status.TotalAllocatedGB)
	}
}

func TestClone_AsyncCloneCrossShare(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Use binpack strategy to control share placement:
	// share-1 (20 GB) is smaller → binpack places source there first.
	// share-2 (100 GB) has room for the larger clone.
	pool := newTestPool("clone-cross", "binpack", 100,
		newStableShare("share-1", "s1", 20, 0, 0),
		newStableShare("share-2", "s2", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Binpack picks share-1 (least free = smallest total when both empty)
	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-cross", 5))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	srcSV := h.k8sClient.getSubVolume(srcPV)
	if srcSV.Spec.ShareID != "share-1" {
		t.Fatalf("expected source on share-1 (binpack), got %q", srcSV.Spec.ShareID)
	}

	// Clone 20 GB → share-1 has 15 free (not enough), falls to share-2 (100 free)
	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	_, err = h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clonePV, "clone-cross", 20, sourceVolumeID))
	if err != nil {
		t.Fatalf("CreateVolume (cross-share clone) failed: %v", err)
	}

	cloneSV := h.k8sClient.getSubVolume(clonePV)
	if cloneSV.Spec.ShareID == "share-1" {
		t.Error("expected clone on different share (cross-share), but got share-1")
	}

	// Cross-share clone is always async
	if cloneSV.Status.CloneStatus != "Pending" {
		t.Errorf("expected CloneStatus 'Pending' for cross-share clone, got %q", cloneSV.Status.CloneStatus)
	}
	if cloneSV.Status.Phase != "Cloning" {
		t.Errorf("expected Phase 'Cloning', got %q", cloneSV.Status.Phase)
	}

	// No NFS copy for async clone
	if h.nfsOps.copyCount() != 0 {
		t.Errorf("expected 0 NFS copies for cross-share async clone, got %d", h.nfsOps.copyCount())
	}
}

func TestClone_CustomSyncThreshold(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("clone-thresh", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Create 20 GB source
	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-thresh", 20))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	// Clone with threshold=50 → 20 GB is under 50 GB → sync
	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	_, err = h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequestWithThreshold(clonePV, "clone-thresh", 20, sourceVolumeID, 50))
	if err != nil {
		t.Fatalf("CreateVolume (clone with threshold) failed: %v", err)
	}

	sv := h.k8sClient.getSubVolume(clonePV)
	if sv.Status.CloneStatus != "Complete" {
		t.Errorf("expected sync clone (Complete) with threshold=50, got %q", sv.Status.CloneStatus)
	}

	// NFS copy should have happened (sync path)
	if h.nfsOps.copyCount() != 1 {
		t.Errorf("expected 1 NFS copy for sync clone, got %d", h.nfsOps.copyCount())
	}
}

// ---------------------------------------------------------------------------
// Clone — Idempotency
// ---------------------------------------------------------------------------

func TestClone_Idempotent(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("clone-idem", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-idem", 5))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	req := createVolumeFromCloneRequest(clonePV, "clone-idem", 5, sourceVolumeID)

	// First call
	resp1, err := h.driver.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("first clone call failed: %v", err)
	}

	// Second call (idempotent)
	resp2, err := h.driver.CreateVolume(ctx, req)
	if err != nil {
		t.Fatalf("second clone call failed: %v", err)
	}

	if resp1.GetVolume().VolumeId != resp2.GetVolume().VolumeId {
		t.Errorf("idempotent calls returned different volume IDs: %q vs %q",
			resp1.GetVolume().VolumeId, resp2.GetVolume().VolumeId)
	}

	// Should only have 2 SubVolumes (source + 1 clone)
	if h.k8sClient.subVolumeCount() != 2 {
		t.Errorf("expected 2 SubVolumes (source + clone), got %d", h.k8sClient.subVolumeCount())
	}
}

// ---------------------------------------------------------------------------
// Clone — Error Cases
// ---------------------------------------------------------------------------

func TestClone_SourceNotFound(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("clone-nf", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	_, err := h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clonePV, "clone-nf", 10,
			"clone-nf/share-1/pvc-nonexistent-0000-0000-0000-000000000000"))

	assertGRPCCode(t, err, codes.NotFound)
}

func TestClone_SizeTooSmall(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("clone-small", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Create 20 GB source
	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-small", 20))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	// Clone with 10 GB (less than source 20 GB) → should fail
	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	_, err = h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clonePV, "clone-small", 10, sourceVolumeID))

	assertGRPCCode(t, err, codes.Internal)
}

func TestClone_PoolExhausted(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Pool with 20 GB share, auto-expand disabled
	pool := newTestPool("clone-exhaust", "spread", 20,
		newStableShare("share-1", "s1", 20, 0, 0),
	)
	pool.Spec.AutoExpand = false
	h.k8sClient.addPool(pool)

	// Create 15 GB source → 5 GB free
	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-exhaust", 15))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	// Clone 15 GB → needs 15 GB but only 5 GB free → ResourceExhausted
	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	_, err = h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clonePV, "clone-exhaust", 15, sourceVolumeID))

	assertGRPCCode(t, err, codes.ResourceExhausted)
}

func TestClone_CopyFailure(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("clone-cpyerr", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-cpyerr", 5))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	// Inject CopyDir error
	h.nfsOps.CopyErr = fmt.Errorf("NFS: I/O error during copy")

	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	_, err = h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clonePV, "clone-cpyerr", 5, sourceVolumeID))

	assertGRPCCode(t, err, codes.Internal)

	// No clone SubVolume should have been created
	if h.k8sClient.subVolumeCount() != 1 { // only source
		t.Errorf("expected 1 SubVolume (source only), got %d", h.k8sClient.subVolumeCount())
	}

	// Pool capacity should be only source volume
	p := h.k8sClient.getPool("clone-cpyerr")
	if p.Status.TotalAllocatedGB != 5 {
		t.Errorf("expected TotalAllocatedGB=5 after copy failure, got %d", p.Status.TotalAllocatedGB)
	}

	// Clear error and retry → success
	h.nfsOps.CopyErr = nil
	_, err = h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clonePV, "clone-cpyerr", 5, sourceVolumeID))
	if err != nil {
		t.Fatalf("Clone should succeed after clearing CopyErr: %v", err)
	}

	if h.k8sClient.subVolumeCount() != 2 {
		t.Errorf("expected 2 SubVolumes after recovery, got %d", h.k8sClient.subVolumeCount())
	}
}

// ---------------------------------------------------------------------------
// Clone — Capacity Tracking
// ---------------------------------------------------------------------------

func TestClone_CapacityTracking(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("clone-cap", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Create 10 GB source
	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-cap", 10))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	p := h.k8sClient.getPool("clone-cap")
	if p.Status.TotalAllocatedGB != 10 {
		t.Errorf("after source: expected TotalAllocatedGB=10, got %d", p.Status.TotalAllocatedGB)
	}

	// Clone 1: 10 GB → total = 20
	clone1PV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	clone1Resp, err := h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clone1PV, "clone-cap", 10, sourceVolumeID))
	if err != nil {
		t.Fatalf("Clone 1 failed: %v", err)
	}

	p = h.k8sClient.getPool("clone-cap")
	if p.Status.TotalAllocatedGB != 20 {
		t.Errorf("after clone 1: expected TotalAllocatedGB=20, got %d", p.Status.TotalAllocatedGB)
	}

	// Clone 2: 15 GB (bigger than source) → total = 35
	clone2PV := "pvc-c3d4e5f6-7890-abcd-ef01-345678901234"
	_, err = h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clone2PV, "clone-cap", 15, sourceVolumeID))
	if err != nil {
		t.Fatalf("Clone 2 failed: %v", err)
	}

	p = h.k8sClient.getPool("clone-cap")
	if p.Status.TotalAllocatedGB != 35 {
		t.Errorf("after clone 2: expected TotalAllocatedGB=35, got %d", p.Status.TotalAllocatedGB)
	}

	// Delete clone 1 → total = 25
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(clone1Resp.GetVolume().VolumeId))
	if err != nil {
		t.Fatalf("DeleteVolume (clone 1) failed: %v", err)
	}

	p = h.k8sClient.getPool("clone-cap")
	if p.Status.TotalAllocatedGB != 25 {
		t.Errorf("after deleting clone 1: expected TotalAllocatedGB=25, got %d", p.Status.TotalAllocatedGB)
	}

	// Delete source → total = 15 (clone 2 remains)
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(sourceVolumeID))
	if err != nil {
		t.Fatalf("DeleteVolume (source) failed: %v", err)
	}

	p = h.k8sClient.getPool("clone-cap")
	if p.Status.TotalAllocatedGB != 15 {
		t.Errorf("after deleting source: expected TotalAllocatedGB=15, got %d", p.Status.TotalAllocatedGB)
	}
}

func TestClone_MultipleFromSameSource(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("clone-multi", "spread", 200,
		newStableShare("share-1", "s1", 200, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Create 5 GB source
	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-multi", 5))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	// Create 5 clones from the same source
	for i := 0; i < 5; i++ {
		clonePV := fmt.Sprintf("pvc-b2c3d4e5-6789-0abc-def0-%012d", i)
		_, err := h.driver.CreateVolume(ctx,
			createVolumeFromCloneRequest(clonePV, "clone-multi", 5, sourceVolumeID))
		if err != nil {
			t.Fatalf("Clone %d failed: %v", i, err)
		}
	}

	// 1 source + 5 clones = 6 SubVolumes
	if h.k8sClient.subVolumeCount() != 6 {
		t.Errorf("expected 6 SubVolumes, got %d", h.k8sClient.subVolumeCount())
	}

	// All sync clones should have been copied
	if h.nfsOps.copyCount() != 5 {
		t.Errorf("expected 5 NFS copies, got %d", h.nfsOps.copyCount())
	}

	// Capacity: 5 (source) + 5*5 (clones) = 30
	p := h.k8sClient.getPool("clone-multi")
	if p.Status.TotalAllocatedGB != 30 {
		t.Errorf("expected TotalAllocatedGB=30, got %d", p.Status.TotalAllocatedGB)
	}

	// All clones should reference the source
	for i := 0; i < 5; i++ {
		clonePV := fmt.Sprintf("pvc-b2c3d4e5-6789-0abc-def0-%012d", i)
		sv := h.k8sClient.getSubVolume(clonePV)
		if sv.Spec.SourceVolume != srcPV {
			t.Errorf("clone %d: expected SourceVolume %q, got %q", i, srcPV, sv.Spec.SourceVolume)
		}
	}
}

// ---------------------------------------------------------------------------
// Clone — Delete cloned volume
// ---------------------------------------------------------------------------

func TestClone_DeleteClonedVolume(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("clone-del", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-del", 10))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	cloneResp, err := h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clonePV, "clone-del", 10, sourceVolumeID))
	if err != nil {
		t.Fatalf("Clone failed: %v", err)
	}

	// Delete the clone
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(cloneResp.GetVolume().VolumeId))
	if err != nil {
		t.Fatalf("DeleteVolume (clone) failed: %v", err)
	}

	// Clone SubVolume should be gone
	if h.k8sClient.getSubVolume(clonePV) != nil {
		t.Error("expected cloned SubVolume to be deleted")
	}

	// Source should still exist
	if h.k8sClient.getSubVolume(srcPV) == nil {
		t.Error("expected source SubVolume to still exist")
	}

	// Capacity: only source remains
	p := h.k8sClient.getPool("clone-del")
	if p.Status.TotalAllocatedGB != 10 {
		t.Errorf("expected TotalAllocatedGB=10 after clone deletion, got %d", p.Status.TotalAllocatedGB)
	}

	// 1 SubVolume remains
	if h.k8sClient.subVolumeCount() != 1 {
		t.Errorf("expected 1 SubVolume remaining, got %d", h.k8sClient.subVolumeCount())
	}
}

// ---------------------------------------------------------------------------
// Clone — Expanded clone (bigger than source)
// ---------------------------------------------------------------------------

func TestClone_LargerThanSource(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("clone-bigger", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Create 5 GB source
	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "clone-bigger", 5))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	// Clone with 20 GB (bigger than 5 GB source) — should succeed
	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	cloneResp, err := h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clonePV, "clone-bigger", 20, sourceVolumeID))
	if err != nil {
		t.Fatalf("Clone with larger size failed: %v", err)
	}

	cloneVol := cloneResp.GetVolume()
	if cloneVol.CapacityBytes != gbToBytes(20) {
		t.Errorf("expected clone capacity %d, got %d", gbToBytes(20), cloneVol.CapacityBytes)
	}

	sv := h.k8sClient.getSubVolume(clonePV)
	if sv.Spec.RequestedGB != 20 {
		t.Errorf("expected RequestedGB=20, got %d", sv.Spec.RequestedGB)
	}

	// Capacity: 5 (source) + 20 (clone) = 25
	p := h.k8sClient.getPool("clone-bigger")
	if p.Status.TotalAllocatedGB != 25 {
		t.Errorf("expected TotalAllocatedGB=25, got %d", p.Status.TotalAllocatedGB)
	}
}
