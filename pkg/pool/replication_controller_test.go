package pool

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Fake K8s Client extension for ReplicationPolicy
// ---------------------------------------------------------------------------

// replFakeK8sClient embeds fakeK8sClient and adds ReplicationPolicy operations.
type replFakeK8sClient struct {
	*fakeK8sClient

	replMu        sync.Mutex
	policies      map[string]*v1alpha1.ReplicationPolicy
	listPolicyErr error
}

func newReplFakeK8sClient() *replFakeK8sClient {
	return &replFakeK8sClient{
		fakeK8sClient: newFakeK8sClient(),
		policies:      make(map[string]*v1alpha1.ReplicationPolicy),
	}
}

func (f *replFakeK8sClient) GetReplicationPolicy(_ context.Context, name string) (*v1alpha1.ReplicationPolicy, error) {
	f.replMu.Lock()
	defer f.replMu.Unlock()
	rp, ok := f.policies[name]
	if !ok {
		return nil, errors.New("replication policy not found: " + name)
	}
	return rp.DeepCopy(), nil
}

func (f *replFakeK8sClient) ListReplicationPolicies(_ context.Context) ([]v1alpha1.ReplicationPolicy, error) {
	f.replMu.Lock()
	defer f.replMu.Unlock()
	if f.listPolicyErr != nil {
		return nil, f.listPolicyErr
	}
	var result []v1alpha1.ReplicationPolicy
	for _, rp := range f.policies {
		result = append(result, *rp.DeepCopy())
	}
	return result, nil
}

func (f *replFakeK8sClient) CreateReplicationPolicy(_ context.Context, rp *v1alpha1.ReplicationPolicy) error {
	f.replMu.Lock()
	defer f.replMu.Unlock()
	if _, exists := f.policies[rp.Name]; exists {
		return errors.New("replication policy already exists: " + rp.Name)
	}
	f.policies[rp.Name] = rp.DeepCopy()
	return nil
}

func (f *replFakeK8sClient) UpdateReplicationPolicyStatus(_ context.Context, rp *v1alpha1.ReplicationPolicy) error {
	f.replMu.Lock()
	defer f.replMu.Unlock()
	existing, ok := f.policies[rp.Name]
	if !ok {
		return errors.New("replication policy not found: " + rp.Name)
	}
	existing.Status = *rp.Status.DeepCopy()
	return nil
}

func (f *replFakeK8sClient) DeleteReplicationPolicy(_ context.Context, name string) error {
	f.replMu.Lock()
	defer f.replMu.Unlock()
	if _, exists := f.policies[name]; !exists {
		return errors.New("replication policy not found: " + name)
	}
	delete(f.policies, name)
	return nil
}

func (f *replFakeK8sClient) addPolicy(rp *v1alpha1.ReplicationPolicy) {
	f.replMu.Lock()
	defer f.replMu.Unlock()
	f.policies[rp.Name] = rp.DeepCopy()
}

func (f *replFakeK8sClient) getPolicy(name string) *v1alpha1.ReplicationPolicy {
	f.replMu.Lock()
	defer f.replMu.Unlock()
	rp, ok := f.policies[name]
	if !ok {
		return nil
	}
	return rp.DeepCopy()
}

// ---------------------------------------------------------------------------
// Test Helpers
// ---------------------------------------------------------------------------

func newTestReplicationPolicy(name, sourcePool, schedule string) *v1alpha1.ReplicationPolicy {
	return &v1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ReplicationPolicySpec{
			SourcePoolName:       sourcePool,
			DestinationNFSServer: "10.245.3.8",
			DestinationBasePath:  "/pvcs",
			Schedule:             schedule,
			MaxRetries:           3,
		},
		Status: v1alpha1.ReplicationPolicyStatus{
			Phase: "Active",
		},
	}
}

func newTestReplicationPolicyWithSelector(name, sourcePool, schedule string, labels map[string]string) *v1alpha1.ReplicationPolicy {
	rp := newTestReplicationPolicy(name, sourcePool, schedule)
	rp.Spec.SubVolumeSelector = &metav1.LabelSelector{
		MatchLabels: labels,
	}
	return rp
}

func newReplController(k8s *replFakeK8sClient, nfs *fakeNFSOperations) *ReplicationController {
	rc := NewReplicationController(k8s, nfs)
	rc.SetInterval(50 * time.Millisecond)
	return rc
}

// waitForPolicyPhase polls until the policy reaches the expected phase.
func waitForPolicyPhase(t *testing.T, k *replFakeK8sClient, policyName, expectedPhase string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			rp := k.getPolicy(policyName)
			if rp == nil {
				t.Fatalf("timed out waiting for policy %q to reach phase %q (policy not found)", policyName, expectedPhase)
			}
			t.Fatalf("timed out waiting for policy %q to reach phase %q (current: %q)",
				policyName, expectedPhase, rp.Status.Phase)
		case <-ticker.C:
			rp := k.getPolicy(policyName)
			if rp != nil && rp.Status.Phase == expectedPhase {
				return
			}
		}
	}
}

// waitForPolicySync polls until the policy has a LastSyncTime set.
func waitForPolicySync(t *testing.T, k *replFakeK8sClient, policyName string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for policy %q to sync", policyName)
		case <-ticker.C:
			rp := k.getPolicy(policyName)
			if rp != nil && rp.Status.LastSyncTime != nil {
				return
			}
		}
	}
}

// waitForConsecutiveFailures polls until the policy reaches the expected failure count.
func waitForConsecutiveFailures(t *testing.T, k *replFakeK8sClient, policyName string, minFailures int32, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			rp := k.getPolicy(policyName)
			if rp == nil {
				t.Fatalf("timed out waiting for policy %q to reach %d failures (policy not found)", policyName, minFailures)
			}
			t.Fatalf("timed out waiting for policy %q to reach %d consecutive failures (current: %d)",
				policyName, minFailures, rp.Status.ConsecutiveFailures)
		case <-ticker.C:
			rp := k.getPolicy(policyName)
			if rp != nil && rp.Status.ConsecutiveFailures >= minFailures {
				return
			}
		}
	}
}

func readReplicationCounterValue(policy, sourcePool, result string) float64 {
	var m dto.Metric
	_ = metrics.ReplicationSyncsTotal.WithLabelValues(policy, sourcePool, result).Write(&m)
	return getCounterValue(&m)
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestReplicationController_BasicSyncCycle(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Set up SubVolumes in source pool.
	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv2 := newTestSubVolume("pvc-22222222-2222-2222-2222-222222222222", "prod-pool", "share-1", 5)
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)

	// Create replication policy with a very short schedule.
	policy := newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Wait for sync to complete.
	waitForPolicySync(t, k, "prod-to-dr", 3*time.Second)

	// Verify policy status.
	rp := k.getPolicy("prod-to-dr")
	if rp.Status.Phase != "Active" {
		t.Errorf("expected phase Active, got %s", rp.Status.Phase)
	}
	if rp.Status.LastSyncTime == nil {
		t.Error("expected LastSyncTime to be set")
	}
	if rp.Status.LastSyncDuration == "" {
		t.Error("expected LastSyncDuration to be set")
	}
	if rp.Status.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", rp.Status.ConsecutiveFailures)
	}
	if rp.Status.LastError != "" {
		t.Errorf("expected no error, got %q", rp.Status.LastError)
	}

	// Verify per-SubVolume statuses.
	if len(rp.Status.SubVolumeStatuses) != 2 {
		t.Fatalf("expected 2 SubVolume statuses, got %d", len(rp.Status.SubVolumeStatuses))
	}
	for _, svs := range rp.Status.SubVolumeStatuses {
		if svs.LastSyncTime == nil {
			t.Errorf("expected LastSyncTime for SubVolume %q", svs.SubVolumeName)
		}
		if svs.LastError != "" {
			t.Errorf("expected no error for SubVolume %q, got %q", svs.SubVolumeName, svs.LastError)
		}
	}

	// Verify NFS copies were performed (one per SubVolume).
	if nfs.copyCount() < 2 {
		t.Errorf("expected at least 2 NFS copies, got %d", nfs.copyCount())
	}
}

func TestReplicationController_ScheduleTracking(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	// Policy with 1 hour schedule -- should only sync once.
	policy := newTestReplicationPolicy("hourly-policy", "prod-pool", "1h")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	// Run processOnce directly.
	ctx := context.Background()
	rc.processOnce(ctx)

	// Should have synced.
	rp := k.getPolicy("hourly-policy")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected first sync to occur")
	}

	// Run processOnce again -- should NOT sync because the interval hasn't elapsed.
	initialCopyCount := nfs.copyCount()
	rc.processOnce(ctx)

	// Copy count should not have increased.
	if nfs.copyCount() != initialCopyCount {
		t.Errorf("expected copy count to remain %d after second processOnce, got %d",
			initialCopyCount, nfs.copyCount())
	}
}

func TestReplicationController_FailureHandling(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()
	nfs.CopyErr = errors.New("NFS write error: destination unreachable")

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("fail-policy", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Wait for at least one failure.
	waitForConsecutiveFailures(t, k, "fail-policy", 1, 3*time.Second)

	rp := k.getPolicy("fail-policy")
	if rp.Status.ConsecutiveFailures < 1 {
		t.Errorf("expected at least 1 consecutive failure, got %d", rp.Status.ConsecutiveFailures)
	}
	if rp.Status.LastError == "" {
		t.Error("expected LastError to be set")
	}
}

func TestReplicationController_MaxRetriesExceeded(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()
	nfs.CopyErr = errors.New("persistent NFS error")

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("pause-policy", "prod-pool", "1ms")
	policy.Spec.MaxRetries = 2
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Wait for the policy to be paused.
	waitForPolicyPhase(t, k, "pause-policy", "Paused", 5*time.Second)

	rp := k.getPolicy("pause-policy")
	if rp.Status.ConsecutiveFailures < 2 {
		t.Errorf("expected at least 2 consecutive failures, got %d", rp.Status.ConsecutiveFailures)
	}
}

func TestReplicationController_SubVolumeSelector(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Two SubVolumes, only one with the replication label.
	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv1.Labels["replication"] = "enabled"
	k.addSubVolume(sv1)

	sv2 := newTestSubVolume("pvc-22222222-2222-2222-2222-222222222222", "prod-pool", "share-1", 5)
	// sv2 does NOT have replication=enabled label.
	k.addSubVolume(sv2)

	policy := newTestReplicationPolicyWithSelector("select-policy", "prod-pool", "1ms",
		map[string]string{"replication": "enabled"})
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// Wait a bit for async processing.
	time.Sleep(50 * time.Millisecond)

	rp := k.getPolicy("select-policy")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected sync to occur")
	}

	// Should have synced only 1 SubVolume.
	if len(rp.Status.SubVolumeStatuses) != 1 {
		t.Fatalf("expected 1 SubVolume status (selector filtering), got %d", len(rp.Status.SubVolumeStatuses))
	}
	if rp.Status.SubVolumeStatuses[0].SubVolumeName != "pvc-11111111-1111-1111-1111-111111111111" {
		t.Errorf("expected selected SubVolume pvc-111..., got %s", rp.Status.SubVolumeStatuses[0].SubVolumeName)
	}

	// Only 1 copy should have been made.
	if nfs.copyCount() != 1 {
		t.Errorf("expected 1 NFS copy (selector filtering), got %d", nfs.copyCount())
	}
}

func TestReplicationController_SkipsPausedPolicy(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("paused-policy", "prod-pool", "1ms")
	policy.Status.Phase = "Paused"
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// No copies should have been made.
	if nfs.copyCount() != 0 {
		t.Errorf("expected 0 NFS copies for paused policy, got %d", nfs.copyCount())
	}
}

func TestReplicationController_SkipsFailedPolicy(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("failed-policy", "prod-pool", "1ms")
	policy.Status.Phase = "Failed"
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	if nfs.copyCount() != 0 {
		t.Errorf("expected 0 NFS copies for failed policy, got %d", nfs.copyCount())
	}
}

func TestReplicationController_InvalidSchedule(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("bad-schedule", "prod-pool", "not-a-duration")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// No copies should have been made.
	if nfs.copyCount() != 0 {
		t.Errorf("expected 0 NFS copies for invalid schedule, got %d", nfs.copyCount())
	}
}

func TestReplicationController_GracefulShutdown(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	rc := newReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		rc.Run(ctx)
		close(done)
	}()

	// Let it run for a bit.
	time.Sleep(100 * time.Millisecond)

	// Cancel the context.
	cancel()

	// The controller should stop promptly.
	select {
	case <-done:
		// Good, controller stopped.
	case <-time.After(2 * time.Second):
		t.Fatal("replication controller did not stop after context cancellation")
	}
}

func TestReplicationController_MetricsRecorded(t *testing.T) {
	// Reset metrics for a clean test.
	metrics.ReplicationSyncsTotal.Reset()
	metrics.ReplicationSyncDuration.Reset()
	metrics.ReplicationLagSeconds.Reset()
	metrics.ReplicationConsecutiveFailures.Reset()
	metrics.ReplicationSubVolumeCount.Reset()

	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("metrics-policy", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	waitForPolicySync(t, k, "metrics-policy", 3*time.Second)

	// Check success metric.
	val := readReplicationCounterValue("metrics-policy", "prod-pool", "success")
	if val < 1.0 {
		t.Errorf("expected ReplicationSyncsTotal(success) >= 1, got %f", val)
	}
}

func TestReplicationController_MetricsRecordedOnFailure(t *testing.T) {
	metrics.ReplicationSyncsTotal.Reset()

	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()
	nfs.CopyErr = errors.New("simulated NFS error")

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("fail-metrics", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	waitForConsecutiveFailures(t, k, "fail-metrics", 1, 3*time.Second)

	val := readReplicationCounterValue("fail-metrics", "prod-pool", "failure")
	if val < 1.0 {
		t.Errorf("expected ReplicationSyncsTotal(failure) >= 1, got %f", val)
	}
}

func TestReplicationController_SuccessResetsConsecutiveFailures(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	// Start with a policy that already has some failures.
	policy := newTestReplicationPolicy("recover-policy", "prod-pool", "1ms")
	policy.Status.ConsecutiveFailures = 2
	policy.Status.LastError = "previous error"
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	waitForPolicySync(t, k, "recover-policy", 3*time.Second)

	rp := k.getPolicy("recover-policy")
	if rp.Status.ConsecutiveFailures != 0 {
		t.Errorf("expected consecutive failures to reset to 0, got %d", rp.Status.ConsecutiveFailures)
	}
	if rp.Status.LastError != "" {
		t.Errorf("expected LastError to be cleared, got %q", rp.Status.LastError)
	}
}

func TestReplicationController_EmptyPool(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	// No SubVolumes in the pool.
	policy := newTestReplicationPolicy("empty-policy", "empty-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	rp := k.getPolicy("empty-policy")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected sync to complete even with no SubVolumes")
	}
	if len(rp.Status.SubVolumeStatuses) != 0 {
		t.Errorf("expected 0 SubVolume statuses, got %d", len(rp.Status.SubVolumeStatuses))
	}
	if rp.Status.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 failures for empty pool, got %d", rp.Status.ConsecutiveFailures)
	}
}

func TestReplicationController_ListPoliciesError(t *testing.T) {
	k := newReplFakeK8sClient()
	k.listPolicyErr = errors.New("API server unavailable")
	nfs := newFakeNFSOperations()

	rc := newReplController(k, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// Should not panic and should not create any NFS copies.
	if nfs.copyCount() != 0 {
		t.Errorf("expected 0 NFS copies when list fails, got %d", nfs.copyCount())
	}
}

func TestReplicationController_ConcurrentSafety(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("concurrent-policy", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	// Run processOnce concurrently from multiple goroutines.
	var wg sync.WaitGroup
	ctx := context.Background()
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rc.processOnce(ctx)
		}()
	}
	wg.Wait()

	// Should not panic, and policy should be in a valid state.
	rp := k.getPolicy("concurrent-policy")
	if rp == nil {
		t.Fatal("policy should still exist after concurrent access")
	}
}

func TestReplicationController_NowFuncOverride(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("time-policy", "prod-pool", "1h")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	// Set now to a fixed time.
	fixedTime := time.Date(2026, 2, 17, 12, 0, 0, 0, time.UTC)
	var callCount atomic.Int32
	rc.SetNowFunc(func() time.Time {
		callCount.Add(1)
		return fixedTime
	})

	ctx := context.Background()
	rc.processOnce(ctx)

	rp := k.getPolicy("time-policy")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected sync to occur")
	}

	// Verify fixed time was used.
	if !rp.Status.LastSyncTime.Time.Equal(fixedTime) {
		t.Errorf("expected LastSyncTime to be fixed time %v, got %v", fixedTime, rp.Status.LastSyncTime.Time)
	}

	// Verify nowFunc was called.
	if callCount.Load() == 0 {
		t.Error("expected nowFunc to be called at least once")
	}
}

func TestReplicationController_NilSelector(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv2 := newTestSubVolume("pvc-22222222-2222-2222-2222-222222222222", "prod-pool", "share-1", 5)
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)

	// Policy with no selector -- should replicate all SubVolumes.
	policy := newTestReplicationPolicy("all-policy", "prod-pool", "1ms")
	policy.Spec.SubVolumeSelector = nil
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	rp := k.getPolicy("all-policy")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected sync to occur")
	}
	if len(rp.Status.SubVolumeStatuses) != 2 {
		t.Errorf("expected 2 SubVolume statuses (nil selector = all), got %d", len(rp.Status.SubVolumeStatuses))
	}
}

func TestReplicationController_EmptyLabelSelector(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	// Policy with empty MatchLabels -- should replicate all SubVolumes.
	policy := newTestReplicationPolicy("empty-selector", "prod-pool", "1ms")
	policy.Spec.SubVolumeSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{},
	}
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	rp := k.getPolicy("empty-selector")
	if len(rp.Status.SubVolumeStatuses) != 1 {
		t.Errorf("expected 1 SubVolume status (empty selector = all), got %d", len(rp.Status.SubVolumeStatuses))
	}
}

func TestReplicationController_SelectorMatchesNone(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	// Policy with a selector that matches no SubVolumes.
	policy := newTestReplicationPolicyWithSelector("no-match", "prod-pool", "1ms",
		map[string]string{"nonexistent": "label"})
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	rp := k.getPolicy("no-match")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected sync to complete (even with no matched SubVolumes)")
	}
	if len(rp.Status.SubVolumeStatuses) != 0 {
		t.Errorf("expected 0 SubVolume statuses, got %d", len(rp.Status.SubVolumeStatuses))
	}
	if nfs.copyCount() != 0 {
		t.Errorf("expected 0 copies for non-matching selector, got %d", nfs.copyCount())
	}
}

func TestReplicationController_MkdirFailure(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()
	nfs.MkdirErr = errors.New("permission denied")

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("mkdir-fail", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	rp := k.getPolicy("mkdir-fail")
	if rp.Status.ConsecutiveFailures < 1 {
		t.Errorf("expected at least 1 failure when mkdir fails, got %d", rp.Status.ConsecutiveFailures)
	}
}

func TestReplicationController_MultiplePoliciess(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	// SubVolumes for two pools.
	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "pool-a", "share-1", 10)
	sv2 := newTestSubVolume("pvc-22222222-2222-2222-2222-222222222222", "pool-b", "share-2", 5)
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)

	policy1 := newTestReplicationPolicy("policy-a", "pool-a", "1ms")
	policy2 := newTestReplicationPolicy("policy-b", "pool-b", "1ms")
	k.addPolicy(policy1)
	k.addPolicy(policy2)

	rc := newReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	waitForPolicySync(t, k, "policy-a", 3*time.Second)
	waitForPolicySync(t, k, "policy-b", 3*time.Second)

	rpA := k.getPolicy("policy-a")
	rpB := k.getPolicy("policy-b")

	if rpA.Status.LastSyncTime == nil {
		t.Error("policy-a should have synced")
	}
	if rpB.Status.LastSyncTime == nil {
		t.Error("policy-b should have synced")
	}
}

func TestFilterSubVolumes(t *testing.T) {
	tests := []struct {
		name     string
		svs      []v1alpha1.SubVolume
		selector *metav1.LabelSelector
		wantLen  int
	}{
		{
			name: "nil selector returns all",
			svs: []v1alpha1.SubVolume{
				{ObjectMeta: metav1.ObjectMeta{Name: "sv1", Labels: map[string]string{"a": "b"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "sv2", Labels: map[string]string{"c": "d"}}},
			},
			selector: nil,
			wantLen:  2,
		},
		{
			name: "empty match labels returns all",
			svs: []v1alpha1.SubVolume{
				{ObjectMeta: metav1.ObjectMeta{Name: "sv1", Labels: map[string]string{"a": "b"}}},
			},
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{}},
			wantLen:  1,
		},
		{
			name: "filters matching labels",
			svs: []v1alpha1.SubVolume{
				{ObjectMeta: metav1.ObjectMeta{Name: "sv1", Labels: map[string]string{"repl": "yes"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "sv2", Labels: map[string]string{"repl": "no"}}},
				{ObjectMeta: metav1.ObjectMeta{Name: "sv3", Labels: map[string]string{"repl": "yes", "extra": "val"}}},
			},
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{"repl": "yes"}},
			wantLen:  2,
		},
		{
			name: "no matches",
			svs: []v1alpha1.SubVolume{
				{ObjectMeta: metav1.ObjectMeta{Name: "sv1", Labels: map[string]string{"a": "b"}}},
			},
			selector: &metav1.LabelSelector{MatchLabels: map[string]string{"x": "y"}},
			wantLen:  0,
		},
		{
			name:     "empty input",
			svs:      nil,
			selector: nil,
			wantLen:  0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterSubVolumes(tt.svs, tt.selector)
			if len(got) != tt.wantLen {
				t.Errorf("filterSubVolumes() returned %d items, want %d", len(got), tt.wantLen)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Phase 1 & 2 Tests: IncrementalSync, BandwidthLimit, Parallel Syncs, Metadata
// ---------------------------------------------------------------------------

func TestReplicationController_IncrementalSyncDefault(t *testing.T) {
	// nil IncrementalSync → uses SyncDirWithOptions (rsync).
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("inc-default", "prod-pool", "1ms")
	policy.Spec.IncrementalSync = nil // nil → treated as true
	k.addPolicy(policy)

	rc := newReplController(k, nfs)
	rc.processOnce(context.Background())

	rp := k.getPolicy("inc-default")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected sync to complete")
	}
	// SyncDirWithOptions should have been called (not just CopyDir).
	if nfs.getSyncCallCount() < 1 {
		t.Errorf("expected SyncDirWithOptions to be called, got %d calls", nfs.getSyncCallCount())
	}
}

func TestReplicationController_IncrementalSyncFalse(t *testing.T) {
	// IncrementalSync=false → uses CopyDir.
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("inc-false", "prod-pool", "1ms")
	f := false
	policy.Spec.IncrementalSync = &f
	k.addPolicy(policy)

	rc := newReplController(k, nfs)
	rc.processOnce(context.Background())

	rp := k.getPolicy("inc-false")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected sync to complete")
	}
	// SyncDirWithOptions should NOT have been called.
	if nfs.getSyncCallCount() != 0 {
		t.Errorf("expected SyncDirWithOptions NOT to be called when IncrementalSync=false, got %d calls", nfs.getSyncCallCount())
	}
	// CopyDir should have been called instead.
	if nfs.copyCount() < 1 {
		t.Errorf("expected CopyDir to be called when IncrementalSync=false, got %d calls", nfs.copyCount())
	}
}

func TestReplicationController_BandwidthLimit(t *testing.T) {
	// Verify Mbps→KBps conversion flows through to SyncOptions.
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("bw-limit", "prod-pool", "1ms")
	policy.Spec.BandwidthLimitMbps = 100 // 100 Mbps = 12500 KBps
	k.addPolicy(policy)

	rc := newReplController(k, nfs)
	rc.processOnce(context.Background())

	opts := nfs.getLastSyncOpts()
	if opts.BandwidthLimitKBps != 12500 {
		t.Errorf("expected BandwidthLimitKBps=12500 (100*125), got %d", opts.BandwidthLimitKBps)
	}
}

func TestReplicationController_RsyncOptions(t *testing.T) {
	// Verify extra rsync args flow through to SyncOptions.
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("rsync-opts", "prod-pool", "1ms")
	policy.Spec.RsyncOptions = []string{"--compress", "--checksum"}
	k.addPolicy(policy)

	rc := newReplController(k, nfs)
	rc.processOnce(context.Background())

	opts := nfs.getLastSyncOpts()
	if len(opts.ExtraArgs) != 2 || opts.ExtraArgs[0] != "--compress" || opts.ExtraArgs[1] != "--checksum" {
		t.Errorf("expected ExtraArgs=[--compress --checksum], got %v", opts.ExtraArgs)
	}
}

func TestReplicationController_ParallelSyncs(t *testing.T) {
	// With MaxParallelSyncs=3 and 5 SubVolumes, all 5 should sync successfully.
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	for i := 0; i < 5; i++ {
		name := fmt.Sprintf("pvc-%08d-0000-0000-0000-%012d", i, i)
		sv := newTestSubVolume(name, "prod-pool", "share-1", 10)
		k.addSubVolume(sv)
	}

	policy := newTestReplicationPolicy("parallel", "prod-pool", "1ms")
	policy.Spec.MaxParallelSyncs = 3
	k.addPolicy(policy)

	rc := newReplController(k, nfs)
	rc.processOnce(context.Background())

	rp := k.getPolicy("parallel")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected sync to complete")
	}
	if len(rp.Status.SubVolumeStatuses) != 5 {
		t.Fatalf("expected 5 SubVolume statuses, got %d", len(rp.Status.SubVolumeStatuses))
	}
	for _, svs := range rp.Status.SubVolumeStatuses {
		if svs.LastError != "" {
			t.Errorf("unexpected error for SubVolume %q: %s", svs.SubVolumeName, svs.LastError)
		}
	}
}

func TestReplicationController_ParallelPartialFailure(t *testing.T) {
	// With parallel syncs, some SubVolumes fail and are reported correctly.
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()
	nfs.SyncErr = errors.New("simulated rsync failure")

	for i := 0; i < 3; i++ {
		name := fmt.Sprintf("pvc-%08d-0000-0000-0000-%012d", i, i)
		sv := newTestSubVolume(name, "prod-pool", "share-1", 10)
		k.addSubVolume(sv)
	}

	policy := newTestReplicationPolicy("parallel-fail", "prod-pool", "1ms")
	policy.Spec.MaxParallelSyncs = 2
	k.addPolicy(policy)

	rc := newReplController(k, nfs)
	rc.processOnce(context.Background())

	rp := k.getPolicy("parallel-fail")
	if rp.Status.ConsecutiveFailures < 1 {
		t.Errorf("expected at least 1 consecutive failure, got %d", rp.Status.ConsecutiveFailures)
	}
	// SubVolume statuses should still be present with errors.
	if len(rp.Status.SubVolumeStatuses) != 3 {
		t.Fatalf("expected 3 SubVolume statuses, got %d", len(rp.Status.SubVolumeStatuses))
	}
	for _, svs := range rp.Status.SubVolumeStatuses {
		if svs.LastError == "" {
			t.Errorf("expected error for SubVolume %q", svs.SubVolumeName)
		}
	}
}

func TestReplicationController_SyncDirFailure(t *testing.T) {
	// SyncErr injected → sync fails.
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()
	nfs.SyncErr = errors.New("rsync transport error")

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("sync-fail", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)
	rc.processOnce(context.Background())

	rp := k.getPolicy("sync-fail")
	if rp.Status.ConsecutiveFailures < 1 {
		t.Errorf("expected at least 1 consecutive failure, got %d", rp.Status.ConsecutiveFailures)
	}
	if rp.Status.LastError == "" {
		t.Error("expected LastError to be set")
	}
}

func TestReplicationController_WritesMetadataSidecar(t *testing.T) {
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("meta-write", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)
	rc.processOnce(context.Background())

	// Verify metadata file was written.
	metaPath := "/repl/dst/10.245.3.8/pvcs/pvc-11111111-1111-1111-1111-111111111111/.subvolume-metadata.json"
	data, ok := nfs.getFile(metaPath)
	if !ok {
		t.Fatalf("expected metadata file at %s", metaPath)
	}
	if len(data) == 0 {
		t.Fatal("metadata file is empty")
	}

	// Verify it round-trips.
	meta, err := readMetadata(nfs, "/repl/dst/10.245.3.8/pvcs/pvc-11111111-1111-1111-1111-111111111111")
	if err != nil {
		t.Fatalf("readMetadata failed: %v", err)
	}
	if meta.SubVolumeName != "pvc-11111111-1111-1111-1111-111111111111" {
		t.Errorf("expected SubVolumeName pvc-111..., got %s", meta.SubVolumeName)
	}
	if meta.ReplicationPolicy != "meta-write" {
		t.Errorf("expected ReplicationPolicy meta-write, got %s", meta.ReplicationPolicy)
	}
	if meta.Spec.PoolName != "prod-pool" {
		t.Errorf("expected Spec.PoolName prod-pool, got %s", meta.Spec.PoolName)
	}
}

func TestReplicationController_MetadataRoundTrip(t *testing.T) {
	nfs := newFakeNFSOperations()
	// Ensure parent dir exists for WriteFile.
	meta := &SubVolumeMetadata{
		SubVolumeName: "pvc-test",
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           "test-pool",
			ShareID:            "share-123",
			ShareMountTargetIP: "10.0.0.1",
			SubPath:            "/pvcs/pvc-test",
			RequestedGB:        50,
			PVName:             "pv-test",
			PVCName:            "my-pvc",
			PVCNamespace:       "default",
			ReclaimPolicy:      "Delete",
		},
		Labels:            map[string]string{"app": "web", "env": "prod"},
		ReplicationPolicy: "my-policy",
	}

	if err := writeMetadata(nfs, "/tmp/test", meta); err != nil {
		t.Fatalf("writeMetadata failed: %v", err)
	}

	got, err := readMetadata(nfs, "/tmp/test")
	if err != nil {
		t.Fatalf("readMetadata failed: %v", err)
	}

	if got.SubVolumeName != meta.SubVolumeName {
		t.Errorf("SubVolumeName: got %q, want %q", got.SubVolumeName, meta.SubVolumeName)
	}
	if got.Spec.PoolName != meta.Spec.PoolName {
		t.Errorf("Spec.PoolName: got %q, want %q", got.Spec.PoolName, meta.Spec.PoolName)
	}
	if got.Spec.ShareID != meta.Spec.ShareID {
		t.Errorf("Spec.ShareID: got %q, want %q", got.Spec.ShareID, meta.Spec.ShareID)
	}
	if got.Spec.PVCName != meta.Spec.PVCName {
		t.Errorf("Spec.PVCName: got %q, want %q", got.Spec.PVCName, meta.Spec.PVCName)
	}
	if got.Spec.PVCNamespace != meta.Spec.PVCNamespace {
		t.Errorf("Spec.PVCNamespace: got %q, want %q", got.Spec.PVCNamespace, meta.Spec.PVCNamespace)
	}
	if got.Spec.RequestedGB != meta.Spec.RequestedGB {
		t.Errorf("Spec.RequestedGB: got %d, want %d", got.Spec.RequestedGB, meta.Spec.RequestedGB)
	}
	if got.Labels["app"] != "web" || got.Labels["env"] != "prod" {
		t.Errorf("Labels mismatch: got %v", got.Labels)
	}
	if got.ReplicationPolicy != meta.ReplicationPolicy {
		t.Errorf("ReplicationPolicy: got %q, want %q", got.ReplicationPolicy, meta.ReplicationPolicy)
	}
}

func TestReplicationController_MetadataWriteFailureNonFatal(t *testing.T) {
	// WriteFile error does not fail the sync.
	k := newReplFakeK8sClient()
	nfs := newFakeNFSOperations()
	nfs.WriteFileErr = errors.New("disk full")

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("meta-fail", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, nfs)
	rc.processOnce(context.Background())

	rp := k.getPolicy("meta-fail")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected sync to complete despite metadata write failure")
	}
	if rp.Status.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 failures (metadata write is non-fatal), got %d", rp.Status.ConsecutiveFailures)
	}
}

func TestMatchesLabels(t *testing.T) {
	tests := []struct {
		name     string
		resource map[string]string
		required map[string]string
		want     bool
	}{
		{
			name:     "all required present",
			resource: map[string]string{"a": "1", "b": "2", "c": "3"},
			required: map[string]string{"a": "1", "b": "2"},
			want:     true,
		},
		{
			name:     "missing required key",
			resource: map[string]string{"a": "1"},
			required: map[string]string{"a": "1", "b": "2"},
			want:     false,
		},
		{
			name:     "wrong value",
			resource: map[string]string{"a": "1"},
			required: map[string]string{"a": "wrong"},
			want:     false,
		},
		{
			name:     "empty required matches all",
			resource: map[string]string{"a": "1"},
			required: map[string]string{},
			want:     true,
		},
		{
			name:     "nil resource labels",
			resource: nil,
			required: map[string]string{"a": "1"},
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := matchesLabels(tt.resource, tt.required)
			if got != tt.want {
				t.Errorf("matchesLabels() = %v, want %v", got, tt.want)
			}
		})
	}
}
