//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"

	"google.golang.org/grpc/codes"
)

// ---------------------------------------------------------------------------
// Group Snapshot Lifecycle Tests
// ---------------------------------------------------------------------------

func TestGroupSnapshot_CreateAndDelete(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// 1. Create pool with 1 share (200 GB)
	testPool := newTestPool("gs-pool", "spread", 200,
		newStableShare("share-1", "s1", 200, 0, 0),
	)
	h.k8sClient.addPool(testPool)

	// 2. Create 2 volumes (10 GB each)
	pv1 := "pvc-d1000000-0000-0000-0000-000000000001"
	pv2 := "pvc-d1000000-0000-0000-0000-000000000002"

	vol1Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv1, "gs-pool", 10))
	if err != nil {
		t.Fatalf("CreateVolume 1 failed: %v", err)
	}
	vol2Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv2, "gs-pool", 10))
	if err != nil {
		t.Fatalf("CreateVolume 2 failed: %v", err)
	}
	vol1ID := vol1Resp.GetVolume().VolumeId
	vol2ID := vol2Resp.GetVolume().VolumeId

	// 3. Create group snapshot
	gsResp, err := h.driver.CreateVolumeGroupSnapshot(ctx, createVolumeGroupSnapshotRequest(
		"gs-test-1",
		[]string{vol1ID, vol2ID},
		map[string]string{"pool": "gs-pool"},
	))
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot failed: %v", err)
	}

	gs := gsResp.GetGroupSnapshot()
	if gs == nil {
		t.Fatal("expected group snapshot in response")
	}

	// 4. Verify response has 2 member snapshots
	if len(gs.Snapshots) != 2 {
		t.Errorf("expected 2 member snapshots, got %d", len(gs.Snapshots))
	}
	if !gs.ReadyToUse {
		t.Error("expected ReadyToUse=true")
	}

	expectedGroupID := "gs-pool/gs-test-1"
	if gs.GroupSnapshotId != expectedGroupID {
		t.Errorf("expected group snapshot ID %q, got %q", expectedGroupID, gs.GroupSnapshotId)
	}

	// Verify each member snapshot has correct GroupSnapshotId and SizeBytes
	for _, snap := range gs.Snapshots {
		if snap.GroupSnapshotId != expectedGroupID {
			t.Errorf("member snapshot has wrong GroupSnapshotId: expected %q, got %q",
				expectedGroupID, snap.GroupSnapshotId)
		}
		if snap.SizeBytes != gbToBytes(10) {
			t.Errorf("expected member sizeBytes %d, got %d", gbToBytes(10), snap.SizeBytes)
		}
		if !snap.ReadyToUse {
			t.Error("expected member readyToUse=true")
		}
	}

	// 5. Verify VolumeGroupSnapshot CR exists
	vgsCR := h.k8sClient.getGroupSnapshot("gs-test-1")
	if vgsCR == nil {
		t.Fatal("expected VolumeGroupSnapshot CR to exist")
	}
	if vgsCR.Status.Phase != "Complete" {
		t.Errorf("expected phase 'Complete', got %q", vgsCR.Status.Phase)
	}
	if vgsCR.Status.MemberCount != 2 {
		t.Errorf("expected MemberCount=2, got %d", vgsCR.Status.MemberCount)
	}
	if vgsCR.Status.ReadyCount != 2 {
		t.Errorf("expected ReadyCount=2, got %d", vgsCR.Status.ReadyCount)
	}

	// 6. Verify individual Snapshot CRs exist (2 snapshots)
	if h.k8sClient.snapshotCount() != 2 {
		t.Errorf("expected 2 Snapshot CRs, got %d", h.k8sClient.snapshotCount())
	}

	// 7. Verify pool capacity: 10 (vol1) + 10 (vol2) + 10 (snap1) + 10 (snap2) = 40
	p := h.k8sClient.getPool("gs-pool")
	if p.Status.TotalAllocatedGB != 40 {
		t.Errorf("expected TotalAllocatedGB=40, got %d", p.Status.TotalAllocatedGB)
	}

	// 8. Delete group snapshot
	_, err = h.driver.DeleteVolumeGroupSnapshot(ctx, deleteVolumeGroupSnapshotRequest(gs.GroupSnapshotId))
	if err != nil {
		t.Fatalf("DeleteVolumeGroupSnapshot failed: %v", err)
	}

	// 9. Verify group snapshot CR removed
	if h.k8sClient.groupSnapshotCount() != 0 {
		t.Error("expected VolumeGroupSnapshot CR to be deleted")
	}

	// 10. Verify individual Snapshot CRs removed
	if h.k8sClient.snapshotCount() != 0 {
		t.Errorf("expected 0 Snapshot CRs after group delete, got %d", h.k8sClient.snapshotCount())
	}

	// 11. Verify pool capacity decreased: only the 2 volumes remain = 20
	p = h.k8sClient.getPool("gs-pool")
	if p.Status.TotalAllocatedGB != 20 {
		t.Errorf("expected TotalAllocatedGB=20 after group snapshot deletion, got %d", p.Status.TotalAllocatedGB)
	}
}

func TestGroupSnapshot_CreateIdempotent(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	testPool := newTestPool("gs-idem", "spread", 200,
		newStableShare("share-1", "s1", 200, 0, 0),
	)
	h.k8sClient.addPool(testPool)

	pv1 := "pvc-d1000000-0000-0000-0000-000000000001"
	pv2 := "pvc-d1000000-0000-0000-0000-000000000002"

	vol1Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv1, "gs-idem", 10))
	if err != nil {
		t.Fatalf("CreateVolume 1 failed: %v", err)
	}
	vol2Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv2, "gs-idem", 10))
	if err != nil {
		t.Fatalf("CreateVolume 2 failed: %v", err)
	}
	vol1ID := vol1Resp.GetVolume().VolumeId
	vol2ID := vol2Resp.GetVolume().VolumeId

	sourceIDs := []string{vol1ID, vol2ID}
	params := map[string]string{"pool": "gs-idem"}

	// First create
	resp1, err := h.driver.CreateVolumeGroupSnapshot(ctx, createVolumeGroupSnapshotRequest("gs-idem-1", sourceIDs, params))
	if err != nil {
		t.Fatalf("first CreateVolumeGroupSnapshot failed: %v", err)
	}

	// Second create with same name (idempotent)
	resp2, err := h.driver.CreateVolumeGroupSnapshot(ctx, createVolumeGroupSnapshotRequest("gs-idem-1", sourceIDs, params))
	if err != nil {
		t.Fatalf("second CreateVolumeGroupSnapshot failed: %v", err)
	}

	// Should return the same group snapshot ID
	if resp1.GetGroupSnapshot().GroupSnapshotId != resp2.GetGroupSnapshot().GroupSnapshotId {
		t.Errorf("idempotent calls returned different group snapshot IDs: %q vs %q",
			resp1.GetGroupSnapshot().GroupSnapshotId, resp2.GetGroupSnapshot().GroupSnapshotId)
	}

	// Should still have exactly 1 VolumeGroupSnapshot CR
	if h.k8sClient.groupSnapshotCount() != 1 {
		t.Errorf("expected 1 VolumeGroupSnapshot CR (idempotent), got %d", h.k8sClient.groupSnapshotCount())
	}

	// Should have 2 member Snapshot CRs (not 4)
	if h.k8sClient.snapshotCount() != 2 {
		t.Errorf("expected 2 Snapshot CRs (idempotent), got %d", h.k8sClient.snapshotCount())
	}
}

func TestGroupSnapshot_EmptySourceList(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	testPool := newTestPool("gs-empty", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(testPool)

	// CreateVolumeGroupSnapshot with no source volume IDs
	_, err := h.driver.CreateVolumeGroupSnapshot(ctx, createVolumeGroupSnapshotRequest(
		"gs-empty-1",
		[]string{},
		map[string]string{"pool": "gs-empty"},
	))

	assertGRPCCode(t, err, codes.InvalidArgument)
}

func TestGroupSnapshot_SourceVolumeNotFound(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	testPool := newTestPool("gs-notfound", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(testPool)

	// Create one real volume
	pv1 := "pvc-d1000000-0000-0000-0000-000000000001"
	vol1Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv1, "gs-notfound", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	vol1ID := vol1Resp.GetVolume().VolumeId

	// Try group snapshot with one real and one non-existent volume
	_, err = h.driver.CreateVolumeGroupSnapshot(ctx, createVolumeGroupSnapshotRequest(
		"gs-notfound-1",
		[]string{vol1ID, "gs-notfound/share-1/pvc-d1000000-0000-0000-0000-999999999999"},
		map[string]string{"pool": "gs-notfound"},
	))

	assertGRPCCode(t, err, codes.NotFound)
}

func TestGroupSnapshot_AbortOnFailure(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	testPool := newTestPool("gs-abort", "spread", 200,
		newStableShare("share-1", "s1", 200, 0, 0),
	)
	h.k8sClient.addPool(testPool)

	// Create 2 volumes
	pv1 := "pvc-d1000000-0000-0000-0000-000000000001"
	pv2 := "pvc-d1000000-0000-0000-0000-000000000002"

	vol1Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv1, "gs-abort", 10))
	if err != nil {
		t.Fatalf("CreateVolume 1 failed: %v", err)
	}
	vol2Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv2, "gs-abort", 10))
	if err != nil {
		t.Fatalf("CreateVolume 2 failed: %v", err)
	}
	vol1ID := vol1Resp.GetVolume().VolumeId
	vol2ID := vol2Resp.GetVolume().VolumeId

	// Inject NFS CopyErr -- all copy operations will fail
	h.nfsOps.CopyErr = fmt.Errorf("NFS: disk full")

	// Create group snapshot with Abort policy
	_, err = h.driver.CreateVolumeGroupSnapshot(ctx, createVolumeGroupSnapshotRequest(
		"gs-abort-1",
		[]string{vol1ID, vol2ID},
		map[string]string{"pool": "gs-abort", "failurePolicy": "Abort"},
	))

	// Expect Internal error (ErrGroupSnapshotFailed maps to codes.Internal)
	assertGRPCCode(t, err, codes.Internal)

	// Verify no partial snapshots remain (rollback should have cleaned them up)
	if h.k8sClient.snapshotCount() != 0 {
		t.Errorf("expected 0 Snapshot CRs after Abort rollback, got %d", h.k8sClient.snapshotCount())
	}

	// Verify the group snapshot CR exists and is Failed
	vgsCR := h.k8sClient.getGroupSnapshot("gs-abort-1")
	if vgsCR == nil {
		t.Fatal("expected VolumeGroupSnapshot CR to exist after failure")
	}
	if vgsCR.Status.Phase != "Failed" {
		t.Errorf("expected phase 'Failed', got %q", vgsCR.Status.Phase)
	}

	// Pool capacity should only include the 2 volumes (no snapshot overhead)
	p := h.k8sClient.getPool("gs-abort")
	if p.Status.TotalAllocatedGB != 20 {
		t.Errorf("expected TotalAllocatedGB=20 after abort, got %d", p.Status.TotalAllocatedGB)
	}
}

func TestGroupSnapshot_ContinueOnFailure(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	testPool := newTestPool("gs-continue", "spread", 200,
		newStableShare("share-1", "s1", 200, 0, 0),
	)
	h.k8sClient.addPool(testPool)

	// Create 2 volumes
	pv1 := "pvc-d1000000-0000-0000-0000-000000000001"
	pv2 := "pvc-d1000000-0000-0000-0000-000000000002"

	vol1Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv1, "gs-continue", 10))
	if err != nil {
		t.Fatalf("CreateVolume 1 failed: %v", err)
	}
	vol2Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv2, "gs-continue", 10))
	if err != nil {
		t.Fatalf("CreateVolume 2 failed: %v", err)
	}
	vol1ID := vol1Resp.GetVolume().VolumeId
	vol2ID := vol2Resp.GetVolume().VolumeId

	// Inject NFS CopyErr after 1 successful copy -- first snapshot succeeds, second fails
	h.nfsOps.CopyErr = fmt.Errorf("NFS: I/O error")
	h.nfsOps.CopyErrAfterN = 1

	// Create group snapshot with Continue policy
	gsResp, err := h.driver.CreateVolumeGroupSnapshot(ctx, createVolumeGroupSnapshotRequest(
		"gs-continue-1",
		[]string{vol1ID, vol2ID},
		map[string]string{"pool": "gs-continue", "failurePolicy": "Continue"},
	))
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot with Continue policy should not error, got: %v", err)
	}

	gs := gsResp.GetGroupSnapshot()
	if gs == nil {
		t.Fatal("expected group snapshot in response")
	}

	// ReadyToUse should be false for partial failure
	if gs.ReadyToUse {
		t.Error("expected ReadyToUse=false for partial failure")
	}

	// Should have 1 successful member snapshot in the response
	if len(gs.Snapshots) != 1 {
		t.Errorf("expected 1 successful member snapshot, got %d", len(gs.Snapshots))
	}

	// Verify the VolumeGroupSnapshot CR shows PartialFailure
	vgsCR := h.k8sClient.getGroupSnapshot("gs-continue-1")
	if vgsCR == nil {
		t.Fatal("expected VolumeGroupSnapshot CR to exist")
	}
	if vgsCR.Status.Phase != "PartialFailure" {
		t.Errorf("expected phase 'PartialFailure', got %q", vgsCR.Status.Phase)
	}
	if vgsCR.Status.ReadyCount != 1 {
		t.Errorf("expected ReadyCount=1, got %d", vgsCR.Status.ReadyCount)
	}
	if vgsCR.Status.FailedCount != 1 {
		t.Errorf("expected FailedCount=1, got %d", vgsCR.Status.FailedCount)
	}

	// 1 individual Snapshot CR should exist (the successful one)
	if h.k8sClient.snapshotCount() != 1 {
		t.Errorf("expected 1 Snapshot CR for partial success, got %d", h.k8sClient.snapshotCount())
	}
}

func TestGroupSnapshot_CapacityTracking(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	testPool := newTestPool("gs-cap", "spread", 200,
		newStableShare("share-1", "s1", 200, 0, 0),
	)
	h.k8sClient.addPool(testPool)

	// Create 2 volumes (10 GB each)
	pv1 := "pvc-d1000000-0000-0000-0000-000000000001"
	pv2 := "pvc-d1000000-0000-0000-0000-000000000002"

	vol1Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv1, "gs-cap", 10))
	if err != nil {
		t.Fatalf("CreateVolume 1 failed: %v", err)
	}
	vol2Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv2, "gs-cap", 10))
	if err != nil {
		t.Fatalf("CreateVolume 2 failed: %v", err)
	}
	vol1ID := vol1Resp.GetVolume().VolumeId
	vol2ID := vol2Resp.GetVolume().VolumeId

	// Verify baseline: 20 GB allocated (2 volumes * 10 GB)
	p := h.k8sClient.getPool("gs-cap")
	if p.Status.TotalAllocatedGB != 20 {
		t.Errorf("baseline: expected TotalAllocatedGB=20, got %d", p.Status.TotalAllocatedGB)
	}

	// Create group snapshot (2 snapshots * 10 GB = +20 GB)
	gsResp, err := h.driver.CreateVolumeGroupSnapshot(ctx, createVolumeGroupSnapshotRequest(
		"gs-cap-1",
		[]string{vol1ID, vol2ID},
		map[string]string{"pool": "gs-cap"},
	))
	if err != nil {
		t.Fatalf("CreateVolumeGroupSnapshot failed: %v", err)
	}

	// Verify: 20 (volumes) + 20 (group snapshots) = 40 GB
	p = h.k8sClient.getPool("gs-cap")
	if p.Status.TotalAllocatedGB != 40 {
		t.Errorf("after group snapshot: expected TotalAllocatedGB=40, got %d", p.Status.TotalAllocatedGB)
	}

	// Delete group snapshot
	_, err = h.driver.DeleteVolumeGroupSnapshot(ctx,
		deleteVolumeGroupSnapshotRequest(gsResp.GetGroupSnapshot().GroupSnapshotId))
	if err != nil {
		t.Fatalf("DeleteVolumeGroupSnapshot failed: %v", err)
	}

	// Verify: back to 20 GB (only volumes remain)
	p = h.k8sClient.getPool("gs-cap")
	if p.Status.TotalAllocatedGB != 20 {
		t.Errorf("after group snapshot deletion: expected TotalAllocatedGB=20, got %d", p.Status.TotalAllocatedGB)
	}

	// Delete volumes
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(vol1ID))
	if err != nil {
		t.Fatalf("DeleteVolume 1 failed: %v", err)
	}
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(vol2ID))
	if err != nil {
		t.Fatalf("DeleteVolume 2 failed: %v", err)
	}

	// Verify: 0 GB
	p = h.k8sClient.getPool("gs-cap")
	if p.Status.TotalAllocatedGB != 0 {
		t.Errorf("after deleting everything: expected TotalAllocatedGB=0, got %d", p.Status.TotalAllocatedGB)
	}
}

func TestGroupSnapshot_DeleteNonExistent(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Delete non-existent group snapshot -- should succeed (idempotent)
	_, err := h.driver.DeleteVolumeGroupSnapshot(ctx,
		deleteVolumeGroupSnapshotRequest("gs-pool/gs-nonexistent"))
	if err != nil {
		t.Fatalf("DeleteVolumeGroupSnapshot for non-existent should be idempotent, got: %v", err)
	}
}
