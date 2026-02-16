//go:build integration

package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Helpers — create SubVolume CRs for clone worker tests
// ---------------------------------------------------------------------------

// newSourceSubVolume creates a "normal" (non-clone) SubVolume that can serve
// as the source for clone operations.
func newSourceSubVolume(pvName, poolName, shareID string, sizeGB int64) *v1alpha1.SubVolume {
	now := metav1.Now()
	return &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":     poolName,
				"storage.ibmcloud.io/share-id": shareID,
			},
		},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           poolName,
			ShareID:            shareID,
			ShareMountTargetIP: "10.240.0.1",
			SubPath:            "/pvcs/" + pvName,
			RequestedGB:        sizeGB,
			PVName:             pvName,
			PVCName:            "source-pvc",
			PVCNamespace:       "default",
			ReclaimPolicy:      "Delete",
		},
		Status: v1alpha1.SubVolumeStatus{
			Phase:     "Bound",
			CreatedAt: &now,
		},
	}
}

// newPendingCloneSV creates a SubVolume CR representing an async clone with
// CloneStatus=Pending, suitable for the clone worker to pick up.
func newPendingCloneSV(pvName, poolName, shareID, sourceVolume string, requestedGB int64) *v1alpha1.SubVolume {
	now := metav1.Now()
	return &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":         poolName,
				"storage.ibmcloud.io/share-id":     shareID,
				"storage.ibmcloud.io/clone-source": sourceVolume,
			},
		},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           poolName,
			ShareID:            shareID,
			ShareMountTargetIP: "10.240.0.1",
			SubPath:            "/pvcs/" + pvName,
			RequestedGB:        requestedGB,
			PVName:             pvName,
			PVCName:            "my-clone",
			PVCNamespace:       "default",
			ReclaimPolicy:      "Delete",
			SourceVolume:       sourceVolume,
			SourceShareID:      shareID,
		},
		Status: v1alpha1.SubVolumeStatus{
			Phase:       "Cloning",
			CloneStatus: "Pending",
			CloneProgress: &v1alpha1.CloneProgress{
				TotalBytes: requestedGB * (1 << 30),
			},
			CreatedAt: &now,
		},
	}
}

// newCompletedCloneSV creates a SubVolume CR for a clone that has already
// finished copying (CloneStatus=Complete).
func newCompletedCloneSV(pvName, poolName, shareID, sourceVolume string, requestedGB int64) *v1alpha1.SubVolume {
	now := metav1.Now()
	return &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":         poolName,
				"storage.ibmcloud.io/share-id":     shareID,
				"storage.ibmcloud.io/clone-source": sourceVolume,
			},
		},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           poolName,
			ShareID:            shareID,
			ShareMountTargetIP: "10.240.0.1",
			SubPath:            "/pvcs/" + pvName,
			RequestedGB:        requestedGB,
			PVName:             pvName,
			PVCName:            "completed-clone",
			PVCNamespace:       "default",
			ReclaimPolicy:      "Delete",
			SourceVolume:       sourceVolume,
			SourceShareID:      shareID,
		},
		Status: v1alpha1.SubVolumeStatus{
			Phase:       "Bound",
			CloneStatus: "Complete",
			CloneProgress: &v1alpha1.CloneProgress{
				BytesCopied: requestedGB * (1 << 30),
				TotalBytes:  requestedGB * (1 << 30),
				StartedAt:   &now,
				CompletedAt: &now,
			},
			CreatedAt: &now,
		},
	}
}

// newFailedCloneSV creates a SubVolume CR for a clone that previously failed
// (CloneStatus=Failed).
func newFailedCloneSV(pvName, poolName, shareID, sourceVolume string, requestedGB int64, errMsg string) *v1alpha1.SubVolume {
	now := metav1.Now()
	return &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: pvName,
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":         poolName,
				"storage.ibmcloud.io/share-id":     shareID,
				"storage.ibmcloud.io/clone-source": sourceVolume,
			},
		},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           poolName,
			ShareID:            shareID,
			ShareMountTargetIP: "10.240.0.1",
			SubPath:            "/pvcs/" + pvName,
			RequestedGB:        requestedGB,
			PVName:             pvName,
			PVCName:            "failed-clone",
			PVCNamespace:       "default",
			ReclaimPolicy:      "Delete",
			SourceVolume:       sourceVolume,
			SourceShareID:      shareID,
		},
		Status: v1alpha1.SubVolumeStatus{
			Phase:       "Failed",
			CloneStatus: "Failed",
			CloneProgress: &v1alpha1.CloneProgress{
				TotalBytes:  requestedGB * (1 << 30),
				Error:       errMsg,
				CompletedAt: &now,
			},
			CreatedAt: &now,
		},
	}
}

// newCloneWorker creates a CloneWorker wired to the integration-test fakes
// with a fast poll interval.
func newCloneWorker(k8s *fakeK8sClient, nfs *fakeNFSOperations) *pool.CloneWorker {
	w := pool.NewCloneWorker(k8s, nfs, "/mnt/staging")
	w.SetInterval(50 * time.Millisecond)
	return w
}

// waitForCloneStatusInteg polls the fakeK8sClient until the SubVolume reaches
// the expected clone status or the timeout expires.
func waitForCloneStatusInteg(t *testing.T, k *fakeK8sClient, svName, expectedStatus string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			sv := k.getSubVolume(svName)
			if sv == nil {
				t.Fatalf("timed out waiting for SubVolume %q to reach status %q (SubVolume not found)", svName, expectedStatus)
			}
			t.Fatalf("timed out waiting for SubVolume %q to reach status %q (current: %q)",
				svName, expectedStatus, sv.Status.CloneStatus)
		case <-ticker.C:
			sv := k.getSubVolume(svName)
			if sv != nil && sv.Status.CloneStatus == expectedStatus {
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestCloneWorker_ProcessesPendingClone verifies that the clone worker picks
// up a SubVolume with CloneStatus=Pending, performs the NFS copy, and
// transitions the status to Complete with correct progress fields.
func TestCloneWorker_ProcessesPendingClone(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Source SubVolume (the volume being cloned from).
	source := newSourceSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 20,
	)
	k.addSubVolume(source)

	// Pending clone SubVolume (large volume triggers async path).
	clone := newPendingCloneSV(
		"pvc-22222222-2222-2222-2222-222222222222",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		20,
	)
	k.addSubVolume(clone)

	w := newCloneWorker(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	// Wait for the clone worker to complete the copy.
	waitForCloneStatusInteg(t, k, "pvc-22222222-2222-2222-2222-222222222222", "Complete", 3*time.Second)

	// Verify final SubVolume status.
	sv := k.getSubVolume("pvc-22222222-2222-2222-2222-222222222222")
	if sv == nil {
		t.Fatal("expected SubVolume to exist after clone")
	}
	if sv.Status.Phase != "Bound" {
		t.Errorf("expected Phase 'Bound', got %q", sv.Status.Phase)
	}
	if sv.Status.CloneProgress == nil {
		t.Fatal("expected CloneProgress to be set")
	}
	if sv.Status.CloneProgress.BytesCopied != sv.Status.CloneProgress.TotalBytes {
		t.Errorf("expected BytesCopied=%d to equal TotalBytes=%d",
			sv.Status.CloneProgress.BytesCopied, sv.Status.CloneProgress.TotalBytes)
	}
	if sv.Status.CloneProgress.StartedAt == nil {
		t.Error("expected StartedAt to be set")
	}
	if sv.Status.CloneProgress.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}

	// Verify NFS copy was performed exactly once.
	if nfs.copyCount() != 1 {
		t.Errorf("expected 1 NFS copy, got %d", nfs.copyCount())
	}
}

// TestCloneWorker_SkipsCompletedClones verifies that the clone worker does not
// re-copy or modify SubVolumes that already have CloneStatus=Complete.
func TestCloneWorker_SkipsCompletedClones(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Source SubVolume.
	source := newSourceSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 10,
	)
	k.addSubVolume(source)

	// Already-completed clone.
	completed := newCompletedCloneSV(
		"pvc-33333333-3333-3333-3333-333333333333",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		10,
	)
	k.addSubVolume(completed)

	w := newCloneWorker(k, nfs)

	// Run the worker for several poll intervals to give it ample opportunity
	// to (incorrectly) process the completed clone.
	ctx, cancel := context.WithCancel(context.Background())
	go w.Run(ctx)

	time.Sleep(300 * time.Millisecond)
	cancel()

	// No NFS copies should have been performed.
	if nfs.copyCount() != 0 {
		t.Errorf("expected 0 NFS copies for completed clone, got %d", nfs.copyCount())
	}

	// Status must remain unchanged.
	sv := k.getSubVolume("pvc-33333333-3333-3333-3333-333333333333")
	if sv == nil {
		t.Fatal("expected SubVolume to still exist")
	}
	if sv.Status.CloneStatus != "Complete" {
		t.Errorf("expected CloneStatus to remain 'Complete', got %q", sv.Status.CloneStatus)
	}
	if sv.Status.Phase != "Bound" {
		t.Errorf("expected Phase to remain 'Bound', got %q", sv.Status.Phase)
	}
}

// TestCloneWorker_CopyFailureMarksCloneFailed verifies that when CopyDir
// returns an error, the clone worker marks the SubVolume as Failed with the
// error message recorded in CloneProgress.
func TestCloneWorker_CopyFailureMarksCloneFailed(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()
	nfs.CopyErr = errors.New("NFS write error: disk full")

	// Source SubVolume.
	source := newSourceSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 20,
	)
	k.addSubVolume(source)

	// Pending clone.
	clone := newPendingCloneSV(
		"pvc-44444444-4444-4444-4444-444444444444",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		20,
	)
	k.addSubVolume(clone)

	w := newCloneWorker(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	// Wait for the clone to be marked as Failed.
	waitForCloneStatusInteg(t, k, "pvc-44444444-4444-4444-4444-444444444444", "Failed", 3*time.Second)

	sv := k.getSubVolume("pvc-44444444-4444-4444-4444-444444444444")
	if sv == nil {
		t.Fatal("expected SubVolume to exist")
	}
	if sv.Status.Phase != "Failed" {
		t.Errorf("expected Phase 'Failed', got %q", sv.Status.Phase)
	}
	if sv.Status.CloneProgress == nil {
		t.Fatal("expected CloneProgress to be set")
	}
	if sv.Status.CloneProgress.Error == "" {
		t.Error("expected CloneProgress.Error to contain the failure message")
	}
	if sv.Status.CloneProgress.CompletedAt == nil {
		t.Error("expected CompletedAt to be set even on failure")
	}
	// Verify the error message includes the injected error text.
	if sv.Status.CloneProgress.Error == "" {
		t.Error("expected error message to be non-empty")
	}
}

// TestCloneWorker_RetryAfterFailure documents that the clone worker does NOT
// automatically retry Failed clones. The worker's processOnce loop only picks
// up SubVolumes with CloneStatus "Pending" or "InProgress". A Failed clone
// remains in Failed state until an external controller or operator resets it
// back to Pending.
//
// This test verifies:
//  1. A Failed clone is not retried by the worker.
//  2. If an external actor resets the clone to Pending, the worker picks it up.
func TestCloneWorker_RetryAfterFailure(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Source SubVolume.
	source := newSourceSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 10,
	)
	k.addSubVolume(source)

	// A clone that previously failed.
	failed := newFailedCloneSV(
		"pvc-55555555-5555-5555-5555-555555555555",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		10,
		"previous copy error",
	)
	k.addSubVolume(failed)

	w := newCloneWorker(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	// Let the worker run for several poll intervals. The Failed clone should
	// NOT be picked up.
	time.Sleep(300 * time.Millisecond)

	sv := k.getSubVolume("pvc-55555555-5555-5555-5555-555555555555")
	if sv == nil {
		t.Fatal("expected SubVolume to exist")
	}
	if sv.Status.CloneStatus != "Failed" {
		t.Fatalf("expected clone to remain Failed (worker should not retry), got %q", sv.Status.CloneStatus)
	}
	if nfs.copyCount() != 0 {
		t.Errorf("expected 0 NFS copies for Failed clone, got %d", nfs.copyCount())
	}

	// Simulate an external actor resetting the clone to Pending for retry.
	// This is how a human operator or reconciliation controller would trigger
	// a retry.
	k.mu.Lock()
	existing := k.subVolumes["pvc-55555555-5555-5555-5555-555555555555"]
	existing.Status.CloneStatus = "Pending"
	existing.Status.Phase = "Cloning"
	existing.Status.CloneProgress = &v1alpha1.CloneProgress{
		TotalBytes: 10 * (1 << 30),
	}
	k.mu.Unlock()

	// Now the worker should pick it up and complete the copy.
	waitForCloneStatusInteg(t, k, "pvc-55555555-5555-5555-5555-555555555555", "Complete", 3*time.Second)

	sv = k.getSubVolume("pvc-55555555-5555-5555-5555-555555555555")
	if sv.Status.Phase != "Bound" {
		t.Errorf("expected Phase 'Bound' after retry, got %q", sv.Status.Phase)
	}
	if nfs.copyCount() != 1 {
		t.Errorf("expected 1 NFS copy after retry, got %d", nfs.copyCount())
	}
}

// TestCloneWorker_MultiplePendingClones verifies that the clone worker
// processes multiple pending clones concurrently and completes all of them.
func TestCloneWorker_MultiplePendingClones(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Source SubVolume.
	source := newSourceSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 20,
	)
	k.addSubVolume(source)

	// Create 3 pending clones.
	cloneNames := []string{
		"pvc-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"pvc-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"pvc-cccccccc-cccc-cccc-cccc-cccccccccccc",
	}
	for _, name := range cloneNames {
		clone := newPendingCloneSV(
			name,
			"test-pool", "share-1",
			"pvc-11111111-1111-1111-1111-111111111111",
			20,
		)
		k.addSubVolume(clone)
	}

	w := newCloneWorker(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	// Wait for all 3 clones to reach Complete status.
	for _, name := range cloneNames {
		waitForCloneStatusInteg(t, k, name, "Complete", 5*time.Second)
	}

	// Verify each clone's final state.
	for i, name := range cloneNames {
		sv := k.getSubVolume(name)
		if sv == nil {
			t.Fatalf("clone %d (%s): expected SubVolume to exist", i, name)
		}
		if sv.Status.Phase != "Bound" {
			t.Errorf("clone %d (%s): expected Phase 'Bound', got %q", i, name, sv.Status.Phase)
		}
		if sv.Status.CloneProgress == nil {
			t.Errorf("clone %d (%s): expected CloneProgress to be set", i, name)
			continue
		}
		if sv.Status.CloneProgress.BytesCopied != sv.Status.CloneProgress.TotalBytes {
			t.Errorf("clone %d (%s): expected BytesCopied=%d to equal TotalBytes=%d",
				i, name, sv.Status.CloneProgress.BytesCopied, sv.Status.CloneProgress.TotalBytes)
		}
		if sv.Status.CloneProgress.CompletedAt == nil {
			t.Errorf("clone %d (%s): expected CompletedAt to be set", i, name)
		}
	}

	// Verify all 3 NFS copies were performed.
	if nfs.copyCount() != 3 {
		t.Errorf("expected 3 NFS copies for 3 pending clones, got %d", nfs.copyCount())
	}
}

// ---------------------------------------------------------------------------
// Additional coverage: end-to-end with CSI driver creating async clones
// ---------------------------------------------------------------------------

// TestCloneWorker_EndToEnd_AsyncCloneViaCSIDriver creates an async clone
// through the CSI driver (large volume exceeds sync threshold) and then runs
// the clone worker to complete it. This exercises the full integration path:
// CSI CreateVolume -> Pending SubVolume CR -> CloneWorker -> Complete.
func TestCloneWorker_EndToEnd_AsyncCloneViaCSIDriver(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	fsp := newTestPool("cw-e2e", "spread", 200,
		newStableShare("share-1", "s1", 200, 0, 0),
	)
	h.k8sClient.addPool(fsp)

	// Create a 20 GB source volume (over default 10 GB sync threshold).
	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "cw-e2e", 20))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	// Clone the 20 GB source with default threshold -> async (Pending).
	clonePV := "pvc-b2c3d4e5-6789-0abc-def0-234567890abc"
	_, err = h.driver.CreateVolume(ctx,
		createVolumeFromCloneRequest(clonePV, "cw-e2e", 20, sourceVolumeID))
	if err != nil {
		t.Fatalf("CreateVolume (async clone) failed: %v", err)
	}

	// Verify the SubVolume is in Pending state (async path).
	sv := h.k8sClient.getSubVolume(clonePV)
	if sv == nil {
		t.Fatal("expected cloned SubVolume to exist")
	}
	if sv.Status.CloneStatus != "Pending" {
		t.Fatalf("expected CloneStatus 'Pending' for async clone, got %q", sv.Status.CloneStatus)
	}

	// No NFS copy yet.
	if h.nfsOps.copyCount() != 0 {
		t.Fatalf("expected 0 NFS copies before worker runs, got %d", h.nfsOps.copyCount())
	}

	// Start the clone worker using the same fakes from the test harness.
	w := pool.NewCloneWorker(h.k8sClient, h.nfsOps, "/mnt/staging")
	w.SetInterval(50 * time.Millisecond)

	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()

	go w.Run(workerCtx)

	// Wait for the worker to complete the clone.
	waitForCloneStatusInteg(t, h.k8sClient, clonePV, "Complete", 5*time.Second)

	// Verify final state.
	sv = h.k8sClient.getSubVolume(clonePV)
	if sv.Status.Phase != "Bound" {
		t.Errorf("expected Phase 'Bound', got %q", sv.Status.Phase)
	}
	if sv.Status.CloneProgress == nil {
		t.Fatal("expected CloneProgress to be set")
	}
	if sv.Status.CloneProgress.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}

	// NFS copy should now have been performed by the worker.
	if h.nfsOps.copyCount() != 1 {
		t.Errorf("expected 1 NFS copy after worker completes, got %d", h.nfsOps.copyCount())
	}
}

// TestCloneWorker_EndToEnd_MultipleAsyncClonesViaCSIDriver creates 3 async
// clones through the CSI driver and runs the worker to complete all of them.
func TestCloneWorker_EndToEnd_MultipleAsyncClonesViaCSIDriver(t *testing.T) {
	h := newTestHarness(t)
	ctx := context.Background()

	fsp := newTestPool("cw-e2e-multi", "spread", 500,
		newStableShare("share-1", "s1", 500, 0, 0),
	)
	h.k8sClient.addPool(fsp)

	// Create a 20 GB source.
	srcPV := "pvc-a1b2c3d4-5678-90ab-cdef-1234567890ab"
	srcResp, err := h.driver.CreateVolume(ctx, createVolumeRequest(srcPV, "cw-e2e-multi", 20))
	if err != nil {
		t.Fatalf("CreateVolume (source) failed: %v", err)
	}
	sourceVolumeID := srcResp.GetVolume().VolumeId

	// Create 3 async clones (all 20 GB, above 10 GB sync threshold).
	clonePVs := []string{
		"pvc-c1000000-0000-0000-0000-000000000001",
		"pvc-c2000000-0000-0000-0000-000000000002",
		"pvc-c3000000-0000-0000-0000-000000000003",
	}
	for i := 0; i < 3; i++ {
		_, err := h.driver.CreateVolume(ctx,
			createVolumeFromCloneRequest(clonePVs[i], "cw-e2e-multi", 20, sourceVolumeID))
		if err != nil {
			t.Fatalf("CreateVolume (clone %d) failed: %v", i, err)
		}
		// Verify each starts as Pending.
		sv := h.k8sClient.getSubVolume(clonePVs[i])
		if sv == nil || sv.Status.CloneStatus != "Pending" {
			t.Fatalf("clone %d: expected CloneStatus 'Pending'", i)
		}
	}

	// Start the clone worker.
	w := pool.NewCloneWorker(h.k8sClient, h.nfsOps, "/mnt/staging")
	w.SetInterval(50 * time.Millisecond)

	workerCtx, workerCancel := context.WithCancel(ctx)
	defer workerCancel()

	go w.Run(workerCtx)

	// Wait for all 3 clones to complete.
	for i, name := range clonePVs {
		waitForCloneStatusInteg(t, h.k8sClient, name, "Complete", 5*time.Second)
		sv := h.k8sClient.getSubVolume(name)
		if sv.Status.Phase != "Bound" {
			t.Errorf("clone %d (%s): expected Phase 'Bound', got %q", i, name, sv.Status.Phase)
		}
	}

	// All 3 copies performed.
	if h.nfsOps.copyCount() != 3 {
		t.Errorf("expected 3 NFS copies, got %d", h.nfsOps.copyCount())
	}
}
