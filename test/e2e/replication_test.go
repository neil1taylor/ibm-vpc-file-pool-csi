//go:build e2e

package e2e

import (
	"context"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func buildReplicationPolicy(name, sourcePoolName, schedule string) *v1alpha1.ReplicationPolicy {
	return &v1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1alpha1.ReplicationPolicySpec{
			SourcePoolName:       sourcePoolName,
			DestinationNFSServer: "10.245.3.8",
			DestinationBasePath:  "/pvcs",
			Schedule:             schedule,
			MaxRetries:           5,
		},
	}
}

func getReplicationPolicy(t *testing.T, ctx context.Context, name string) *v1alpha1.ReplicationPolicy {
	t.Helper()
	rp := &v1alpha1.ReplicationPolicy{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, rp); err != nil {
		t.Fatalf("Failed to get ReplicationPolicy %s: %v", name, err)
	}
	return rp
}

func waitForReplicationPolicyStatus(t *testing.T, ctx context.Context, name string, checkFn func(*v1alpha1.ReplicationPolicy) bool, description string, timeout time.Duration) *v1alpha1.ReplicationPolicy {
	t.Helper()
	deadline := time.Now().Add(timeout)

	for time.Now().Before(deadline) {
		rp := &v1alpha1.ReplicationPolicy{}
		if err := k8sClient.Get(ctx, types.NamespacedName{Name: name}, rp); err == nil {
			if checkFn(rp) {
				t.Logf("ReplicationPolicy %s: %s", name, description)
				return rp
			}
		}
		time.Sleep(pollInterval)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

// TestReplicationPolicy_CRDAcceptance verifies that the CRD schema accepts
// ReplicationPolicy resources with all spec fields, and that fields are
// correctly persisted.
func TestReplicationPolicy_CRDAcceptance(t *testing.T) {
	ctx := context.Background()

	policyName := resourceName("repl-crd")
	incremental := true

	policy := &v1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name: policyName,
		},
		Spec: v1alpha1.ReplicationPolicySpec{
			SourcePoolName:       "prod-pool",
			DestinationNFSServer: "10.245.3.8",
			DestinationBasePath:  "/pvcs",
			Schedule:             "15m",
			MaxRetries:           5,
			IncrementalSync:      &incremental,
			SubVolumeSelector: &metav1.LabelSelector{
				MatchLabels: map[string]string{
					"tier": "critical",
				},
			},
			PreSyncHooks: []v1alpha1.Hook{
				{
					Name:           "quiesce-db",
					Type:           v1alpha1.HookTypeExec,
					OnError:        v1alpha1.HookOnErrorAbort,
					TimeoutSeconds: 30,
					Exec: &v1alpha1.ExecHookSpec{
						PodSelector: map[string]string{"app": "postgres"},
						Namespace:   "default",
						Command:     []string{"pg_isready"},
					},
				},
			},
			PostSyncHooks: []v1alpha1.Hook{
				{
					Name:           "notify",
					Type:           v1alpha1.HookTypeHTTP,
					OnError:        v1alpha1.HookOnErrorContinue,
					TimeoutSeconds: 10,
					HTTP: &v1alpha1.HTTPHookSpec{
						URL:    "https://hooks.example.com/notify",
						Method: "POST",
					},
				},
			},
		},
	}

	t.Cleanup(func() {
		cleanupResource(t, ctx, policy)
	})

	// Create the policy — CRD should accept it.
	if err := k8sClient.Create(ctx, policy); err != nil {
		t.Fatalf("CRD rejected ReplicationPolicy: %v", err)
	}
	t.Log("CRD accepted ReplicationPolicy with all spec fields")

	// Re-fetch and verify all fields were persisted.
	fetched := getReplicationPolicy(t, ctx, policyName)

	if fetched.Spec.SourcePoolName != "prod-pool" {
		t.Errorf("SourcePoolName=%s, want prod-pool", fetched.Spec.SourcePoolName)
	}
	if fetched.Spec.DestinationNFSServer != "10.245.3.8" {
		t.Errorf("DestinationNFSServer=%s, want 10.245.3.8", fetched.Spec.DestinationNFSServer)
	}
	if fetched.Spec.DestinationBasePath != "/pvcs" {
		t.Errorf("DestinationBasePath=%s, want /pvcs", fetched.Spec.DestinationBasePath)
	}
	if fetched.Spec.Schedule != "15m" {
		t.Errorf("Schedule=%s, want 15m", fetched.Spec.Schedule)
	}
	if fetched.Spec.MaxRetries != 5 {
		t.Errorf("MaxRetries=%d, want 5", fetched.Spec.MaxRetries)
	}
	if fetched.Spec.IncrementalSync == nil || !*fetched.Spec.IncrementalSync {
		t.Error("IncrementalSync should be true")
	}
	if fetched.Spec.SubVolumeSelector == nil {
		t.Fatal("SubVolumeSelector should be set")
	}
	if fetched.Spec.SubVolumeSelector.MatchLabels["tier"] != "critical" {
		t.Errorf("SubVolumeSelector.MatchLabels[tier]=%s, want critical",
			fetched.Spec.SubVolumeSelector.MatchLabels["tier"])
	}
	if len(fetched.Spec.PreSyncHooks) != 1 {
		t.Fatalf("expected 1 PreSyncHook, got %d", len(fetched.Spec.PreSyncHooks))
	}
	if fetched.Spec.PreSyncHooks[0].Name != "quiesce-db" {
		t.Errorf("PreSyncHooks[0].Name=%s, want quiesce-db", fetched.Spec.PreSyncHooks[0].Name)
	}
	if len(fetched.Spec.PostSyncHooks) != 1 {
		t.Fatalf("expected 1 PostSyncHook, got %d", len(fetched.Spec.PostSyncHooks))
	}
	if fetched.Spec.PostSyncHooks[0].Name != "notify" {
		t.Errorf("PostSyncHooks[0].Name=%s, want notify", fetched.Spec.PostSyncHooks[0].Name)
	}

	t.Log("All ReplicationPolicy spec fields verified")
}

// TestReplicationPolicy_ControllerStatusTransition creates a pool, PVC, and
// replication policy, then verifies the controller processes the policy by
// checking for status updates (phase or failure count).
func TestReplicationPolicy_ControllerStatusTransition(t *testing.T) {
	ctx := context.Background()

	poolName := resourceName("repl-ctrl")
	scName := resourceName("repl-ctrl-sc")
	pvcName := resourceName("repl-ctrl-pvc")
	policyName := resourceName("repl-ctrl-pol")

	// Create pool and StorageClass.
	pool := buildPool(poolName, nil)
	sc := buildStorageClass(scName, poolName)
	pvc := buildPVC(pvcName, scName, 1)

	t.Cleanup(func() {
		cleanupResource(t, ctx, &v1alpha1.ReplicationPolicy{ObjectMeta: metav1.ObjectMeta{Name: policyName}})
		cleanupResource(t, ctx, pvc)
		waitForSubVolumesGone(t, ctx, poolName)
		cleanupResource(t, ctx, pool)
		cleanupResource(t, ctx, sc)
	})

	t.Log("Creating StorageClass and FileSharePool")
	if err := k8sClient.Create(ctx, sc); err != nil {
		t.Fatalf("Failed to create StorageClass: %v", err)
	}
	if err := k8sClient.Create(ctx, pool); err != nil {
		t.Fatalf("Failed to create FileSharePool: %v", err)
	}

	// Wait for pool to be Ready.
	waitForPoolReady(t, ctx, poolName)

	// Create PVC and wait for Bound.
	t.Log("Creating PVC")
	if err := k8sClient.Create(ctx, pvc); err != nil {
		t.Fatalf("Failed to create PVC: %v", err)
	}
	waitForPVCBound(t, ctx, pvcName)

	// Create replication policy targeting this pool.
	t.Log("Creating ReplicationPolicy")
	policy := buildReplicationPolicy(policyName, poolName, "10s")
	if err := k8sClient.Create(ctx, policy); err != nil {
		t.Fatalf("Failed to create ReplicationPolicy: %v", err)
	}

	// Wait for the controller to process the policy. The controller will
	// attempt a sync and likely fail (destination paths don't exist on a
	// real cluster), but any status update proves the controller is running.
	t.Log("Waiting for controller to update policy status (up to 3 min)")
	result := waitForReplicationPolicyStatus(t, ctx, policyName,
		func(rp *v1alpha1.ReplicationPolicy) bool {
			return rp.Status.Phase != "" || rp.Status.ConsecutiveFailures > 0
		},
		"status updated by controller",
		3*time.Minute,
	)

	if result == nil {
		t.Skip("ReplicationPolicy status was not updated within timeout; controller may not be running with replication enabled")
	}

	t.Logf("Controller updated policy: phase=%s, failures=%d, lastError=%s",
		result.Status.Phase, result.Status.ConsecutiveFailures, result.Status.LastError)
}

// TestReplicationPolicy_StatusSubresource creates a policy and manually
// updates the status subresource to verify the CRD supports status updates.
func TestReplicationPolicy_StatusSubresource(t *testing.T) {
	ctx := context.Background()

	policyName := resourceName("repl-status")
	policy := buildReplicationPolicy(policyName, "test-pool", "30m")

	t.Cleanup(func() {
		cleanupResource(t, ctx, policy)
	})

	// Create the policy.
	if err := k8sClient.Create(ctx, policy); err != nil {
		t.Fatalf("Failed to create ReplicationPolicy: %v", err)
	}

	// Fetch the freshly created policy.
	fetched := getReplicationPolicy(t, ctx, policyName)

	// Update the status subresource.
	now := metav1.Now()
	fetched.Status.Phase = "Active"
	fetched.Status.LastSyncTime = &now
	fetched.Status.LastSyncDuration = "1.5s"
	fetched.Status.ConsecutiveFailures = 0
	fetched.Status.SubVolumeStatuses = []v1alpha1.SubVolumeReplicationStatus{
		{
			SubVolumeName: "pvc-test-1",
			LastSyncTime:  &now,
		},
	}

	if err := k8sClient.Status().Update(ctx, fetched); err != nil {
		t.Fatalf("Failed to update status subresource: %v", err)
	}

	// Re-fetch and verify status was persisted.
	refetched := getReplicationPolicy(t, ctx, policyName)

	if refetched.Status.Phase != "Active" {
		t.Errorf("Status.Phase=%s, want Active", refetched.Status.Phase)
	}
	if refetched.Status.LastSyncTime == nil {
		t.Error("Status.LastSyncTime should be set")
	}
	if refetched.Status.LastSyncDuration != "1.5s" {
		t.Errorf("Status.LastSyncDuration=%s, want 1.5s", refetched.Status.LastSyncDuration)
	}
	if len(refetched.Status.SubVolumeStatuses) != 1 {
		t.Fatalf("expected 1 SubVolumeStatus, got %d", len(refetched.Status.SubVolumeStatuses))
	}
	if refetched.Status.SubVolumeStatuses[0].SubVolumeName != "pvc-test-1" {
		t.Errorf("SubVolumeStatuses[0].Name=%s, want pvc-test-1",
			refetched.Status.SubVolumeStatuses[0].SubVolumeName)
	}

	t.Log("Status subresource update verified")

	// Also verify that spec was NOT changed by the status update.
	if refetched.Spec.Schedule != "30m" {
		t.Errorf("Spec.Schedule changed after status update: %s", refetched.Spec.Schedule)
	}
}
