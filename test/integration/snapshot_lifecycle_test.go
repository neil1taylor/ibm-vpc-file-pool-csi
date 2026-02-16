//go:build integration

package integration

import (
	"context"
	"fmt"
	"testing"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
)

// ---------------------------------------------------------------------------
// Snapshot Lifecycle Tests
// ---------------------------------------------------------------------------

func TestSnapshot_CreateAndDelete(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// 1. Create pool with 1 share (100 GB)
	pool := newTestPool("snap-pool", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// 2. CreateVolume (10 GB)
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	volResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "snap-pool", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	volumeID := volResp.GetVolume().VolumeId

	// 3. CreateSnapshot
	snapResp, err := h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-1", volumeID))
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}

	snap := snapResp.GetSnapshot()
	if snap == nil {
		t.Fatal("expected snapshot in response")
	}

	// Verify snapshot response fields
	expectedSnapshotID := "snap-pool/share-1/snap-1"
	if snap.SnapshotId != expectedSnapshotID {
		t.Errorf("expected snapshotID %q, got %q", expectedSnapshotID, snap.SnapshotId)
	}
	if snap.SourceVolumeId != volumeID {
		t.Errorf("expected sourceVolumeId %q, got %q", volumeID, snap.SourceVolumeId)
	}
	if snap.SizeBytes != gbToBytes(10) {
		t.Errorf("expected sizeBytes %d, got %d", gbToBytes(10), snap.SizeBytes)
	}
	if !snap.ReadyToUse {
		t.Error("expected readyToUse=true")
	}

	// 4. Verify Snapshot CR exists
	snapCR := h.k8sClient.getSnapshot("snap-1")
	if snapCR == nil {
		t.Fatal("expected Snapshot CR to exist")
	}
	if snapCR.Spec.SourceSubVolume != pvName {
		t.Errorf("expected sourceSubVolume %q, got %q", pvName, snapCR.Spec.SourceSubVolume)
	}

	// 5. Verify pool capacity increased by snapshot size
	p := h.k8sClient.getPool("snap-pool")
	if p.Status.TotalAllocatedGB != 20 { // 10 (volume) + 10 (snapshot)
		t.Errorf("expected TotalAllocatedGB=20, got %d", p.Status.TotalAllocatedGB)
	}

	// 6. DeleteSnapshot
	_, err = h.driver.DeleteSnapshot(ctx, deleteSnapshotRequest(snap.SnapshotId))
	if err != nil {
		t.Fatalf("DeleteSnapshot failed: %v", err)
	}

	// 7. Verify Snapshot CR removed
	if h.k8sClient.snapshotCount() != 0 {
		t.Error("expected Snapshot CR to be deleted")
	}

	// 8. Verify pool capacity decreased back
	p = h.k8sClient.getPool("snap-pool")
	if p.Status.TotalAllocatedGB != 10 { // only the volume remains
		t.Errorf("expected TotalAllocatedGB=10 after snapshot deletion, got %d", p.Status.TotalAllocatedGB)
	}
}

func TestSnapshot_CreateIdempotent(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("snap-idem", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	volResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "snap-idem", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	volumeID := volResp.GetVolume().VolumeId

	// First CreateSnapshot
	resp1, err := h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-idem-1", volumeID))
	if err != nil {
		t.Fatalf("first CreateSnapshot failed: %v", err)
	}

	// Second CreateSnapshot with same name/source (idempotent)
	resp2, err := h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-idem-1", volumeID))
	if err != nil {
		t.Fatalf("second CreateSnapshot failed: %v", err)
	}

	// Should return the same snapshot ID
	if resp1.GetSnapshot().SnapshotId != resp2.GetSnapshot().SnapshotId {
		t.Errorf("idempotent calls returned different snapshot IDs: %q vs %q",
			resp1.GetSnapshot().SnapshotId, resp2.GetSnapshot().SnapshotId)
	}

	// Should still only have 1 Snapshot CR
	if h.k8sClient.snapshotCount() != 1 {
		t.Errorf("expected 1 Snapshot CR (idempotent), got %d", h.k8sClient.snapshotCount())
	}
}

func TestSnapshot_SourceNotFound(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("snap-notfound", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// CreateSnapshot with non-existent sourceVolumeId
	_, err := h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-x",
		"snap-notfound/share-1/pvc-nonexistent-0000-0000-0000-000000000000"))

	assertGRPCCode(t, err, codes.NotFound)
}

func TestSnapshot_DeleteIdempotent(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// DeleteSnapshot for non-existent snapshot → should succeed (idempotent)
	_, err := h.driver.DeleteSnapshot(ctx, deleteSnapshotRequest("snap-pool/share-1/snap-nonexistent"))
	if err != nil {
		t.Fatalf("DeleteSnapshot for non-existent snapshot should be idempotent, got: %v", err)
	}
}

func TestSnapshot_MultipleSnapshotsFromSameSource(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("snap-multi", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Create volume (10 GB)
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	volResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "snap-multi", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	volumeID := volResp.GetVolume().VolumeId

	// Create 3 snapshots from the same volume
	snapshotIDs := make([]string, 3)
	for i := 0; i < 3; i++ {
		snapName := fmt.Sprintf("snap-multi-%d", i)
		resp, err := h.driver.CreateSnapshot(ctx, createSnapshotRequest(snapName, volumeID))
		if err != nil {
			t.Fatalf("CreateSnapshot %d failed: %v", i, err)
		}
		snapshotIDs[i] = resp.GetSnapshot().SnapshotId
	}

	// Verify all 3 Snapshot CRs exist
	if h.k8sClient.snapshotCount() != 3 {
		t.Errorf("expected 3 Snapshot CRs, got %d", h.k8sClient.snapshotCount())
	}

	// Verify pool allocatedGB = 10 (volume) + 30 (3 * 10 GB snapshots) = 40 GB
	p := h.k8sClient.getPool("snap-multi")
	if p.Status.TotalAllocatedGB != 40 {
		t.Errorf("expected TotalAllocatedGB=40, got %d", p.Status.TotalAllocatedGB)
	}

	// Delete 1 snapshot → verify allocatedGB = 30 GB
	_, err = h.driver.DeleteSnapshot(ctx, deleteSnapshotRequest(snapshotIDs[0]))
	if err != nil {
		t.Fatalf("DeleteSnapshot failed: %v", err)
	}

	p = h.k8sClient.getPool("snap-multi")
	if p.Status.TotalAllocatedGB != 30 {
		t.Errorf("expected TotalAllocatedGB=30 after deleting 1 snapshot, got %d", p.Status.TotalAllocatedGB)
	}

	if h.k8sClient.snapshotCount() != 2 {
		t.Errorf("expected 2 Snapshot CRs after deletion, got %d", h.k8sClient.snapshotCount())
	}
}

// ---------------------------------------------------------------------------
// Snapshot Restore Tests
// ---------------------------------------------------------------------------

func TestSnapshot_RestoreToNewVolume(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("snap-restore", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Create volume and snapshot
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	volResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "snap-restore", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	volumeID := volResp.GetVolume().VolumeId

	snapResp, err := h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-restore-1", volumeID))
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}
	snapshotID := snapResp.GetSnapshot().SnapshotId

	// Restore to new volume
	restorePVName := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	restoreResp, err := h.driver.CreateVolume(ctx,
		createVolumeFromSnapshotRequest(restorePVName, "snap-restore", 10, snapshotID))
	if err != nil {
		t.Fatalf("CreateVolume from snapshot failed: %v", err)
	}

	restoredVol := restoreResp.GetVolume()
	if restoredVol == nil {
		t.Fatal("expected volume in restore response")
	}

	// Verify new SubVolume CR exists
	sv := h.k8sClient.getSubVolume(restorePVName)
	if sv == nil {
		t.Fatal("expected restored SubVolume CR to exist")
	}

	// Verify ContentSource is set
	if restoredVol.ContentSource == nil {
		t.Fatal("expected ContentSource to be set")
	}
	snapSource := restoredVol.ContentSource.GetSnapshot()
	if snapSource == nil {
		t.Fatal("expected Snapshot content source")
	}
	if snapSource.SnapshotId != snapshotID {
		t.Errorf("expected snapshot ID %q, got %q", snapshotID, snapSource.SnapshotId)
	}

	// Verify pool allocatedGB includes restored volume
	// 10 (original) + 10 (snapshot) + 10 (restored) = 30
	p := h.k8sClient.getPool("snap-restore")
	if p.Status.TotalAllocatedGB != 30 {
		t.Errorf("expected TotalAllocatedGB=30, got %d", p.Status.TotalAllocatedGB)
	}
}

func TestSnapshot_RestoreSnapshotNotFound(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("snap-restore-nf", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Try to create volume from non-existent snapshot
	restorePVName := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	_, err := h.driver.CreateVolume(ctx,
		createVolumeFromSnapshotRequest(restorePVName, "snap-restore-nf", 10,
			"snap-restore-nf/share-1/snap-nonexistent"))

	assertGRPCCode(t, err, codes.NotFound)
}

func TestSnapshot_RestoreExhaustsPool(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	// Create pool with small share (20 GB), auto-expand disabled
	pool := newTestPool("snap-exhaust", "spread", 20,
		newStableShare("share-1", "s1", 20, 0, 0),
	)
	pool.Spec.AutoExpand = false
	h.k8sClient.addPool(pool)

	// Fill with 10 GB volume
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	volResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "snap-exhaust", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	volumeID := volResp.GetVolume().VolumeId

	// Take 10 GB snapshot → now share is at 20/20 GB
	snapResp, err := h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-exhaust-1", volumeID))
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}
	snapshotID := snapResp.GetSnapshot().SnapshotId

	// Attempt restore → should fail with ResourceExhausted
	restorePVName := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	_, err = h.driver.CreateVolume(ctx,
		createVolumeFromSnapshotRequest(restorePVName, "snap-exhaust", 10, snapshotID))

	assertGRPCCode(t, err, codes.ResourceExhausted)
}

// ---------------------------------------------------------------------------
// Snapshot Listing Tests
// ---------------------------------------------------------------------------

func TestSnapshot_ListBySourceVolume(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("snap-list", "spread", 200,
		newStableShare("share-1", "s1", 200, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Create 2 volumes
	pv1 := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	pv2 := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"

	vol1Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv1, "snap-list", 10))
	if err != nil {
		t.Fatalf("CreateVolume 1 failed: %v", err)
	}
	vol2Resp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pv2, "snap-list", 10))
	if err != nil {
		t.Fatalf("CreateVolume 2 failed: %v", err)
	}
	vol1ID := vol1Resp.GetVolume().VolumeId
	vol2ID := vol2Resp.GetVolume().VolumeId

	// Create 2 snapshots from vol1, 1 from vol2
	for i := 0; i < 2; i++ {
		_, err := h.driver.CreateSnapshot(ctx, createSnapshotRequest(fmt.Sprintf("snap-v1-%d", i), vol1ID))
		if err != nil {
			t.Fatalf("CreateSnapshot v1-%d failed: %v", i, err)
		}
	}
	_, err = h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-v2-0", vol2ID))
	if err != nil {
		t.Fatalf("CreateSnapshot v2-0 failed: %v", err)
	}

	// ListSnapshots with sourceVolumeId=vol1 → expect 2 entries
	listResp1, err := h.driver.ListSnapshots(ctx, listSnapshotsRequest(vol1ID))
	if err != nil {
		t.Fatalf("ListSnapshots for vol1 failed: %v", err)
	}
	if len(listResp1.Entries) != 2 {
		t.Errorf("expected 2 snapshots for vol1, got %d", len(listResp1.Entries))
	}

	// ListSnapshots with sourceVolumeId=vol2 → expect 1 entry
	listResp2, err := h.driver.ListSnapshots(ctx, listSnapshotsRequest(vol2ID))
	if err != nil {
		t.Fatalf("ListSnapshots for vol2 failed: %v", err)
	}
	if len(listResp2.Entries) != 1 {
		t.Errorf("expected 1 snapshot for vol2, got %d", len(listResp2.Entries))
	}
}

func TestSnapshot_ListBySnapshotID(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("snap-listid", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	volResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "snap-listid", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	volumeID := volResp.GetVolume().VolumeId

	snapResp, err := h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-lookup", volumeID))
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}
	snapshotID := snapResp.GetSnapshot().SnapshotId

	// ListSnapshots with snapshotId → expect exactly 1 entry
	listResp, err := h.driver.ListSnapshots(ctx, &csi.ListSnapshotsRequest{
		SnapshotId: snapshotID,
	})
	if err != nil {
		t.Fatalf("ListSnapshots by ID failed: %v", err)
	}
	if len(listResp.Entries) != 1 {
		t.Fatalf("expected 1 snapshot entry, got %d", len(listResp.Entries))
	}

	entry := listResp.Entries[0].Snapshot
	if entry.SnapshotId != snapshotID {
		t.Errorf("expected snapshotID %q, got %q", snapshotID, entry.SnapshotId)
	}
	if entry.SizeBytes != gbToBytes(10) {
		t.Errorf("expected sizeBytes %d, got %d", gbToBytes(10), entry.SizeBytes)
	}
	if !entry.ReadyToUse {
		t.Error("expected readyToUse=true")
	}
}

func TestSnapshot_ListPagination(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("snap-page", "spread", 200,
		newStableShare("share-1", "s1", 200, 0, 0),
	)
	h.k8sClient.addPool(pool)

	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	volResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "snap-page", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	volumeID := volResp.GetVolume().VolumeId

	// Create 5 snapshots
	for i := 0; i < 5; i++ {
		_, err := h.driver.CreateSnapshot(ctx,
			createSnapshotRequest(fmt.Sprintf("snap-page-%d", i), volumeID))
		if err != nil {
			t.Fatalf("CreateSnapshot %d failed: %v", i, err)
		}
	}

	// ListSnapshots with maxEntries=2 → 2 entries + nextToken
	resp1, err := h.driver.ListSnapshots(ctx, &csi.ListSnapshotsRequest{
		SourceVolumeId: volumeID,
		MaxEntries:     2,
	})
	if err != nil {
		t.Fatalf("ListSnapshots page 1 failed: %v", err)
	}
	if len(resp1.Entries) != 2 {
		t.Errorf("page 1: expected 2 entries, got %d", len(resp1.Entries))
	}
	if resp1.NextToken == "" {
		t.Error("page 1: expected non-empty nextToken")
	}

	// ListSnapshots with startingToken → remaining entries
	resp2, err := h.driver.ListSnapshots(ctx, &csi.ListSnapshotsRequest{
		SourceVolumeId: volumeID,
		MaxEntries:     2,
		StartingToken:  resp1.NextToken,
	})
	if err != nil {
		t.Fatalf("ListSnapshots page 2 failed: %v", err)
	}
	if len(resp2.Entries) != 2 {
		t.Errorf("page 2: expected 2 entries, got %d", len(resp2.Entries))
	}

	// Final page
	resp3, err := h.driver.ListSnapshots(ctx, &csi.ListSnapshotsRequest{
		SourceVolumeId: volumeID,
		MaxEntries:     2,
		StartingToken:  resp2.NextToken,
	})
	if err != nil {
		t.Fatalf("ListSnapshots page 3 failed: %v", err)
	}
	if len(resp3.Entries) != 1 {
		t.Errorf("page 3: expected 1 entry, got %d", len(resp3.Entries))
	}
	if resp3.NextToken != "" {
		t.Errorf("page 3: expected empty nextToken, got %q", resp3.NextToken)
	}
}

// ---------------------------------------------------------------------------
// Capacity Tracking Tests
// ---------------------------------------------------------------------------

func TestSnapshot_CapacityTracking(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("snap-cap", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	// Allocate 10 GB volume
	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	volResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "snap-cap", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	volumeID := volResp.GetVolume().VolumeId

	// Create snapshot → pool.allocatedGB = 20
	snap1Resp, err := h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-cap-1", volumeID))
	if err != nil {
		t.Fatalf("CreateSnapshot 1 failed: %v", err)
	}

	p := h.k8sClient.getPool("snap-cap")
	if p.Status.TotalAllocatedGB != 20 {
		t.Errorf("after snap1: expected TotalAllocatedGB=20, got %d", p.Status.TotalAllocatedGB)
	}

	// Create another snapshot → pool.allocatedGB = 30
	snap2Resp, err := h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-cap-2", volumeID))
	if err != nil {
		t.Fatalf("CreateSnapshot 2 failed: %v", err)
	}

	p = h.k8sClient.getPool("snap-cap")
	if p.Status.TotalAllocatedGB != 30 {
		t.Errorf("after snap2: expected TotalAllocatedGB=30, got %d", p.Status.TotalAllocatedGB)
	}

	// Delete first snapshot → pool.allocatedGB = 20
	_, err = h.driver.DeleteSnapshot(ctx, deleteSnapshotRequest(snap1Resp.GetSnapshot().SnapshotId))
	if err != nil {
		t.Fatalf("DeleteSnapshot 1 failed: %v", err)
	}

	p = h.k8sClient.getPool("snap-cap")
	if p.Status.TotalAllocatedGB != 20 {
		t.Errorf("after deleting snap1: expected TotalAllocatedGB=20, got %d", p.Status.TotalAllocatedGB)
	}

	// Delete volume → pool.allocatedGB = 10 (one snapshot remains)
	_, err = h.driver.DeleteVolume(ctx, deleteVolumeRequest(volumeID))
	if err != nil {
		t.Fatalf("DeleteVolume failed: %v", err)
	}

	p = h.k8sClient.getPool("snap-cap")
	if p.Status.TotalAllocatedGB != 10 {
		t.Errorf("after deleting volume: expected TotalAllocatedGB=10, got %d", p.Status.TotalAllocatedGB)
	}

	// Delete remaining snapshot → pool.allocatedGB = 0
	_, err = h.driver.DeleteSnapshot(ctx, deleteSnapshotRequest(snap2Resp.GetSnapshot().SnapshotId))
	if err != nil {
		t.Fatalf("DeleteSnapshot 2 failed: %v", err)
	}

	p = h.k8sClient.getPool("snap-cap")
	if p.Status.TotalAllocatedGB != 0 {
		t.Errorf("after deleting everything: expected TotalAllocatedGB=0, got %d", p.Status.TotalAllocatedGB)
	}
}

// ---------------------------------------------------------------------------
// Error Recovery Tests
// ---------------------------------------------------------------------------

func TestSnapshot_CopyFailure(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("snap-copyerr", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	volResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "snap-copyerr", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	volumeID := volResp.GetVolume().VolumeId

	// Inject CopyDir error
	h.nfsOps.CopyErr = fmt.Errorf("NFS: I/O error during copy")

	// CreateSnapshot → expect gRPC Internal error
	_, err = h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-fail", volumeID))
	assertGRPCCode(t, err, codes.Internal)

	// Verify no Snapshot CR was created
	if h.k8sClient.snapshotCount() != 0 {
		t.Error("expected no Snapshot CR when CopyDir fails")
	}

	// Pool capacity should not have changed (only the volume's 10 GB)
	p := h.k8sClient.getPool("snap-copyerr")
	if p.Status.TotalAllocatedGB != 10 {
		t.Errorf("expected TotalAllocatedGB=10 after copy failure, got %d", p.Status.TotalAllocatedGB)
	}

	// Clear error and retry → success
	h.nfsOps.CopyErr = nil
	_, err = h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-fail", volumeID))
	if err != nil {
		t.Fatalf("CreateSnapshot should succeed after clearing CopyErr: %v", err)
	}

	if h.k8sClient.snapshotCount() != 1 {
		t.Errorf("expected 1 Snapshot CR after recovery, got %d", h.k8sClient.snapshotCount())
	}
}

func TestSnapshot_DeleteWithNfsRemoveFailure(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	pool := newTestPool("snap-delerr", "spread", 100,
		newStableShare("share-1", "s1", 100, 0, 0),
	)
	h.k8sClient.addPool(pool)

	pvName := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	volResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(pvName, "snap-delerr", 10))
	if err != nil {
		t.Fatalf("CreateVolume failed: %v", err)
	}
	volumeID := volResp.GetVolume().VolumeId

	snapResp, err := h.driver.CreateSnapshot(ctx, createSnapshotRequest("snap-delerr-1", volumeID))
	if err != nil {
		t.Fatalf("CreateSnapshot failed: %v", err)
	}
	snapshotID := snapResp.GetSnapshot().SnapshotId

	// Inject RemoveAll error
	h.nfsOps.RemoveErr = fmt.Errorf("NFS: permission denied")

	// DeleteSnapshot → expect gRPC Internal error
	_, err = h.driver.DeleteSnapshot(ctx, deleteSnapshotRequest(snapshotID))
	assertGRPCCode(t, err, codes.Internal)

	// Verify Snapshot CR still exists (not deleted on NFS failure)
	if h.k8sClient.snapshotCount() != 1 {
		t.Error("expected Snapshot CR to be retained when NFS remove fails")
	}

	// Clear error and retry → success
	h.nfsOps.RemoveErr = nil
	_, err = h.driver.DeleteSnapshot(ctx, deleteSnapshotRequest(snapshotID))
	if err != nil {
		t.Fatalf("DeleteSnapshot should succeed after clearing RemoveErr: %v", err)
	}

	if h.k8sClient.snapshotCount() != 0 {
		t.Error("expected Snapshot CR to be deleted after successful retry")
	}
}
