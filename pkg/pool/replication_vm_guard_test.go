package pool

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const (
	vmGuardSV1 = "pvc-11111111-1111-1111-1111-111111111111"
	vmGuardSV2 = "pvc-22222222-2222-2222-2222-222222222222"
)

// newVMPod returns a KubeVirt pod of the given kind ("virt-launcher" or
// "hotplug-disk") that mounts claimName in namespace ns.
func newVMPod(name, ns, kind, claimName string, phase corev1.PodPhase) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{"kubevirt.io": kind},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{{
				Name: "disk0",
				VolumeSource: corev1.VolumeSource{
					PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claimName},
				},
			}},
		},
		Status: corev1.PodStatus{Phase: phase},
	}
}

func replJobExists(t *testing.T, dc client.Client, policyName, svName string) bool {
	t.Helper()
	job := &batchv1.Job{}
	err := dc.Get(context.Background(), types.NamespacedName{
		Namespace: replJobNamespace,
		Name:      replJobName(policyName, svName),
	}, job)
	return err == nil
}

func svReplStatus(t *testing.T, k *replFakeK8sClient, policyName, svName string) (lastError string, synced bool) {
	t.Helper()
	for _, s := range k.getPolicy(policyName).Status.SubVolumeStatuses {
		if s.SubVolumeName == svName {
			return s.LastError, s.LastSyncTime != nil
		}
	}
	t.Fatalf("no status for SubVolume %q in policy %q", svName, policyName)
	return "", false
}

// TestReplicationController_SkipsVolumeOfRunningVM verifies that the
// controller never starts a file copy of a volume that a running or starting
// VM uses, because such a copy may not boot.
func TestReplicationController_SkipsVolumeOfRunningVM(t *testing.T) {
	tests := []struct {
		name     string
		pod      *corev1.Pod
		wantSkip bool
	}{
		{"running VM", newVMPod("virt-launcher-vm1-abcde", "default", "virt-launcher", "my-pvc", corev1.PodRunning), true},
		{"starting VM", newVMPod("virt-launcher-vm1-abcde", "default", "virt-launcher", "my-pvc", corev1.PodPending), true},
		{"hot-plugged disk", newVMPod("hp-volume-xyz", "default", "hotplug-disk", "my-pvc", corev1.PodRunning), true},
		{"stopped VM", newVMPod("virt-launcher-vm1-abcde", "default", "virt-launcher", "my-pvc", corev1.PodSucceeded), false},
		{"failed VM pod", newVMPod("virt-launcher-vm1-abcde", "default", "virt-launcher", "my-pvc", corev1.PodFailed), false},
		{"VM on another claim", newVMPod("virt-launcher-vm2-abcde", "default", "virt-launcher", "other-pvc", corev1.PodRunning), false},
		{"same claim name in another namespace", newVMPod("virt-launcher-vm1-abcde", "other-ns", "virt-launcher", "my-pvc", corev1.PodRunning), false},
		{"pod that is not a VM", func() *corev1.Pod {
			p := newVMPod("app-abcde", "default", "virt-launcher", "my-pvc", corev1.PodRunning)
			p.Labels = map[string]string{"app": "web"}
			return p
		}(), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k := newReplFakeK8sClient()
			dc := newReplFakeDirectClient()
			k.addSubVolume(newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10))
			k.addPolicy(newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms"))
			if err := dc.Create(context.Background(), tt.pod); err != nil {
				t.Fatalf("create pod: %v", err)
			}

			rc := newReplController(k, dc, newFakeNFSOperations())
			rc.processOnce(context.Background())

			if got := replJobExists(t, dc, "prod-to-dr", vmGuardSV1); got == tt.wantSkip {
				t.Fatalf("Job exists = %v, want %v", got, !tt.wantSkip)
			}
			lastErr, _ := svReplStatus(t, k, "prod-to-dr", vmGuardSV1)
			if tt.wantSkip && !strings.Contains(lastErr, "running VM") {
				t.Errorf("expected the status to name the running VM, got %q", lastErr)
			}
			if tt.wantSkip && !strings.Contains(lastErr, tt.pod.Name) {
				t.Errorf("expected the status to name pod %q, got %q", tt.pod.Name, lastErr)
			}
		})
	}
}

// TestReplicationController_SkippedVMVolumeDoesNotPausePolicy verifies that a
// skipped VM volume is reported on the policy, but does not count as a failed
// cycle. Otherwise the policy would pause and stop the other volumes too.
func TestReplicationController_SkippedVMVolumeDoesNotPausePolicy(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()

	sv1 := newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10)
	sv2 := newTestSubVolume(vmGuardSV2, "prod-pool", "share-1", 10)
	sv2.Spec.PVCName = "app-data"
	k.addSubVolume(sv1)
	k.addSubVolume(sv2)

	policy := newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms")
	policy.Spec.MaxRetries = 1 // one counted failure would pause the policy
	k.addPolicy(policy)

	ctx := context.Background()
	if err := dc.Create(ctx, newVMPod("virt-launcher-vm1-abcde", "default", "virt-launcher", "my-pvc", corev1.PodRunning)); err != nil {
		t.Fatalf("create pod: %v", err)
	}

	rc := newReplController(k, dc, newFakeNFSOperations())
	clock := time.Now()
	rc.SetNowFunc(func() time.Time { return clock })

	rc.processOnce(ctx)
	simulateReplJobSucceeded(t, dc, "prod-to-dr", vmGuardSV2)
	clock = clock.Add(time.Second)
	rc.processOnce(ctx)

	rp := k.getPolicy("prod-to-dr")
	if rp.Status.Phase != "Active" {
		t.Errorf("expected phase Active, got %q", rp.Status.Phase)
	}
	if rp.Annotations[pausedAnnotation] == "true" {
		t.Error("expected the policy not to be paused by a skipped VM volume")
	}
	if rp.Status.ConsecutiveFailures != 0 {
		t.Errorf("expected 0 consecutive failures, got %d", rp.Status.ConsecutiveFailures)
	}
	if !strings.Contains(rp.Status.LastError, "1 SubVolume skipped") {
		t.Errorf("expected the policy error to report the skipped volume, got %q", rp.Status.LastError)
	}
	if rp.Status.LastSyncTime == nil {
		t.Error("expected LastSyncTime, because the other volume synced")
	}
	if _, synced := svReplStatus(t, k, "prod-to-dr", vmGuardSV2); !synced {
		t.Error("expected the volume without a VM to sync")
	}
	if _, synced := svReplStatus(t, k, "prod-to-dr", vmGuardSV1); synced {
		t.Error("expected the running VM volume not to be marked synced")
	}
}

// TestReplicationController_VMStartedDuringCopy verifies that a copy is not
// reported as a good sync if a VM started using the volume while it ran.
func TestReplicationController_VMStartedDuringCopy(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	k.addSubVolume(newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10))
	k.addPolicy(newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms"))

	rc := newReplController(k, dc, newFakeNFSOperations())
	clock := time.Now()
	rc.SetNowFunc(func() time.Time { return clock })
	ctx := context.Background()

	rc.processOnce(ctx)
	if !replJobExists(t, dc, "prod-to-dr", vmGuardSV1) {
		t.Fatal("expected a Job while no VM uses the volume")
	}

	if err := dc.Create(ctx, newVMPod("virt-launcher-vm1-abcde", "default", "virt-launcher", "my-pvc", corev1.PodRunning)); err != nil {
		t.Fatalf("create pod: %v", err)
	}
	simulateReplJobSucceeded(t, dc, "prod-to-dr", vmGuardSV1)
	clock = clock.Add(time.Second)
	rc.processOnce(ctx)

	lastErr, synced := svReplStatus(t, k, "prod-to-dr", vmGuardSV1)
	if synced {
		t.Error("expected the copy not to count as a sync")
	}
	if !strings.Contains(lastErr, "started during the copy") {
		t.Errorf("expected the status to say a VM started during the copy, got %q", lastErr)
	}
	if replJobExists(t, dc, "prod-to-dr", vmGuardSV1) {
		t.Error("expected the finished Job to be cleaned up")
	}
}

// TestReplicationController_SkipsVolumeWhenVMCheckFails verifies that the
// controller does not copy a volume when it cannot tell whether a VM uses it.
func TestReplicationController_SkipsVolumeWhenVMCheckFails(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := fakeclient.NewClientBuilder().
		WithScheme(newReplTestScheme()).
		WithStatusSubresource(&batchv1.Job{}).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				if _, ok := list.(*corev1.PodList); ok {
					return errors.New("pods is forbidden")
				}
				return c.List(ctx, list, opts...)
			},
		}).
		Build()
	k.addSubVolume(newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10))
	k.addPolicy(newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms"))

	rc := newReplController(k, dc, newFakeNFSOperations())
	rc.processOnce(context.Background())

	if replJobExists(t, dc, "prod-to-dr", vmGuardSV1) {
		t.Fatal("expected no Job when the VM check fails")
	}
	lastErr, _ := svReplStatus(t, k, "prod-to-dr", vmGuardSV1)
	if !strings.Contains(lastErr, "pods is forbidden") {
		t.Errorf("expected the status to carry the check error, got %q", lastErr)
	}
}
