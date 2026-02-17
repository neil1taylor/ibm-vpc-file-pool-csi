package pool

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

// newPendingClone creates a SubVolume CR representing an async clone with cloneStatus=Pending.
func newPendingClone(pvName, poolName, shareID, sourceVolume string, requestedGB int64) *v1alpha1.SubVolume {
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

// newInProgressClone creates a SubVolume CR representing a clone in InProgress state
// (simulates a crash recovery scenario).
func newInProgressClone(pvName, poolName, shareID, sourceVolume string, requestedGB int64) *v1alpha1.SubVolume {
	sv := newPendingClone(pvName, poolName, shareID, sourceVolume, requestedGB)
	sv.Status.CloneStatus = "InProgress"
	now := metav1.Now()
	sv.Status.CloneProgress.StartedAt = &now
	return sv
}

func newCloneWorkerForTest(k8s *fakeK8sClient, nfs *fakeNFSOperations) *CloneWorker {
	w := NewCloneWorker(k8s, nfs, "/mnt/staging")
	w.SetInterval(50 * time.Millisecond)
	return w
}

// waitForCloneStatus polls the fake k8s client until the SubVolume has the expected
// clone status or the timeout expires. Uses the thread-safe GetSubVolume interface
// method (which returns a DeepCopy) to avoid races with the clone worker goroutine.
func waitForCloneStatus(t *testing.T, k *fakeK8sClient, svName, expectedStatus string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	ctx := context.Background()
	for {
		select {
		case <-deadline:
			sv, err := k.GetSubVolume(ctx, svName)
			if err != nil {
				t.Fatalf("timed out waiting for SubVolume %q to reach status %q (SubVolume not found)", svName, expectedStatus)
			}
			t.Fatalf("timed out waiting for SubVolume %q to reach status %q (current: %q)",
				svName, expectedStatus, sv.Status.CloneStatus)
		case <-ticker.C:
			sv, err := k.GetSubVolume(ctx, svName)
			if err == nil && sv.Status.CloneStatus == expectedStatus {
				return
			}
		}
	}
}

// readCloneCounterValue reads the current value of a Prometheus counter.
func readCloneCounterValue(pool, status string) float64 {
	var m dto.Metric
	_ = metrics.ClonesTotal.WithLabelValues(pool, status).Write(&m)
	return getCounterValue(&m)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCloneWorker_PendingCloneCompletesSuccessfully(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Set up source SubVolume.
	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	k.addSubVolume(source)

	// Set up pending clone SubVolume.
	clone := newPendingClone(
		"pvc-22222222-2222-2222-2222-222222222222",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		5,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	// Wait for clone to complete.
	waitForCloneStatus(t, k, "pvc-22222222-2222-2222-2222-222222222222", "Complete", 3*time.Second)

	// Verify the SubVolume status.
	sv, _ := k.GetSubVolume(context.Background(), "pvc-22222222-2222-2222-2222-222222222222")
	if sv.Status.Phase != "Bound" {
		t.Errorf("expected phase Bound, got %s", sv.Status.Phase)
	}
	if sv.Status.CloneProgress == nil {
		t.Fatal("expected CloneProgress to be set")
	}
	if sv.Status.CloneProgress.BytesCopied != sv.Status.CloneProgress.TotalBytes {
		t.Errorf("expected BytesCopied=%d, got %d",
			sv.Status.CloneProgress.TotalBytes, sv.Status.CloneProgress.BytesCopied)
	}
	if sv.Status.CloneProgress.StartedAt == nil {
		t.Error("expected StartedAt to be set")
	}
	if sv.Status.CloneProgress.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}

	// Verify NFS copy was performed.
	if nfs.copyCount() != 1 {
		t.Errorf("expected 1 NFS copy, got %d", nfs.copyCount())
	}
}

func TestCloneWorker_PendingCloneFails(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()
	nfs.CopyErr = errors.New("NFS write error: disk full")

	// Set up source SubVolume.
	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 20,
	)
	k.addSubVolume(source)

	// Set up pending clone SubVolume.
	clone := newPendingClone(
		"pvc-33333333-3333-3333-3333-333333333333",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		20,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	// Wait for clone to fail.
	waitForCloneStatus(t, k, "pvc-33333333-3333-3333-3333-333333333333", "Failed", 3*time.Second)

	// Verify the SubVolume status.
	sv, _ := k.GetSubVolume(context.Background(), "pvc-33333333-3333-3333-3333-333333333333")
	if sv.Status.Phase != "Failed" {
		t.Errorf("expected phase Failed, got %s", sv.Status.Phase)
	}
	if sv.Status.CloneProgress == nil {
		t.Fatal("expected CloneProgress to be set")
	}
	if sv.Status.CloneProgress.Error == "" {
		t.Error("expected clone error message to be set")
	}
	if sv.Status.CloneProgress.CompletedAt == nil {
		t.Error("expected CompletedAt to be set even on failure")
	}
}

func TestCloneWorker_SkipsCompleteClones(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Set up a completed clone SubVolume.
	now := metav1.Now()
	completedClone := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-44444444-4444-4444-4444-444444444444",
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":         "test-pool",
				"storage.ibmcloud.io/share-id":     "share-1",
				"storage.ibmcloud.io/clone-source": "pvc-11111111-1111-1111-1111-111111111111",
			},
		},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           "test-pool",
			ShareID:            "share-1",
			ShareMountTargetIP: "10.240.0.1",
			SubPath:            "/pvcs/pvc-44444444-4444-4444-4444-444444444444",
			RequestedGB:        5,
			PVName:             "pvc-44444444-4444-4444-4444-444444444444",
			PVCName:            "my-clone",
			PVCNamespace:       "default",
			ReclaimPolicy:      "Delete",
			SourceVolume:       "pvc-11111111-1111-1111-1111-111111111111",
			SourceShareID:      "share-1",
		},
		Status: v1alpha1.SubVolumeStatus{
			Phase:       "Bound",
			CloneStatus: "Complete",
			CloneProgress: &v1alpha1.CloneProgress{
				BytesCopied: 5 * (1 << 30),
				TotalBytes:  5 * (1 << 30),
				StartedAt:   &now,
				CompletedAt: &now,
			},
			CreatedAt: &now,
		},
	}
	k.addSubVolume(completedClone)

	w := newCloneWorkerForTest(k, nfs)

	// Run processOnce directly and verify no copies happen.
	w.processOnce(context.Background())

	// Give a moment for any goroutines to run.
	time.Sleep(100 * time.Millisecond)

	if nfs.copyCount() != 0 {
		t.Errorf("expected 0 NFS copies for complete clone, got %d", nfs.copyCount())
	}

	// Status should remain unchanged.
	sv, _ := k.GetSubVolume(context.Background(), "pvc-44444444-4444-4444-4444-444444444444")
	if sv.Status.CloneStatus != "Complete" {
		t.Errorf("expected cloneStatus to remain Complete, got %s", sv.Status.CloneStatus)
	}
}

func TestCloneWorker_RetriesInProgressClones(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Set up source SubVolume.
	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 50,
	)
	k.addSubVolume(source)

	// Set up an InProgress clone (simulating crash recovery).
	clone := newInProgressClone(
		"pvc-55555555-5555-5555-5555-555555555555",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		50,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	// Wait for clone to complete (it should retry the InProgress clone).
	waitForCloneStatus(t, k, "pvc-55555555-5555-5555-5555-555555555555", "Complete", 3*time.Second)

	sv, _ := k.GetSubVolume(context.Background(), "pvc-55555555-5555-5555-5555-555555555555")
	if sv.Status.Phase != "Bound" {
		t.Errorf("expected phase Bound after retry, got %s", sv.Status.Phase)
	}
	if nfs.copyCount() != 1 {
		t.Errorf("expected 1 NFS copy for retried clone, got %d", nfs.copyCount())
	}
}

func TestCloneWorker_ConcurrentPendingClones(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Set up source SubVolume.
	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 10,
	)
	k.addSubVolume(source)

	// Create two pending clones.
	clone1 := newPendingClone(
		"pvc-66666666-6666-6666-6666-666666666666",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		10,
	)
	k.addSubVolume(clone1)

	clone2 := newPendingClone(
		"pvc-77777777-7777-7777-7777-777777777777",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		10,
	)
	k.addSubVolume(clone2)

	w := newCloneWorkerForTest(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	// Wait for both clones to complete.
	waitForCloneStatus(t, k, "pvc-66666666-6666-6666-6666-666666666666", "Complete", 3*time.Second)
	waitForCloneStatus(t, k, "pvc-77777777-7777-7777-7777-777777777777", "Complete", 3*time.Second)

	// Both should have been copied.
	if nfs.copyCount() != 2 {
		t.Errorf("expected 2 NFS copies for concurrent clones, got %d", nfs.copyCount())
	}
}

func TestCloneWorker_MetricsRecorded(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Reset metrics for a clean test.
	metrics.ClonesTotal.Reset()
	metrics.CloneDuration.Reset()

	// Set up source SubVolume.
	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	k.addSubVolume(source)

	// Set up pending clone.
	clone := newPendingClone(
		"pvc-88888888-8888-8888-8888-888888888888",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		5,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	waitForCloneStatus(t, k, "pvc-88888888-8888-8888-8888-888888888888", "Complete", 3*time.Second)

	// Check success metric.
	val := readCloneCounterValue("test-pool", "async_success")
	if val < 1.0 {
		t.Errorf("expected ClonesTotal(async_success) >= 1, got %f", val)
	}
}

func TestCloneWorker_MetricsRecordedOnFailure(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()
	nfs.CopyErr = errors.New("simulated NFS error")

	// Reset metrics for a clean test.
	metrics.ClonesTotal.Reset()

	// Set up source SubVolume.
	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	k.addSubVolume(source)

	// Set up pending clone.
	clone := newPendingClone(
		"pvc-99999999-9999-9999-9999-999999999999",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		5,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	waitForCloneStatus(t, k, "pvc-99999999-9999-9999-9999-999999999999", "Failed", 3*time.Second)

	// Check error metric.
	val := readCloneCounterValue("test-pool", "async_error")
	if val < 1.0 {
		t.Errorf("expected ClonesTotal(async_error) >= 1, got %f", val)
	}
}

func TestCloneWorker_GracefulShutdown(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	w := newCloneWorkerForTest(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	// Let it run for a bit.
	time.Sleep(100 * time.Millisecond)

	// Cancel the context.
	cancel()

	// The worker should stop promptly.
	select {
	case <-done:
		// Good, worker stopped.
	case <-time.After(2 * time.Second):
		t.Fatal("clone worker did not stop after context cancellation")
	}
}

func TestCloneWorker_SourceNotFoundFails(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Set up a pending clone whose source does not exist.
	clone := newPendingClone(
		"pvc-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"test-pool", "share-1",
		"pvc-nonexist-0000-0000-0000-000000000000",
		5,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	waitForCloneStatus(t, k, "pvc-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa", "Failed", 3*time.Second)

	sv, _ := k.GetSubVolume(context.Background(), "pvc-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	if sv.Status.CloneProgress == nil {
		t.Fatal("expected CloneProgress to be set")
	}
	if sv.Status.CloneProgress.Error == "" {
		t.Error("expected error message about source not found")
	}
}

func TestCloneWorker_DoesNotProcessSameCloneTwice(t *testing.T) {
	k := newFakeK8sClient()

	// Use a slow NFS fake to simulate a long copy.
	var copyCount atomic.Int32
	slowNFS := &slowFakeNFSOperations{
		delay:     200 * time.Millisecond,
		copyCount: &copyCount,
	}

	// Set up source SubVolume.
	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	k.addSubVolume(source)

	// Set up a single pending clone.
	clone := newPendingClone(
		"pvc-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		5,
	)
	k.addSubVolume(clone)

	w := NewCloneWorker(k, slowNFS, "/mnt/staging")
	w.SetInterval(50 * time.Millisecond) // polls faster than the copy takes

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	waitForCloneStatus(t, k, "pvc-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb", "Complete", 5*time.Second)

	// The copy should have been performed exactly once, even though the ticker
	// fired multiple times during the copy.
	count := copyCount.Load()
	if count != 1 {
		t.Errorf("expected exactly 1 copy, got %d (concurrent duplicate detected)", count)
	}
}

func TestCloneWorker_ProcessOnceDirectCall(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Set up source SubVolume.
	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	k.addSubVolume(source)

	// Set up pending clone.
	clone := newPendingClone(
		"pvc-cccccccc-cccc-cccc-cccc-cccccccccccc",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		5,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, nfs)

	// Call processOnce directly (not via Run loop).
	w.processOnce(context.Background())

	// Wait for the spawned goroutine to complete.
	waitForCloneStatus(t, k, "pvc-cccccccc-cccc-cccc-cccc-cccccccccccc", "Complete", 3*time.Second)

	if nfs.copyCount() != 1 {
		t.Errorf("expected 1 NFS copy, got %d", nfs.copyCount())
	}
}

func TestCloneWorker_CleansUpPartialCopyOnFailure(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()
	nfs.CopyErr = errors.New("NFS write error: connection reset")

	// Set up source SubVolume.
	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	k.addSubVolume(source)

	// Set up pending clone.
	clone := newPendingClone(
		"pvc-dddddddd-dddd-dddd-dddd-dddddddddddd",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		5,
	)
	k.addSubVolume(clone)

	// Pre-create the destination directory to simulate a partial copy that will be cleaned up.
	nfs.mu.Lock()
	nfs.dirs["/mnt/staging/pvcs/pvc-dddddddd-dddd-dddd-dddd-dddddddddddd"] = 0755
	nfs.mu.Unlock()

	w := newCloneWorkerForTest(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	waitForCloneStatus(t, k, "pvc-dddddddd-dddd-dddd-dddd-dddddddddddd", "Failed", 3*time.Second)

	// Verify the partial directory was cleaned up (RemoveAll was called).
	if nfs.exists("/mnt/staging/pvcs/pvc-dddddddd-dddd-dddd-dddd-dddddddddddd") {
		t.Error("expected partial clone directory to be cleaned up after failure")
	}
}

// ---------------------------------------------------------------------------
// slowFakeNFSOperations — adds a configurable delay to CopyDir
// ---------------------------------------------------------------------------

type slowFakeNFSOperations struct {
	delay     time.Duration
	copyCount *atomic.Int32
}

func (s *slowFakeNFSOperations) MkdirAll(_ string, _ os.FileMode) error { return nil }
func (s *slowFakeNFSOperations) RemoveAll(_ string) error               { return nil }
func (s *slowFakeNFSOperations) Stat(_ string) (os.FileInfo, error)     { return nil, nil }
func (s *slowFakeNFSOperations) Chown(_ string, _, _ int) error         { return nil }
func (s *slowFakeNFSOperations) Chmod(_ string, _ os.FileMode) error    { return nil }

func (s *slowFakeNFSOperations) CopyDir(_, _ string) error {
	s.copyCount.Add(1)
	time.Sleep(s.delay)
	return nil
}

func (s *slowFakeNFSOperations) SyncDir(_ context.Context, _, _ string) error {
	return s.CopyDir("", "")
}
