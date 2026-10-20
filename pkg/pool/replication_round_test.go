package pool

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// TestReplicationController_FailureCountedWhenJobsFinishAtDifferentTimes
// verifies that a failed copy counts as a failed cycle even when the Jobs of a
// cycle finish in different controller ticks. Before, a volume whose Job had
// finished got a new Job on the next tick, so a cycle with all Jobs finished
// at once never came, and failures were never counted or paused the policy.
func TestReplicationController_FailureCountedWhenJobsFinishAtDifferentTimes(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	svA := newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10)
	svB := newTestSubVolume(vmGuardSV2, "prod-pool", "share-1", 10)
	svB.Spec.PVCName = "other-pvc"
	k.addSubVolume(svA)
	k.addSubVolume(svB)
	policy := newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms")
	policy.Spec.MaxParallelSyncs = 2
	k.addPolicy(policy)

	rc := newReplController(k, dc, newFakeNFSOperations())
	clock := time.Now()
	rc.SetNowFunc(func() time.Time { return clock })
	ctx := context.Background()
	tick := func() {
		clock = clock.Add(time.Second)
		rc.processOnce(ctx)
	}
	setJob := func(sv string, succeeded bool) {
		t.Helper()
		job := &batchv1.Job{}
		if err := dc.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: replJobName("prod-to-dr", sv)}, job); err != nil {
			t.Fatalf("Job for %s: %v", sv, err)
		}
		if succeeded {
			job.Status.Succeeded = 1
		} else {
			job.Status.Failed = 4
		}
		if err := dc.Status().Update(ctx, job); err != nil {
			t.Fatal(err)
		}
	}

	tick()                    // both Jobs start
	setJob(vmGuardSV1, false) // A fails first
	tick()                    // A's result is recorded; B still runs
	if replJobExists(t, dc, "prod-to-dr", vmGuardSV1) {
		t.Fatal("expected A's finished Job cleaned up")
	}
	tick() // B still runs: A must not start again in the same cycle
	if replJobExists(t, dc, "prod-to-dr", vmGuardSV1) {
		t.Fatal("expected no new Job for A until the cycle's other Jobs finish")
	}
	setJob(vmGuardSV2, true)
	tick() // B finishes: the cycle is complete

	p := k.getPolicy("prod-to-dr")
	if p.Status.ConsecutiveFailures != 1 {
		t.Errorf("ConsecutiveFailures = %d, want 1", p.Status.ConsecutiveFailures)
	}
	if errA, _ := svReplStatus(t, k, "prod-to-dr", vmGuardSV1); errA == "" {
		t.Error("expected A's status to keep its failure")
	}
	if errB, synced := svReplStatus(t, k, "prod-to-dr", vmGuardSV2); errB != "" || !synced {
		t.Errorf("expected B synced, got err %q synced %v", errB, synced)
	}

	tick() // a new cycle starts both again
	if !replJobExists(t, dc, "prod-to-dr", vmGuardSV1) || !replJobExists(t, dc, "prod-to-dr", vmGuardSV2) {
		t.Error("expected the next cycle to start both Jobs again")
	}
}

// TestReplicationController_KeepsLastSyncTimeWhileSyncing verifies that a
// volume keeps its last good sync time in the status while a new sync runs,
// so the age of the last good copy stays visible.
func TestReplicationController_KeepsLastSyncTimeWhileSyncing(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	k.addSubVolume(newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10))
	k.addPolicy(newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms"))

	rc := newReplController(k, dc, newFakeNFSOperations())
	clock := time.Now()
	rc.SetNowFunc(func() time.Time { return clock })
	ctx := context.Background()
	tick := func() {
		clock = clock.Add(time.Second)
		rc.processOnce(ctx)
	}

	tick()
	simulateReplJobSucceeded(t, dc, "prod-to-dr", vmGuardSV1)
	tick() // sync recorded
	var first *metav1.Time
	for _, s := range k.getPolicy("prod-to-dr").Status.SubVolumeStatuses {
		first = s.LastSyncTime
	}
	if first == nil {
		t.Fatal("expected a first sync")
	}

	tick() // a new sync starts
	if !replJobExists(t, dc, "prod-to-dr", vmGuardSV1) {
		t.Fatal("expected a new Job")
	}
	for _, s := range k.getPolicy("prod-to-dr").Status.SubVolumeStatuses {
		if s.LastSyncTime == nil || !s.LastSyncTime.Equal(first) {
			t.Errorf("while syncing, LastSyncTime = %v, want the last good sync %v", s.LastSyncTime, first)
		}
		if s.LastError != "sync in progress" {
			t.Errorf("LastError = %q, want sync in progress", s.LastError)
		}
	}
}
