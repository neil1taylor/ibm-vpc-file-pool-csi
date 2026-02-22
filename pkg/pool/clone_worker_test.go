package pool

import (
	"context"
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
// Test Helpers
// ---------------------------------------------------------------------------

func newCloneTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return s
}

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
			ShareExportPath:    "/share_abc123",
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

// newInProgressClone creates a SubVolume CR representing a clone in InProgress state.
func newInProgressClone(pvName, poolName, shareID, sourceVolume string, requestedGB int64) *v1alpha1.SubVolume {
	sv := newPendingClone(pvName, poolName, shareID, sourceVolume, requestedGB)
	sv.Status.CloneStatus = "InProgress"
	now := metav1.Now()
	sv.Status.CloneProgress.StartedAt = &now
	return sv
}

func newCloneWorkerForTest(k8s *fakeK8sClient, dc client.Client) *CloneWorker {
	w := NewCloneWorker(k8s, dc, DefaultCloneImage)
	w.SetInterval(50 * time.Millisecond)
	return w
}

// waitForCloneStatus polls the fake k8s client until the SubVolume has the expected
// clone status or the timeout expires.
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

// simulateJobSucceeded marks the clone Job as succeeded in the fake client.
func simulateJobSucceeded(t *testing.T, dc client.Client, svName, namespace string) {
	t.Helper()
	ctx := context.Background()
	jobName := cloneJobName(svName)

	// Poll until the Job exists.
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for clone Job %s/%s to be created", namespace, jobName)
		case <-ticker.C:
			job := &batchv1.Job{}
			if err := dc.Get(ctx, types.NamespacedName{Namespace: namespace, Name: jobName}, job); err == nil {
				// Job found — mark it as succeeded.
				job.Status.Succeeded = 1
				if err := dc.Status().Update(ctx, job); err != nil {
					t.Fatalf("failed to update Job status: %v", err)
				}
				return
			}
		}
	}
}

// simulateJobFailed marks the clone Job as failed in the fake client.
func simulateJobFailed(t *testing.T, dc client.Client, svName, namespace string) {
	t.Helper()
	ctx := context.Background()
	jobName := cloneJobName(svName)

	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-deadline:
			t.Fatalf("timed out waiting for clone Job %s/%s to be created", namespace, jobName)
		case <-ticker.C:
			job := &batchv1.Job{}
			if err := dc.Get(ctx, types.NamespacedName{Namespace: namespace, Name: jobName}, job); err == nil {
				// Job found — mark it as failed (backoffLimit+1 = 4).
				job.Status.Failed = 4
				if err := dc.Status().Update(ctx, job); err != nil {
					t.Fatalf("failed to update Job status: %v", err)
				}
				return
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Tests
// ---------------------------------------------------------------------------

func TestCloneWorker_CreatesJobForPendingClone(t *testing.T) {
	k := newFakeK8sClient()
	dc := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		Build()

	// Set up source SubVolume.
	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	source.Spec.PVCName = "source-pvc"
	source.Spec.PVCNamespace = "default"
	source.Spec.ShareExportPath = "/share_abc123"
	k.addSubVolume(source)

	// Set up pending clone SubVolume.
	clone := newPendingClone(
		"pvc-22222222-2222-2222-2222-222222222222",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		5,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, dc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	// Simulate Job success.
	simulateJobSucceeded(t, dc, "pvc-22222222-2222-2222-2222-222222222222", "default")

	// Wait for clone to complete.
	waitForCloneStatus(t, k, "pvc-22222222-2222-2222-2222-222222222222", "Complete", 5*time.Second)

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
	if sv.Status.CloneProgress.CompletedAt == nil {
		t.Error("expected CompletedAt to be set")
	}
}

func TestCloneWorker_CompletesWhenJobSucceeds(t *testing.T) {
	k := newFakeK8sClient()
	dc := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		Build()

	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	source.Spec.PVCName = "source-pvc"
	source.Spec.PVCNamespace = "default"
	k.addSubVolume(source)

	clone := newPendingClone(
		"pvc-33333333-3333-3333-3333-333333333333",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		5,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, dc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	simulateJobSucceeded(t, dc, "pvc-33333333-3333-3333-3333-333333333333", "default")
	waitForCloneStatus(t, k, "pvc-33333333-3333-3333-3333-333333333333", "Complete", 5*time.Second)

	sv, _ := k.GetSubVolume(context.Background(), "pvc-33333333-3333-3333-3333-333333333333")
	if sv.Status.Phase != "Bound" {
		t.Errorf("expected phase Bound, got %s", sv.Status.Phase)
	}
}

func TestCloneWorker_FailsWhenJobFails(t *testing.T) {
	k := newFakeK8sClient()
	dc := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		Build()

	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	source.Spec.PVCName = "source-pvc"
	source.Spec.PVCNamespace = "default"
	k.addSubVolume(source)

	clone := newPendingClone(
		"pvc-44444444-4444-4444-4444-444444444444",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		5,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, dc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	simulateJobFailed(t, dc, "pvc-44444444-4444-4444-4444-444444444444", "default")
	waitForCloneStatus(t, k, "pvc-44444444-4444-4444-4444-444444444444", "Failed", 5*time.Second)

	sv, _ := k.GetSubVolume(context.Background(), "pvc-44444444-4444-4444-4444-444444444444")
	if sv.Status.Phase != "Failed" {
		t.Errorf("expected phase Failed, got %s", sv.Status.Phase)
	}
	if sv.Status.CloneProgress == nil {
		t.Fatal("expected CloneProgress to be set")
	}
	if sv.Status.CloneProgress.Error == "" {
		t.Error("expected error message to be set")
	}
}

func TestCloneWorker_CleansUpTempResources(t *testing.T) {
	k := newFakeK8sClient()
	dc := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		Build()

	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	source.Spec.PVCName = "source-pvc"
	source.Spec.PVCNamespace = "default"
	k.addSubVolume(source)

	svName := "pvc-55555555-5555-5555-5555-555555555555"
	clone := newPendingClone(svName, "test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111", 5)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, dc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	simulateJobSucceeded(t, dc, svName, "default")
	waitForCloneStatus(t, k, svName, "Complete", 5*time.Second)

	// Give cleanup a moment.
	time.Sleep(100 * time.Millisecond)

	// Verify temp PV was deleted.
	pv := &corev1.PersistentVolume{}
	err := dc.Get(ctx, types.NamespacedName{Name: clonePVName(svName)}, pv)
	if err == nil {
		t.Error("expected temp PV to be deleted after completion")
	}

	// Verify temp PVC was deleted.
	pvc := &corev1.PersistentVolumeClaim{}
	err = dc.Get(ctx, types.NamespacedName{Namespace: "default", Name: clonePVCName(svName)}, pvc)
	if err == nil {
		t.Error("expected temp PVC to be deleted after completion")
	}

	// Verify Job was deleted.
	job := &batchv1.Job{}
	err = dc.Get(ctx, types.NamespacedName{Namespace: "default", Name: cloneJobName(svName)}, job)
	if err == nil {
		t.Error("expected clone Job to be deleted after completion")
	}
}

func TestCloneWorker_IdempotentJobCreation(t *testing.T) {
	k := newFakeK8sClient()
	dc := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		Build()

	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	source.Spec.PVCName = "source-pvc"
	source.Spec.PVCNamespace = "default"
	k.addSubVolume(source)

	svName := "pvc-66666666-6666-6666-6666-666666666666"
	clone := newPendingClone(svName, "test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111", 5)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, dc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	// Wait for Job to be created.
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for Job creation")
		case <-ticker.C:
			job := &batchv1.Job{}
			if err := dc.Get(ctx, types.NamespacedName{Namespace: "default", Name: cloneJobName(svName)}, job); err == nil {
				goto found
			}
		}
	}
found:

	// Run several more ticks — should not create a duplicate.
	time.Sleep(200 * time.Millisecond)

	// List all Jobs to verify only one exists.
	jobList := &batchv1.JobList{}
	if err := dc.List(ctx, jobList, client.InNamespace("default")); err != nil {
		t.Fatalf("failed to list Jobs: %v", err)
	}
	cloneJobs := 0
	for _, j := range jobList.Items {
		if j.Labels[cloneTempLabel] == "true" {
			cloneJobs++
		}
	}
	if cloneJobs != 1 {
		t.Errorf("expected exactly 1 clone Job, got %d", cloneJobs)
	}

	// Now mark it succeeded to clean up.
	cancel()
}

func TestCloneWorker_JobMountOptions(t *testing.T) {
	k := newFakeK8sClient()
	dc := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		Build()

	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	source.Spec.PVCName = "source-pvc"
	source.Spec.PVCNamespace = "default"
	source.Spec.ShareExportPath = "/share_abc123"
	k.addSubVolume(source)

	svName := "pvc-77777777-7777-7777-7777-777777777777"
	clone := newPendingClone(svName, "test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111", 5)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, dc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	// Wait for PV to be created.
	var pv corev1.PersistentVolume
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for PV creation")
		case <-ticker.C:
			if err := dc.Get(ctx, types.NamespacedName{Name: clonePVName(svName)}, &pv); err == nil {
				goto foundPV
			}
		}
	}
foundPV:

	// Verify NFS mount options include sec=sys.
	hasSec := false
	for _, opt := range pv.Spec.MountOptions {
		if opt == "sec=sys" {
			hasSec = true
			break
		}
	}
	if !hasSec {
		t.Errorf("expected PV mountOptions to include sec=sys, got %v", pv.Spec.MountOptions)
	}

	// Verify NFS server and path.
	if pv.Spec.NFS == nil {
		t.Fatal("expected NFS volume source")
	}
	if pv.Spec.NFS.Server != "10.240.0.1" {
		t.Errorf("expected NFS server 10.240.0.1, got %s", pv.Spec.NFS.Server)
	}
	if pv.Spec.NFS.Path != "/share_abc123" {
		t.Errorf("expected NFS path /share_abc123, got %s", pv.Spec.NFS.Path)
	}
}

func TestCloneWorker_JobSetsOwnership(t *testing.T) {
	k := newFakeK8sClient()
	dc := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		Build()

	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	source.Spec.PVCName = "source-pvc"
	source.Spec.PVCNamespace = "default"
	k.addSubVolume(source)

	svName := "pvc-88888888-8888-8888-8888-888888888888"
	clone := newPendingClone(svName, "test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111", 5)
	uid := int64(107)
	gid := int64(107)
	clone.Spec.UID = &uid
	clone.Spec.GID = &gid
	clone.Spec.Permissions = "0777"
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, dc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	// Wait for Job to be created.
	var job batchv1.Job
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-deadline:
			t.Fatal("timed out waiting for Job creation")
		case <-ticker.C:
			if err := dc.Get(ctx, types.NamespacedName{Namespace: "default", Name: cloneJobName(svName)}, &job); err == nil {
				goto foundJob
			}
		}
	}
foundJob:

	// Verify the script contains chown and chmod with correct values.
	script := job.Spec.Template.Spec.Containers[0].Command[2]
	if !containsStr(script, "chown -R 107:107") {
		t.Errorf("expected script to contain chown 107:107, got:\n%s", script)
	}
	if !containsStr(script, "chmod 0777") {
		t.Errorf("expected script to contain chmod 0777, got:\n%s", script)
	}
}

func TestCloneWorker_SourceNotFoundFails(t *testing.T) {
	k := newFakeK8sClient()
	dc := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		Build()

	// Set up a pending clone whose source does not exist.
	clone := newPendingClone(
		"pvc-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
		"test-pool", "share-1",
		"pvc-nonexist-0000-0000-0000-000000000000",
		5,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, dc)

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

func TestCloneWorker_SkipsCompleteClones(t *testing.T) {
	k := newFakeK8sClient()
	dc := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		Build()

	// Set up a completed clone SubVolume.
	now := metav1.Now()
	completedClone := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
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
			SubPath:            "/pvcs/pvc-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
			RequestedGB:        5,
			PVName:             "pvc-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
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

	w := newCloneWorkerForTest(k, dc)

	// Run processOnce directly and verify no Jobs are created.
	w.processOnce(context.Background())

	// Give a moment for any goroutines to run.
	time.Sleep(100 * time.Millisecond)

	// No Jobs should have been created.
	jobList := &batchv1.JobList{}
	if err := dc.List(context.Background(), jobList, client.InNamespace("default")); err != nil {
		t.Fatalf("failed to list jobs: %v", err)
	}
	if len(jobList.Items) != 0 {
		t.Errorf("expected 0 Jobs for complete clone, got %d", len(jobList.Items))
	}

	// Status should remain unchanged.
	sv, _ := k.GetSubVolume(context.Background(), "pvc-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	if sv.Status.CloneStatus != "Complete" {
		t.Errorf("expected cloneStatus to remain Complete, got %s", sv.Status.CloneStatus)
	}
}

func TestCloneWorker_GracefulShutdown(t *testing.T) {
	k := newFakeK8sClient()
	dc := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).Build()

	w := newCloneWorkerForTest(k, dc)

	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan struct{})
	go func() {
		w.Run(ctx)
		close(done)
	}()

	// Let it run for a bit.
	time.Sleep(100 * time.Millisecond)

	cancel()

	select {
	case <-done:
		// Good, worker stopped.
	case <-time.After(2 * time.Second):
		t.Fatal("clone worker did not stop after context cancellation")
	}
}

func TestCloneWorker_MetricsRecorded(t *testing.T) {
	k := newFakeK8sClient()
	dc := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		Build()

	metrics.ClonesTotal.Reset()
	metrics.CloneDuration.Reset()

	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	source.Spec.PVCName = "source-pvc"
	source.Spec.PVCNamespace = "default"
	k.addSubVolume(source)

	clone := newPendingClone(
		"pvc-cccccccc-cccc-cccc-cccc-cccccccccccc",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		5,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, dc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	simulateJobSucceeded(t, dc, "pvc-cccccccc-cccc-cccc-cccc-cccccccccccc", "default")
	waitForCloneStatus(t, k, "pvc-cccccccc-cccc-cccc-cccc-cccccccccccc", "Complete", 5*time.Second)

	val := readCloneCounterValue("test-pool", "async_success")
	if val < 1.0 {
		t.Errorf("expected ClonesTotal(async_success) >= 1, got %f", val)
	}
}

func TestCloneWorker_MetricsRecordedOnFailure(t *testing.T) {
	k := newFakeK8sClient()
	dc := fakeclient.NewClientBuilder().WithScheme(newCloneTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		Build()

	metrics.ClonesTotal.Reset()

	source := newTestSubVolume(
		"pvc-11111111-1111-1111-1111-111111111111",
		"test-pool", "share-1", 5,
	)
	source.Spec.PVCName = "source-pvc"
	source.Spec.PVCNamespace = "default"
	k.addSubVolume(source)

	clone := newPendingClone(
		"pvc-dddddddd-dddd-dddd-dddd-dddddddddddd",
		"test-pool", "share-1",
		"pvc-11111111-1111-1111-1111-111111111111",
		5,
	)
	k.addSubVolume(clone)

	w := newCloneWorkerForTest(k, dc)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go w.Run(ctx)

	simulateJobFailed(t, dc, "pvc-dddddddd-dddd-dddd-dddd-dddddddddddd", "default")
	waitForCloneStatus(t, k, "pvc-dddddddd-dddd-dddd-dddd-dddddddddddd", "Failed", 5*time.Second)

	val := readCloneCounterValue("test-pool", "async_error")
	if val < 1.0 {
		t.Errorf("expected ClonesTotal(async_error) >= 1, got %f", val)
	}
}

// containsStr is a simple string search helper for tests.
func containsStr(s, substr string) bool {
	return len(s) >= len(substr) && searchStr(s, substr)
}

func searchStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
