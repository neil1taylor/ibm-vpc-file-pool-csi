package pool

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	dto "github.com/prometheus/client_model/go"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
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
		return nil, fmt.Errorf("replication policy not found: %s", name)
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
		return fmt.Errorf("replication policy already exists: %s", rp.Name)
	}
	f.policies[rp.Name] = rp.DeepCopy()
	return nil
}

func (f *replFakeK8sClient) UpdateReplicationPolicyStatus(_ context.Context, rp *v1alpha1.ReplicationPolicy) error {
	f.replMu.Lock()
	defer f.replMu.Unlock()
	existing, ok := f.policies[rp.Name]
	if !ok {
		return fmt.Errorf("replication policy not found: %s", rp.Name)
	}
	existing.Status = *rp.Status.DeepCopy()
	return nil
}

func (f *replFakeK8sClient) DeleteReplicationPolicy(_ context.Context, name string) error {
	f.replMu.Lock()
	defer f.replMu.Unlock()
	if _, exists := f.policies[name]; !exists {
		return fmt.Errorf("replication policy not found: %s", name)
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

func newReplTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

func newReplFakeDirectClient() client.Client {
	return fakeclient.NewClientBuilder().
		WithScheme(newReplTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		Build()
}

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

func newReplController(k8s *replFakeK8sClient, dc client.Client, nfs *fakeNFSOperations) *ReplicationController {
	rc := NewReplicationController(k8s, dc, nfs)
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

// waitForReplJob polls until a replication Job is created for the given policy+subvolume.
func waitForReplJob(t *testing.T, dc client.Client, policyName, svName string, timeout time.Duration) {
	t.Helper()
	jobName := replJobName(policyName, svName)
	deadline := time.After(timeout)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for replication Job %s/%s", replJobNamespace, jobName)
		case <-ticker.C:
			job := &batchv1.Job{}
			if err := dc.Get(context.Background(), types.NamespacedName{
				Namespace: replJobNamespace,
				Name:      jobName,
			}, job); err == nil {
				return
			}
		}
	}
}

// simulateReplJobSucceeded marks a replication Job as succeeded.
func simulateReplJobSucceeded(t *testing.T, dc client.Client, policyName, svName string) {
	t.Helper()
	jobName := replJobName(policyName, svName)
	ctx := context.Background()

	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for replication Job %s/%s to be created", replJobNamespace, jobName)
		case <-ticker.C:
			job := &batchv1.Job{}
			if err := dc.Get(ctx, types.NamespacedName{
				Namespace: replJobNamespace,
				Name:      jobName,
			}, job); err == nil {
				job.Status.Succeeded = 1
				if err := dc.Status().Update(ctx, job); err != nil {
					t.Fatalf("failed to update Job status: %v", err)
				}
				return
			}
		}
	}
}

// simulateReplJobFailed marks a replication Job as failed.
func simulateReplJobFailed(t *testing.T, dc client.Client, policyName, svName string) {
	t.Helper()
	jobName := replJobName(policyName, svName)
	ctx := context.Background()

	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for replication Job %s/%s to be created", replJobNamespace, jobName)
		case <-ticker.C:
			job := &batchv1.Job{}
			if err := dc.Get(ctx, types.NamespacedName{
				Namespace: replJobNamespace,
				Name:      jobName,
			}, job); err == nil {
				// backoffLimit+1 = 4
				job.Status.Failed = 4
				if err := dc.Status().Update(ctx, job); err != nil {
					t.Fatalf("failed to update Job status: %v", err)
				}
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

// verifyReplPVCreated checks that a replication PV was created with the expected server/path.
func verifyReplPVCreated(t *testing.T, dc client.Client, pvName, expectedServer, expectedPath string) {
	t.Helper()
	pv := &corev1.PersistentVolume{}
	if err := dc.Get(context.Background(), types.NamespacedName{Name: pvName}, pv); err != nil {
		t.Fatalf("expected PV %s to exist: %v", pvName, err)
	}
	if pv.Spec.NFS == nil {
		t.Fatalf("PV %s has no NFS source", pvName)
	}
	if pv.Spec.NFS.Server != expectedServer {
		t.Errorf("PV %s server: got %q, want %q", pvName, pv.Spec.NFS.Server, expectedServer)
	}
	if pv.Spec.NFS.Path != expectedPath {
		t.Errorf("PV %s path: got %q, want %q", pvName, pv.Spec.NFS.Path, expectedPath)
	}
	// Verify mount options include sec=sys
	foundSecSys := false
	for _, opt := range pv.Spec.MountOptions {
		if opt == "sec=sys" {
			foundSecSys = true
			break
		}
	}
	if !foundSecSys {
		t.Errorf("PV %s mount options missing sec=sys: %v", pvName, pv.Spec.MountOptions)
	}
}

// verifyReplPVCCreated checks that a replication PVC was created.
func verifyReplPVCCreated(t *testing.T, dc client.Client, pvcName string) {
	t.Helper()
	pvc := &corev1.PersistentVolumeClaim{}
	if err := dc.Get(context.Background(), types.NamespacedName{
		Namespace: replJobNamespace,
		Name:      pvcName,
	}, pvc); err != nil {
		t.Fatalf("expected PVC %s/%s to exist: %v", replJobNamespace, pvcName, err)
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestReplicationController_CreatesJobForSubVolume(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv1.Spec.ShareMountTargetIP = "10.240.0.1"
	sv1.Spec.ShareExportPath = "/share_abc123"
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Wait for the Job to be created.
	waitForReplJob(t, dc, "prod-to-dr", "pvc-11111111-1111-1111-1111-111111111111", 3*time.Second)

	// Verify source PV was created with correct NFS details.
	srcPVName := replSourcePVName("prod-to-dr", "pvc-11111111-1111-1111-1111-111111111111")
	verifyReplPVCreated(t, dc, srcPVName, "10.240.0.1", "/share_abc123")

	// Verify dest PV was created with correct NFS details.
	dstPVName := replDestPVName("prod-to-dr", "pvc-11111111-1111-1111-1111-111111111111")
	verifyReplPVCreated(t, dc, dstPVName, "10.245.3.8", "/")

	// Verify PVCs created.
	srcPVCName := replSourcePVCName("prod-to-dr", "pvc-11111111-1111-1111-1111-111111111111")
	verifyReplPVCCreated(t, dc, srcPVCName)
	dstPVCName := replDestPVCName("prod-to-dr", "pvc-11111111-1111-1111-1111-111111111111")
	verifyReplPVCCreated(t, dc, dstPVCName)

	// Verify Job has correct volume mounts.
	jobName := replJobName("prod-to-dr", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to exist: %v", err)
	}
	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}
	if len(containers[0].VolumeMounts) != 2 {
		t.Fatalf("expected 2 volume mounts, got %d", len(containers[0].VolumeMounts))
	}
	if containers[0].VolumeMounts[0].Name != "source" || !containers[0].VolumeMounts[0].ReadOnly {
		t.Errorf("expected source volume mount to be read-only")
	}
	if containers[0].VolumeMounts[1].Name != "dest" {
		t.Errorf("expected dest volume mount")
	}
}

func TestReplicationController_BasicSyncCycle(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv2 := newTestSubVolume("pvc-22222222-2222-2222-2222-222222222222", "prod-pool", "share-1", 5)
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)

	policy := newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms")
	policy.Spec.MaxParallelSyncs = 2 // Allow both Jobs to be created in one cycle.
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Simulate both Jobs succeeding (order-independent, each polls for its Job).
	simulateReplJobSucceeded(t, dc, "prod-to-dr", "pvc-11111111-1111-1111-1111-111111111111")
	simulateReplJobSucceeded(t, dc, "prod-to-dr", "pvc-22222222-2222-2222-2222-222222222222")

	// Wait for sync to complete.
	waitForPolicySync(t, k, "prod-to-dr", 5*time.Second)

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
	}
}

func TestReplicationController_ScheduleTracking(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	// Policy with 1 hour schedule -- should only sync once.
	policy := newTestReplicationPolicy("hourly-policy", "prod-pool", "1h")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()

	// First processOnce: creates a Job.
	rc.processOnce(ctx)

	// Job should exist.
	jobName := replJobName("hourly-policy", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created on first processOnce: %v", err)
	}

	// Simulate Job success.
	job.Status.Succeeded = 1
	if err := dc.Status().Update(ctx, job); err != nil {
		t.Fatalf("failed to update Job status: %v", err)
	}

	// Second processOnce: isDue returns false because we just recorded sync time
	// and the schedule is 1h. But the Job completion won't be picked up until
	// the policy is due again. Force isDue by clearing the lastSyncTimes.
	rc.mu.Lock()
	delete(rc.lastSyncTimes, "hourly-policy")
	rc.mu.Unlock()

	// Now processOnce should pick up the completed Job and record success.
	rc.processOnce(ctx)

	rp := k.getPolicy("hourly-policy")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected sync to complete")
	}

	// Third processOnce — should NOT process because the 1h interval hasn't elapsed.
	initialSyncTime := rp.Status.LastSyncTime.Time
	rc.processOnce(ctx)

	rp = k.getPolicy("hourly-policy")
	// LastSyncTime should not have changed.
	if !rp.Status.LastSyncTime.Time.Equal(initialSyncTime) {
		t.Error("expected no re-sync before schedule interval elapsed")
	}
}

func TestReplicationController_JobFailureHandling(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("fail-policy", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Simulate Job failure.
	simulateReplJobFailed(t, dc, "fail-policy", "pvc-11111111-1111-1111-1111-111111111111")

	// Wait for at least one failure recorded.
	waitForConsecutiveFailures(t, k, "fail-policy", 1, 5*time.Second)

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
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("pause-policy", "prod-pool", "1ms")
	policy.Spec.MaxRetries = 2
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Simulate first Job failure (failure count goes to 1).
	simulateReplJobFailed(t, dc, "pause-policy", "pvc-11111111-1111-1111-1111-111111111111")

	// Wait for failure to be recorded.
	waitForConsecutiveFailures(t, k, "pause-policy", 1, 3*time.Second)

	// Simulate second Job failure (failure count goes to 2 = maxRetries → Paused).
	simulateReplJobFailed(t, dc, "pause-policy", "pvc-11111111-1111-1111-1111-111111111111")

	// Wait for the policy to be paused.
	waitForPolicyPhase(t, k, "pause-policy", "Paused", 5*time.Second)

	rp := k.getPolicy("pause-policy")
	if rp.Status.ConsecutiveFailures < 2 {
		t.Errorf("expected at least 2 consecutive failures, got %d", rp.Status.ConsecutiveFailures)
	}
}

func TestReplicationController_SubVolumeSelector(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv1.Labels["replication"] = "enabled"
	k.addSubVolume(sv1)

	sv2 := newTestSubVolume("pvc-22222222-2222-2222-2222-222222222222", "prod-pool", "share-1", 5)
	k.addSubVolume(sv2)

	policy := newTestReplicationPolicyWithSelector("select-policy", "prod-pool", "1ms",
		map[string]string{"replication": "enabled"})
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// Should have created a Job only for sv1 (the one with the matching label).
	job1Name := replJobName("select-policy", "pvc-11111111-1111-1111-1111-111111111111")
	job1 := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: job1Name}, job1); err != nil {
		t.Fatalf("expected Job for sv1 to be created: %v", err)
	}

	// Should NOT have created a Job for sv2.
	job2Name := replJobName("select-policy", "pvc-22222222-2222-2222-2222-222222222222")
	job2 := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: job2Name}, job2); err == nil {
		t.Error("expected no Job for sv2 (not matching selector)")
	}
}

func TestReplicationController_SkipsPausedPolicy(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("paused-policy", "prod-pool", "1ms")
	policy.Status.Phase = "Paused"
	policy.Annotations = map[string]string{"storage.ibmcloud.io/paused": "true"}
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// No Jobs should have been created.
	jobName := replJobName("paused-policy", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err == nil {
		t.Error("expected no Job for paused policy")
	}
}

func TestReplicationController_SkipsFailedPolicy(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("failed-policy", "prod-pool", "1ms")
	policy.Status.Phase = "Failed"
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	jobName := replJobName("failed-policy", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err == nil {
		t.Error("expected no Job for failed policy")
	}
}

func TestReplicationController_InvalidSchedule(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("bad-schedule", "prod-pool", "not-a-duration")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// No Jobs should have been created.
	jobName := replJobName("bad-schedule", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err == nil {
		t.Error("expected no Job for invalid schedule")
	}
}

func TestReplicationController_GracefulShutdown(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	rc := newReplController(k, dc, nfs)

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
	metrics.ReplicationSyncsTotal.Reset()
	metrics.ReplicationSyncDuration.Reset()
	metrics.ReplicationLagSeconds.Reset()
	metrics.ReplicationConsecutiveFailures.Reset()
	metrics.ReplicationSubVolumeCount.Reset()

	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("metrics-policy", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Simulate Job success.
	simulateReplJobSucceeded(t, dc, "metrics-policy", "pvc-11111111-1111-1111-1111-111111111111")

	waitForPolicySync(t, k, "metrics-policy", 5*time.Second)

	// Check success metric.
	val := readReplicationCounterValue("metrics-policy", "prod-pool", "success")
	if val < 1.0 {
		t.Errorf("expected ReplicationSyncsTotal(success) >= 1, got %f", val)
	}
}

func TestReplicationController_MetricsRecordedOnFailure(t *testing.T) {
	metrics.ReplicationSyncsTotal.Reset()

	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("fail-metrics", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	// Simulate Job failure.
	simulateReplJobFailed(t, dc, "fail-metrics", "pvc-11111111-1111-1111-1111-111111111111")

	waitForConsecutiveFailures(t, k, "fail-metrics", 1, 5*time.Second)

	val := readReplicationCounterValue("fail-metrics", "prod-pool", "failure")
	if val < 1.0 {
		t.Errorf("expected ReplicationSyncsTotal(failure) >= 1, got %f", val)
	}
}

func TestReplicationController_SuccessResetsConsecutiveFailures(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("recover-policy", "prod-pool", "1ms")
	policy.Status.ConsecutiveFailures = 2
	policy.Status.LastError = "previous error"
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	simulateReplJobSucceeded(t, dc, "recover-policy", "pvc-11111111-1111-1111-1111-111111111111")

	waitForPolicySync(t, k, "recover-policy", 5*time.Second)

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
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	// No SubVolumes in the pool.
	policy := newTestReplicationPolicy("empty-policy", "empty-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

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
	k.listPolicyErr = fmt.Errorf("API server unavailable")
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// Should not panic and should not create any Jobs.
	// (No assertions needed — just verify no panic.)
}

func TestReplicationController_ConcurrentSafety(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("concurrent-policy", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

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
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	// Empty pool — completes immediately with no Jobs needed.
	policy := newTestReplicationPolicy("time-policy", "empty-pool", "1h")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

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

	if !rp.Status.LastSyncTime.Time.Equal(fixedTime) {
		t.Errorf("expected LastSyncTime to be fixed time %v, got %v", fixedTime, rp.Status.LastSyncTime.Time)
	}

	if callCount.Load() == 0 {
		t.Error("expected nowFunc to be called at least once")
	}
}

func TestReplicationController_NilSelector(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv2 := newTestSubVolume("pvc-22222222-2222-2222-2222-222222222222", "prod-pool", "share-1", 5)
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)

	policy := newTestReplicationPolicy("all-policy", "prod-pool", "1ms")
	policy.Spec.SubVolumeSelector = nil
	policy.Spec.MaxParallelSyncs = 2 // Allow both Jobs in one cycle.
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// Both SubVolumes should have Jobs.
	job1Name := replJobName("all-policy", "pvc-11111111-1111-1111-1111-111111111111")
	job2Name := replJobName("all-policy", "pvc-22222222-2222-2222-2222-222222222222")
	job1 := &batchv1.Job{}
	job2 := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: job1Name}, job1); err != nil {
		t.Errorf("expected Job for sv1: %v", err)
	}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: job2Name}, job2); err != nil {
		t.Errorf("expected Job for sv2: %v", err)
	}
}

func TestReplicationController_EmptyLabelSelector(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("empty-selector", "prod-pool", "1ms")
	policy.Spec.SubVolumeSelector = &metav1.LabelSelector{
		MatchLabels: map[string]string{},
	}
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// Empty selector = match all.
	jobName := replJobName("empty-selector", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created (empty selector = all): %v", err)
	}
}

func TestReplicationController_SelectorMatchesNone(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicyWithSelector("no-match", "prod-pool", "1ms",
		map[string]string{"nonexistent": "label"})
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	rp := k.getPolicy("no-match")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected sync to complete (even with no matched SubVolumes)")
	}
	if len(rp.Status.SubVolumeStatuses) != 0 {
		t.Errorf("expected 0 SubVolume statuses, got %d", len(rp.Status.SubVolumeStatuses))
	}

	// No Jobs should have been created.
	jobName := replJobName("no-match", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err == nil {
		t.Error("expected no Job when selector matches nothing")
	}
}

func TestReplicationController_MultiplePolicies(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "pool-a", "share-1", 10)
	sv2 := newTestSubVolume("pvc-22222222-2222-2222-2222-222222222222", "pool-b", "share-2", 5)
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)

	policy1 := newTestReplicationPolicy("policy-a", "pool-a", "1ms")
	policy2 := newTestReplicationPolicy("policy-b", "pool-b", "1ms")
	k.addPolicy(policy1)
	k.addPolicy(policy2)

	rc := newReplController(k, dc, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	simulateReplJobSucceeded(t, dc, "policy-a", "pvc-11111111-1111-1111-1111-111111111111")
	simulateReplJobSucceeded(t, dc, "policy-b", "pvc-22222222-2222-2222-2222-222222222222")

	waitForPolicySync(t, k, "policy-a", 5*time.Second)
	waitForPolicySync(t, k, "policy-b", 5*time.Second)

	rpA := k.getPolicy("policy-a")
	rpB := k.getPolicy("policy-b")

	if rpA.Status.LastSyncTime == nil {
		t.Error("policy-a should have synced")
	}
	if rpB.Status.LastSyncTime == nil {
		t.Error("policy-b should have synced")
	}
}

func TestReplicationController_DestinationExportPath(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv1.Spec.ShareMountTargetIP = "10.240.0.1"
	sv1.Spec.ShareExportPath = "/share_src_123"
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("export-path-policy", "prod-pool", "1ms")
	policy.Spec.DestinationExportPath = "/share_dst_456"
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// Verify dest PV has the correct export path.
	dstPVName := replDestPVName("export-path-policy", "pvc-11111111-1111-1111-1111-111111111111")
	verifyReplPVCreated(t, dc, dstPVName, "10.245.3.8", "/share_dst_456")

	// Verify source PV has the correct export path.
	srcPVName := replSourcePVName("export-path-policy", "pvc-11111111-1111-1111-1111-111111111111")
	verifyReplPVCreated(t, dc, srcPVName, "10.240.0.1", "/share_src_123")
}

func TestReplicationController_CleanupAfterSuccess(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("cleanup-policy", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)
	ctx := context.Background()

	// First processOnce: creates Job.
	rc.processOnce(ctx)

	// Verify Job was created.
	jobName := replJobName("cleanup-policy", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}

	// Simulate Job success.
	job.Status.Succeeded = 1
	if err := dc.Status().Update(ctx, job); err != nil {
		t.Fatalf("failed to update Job status: %v", err)
	}

	// Clear lastSyncTimes so processOnce processes the policy again.
	rc.mu.Lock()
	delete(rc.lastSyncTimes, "cleanup-policy")
	rc.mu.Unlock()

	// Second processOnce: picks up succeeded Job, cleans up, records sync.
	rc.processOnce(ctx)

	rp := k.getPolicy("cleanup-policy")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected sync to complete")
	}

	// After successful sync, temp resources should be cleaned up.
	srcPVName := replSourcePVName("cleanup-policy", "pvc-11111111-1111-1111-1111-111111111111")
	dstPVName := replDestPVName("cleanup-policy", "pvc-11111111-1111-1111-1111-111111111111")

	// Verify Job was deleted.
	deletedJob := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, deletedJob); err == nil {
		t.Error("expected Job to be cleaned up after success")
	}

	// Verify PVs were deleted.
	pv := &corev1.PersistentVolume{}
	if err := dc.Get(ctx, types.NamespacedName{Name: srcPVName}, pv); err == nil {
		t.Error("expected source PV to be cleaned up after success")
	}
	if err := dc.Get(ctx, types.NamespacedName{Name: dstPVName}, pv); err == nil {
		t.Error("expected dest PV to be cleaned up after success")
	}
}

func TestReplicationController_IncrementalSyncDefault(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("inc-default", "prod-pool", "1ms")
	policy.Spec.IncrementalSync = nil // nil → treated as true
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// Verify the Job was created with rsync command.
	jobName := replJobName("inc-default", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}
	script := job.Spec.Template.Spec.Containers[0].Command[2]
	if len(script) == 0 {
		t.Fatal("expected non-empty script")
	}
	// Verify rsync is used (incremental sync = true).
	if !containsSubstring(script, "rsync") {
		t.Error("expected rsync in Job script for incremental sync (default)")
	}
}

func TestReplicationController_IncrementalSyncFalse(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("inc-false", "prod-pool", "1ms")
	f := false
	policy.Spec.IncrementalSync = &f
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	jobName := replJobName("inc-false", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}
	script := job.Spec.Template.Spec.Containers[0].Command[2]
	// Verify cp -a is used (not rsync).
	if !containsSubstring(script, "cp -a") {
		t.Error("expected cp -a in Job script for non-incremental sync")
	}
	if containsSubstring(script, "rsync") {
		t.Error("expected no rsync in Job script for non-incremental sync")
	}
}

func TestReplicationController_BandwidthLimit(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("bw-limit", "prod-pool", "1ms")
	policy.Spec.BandwidthLimitMbps = 100 // 100 Mbps = 12500 KBps
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	jobName := replJobName("bw-limit", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}
	script := job.Spec.Template.Spec.Containers[0].Command[2]
	if !containsSubstring(script, "--bwlimit=12500") {
		t.Errorf("expected --bwlimit=12500 in Job script, got: %s", script)
	}
}

func TestReplicationController_RsyncOptions(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("rsync-opts", "prod-pool", "1ms")
	policy.Spec.RsyncOptions = []string{"--compress", "--checksum"}
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	jobName := replJobName("rsync-opts", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}
	script := job.Spec.Template.Spec.Containers[0].Command[2]
	if !containsSubstring(script, "--compress") || !containsSubstring(script, "--checksum") {
		t.Errorf("expected --compress and --checksum in Job script, got: %s", script)
	}
}

func TestReplicationController_MetadataSidecarInJob(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("meta-write", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	jobName := replJobName("meta-write", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}
	script := job.Spec.Template.Spec.Containers[0].Command[2]
	if !containsSubstring(script, ".subvolume-metadata.json") {
		t.Error("expected metadata sidecar write in Job script")
	}
	if !containsSubstring(script, "meta-write") {
		t.Error("expected policy name in metadata JSON")
	}
	if !containsSubstring(script, "pvc-11111111-1111-1111-1111-111111111111") {
		t.Error("expected SubVolume name in metadata JSON")
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

func TestReplicationController_JobRunsAsRoot(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReplicationPolicy("root-check", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	jobName := replJobName("root-check", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}

	podSec := job.Spec.Template.Spec.SecurityContext
	if podSec == nil || podSec.RunAsUser == nil || *podSec.RunAsUser != 0 {
		t.Error("expected Job to run as root (uid 0)")
	}
}

func TestReplicationController_NamingHelpers(t *testing.T) {
	// Verify naming helper output is deterministic and within limits.
	job := replJobName("my-policy", "pvc-11111111-1111-1111-1111-111111111111")
	if len(job) > 63 {
		t.Errorf("job name too long: %d chars (%s)", len(job), job)
	}

	srcPV := replSourcePVName("my-policy", "pvc-11111111-1111-1111-1111-111111111111")
	if len(srcPV) > 63 {
		t.Errorf("source PV name too long: %d chars (%s)", len(srcPV), srcPV)
	}

	dstPV := replDestPVName("my-policy", "pvc-11111111-1111-1111-1111-111111111111")
	if len(dstPV) > 63 {
		t.Errorf("dest PV name too long: %d chars (%s)", len(dstPV), dstPV)
	}

	// Verify long names are truncated.
	longPolicy := "this-is-a-really-long-policy-name-that-exceeds-normal-limits"
	longSV := "pvc-11111111-1111-1111-1111-111111111111-extra-chars"
	longJob := replJobName(longPolicy, longSV)
	if len(longJob) > 63 {
		t.Errorf("long job name should be truncated: %d chars (%s)", len(longJob), longJob)
	}
}

// containsSubstring is a test helper that checks if s contains substr.
func containsSubstring(s, substr string) bool {
	return len(s) >= len(substr) && searchSubstring(s, substr)
}

func searchSubstring(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Metadata round-trip tests (these still use fakeNFSOperations since they
// test the metadata serialization code, not the replication controller).
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// Driver-to-driver (receiver mode) tests
// ---------------------------------------------------------------------------

func newTestReceiverModePolicy(name, sourcePool, schedule string) *v1alpha1.ReplicationPolicy {
	return &v1alpha1.ReplicationPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.ReplicationPolicySpec{
			SourcePoolName:           sourcePool,
			DestinationEndpoint:      "https://repl-receiver.apps.cluster.example.com",
			DestinationAuthSecretRef: "replication-auth",
			DestinationBasePath:      "/pvcs",
			Schedule:                 schedule,
			MaxRetries:               3,
		},
		Status: v1alpha1.ReplicationPolicyStatus{
			Phase: "Active",
		},
	}
}

func TestReplicationController_ReceiverMode_CreatesJob(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv1.Spec.ShareMountTargetIP = "10.240.0.1"
	sv1.Spec.ShareExportPath = "/share_abc123"
	k.addSubVolume(sv1)

	policy := newTestReceiverModePolicy("receiver-test", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)
	// Use the driver image for receiver mode.
	rc.SetReplicationImage("de.icr.io/ibm-vpc-file-pool-csi/driver:latest")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	waitForReplJob(t, dc, "receiver-test", "pvc-11111111-1111-1111-1111-111111111111", 3*time.Second)

	// Verify source PV was created (with correct NFS details).
	srcPVName := replSourcePVName("receiver-test", "pvc-11111111-1111-1111-1111-111111111111")
	verifyReplPVCreated(t, dc, srcPVName, "10.240.0.1", "/share_abc123")

	// Verify source PVC was created.
	srcPVCName := replSourcePVCName("receiver-test", "pvc-11111111-1111-1111-1111-111111111111")
	verifyReplPVCCreated(t, dc, srcPVCName)

	// Verify NO dest PV/PVC were created (receiver mode doesn't need them).
	dstPVName := replDestPVName("receiver-test", "pvc-11111111-1111-1111-1111-111111111111")
	dstPV := &corev1.PersistentVolume{}
	if err := dc.Get(ctx, types.NamespacedName{Name: dstPVName}, dstPV); err == nil {
		t.Error("expected NO dest PV in receiver mode")
	}
	dstPVCName := replDestPVCName("receiver-test", "pvc-11111111-1111-1111-1111-111111111111")
	dstPVC := &corev1.PersistentVolumeClaim{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: dstPVCName}, dstPVC); err == nil {
		t.Error("expected NO dest PVC in receiver mode")
	}
}

func TestReplicationController_ReceiverMode_JobHasSyncClientArgs(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv1.Spec.SubPath = "/pvcs/pvc-11111111-1111-1111-1111-111111111111"
	k.addSubVolume(sv1)

	policy := newTestReceiverModePolicy("args-test", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)
	rc.SetDriverImage("de.icr.io/ibm-vpc-file-pool-csi/driver:latest")

	ctx := context.Background()
	rc.processOnce(ctx)

	jobName := replJobName("args-test", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}

	containers := job.Spec.Template.Spec.Containers
	if len(containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(containers))
	}

	container := containers[0]

	// Verify container name.
	if container.Name != "sync-client" {
		t.Errorf("container name = %q, want %q", container.Name, "sync-client")
	}

	// Verify it uses the driver image (not CentOS).
	if container.Image != "de.icr.io/ibm-vpc-file-pool-csi/driver:latest" {
		t.Errorf("image = %q, want driver image", container.Image)
	}

	// Verify Args contains sync-client mode and receiver endpoint.
	foundMode := false
	foundEndpoint := false
	foundAuthFile := false
	for _, arg := range container.Args {
		if arg == "--mode=sync-client" {
			foundMode = true
		}
		if containsSubstring(arg, "--receiver-endpoint=https://repl-receiver.apps.cluster.example.com") {
			foundEndpoint = true
		}
		if containsSubstring(arg, "--auth-token-file=/etc/replication/token") {
			foundAuthFile = true
		}
	}
	if !foundMode {
		t.Error("expected --mode=sync-client in container args")
	}
	if !foundEndpoint {
		t.Error("expected --receiver-endpoint in container args")
	}
	if !foundAuthFile {
		t.Error("expected --auth-token-file in container args")
	}

	// Verify volume mounts: should have source + auth-token (no dest).
	if len(container.VolumeMounts) != 2 {
		t.Fatalf("expected 2 volume mounts, got %d", len(container.VolumeMounts))
	}
	vmNames := map[string]bool{}
	for _, vm := range container.VolumeMounts {
		vmNames[vm.Name] = true
	}
	if !vmNames["source"] {
		t.Error("expected 'source' volume mount")
	}
	if !vmNames["auth-token"] {
		t.Error("expected 'auth-token' volume mount")
	}
	if vmNames["dest"] {
		t.Error("unexpected 'dest' volume mount in receiver mode")
	}

	// Verify volumes: should have source PVC + auth-token Secret (no dest PVC).
	volumes := job.Spec.Template.Spec.Volumes
	if len(volumes) != 2 {
		t.Fatalf("expected 2 volumes, got %d", len(volumes))
	}
	foundSourceVol := false
	foundAuthVol := false
	for _, vol := range volumes {
		if vol.Name == "source" && vol.PersistentVolumeClaim != nil {
			foundSourceVol = true
		}
		if vol.Name == "auth-token" && vol.Secret != nil {
			foundAuthVol = true
			if vol.Secret.SecretName != "replication-auth" {
				t.Errorf("auth secret name = %q, want %q", vol.Secret.SecretName, "replication-auth")
			}
		}
	}
	if !foundSourceVol {
		t.Error("expected source PVC volume")
	}
	if !foundAuthVol {
		t.Error("expected auth-token Secret volume")
	}
}

func TestReplicationController_ReceiverMode_SyncCycleSuccess(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReceiverModePolicy("receiver-cycle", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	simulateReplJobSucceeded(t, dc, "receiver-cycle", "pvc-11111111-1111-1111-1111-111111111111")

	waitForPolicySync(t, k, "receiver-cycle", 5*time.Second)

	rp := k.getPolicy("receiver-cycle")
	if rp.Status.Phase != "Active" {
		t.Errorf("expected phase Active, got %s", rp.Status.Phase)
	}
	if rp.Status.LastSyncTime == nil {
		t.Error("expected LastSyncTime to be set")
	}
	if rp.Status.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", rp.Status.ConsecutiveFailures)
	}
}

func TestReplicationController_ReceiverMode_CleanupSkipsDestResources(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReceiverModePolicy("cleanup-receiver", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// Simulate Job success.
	jobName := replJobName("cleanup-receiver", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}
	job.Status.Succeeded = 1
	if err := dc.Status().Update(ctx, job); err != nil {
		t.Fatalf("failed to update Job status: %v", err)
	}

	// Force re-processing.
	rc.mu.Lock()
	delete(rc.lastSyncTimes, "cleanup-receiver")
	rc.mu.Unlock()
	rc.processOnce(ctx)

	// Verify source PV cleaned up.
	srcPVName := replSourcePVName("cleanup-receiver", "pvc-11111111-1111-1111-1111-111111111111")
	pv := &corev1.PersistentVolume{}
	if err := dc.Get(ctx, types.NamespacedName{Name: srcPVName}, pv); err == nil {
		t.Error("expected source PV to be cleaned up")
	}

	// Verify Job cleaned up.
	cleanedJob := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, cleanedJob); err == nil {
		t.Error("expected Job to be cleaned up")
	}
}

func TestReplicationController_MixedModes(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "pool-a", "share-1", 10)
	sv2 := newTestSubVolume("pvc-22222222-2222-2222-2222-222222222222", "pool-b", "share-2", 5)
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)

	// Direct NFS policy.
	directPolicy := newTestReplicationPolicy("direct-nfs", "pool-a", "1ms")
	k.addPolicy(directPolicy)

	// Receiver mode policy.
	receiverPolicy := newTestReceiverModePolicy("receiver-dr", "pool-b", "1ms")
	k.addPolicy(receiverPolicy)

	rc := newReplController(k, dc, nfs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go rc.Run(ctx)

	simulateReplJobSucceeded(t, dc, "direct-nfs", "pvc-11111111-1111-1111-1111-111111111111")
	simulateReplJobSucceeded(t, dc, "receiver-dr", "pvc-22222222-2222-2222-2222-222222222222")

	waitForPolicySync(t, k, "direct-nfs", 5*time.Second)
	waitForPolicySync(t, k, "receiver-dr", 5*time.Second)

	rpDirect := k.getPolicy("direct-nfs")
	rpReceiver := k.getPolicy("receiver-dr")

	if rpDirect.Status.LastSyncTime == nil {
		t.Error("direct NFS policy should have synced")
	}
	if rpReceiver.Status.LastSyncTime == nil {
		t.Error("receiver mode policy should have synced")
	}
}

func TestIsReceiverMode(t *testing.T) {
	direct := newTestReplicationPolicy("p1", "pool", "5m")
	if isReceiverMode(direct) {
		t.Error("expected direct NFS policy to not be receiver mode")
	}

	receiver := newTestReceiverModePolicy("p2", "pool", "5m")
	if !isReceiverMode(receiver) {
		t.Error("expected receiver policy to be receiver mode")
	}
}

func TestReplicationController_MetadataRoundTrip(t *testing.T) {
	nfs := newFakeNFSOperations()
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

// ---------------------------------------------------------------------------
// Fix 2: Scheduling starvation tests
// ---------------------------------------------------------------------------

func TestReplicationController_SchedulingFairness(t *testing.T) {
	// With maxParallelSyncs=1 and 3 SubVolumes, each SubVolume should
	// eventually get synced, not just the first one every cycle.
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv2 := newTestSubVolume("pvc-22222222-2222-2222-2222-222222222222", "prod-pool", "share-1", 5)
	sv3 := newTestSubVolume("pvc-33333333-3333-3333-3333-333333333333", "prod-pool", "share-1", 8)
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)
	k.addSubVolume(sv3)

	policy := newTestReplicationPolicy("fairness", "prod-pool", "1ms")
	policy.Spec.MaxParallelSyncs = 1 // Only 1 Job at a time.
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()

	// Track which SubVolumes had Jobs created.
	synced := map[string]bool{}

	// Run multiple cycles. Each cycle: process → simulate success → next.
	for cycle := 0; cycle < 6; cycle++ {
		// Force isDue.
		rc.mu.Lock()
		delete(rc.lastSyncTimes, "fairness")
		rc.mu.Unlock()

		rc.processOnce(ctx)

		// Find which Job was created (or is running) this cycle.
		for _, svName := range []string{
			"pvc-11111111-1111-1111-1111-111111111111",
			"pvc-22222222-2222-2222-2222-222222222222",
			"pvc-33333333-3333-3333-3333-333333333333",
		} {
			jobName := replJobName("fairness", svName)
			job := &batchv1.Job{}
			if err := dc.Get(ctx, types.NamespacedName{
				Namespace: replJobNamespace,
				Name:      jobName,
			}, job); err == nil {
				synced[svName] = true
				// Simulate success.
				job.Status.Succeeded = 1
				if err := dc.Status().Update(ctx, job); err != nil {
					t.Fatalf("failed to update Job status: %v", err)
				}
			}
		}

		// Let the controller pick up the succeeded Job.
		rc.mu.Lock()
		delete(rc.lastSyncTimes, "fairness")
		rc.mu.Unlock()
		rc.processOnce(ctx)
	}

	// All 3 SubVolumes should have been synced at least once.
	for _, svName := range []string{
		"pvc-11111111-1111-1111-1111-111111111111",
		"pvc-22222222-2222-2222-2222-222222222222",
		"pvc-33333333-3333-3333-3333-333333333333",
	} {
		if !synced[svName] {
			t.Errorf("SubVolume %s was never synced (scheduling starvation)", svName)
		}
	}
}

func TestReplicationController_SortByLastSyncTime(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()
	rc := newReplController(k, dc, nfs)

	now := time.Date(2026, 2, 25, 12, 0, 0, 0, time.UTC)
	rc.SetNowFunc(func() time.Time { return now })

	// Record sync times: sv2 synced recently, sv1 synced a while ago, sv3 never.
	rc.mu.Lock()
	rc.lastSubVolumeSyncTimes["test-policy/pvc-11111111-1111-1111-1111-111111111111"] = now.Add(-10 * time.Minute)
	rc.lastSubVolumeSyncTimes["test-policy/pvc-22222222-2222-2222-2222-222222222222"] = now.Add(-1 * time.Minute)
	rc.mu.Unlock()

	svs := []v1alpha1.SubVolume{
		{ObjectMeta: metav1.ObjectMeta{Name: "pvc-11111111-1111-1111-1111-111111111111"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pvc-22222222-2222-2222-2222-222222222222"}},
		{ObjectMeta: metav1.ObjectMeta{Name: "pvc-33333333-3333-3333-3333-333333333333"}},
	}

	rc.sortByLastSyncTime("test-policy", svs)

	// Expected order: sv3 (never synced, zero time) → sv1 (10m ago) → sv2 (1m ago)
	if svs[0].Name != "pvc-33333333-3333-3333-3333-333333333333" {
		t.Errorf("expected sv3 first (never synced), got %s", svs[0].Name)
	}
	if svs[1].Name != "pvc-11111111-1111-1111-1111-111111111111" {
		t.Errorf("expected sv1 second (synced 10m ago), got %s", svs[1].Name)
	}
	if svs[2].Name != "pvc-22222222-2222-2222-2222-222222222222" {
		t.Errorf("expected sv2 last (synced 1m ago), got %s", svs[2].Name)
	}
}

// ---------------------------------------------------------------------------
// Fix 3: Policy-level status incremental update tests
// ---------------------------------------------------------------------------

func TestReplicationController_PolicyStatusUpdatedIncrementally(t *testing.T) {
	// With maxParallelSyncs=1 and 2 SubVolumes, the first cycle will complete
	// 1 Job and defer 1. Policy LastSyncTime should still be set.
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv2 := newTestSubVolume("pvc-22222222-2222-2222-2222-222222222222", "prod-pool", "share-1", 5)
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)

	policy := newTestReplicationPolicy("inc-status", "prod-pool", "1ms")
	policy.Spec.MaxParallelSyncs = 1
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()

	// Cycle 1: creates a Job for one SubVolume, defers the other.
	rc.processOnce(ctx)

	// Find which SubVolume got the Job and simulate success.
	for _, svName := range []string{
		"pvc-11111111-1111-1111-1111-111111111111",
		"pvc-22222222-2222-2222-2222-222222222222",
	} {
		jobName := replJobName("inc-status", svName)
		job := &batchv1.Job{}
		if err := dc.Get(ctx, types.NamespacedName{
			Namespace: replJobNamespace,
			Name:      jobName,
		}, job); err == nil {
			job.Status.Succeeded = 1
			if err := dc.Status().Update(ctx, job); err != nil {
				t.Fatalf("failed to update Job status: %v", err)
			}
		}
	}

	// Cycle 2: picks up the succeeded Job + defers the remaining one.
	rc.mu.Lock()
	delete(rc.lastSyncTimes, "inc-status")
	rc.mu.Unlock()
	rc.processOnce(ctx)

	// Even though not all SubVolumes are complete, LastSyncTime should be set.
	rp := k.getPolicy("inc-status")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected LastSyncTime to be set incrementally when some SVs succeeded")
	}
	if rp.Status.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", rp.Status.ConsecutiveFailures)
	}
}

// ---------------------------------------------------------------------------
// Fix 4: BytesSynced wiring tests
// ---------------------------------------------------------------------------

func TestReplicationController_BytesSyncedFromTerminationMessage(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	k.addSubVolume(sv1)

	policy := newTestReceiverModePolicy("bytes-test", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)

	ctx := context.Background()
	rc.processOnce(ctx)

	// Mark Job as succeeded.
	jobName := replJobName("bytes-test", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}
	job.Status.Succeeded = 1
	if err := dc.Status().Update(ctx, job); err != nil {
		t.Fatalf("failed to update Job status: %v", err)
	}

	// Create a pod matching the Job's labels with a termination message.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      jobName + "-pod",
			Namespace: replJobNamespace,
			Labels:    job.Spec.Template.Labels,
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "sync-client",
					State: corev1.ContainerState{
						Terminated: &corev1.ContainerStateTerminated{
							ExitCode: 0,
							Message:  `{"bytesWritten":2097152}`,
						},
					},
				},
			},
		},
	}
	if err := dc.Create(ctx, pod); err != nil {
		t.Fatalf("failed to create pod: %v", err)
	}

	// Process again to pick up succeeded Job.
	rc.mu.Lock()
	delete(rc.lastSyncTimes, "bytes-test")
	rc.mu.Unlock()
	rc.processOnce(ctx)

	rp := k.getPolicy("bytes-test")
	if rp.Status.LastSyncTime == nil {
		t.Fatal("expected LastSyncTime to be set")
	}

	// Check that BytesSynced was recorded in the SubVolume status.
	found := false
	for _, svs := range rp.Status.SubVolumeStatuses {
		if svs.SubVolumeName == "pvc-11111111-1111-1111-1111-111111111111" {
			found = true
			if svs.BytesSynced == nil {
				t.Error("expected BytesSynced to be set")
			} else if *svs.BytesSynced != 2097152 {
				t.Errorf("BytesSynced = %d, want 2097152", *svs.BytesSynced)
			}
		}
	}
	if !found {
		t.Error("SubVolume status not found in policy")
	}
}

// ---------------------------------------------------------------------------
// TLS / CA cert mounting tests
// ---------------------------------------------------------------------------

func TestReplicationController_ReceiverMode_CACertMounting(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv1.Spec.ShareMountTargetIP = "10.240.0.1"
	sv1.Spec.ShareExportPath = "/share_abc123"
	k.addSubVolume(sv1)

	policy := newTestReceiverModePolicy("ca-cert-test", "prod-pool", "1ms")
	policy.Spec.DestinationCACertSecretRef = "my-receiver-certs"
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)
	rc.SetDriverImage("de.icr.io/ibm-vpc-file-pool-csi/driver:latest")

	ctx := context.Background()
	rc.processOnce(ctx)

	jobName := replJobName("ca-cert-test", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]

	// Verify --ca-cert-file arg is present.
	foundCACertArg := false
	for _, arg := range container.Args {
		if arg == "--ca-cert-file=/etc/replication-ca/ca.crt" {
			foundCACertArg = true
			break
		}
	}
	if !foundCACertArg {
		t.Errorf("expected --ca-cert-file arg in container args: %v", container.Args)
	}

	// Verify ca-cert volume mount is present.
	foundCACertMount := false
	for _, vm := range container.VolumeMounts {
		if vm.Name == "ca-cert" && vm.MountPath == "/etc/replication-ca" && vm.ReadOnly {
			foundCACertMount = true
			break
		}
	}
	if !foundCACertMount {
		t.Errorf("expected ca-cert volume mount at /etc/replication-ca: %v", container.VolumeMounts)
	}

	// Verify ca-cert volume is present with correct secret name.
	foundCACertVol := false
	for _, vol := range job.Spec.Template.Spec.Volumes {
		if vol.Name == "ca-cert" && vol.Secret != nil && vol.Secret.SecretName == "my-receiver-certs" {
			foundCACertVol = true
			break
		}
	}
	if !foundCACertVol {
		t.Errorf("expected ca-cert volume with secret 'my-receiver-certs': %v", job.Spec.Template.Spec.Volumes)
	}

	// Verify total counts: 3 volumes (source, auth-token, ca-cert), 3 mounts.
	if len(container.VolumeMounts) != 3 {
		t.Errorf("expected 3 volume mounts, got %d", len(container.VolumeMounts))
	}
	if len(job.Spec.Template.Spec.Volumes) != 3 {
		t.Errorf("expected 3 volumes, got %d", len(job.Spec.Template.Spec.Volumes))
	}
}

func TestReplicationController_ReceiverMode_NoCACert(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	nfs := newFakeNFSOperations()

	sv1 := newTestSubVolume("pvc-11111111-1111-1111-1111-111111111111", "prod-pool", "share-1", 10)
	sv1.Spec.ShareMountTargetIP = "10.240.0.1"
	sv1.Spec.ShareExportPath = "/share_abc123"
	k.addSubVolume(sv1)

	// Policy WITHOUT DestinationCACertSecretRef.
	policy := newTestReceiverModePolicy("no-ca-test", "prod-pool", "1ms")
	k.addPolicy(policy)

	rc := newReplController(k, dc, nfs)
	rc.SetDriverImage("de.icr.io/ibm-vpc-file-pool-csi/driver:latest")

	ctx := context.Background()
	rc.processOnce(ctx)

	jobName := replJobName("no-ca-test", "pvc-11111111-1111-1111-1111-111111111111")
	job := &batchv1.Job{}
	if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, job); err != nil {
		t.Fatalf("expected Job to be created: %v", err)
	}

	container := job.Spec.Template.Spec.Containers[0]

	// Verify NO --ca-cert-file arg.
	for _, arg := range container.Args {
		if containsSubstring(arg, "--ca-cert-file") {
			t.Errorf("unexpected --ca-cert-file arg without DestinationCACertSecretRef: %v", container.Args)
			break
		}
	}

	// Verify NO ca-cert volume mount.
	for _, vm := range container.VolumeMounts {
		if vm.Name == "ca-cert" {
			t.Error("unexpected ca-cert volume mount without DestinationCACertSecretRef")
			break
		}
	}

	// Verify 2 volumes (source, auth-token only).
	if len(container.VolumeMounts) != 2 {
		t.Errorf("expected 2 volume mounts, got %d", len(container.VolumeMounts))
	}
	if len(job.Spec.Template.Spec.Volumes) != 2 {
		t.Errorf("expected 2 volumes, got %d", len(job.Spec.Template.Spec.Volumes))
	}
}
