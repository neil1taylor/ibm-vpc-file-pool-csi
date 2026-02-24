//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/hooks"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ---------------------------------------------------------------------------
// Helpers — ReplicationPolicy + SubVolume fixture builders
// ---------------------------------------------------------------------------

func newIntegReplicationPolicy(name, sourcePool, schedule string) *v1alpha1.ReplicationPolicy {
	return &v1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ReplicationPolicySpec{
			SourcePoolName:       sourcePool,
			DestinationNFSServer: "10.245.3.8",
			DestinationBasePath:  "/pvcs",
			Schedule:             schedule,
			MaxRetries:           3,
		},
	}
}

func newIntegReplicationPolicyWithSelector(name, sourcePool, schedule string, labels map[string]string) *v1alpha1.ReplicationPolicy {
	rp := newIntegReplicationPolicy(name, sourcePool, schedule)
	rp.Spec.SubVolumeSelector = &metav1.LabelSelector{
		MatchLabels: labels,
	}
	return rp
}

func newIntegReplSubVolume(name, poolName, shareID, mountIP string) *v1alpha1.SubVolume {
	now := metav1.Now()
	return &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":     poolName,
				"storage.ibmcloud.io/share-id": shareID,
			},
		},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:           poolName,
			ShareID:            shareID,
			ShareMountTargetIP: mountIP,
			SubPath:            "/pvcs/" + name,
			RequestedGB:        10,
			PVName:             name,
			PVCName:            "repl-pvc",
			PVCNamespace:       "default",
			ReclaimPolicy:      "Delete",
		},
		Status: v1alpha1.SubVolumeStatus{
			Phase:     "Bound",
			CreatedAt: &now,
		},
	}
}

func newIntegReplController(k *fakeK8sClient, nfs *fakeNFSOperations) *pool.ReplicationController {
	rc := pool.NewReplicationController(k, nfs)
	rc.SetInterval(50 * time.Millisecond)
	return rc
}

// waitForIntegPolicyPhase polls the fakeK8sClient until the policy reaches the expected phase.
func waitForIntegPolicyPhase(t *testing.T, k *fakeK8sClient, name, phase string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			rp := k.getPolicy(name)
			if rp == nil {
				t.Fatalf("timed out waiting for policy %q to reach phase %q (not found)", name, phase)
			}
			t.Fatalf("timed out waiting for policy %q to reach phase %q (current: %q, failures: %d, err: %s)",
				name, phase, rp.Status.Phase, rp.Status.ConsecutiveFailures, rp.Status.LastError)
		case <-ticker.C:
			rp := k.getPolicy(name)
			if rp != nil && rp.Status.Phase == phase {
				return
			}
		}
	}
}

// waitForIntegPolicySync polls until LastSyncTime is set on the policy.
func waitForIntegPolicySync(t *testing.T, k *fakeK8sClient, name string, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for policy %q to sync", name)
		case <-ticker.C:
			rp := k.getPolicy(name)
			if rp != nil && rp.Status.LastSyncTime != nil {
				return
			}
		}
	}
}

// waitForIntegConsecutiveFailures polls until ConsecutiveFailures reaches at least min.
func waitForIntegConsecutiveFailures(t *testing.T, k *fakeK8sClient, name string, min int32, timeout time.Duration) {
	t.Helper()
	deadline := time.After(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			rp := k.getPolicy(name)
			current := int32(0)
			if rp != nil {
				current = rp.Status.ConsecutiveFailures
			}
			t.Fatalf("timed out waiting for policy %q to reach %d failures (current: %d)", name, min, current)
		case <-ticker.C:
			rp := k.getPolicy(name)
			if rp != nil && rp.Status.ConsecutiveFailures >= min {
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Fake Hook Executor (for hook tests)
// ---------------------------------------------------------------------------

type fakeHookExecutor struct {
	calls []v1alpha1.Hook
	err   error // if non-nil, Execute returns this error
}

func (f *fakeHookExecutor) Execute(_ context.Context, hook v1alpha1.Hook) (*v1alpha1.HookResult, error) {
	f.calls = append(f.calls, hook)
	result := &v1alpha1.HookResult{
		Name:      hook.Name,
		Success:   f.err == nil,
		StartedAt: metav1.Now(),
		Duration:  "0s",
	}
	if f.err != nil {
		result.Message = f.err.Error()
	}
	return result, f.err
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestReplication_FullLifecycle creates a pool with 2 SubVolumes and a policy,
// starts the replication controller, and verifies that the policy transitions
// to Active with correct SubVolumeStatuses.
func TestReplication_FullLifecycle(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newIntegReplSubVolume("pvc-11111111-1111-1111-1111-111111111111", "repl-pool", "share-1", "10.240.0.1")
	sv2 := newIntegReplSubVolume("pvc-22222222-2222-2222-2222-222222222222", "repl-pool", "share-1", "10.240.0.1")
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)

	policy := newIntegReplicationPolicy("lifecycle-policy", "repl-pool", "1ms")
	k.addPolicy(policy)

	rc := newIntegReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Wait for the first successful sync.
	waitForIntegPolicySync(t, k, "lifecycle-policy", 5*time.Second)

	rp := k.getPolicy("lifecycle-policy")
	if rp == nil {
		t.Fatal("policy should exist")
	}

	// Verify phase is Active.
	if rp.Status.Phase != "Active" {
		t.Errorf("expected phase Active, got %q", rp.Status.Phase)
	}

	// Verify LastSyncTime is set.
	if rp.Status.LastSyncTime == nil {
		t.Error("expected LastSyncTime to be set")
	}

	// Verify 2 SubVolumeStatuses.
	if len(rp.Status.SubVolumeStatuses) != 2 {
		t.Fatalf("expected 2 SubVolume statuses, got %d", len(rp.Status.SubVolumeStatuses))
	}
	for _, svs := range rp.Status.SubVolumeStatuses {
		if svs.LastSyncTime == nil {
			t.Errorf("SubVolume %q: expected LastSyncTime to be set", svs.SubVolumeName)
		}
		if svs.LastError != "" {
			t.Errorf("SubVolume %q: unexpected error %q", svs.SubVolumeName, svs.LastError)
		}
	}

	// Verify ConsecutiveFailures is 0.
	if rp.Status.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", rp.Status.ConsecutiveFailures)
	}

	// Verify NFS copies were performed.
	if nfs.copyCount() < 2 {
		t.Errorf("expected at least 2 NFS copies, got %d", nfs.copyCount())
	}
}

// TestReplication_FailureRecovery injects an NFS error, waits for failures
// to accumulate, clears the error, and verifies the policy recovers.
func TestReplication_FailureRecovery(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newIntegReplSubVolume("pvc-11111111-1111-1111-1111-111111111111", "recovery-pool", "share-1", "10.240.0.1")
	k.addSubVolume(sv1)

	policy := newIntegReplicationPolicy("recovery-policy", "recovery-pool", "1ms")
	k.addPolicy(policy)

	// Inject NFS error.
	nfs.CopyErr = errors.New("destination unreachable")

	rc := newIntegReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Wait for failures to accumulate.
	waitForIntegConsecutiveFailures(t, k, "recovery-policy", 2, 5*time.Second)

	rp := k.getPolicy("recovery-policy")
	if rp.Status.LastError == "" {
		t.Error("expected LastError to be set during failure")
	}

	// Clear the NFS error.
	nfs.mu.Lock()
	nfs.CopyErr = nil
	nfs.mu.Unlock()

	// Wait for the policy to sync successfully (LastSyncTime set, failures reset).
	waitForIntegPolicySync(t, k, "recovery-policy", 5*time.Second)

	rp = k.getPolicy("recovery-policy")
	if rp.Status.ConsecutiveFailures != 0 {
		t.Errorf("expected consecutive failures to reset to 0, got %d", rp.Status.ConsecutiveFailures)
	}
	if rp.Status.LastError != "" {
		t.Errorf("expected LastError to be cleared after recovery, got %q", rp.Status.LastError)
	}
}

// TestReplication_MaxRetriesPauses verifies that a persistent error causes the
// policy to pause after maxRetries.
func TestReplication_MaxRetriesPauses(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()
	nfs.CopyErr = errors.New("persistent NFS failure")

	sv1 := newIntegReplSubVolume("pvc-11111111-1111-1111-1111-111111111111", "pause-pool", "share-1", "10.240.0.1")
	k.addSubVolume(sv1)

	policy := newIntegReplicationPolicy("pause-policy", "pause-pool", "1ms")
	policy.Spec.MaxRetries = 2
	k.addPolicy(policy)

	rc := newIntegReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Wait for the policy to be paused.
	waitForIntegPolicyPhase(t, k, "pause-policy", "Paused", 5*time.Second)

	rp := k.getPolicy("pause-policy")
	if rp.Status.ConsecutiveFailures < 2 {
		t.Errorf("expected at least 2 consecutive failures, got %d", rp.Status.ConsecutiveFailures)
	}

	// Record the total copies at this point.
	copiesBefore := nfs.totalCopyCount()

	// Let the controller run for a bit more — paused policy should not be processed.
	time.Sleep(200 * time.Millisecond)

	copiesAfter := nfs.totalCopyCount()
	if copiesAfter != copiesBefore {
		t.Errorf("expected no new copies after pause (before=%d, after=%d)", copiesBefore, copiesAfter)
	}
}

// TestReplication_PreSyncHookAbort verifies that a failing pre-sync hook with
// OnError=Abort prevents the NFS sync from executing.
func TestReplication_PreSyncHookAbort(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newIntegReplSubVolume("pvc-11111111-1111-1111-1111-111111111111", "hook-pool", "share-1", "10.240.0.1")
	k.addSubVolume(sv1)

	policy := newIntegReplicationPolicy("presync-abort", "hook-pool", "1ms")
	policy.Spec.PreSyncHooks = []v1alpha1.Hook{
		{
			Name:           "check-quiesce",
			Type:           v1alpha1.HookTypeExec,
			OnError:        v1alpha1.HookOnErrorAbort,
			TimeoutSeconds: 5,
			Exec: &v1alpha1.ExecHookSpec{
				PodSelector: map[string]string{"app": "db"},
				Namespace:   "default",
				Command:     []string{"pg_isready"},
			},
		},
	}
	k.addPolicy(policy)

	// Create a failing hook executor.
	hookExec := &fakeHookExecutor{err: errors.New("quiesce check failed")}
	orch := hooks.NewOrchestratorWithExecutor(hookExec)

	rc := newIntegReplController(k, nfs)
	rc.SetOrchestrator(orch)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Wait for at least one failure.
	waitForIntegConsecutiveFailures(t, k, "presync-abort", 1, 5*time.Second)

	rp := k.getPolicy("presync-abort")

	// The pre-sync hook should have been called.
	if len(hookExec.calls) == 0 {
		t.Error("expected pre-sync hook to be called at least once")
	}

	// No NFS copies should have been made (hook aborted the sync).
	if nfs.copyCount() != 0 {
		t.Errorf("expected 0 NFS copies after pre-sync abort, got %d", nfs.copyCount())
	}

	// Policy should have recorded a failure.
	if rp.Status.ConsecutiveFailures < 1 {
		t.Errorf("expected at least 1 failure, got %d", rp.Status.ConsecutiveFailures)
	}
	if rp.Status.LastError == "" {
		t.Error("expected LastError to record the hook failure")
	}
}

// TestReplication_PostSyncHookContinue verifies that a failing post-sync hook
// with OnError=Continue does not prevent the sync from being recorded as success.
func TestReplication_PostSyncHookContinue(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	sv1 := newIntegReplSubVolume("pvc-11111111-1111-1111-1111-111111111111", "hook-pool-2", "share-1", "10.240.0.1")
	k.addSubVolume(sv1)

	policy := newIntegReplicationPolicy("postsync-continue", "hook-pool-2", "1ms")
	policy.Spec.PostSyncHooks = []v1alpha1.Hook{
		{
			Name:           "notify-slack",
			Type:           v1alpha1.HookTypeHTTP,
			OnError:        v1alpha1.HookOnErrorContinue,
			TimeoutSeconds: 5,
			HTTP: &v1alpha1.HTTPHookSpec{
				URL:    "https://hooks.slack.com/test",
				Method: "POST",
			},
		},
	}
	k.addPolicy(policy)

	// The post-sync hook fails, but OnError=Continue means the sync is still a success.
	hookExec := &fakeHookExecutor{err: errors.New("slack notification failed")}
	orch := hooks.NewOrchestratorWithExecutor(hookExec)

	rc := newIntegReplController(k, nfs)
	rc.SetOrchestrator(orch)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Wait for a successful sync.
	waitForIntegPolicySync(t, k, "postsync-continue", 5*time.Second)

	rp := k.getPolicy("postsync-continue")

	// The sync should be recorded as success.
	if rp.Status.LastSyncTime == nil {
		t.Error("expected LastSyncTime to be set")
	}
	if rp.Status.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", rp.Status.ConsecutiveFailures)
	}

	// NFS copy should have been performed.
	if nfs.copyCount() < 1 {
		t.Errorf("expected at least 1 NFS copy, got %d", nfs.copyCount())
	}

	// Post-sync hook should have been called.
	if len(hookExec.calls) == 0 {
		t.Error("expected post-sync hook to be called at least once")
	}
}

// TestReplication_MultiplePolicies creates two policies targeting different
// pools and verifies they are processed independently.
func TestReplication_MultiplePolicies(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// SubVolumes in pool-a.
	svA := newIntegReplSubVolume("pvc-aaaa1111-1111-1111-1111-111111111111", "pool-a", "share-a", "10.240.0.1")
	k.addSubVolume(svA)

	// SubVolumes in pool-b.
	svB1 := newIntegReplSubVolume("pvc-bbbb1111-1111-1111-1111-111111111111", "pool-b", "share-b", "10.240.0.2")
	svB2 := newIntegReplSubVolume("pvc-bbbb2222-2222-2222-2222-222222222222", "pool-b", "share-b", "10.240.0.2")
	k.addSubVolume(svB1)
	k.addSubVolume(svB2)

	policyA := newIntegReplicationPolicy("policy-a", "pool-a", "1ms")
	policyB := newIntegReplicationPolicy("policy-b", "pool-b", "1ms")
	k.addPolicy(policyA)
	k.addPolicy(policyB)

	rc := newIntegReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Wait for both policies to sync.
	waitForIntegPolicySync(t, k, "policy-a", 5*time.Second)
	waitForIntegPolicySync(t, k, "policy-b", 5*time.Second)

	rpA := k.getPolicy("policy-a")
	rpB := k.getPolicy("policy-b")

	// Policy A should have 1 SubVolumeStatus.
	if len(rpA.Status.SubVolumeStatuses) != 1 {
		t.Errorf("policy-a: expected 1 SubVolume status, got %d", len(rpA.Status.SubVolumeStatuses))
	}

	// Policy B should have 2 SubVolumeStatuses.
	if len(rpB.Status.SubVolumeStatuses) != 2 {
		t.Errorf("policy-b: expected 2 SubVolume statuses, got %d", len(rpB.Status.SubVolumeStatuses))
	}

	// Both should be Active with no errors.
	if rpA.Status.Phase != "Active" {
		t.Errorf("policy-a: expected Active, got %q", rpA.Status.Phase)
	}
	if rpB.Status.Phase != "Active" {
		t.Errorf("policy-b: expected Active, got %q", rpB.Status.Phase)
	}

	// NFS should have at least 3 total copies (1 for pool-a, 2 for pool-b).
	if nfs.copyCount() < 3 {
		t.Errorf("expected at least 3 NFS copies, got %d", nfs.copyCount())
	}
}

// TestReplication_PartialFailure verifies that when some SubVolumes fail to
// sync, the policy records both successes and failures in SubVolumeStatuses.
func TestReplication_PartialFailure(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// Create 3 SubVolumes.
	for i := 1; i <= 3; i++ {
		name := fmt.Sprintf("pvc-%08d-0000-0000-0000-000000000000", i)
		sv := newIntegReplSubVolume(name, "partial-pool", "share-1", "10.240.0.1")
		k.addSubVolume(sv)
	}

	policy := newIntegReplicationPolicy("partial-policy", "partial-pool", "1ms")
	k.addPolicy(policy)

	// First copy succeeds, then subsequent copies fail.
	nfs.CopyErr = errors.New("disk full")
	nfs.CopyErrAfterN = 1

	rc := newIntegReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Wait for at least one failure (partial failure is reported as a failure).
	waitForIntegConsecutiveFailures(t, k, "partial-policy", 1, 5*time.Second)

	rp := k.getPolicy("partial-policy")

	// Should have SubVolumeStatuses for all 3 SubVolumes.
	if len(rp.Status.SubVolumeStatuses) != 3 {
		t.Fatalf("expected 3 SubVolume statuses, got %d", len(rp.Status.SubVolumeStatuses))
	}

	// At least 1 should have succeeded (no error) and at least 1 should have failed.
	var successCount, failCount int
	for _, svs := range rp.Status.SubVolumeStatuses {
		if svs.LastError == "" {
			successCount++
		} else {
			failCount++
		}
	}

	if successCount < 1 {
		t.Error("expected at least 1 SubVolume to succeed in partial failure scenario")
	}
	if failCount < 1 {
		t.Error("expected at least 1 SubVolume to fail in partial failure scenario")
	}
}

// TestReplication_LabelSelector verifies that the SubVolumeSelector filters
// which SubVolumes are replicated.
func TestReplication_LabelSelector(t *testing.T) {
	k := newFakeK8sClient()
	nfs := newFakeNFSOperations()

	// 3 SubVolumes, only 1 has the matching label.
	sv1 := newIntegReplSubVolume("pvc-sel-11111111-1111-1111-1111-111111111111", "selector-pool", "share-1", "10.240.0.1")
	sv1.Labels["tier"] = "critical"
	k.addSubVolume(sv1)

	sv2 := newIntegReplSubVolume("pvc-sel-22222222-2222-2222-2222-222222222222", "selector-pool", "share-1", "10.240.0.1")
	sv2.Labels["tier"] = "standard"
	k.addSubVolume(sv2)

	sv3 := newIntegReplSubVolume("pvc-sel-33333333-3333-3333-3333-333333333333", "selector-pool", "share-1", "10.240.0.1")
	// sv3 has no tier label.
	k.addSubVolume(sv3)

	policy := newIntegReplicationPolicyWithSelector("selector-policy", "selector-pool", "1ms",
		map[string]string{"tier": "critical"})
	k.addPolicy(policy)

	rc := newIntegReplController(k, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Wait for a successful sync.
	waitForIntegPolicySync(t, k, "selector-policy", 5*time.Second)

	rp := k.getPolicy("selector-policy")

	// Only 1 SubVolume should have been synced.
	if len(rp.Status.SubVolumeStatuses) != 1 {
		t.Fatalf("expected 1 SubVolume status (selector filtering), got %d", len(rp.Status.SubVolumeStatuses))
	}

	if rp.Status.SubVolumeStatuses[0].SubVolumeName != "pvc-sel-11111111-1111-1111-1111-111111111111" {
		t.Errorf("expected selected SubVolume pvc-sel-111..., got %s",
			rp.Status.SubVolumeStatuses[0].SubVolumeName)
	}

	// Only 1 NFS copy should have been made.
	if nfs.copyCount() != 1 {
		t.Errorf("expected 1 NFS copy (selector filtering), got %d", nfs.copyCount())
	}
}
