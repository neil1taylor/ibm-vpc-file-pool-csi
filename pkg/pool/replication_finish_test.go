package pool

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/hooks"
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/client-go/tools/cache"
)

// A policy with a long schedule must notice at once, not at the next
// scheduled cycle, that its copy Jobs have finished (issue #192).
func TestReplicationController_NoticesAFinishedJobBeforeTheNextCycle(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	k.addSubVolume(newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10))
	k.addPolicy(newTestReplicationPolicy("prod-to-dr", "prod-pool", "1h"))

	rc := newReplController(k, dc, newFakeNFSOperations())
	clock := time.Now()
	rc.SetNowFunc(func() time.Time { return clock })
	ctx := context.Background()
	tick := func(d time.Duration) {
		clock = clock.Add(d)
		rc.processOnce(ctx)
	}

	tick(time.Second) // the cycle starts and makes the Job
	if e, synced := svReplStatus(t, k, "prod-to-dr", vmGuardSV1); e != "sync in progress" || synced {
		t.Fatalf("while the Job runs: error %q synced %v", e, synced)
	}
	tick(30 * time.Second) // Job still running: nothing changes
	simulateReplJobSucceeded(t, dc, "prod-to-dr", vmGuardSV1)
	tick(30 * time.Second) // the next tick, 1 minute into a 1 hour schedule

	if e, synced := svReplStatus(t, k, "prod-to-dr", vmGuardSV1); e != "" || !synced {
		t.Errorf("after the Job finished: error %q synced %v, want synced", e, synced)
	}
	p := k.getPolicy("prod-to-dr")
	if p.Status.LastSyncTime == nil || p.Status.LastError != "" {
		t.Errorf("policy status = %+v", p.Status)
	}
	if replJobExists(t, dc, "prod-to-dr", vmGuardSV1) {
		t.Error("the finished Job must be cleaned up")
	}

	// The next cycle still starts only when the schedule says so.
	tick(time.Minute)
	if replJobExists(t, dc, "prod-to-dr", vmGuardSV1) {
		t.Error("a new cycle must not start before the schedule is due")
	}
	tick(time.Hour)
	if !replJobExists(t, dc, "prod-to-dr", vmGuardSV1) {
		t.Error("a new cycle must start when the schedule is due")
	}
}

// Pre-sync hooks belong to the start of a cycle. Looking at a running cycle
// again must not run them again.
func TestReplicationController_PreSyncHooksRunOncePerCycle(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	k.addSubVolume(newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10))
	policy := newTestReplicationPolicy("prod-to-dr", "prod-pool", "1h")
	policy.Spec.PreSyncHooks = []v1alpha1.Hook{{Name: "freeze", Type: v1alpha1.HookTypeExec}}
	policy.Spec.PostSyncHooks = []v1alpha1.Hook{{Name: "thaw", Type: v1alpha1.HookTypeExec}}
	k.addPolicy(policy)

	var pre, post atomic.Int32
	rc := newReplController(k, dc, newFakeNFSOperations())
	rc.SetOrchestrator(hooks.NewOrchestratorWithExecutor(hooks.ExecutorFunc(
		func(_ context.Context, h v1alpha1.Hook) (*v1alpha1.HookResult, error) {
			if h.Name == "freeze" {
				pre.Add(1)
			} else {
				post.Add(1)
			}
			return &v1alpha1.HookResult{Name: h.Name, Success: true}, nil
		})))
	clock := time.Now()
	rc.SetNowFunc(func() time.Time { return clock })
	ctx := context.Background()
	tick := func(d time.Duration) {
		clock = clock.Add(d)
		rc.processOnce(ctx)
	}

	tick(time.Second)
	tick(30 * time.Second)
	tick(30 * time.Second)
	simulateReplJobSucceeded(t, dc, "prod-to-dr", vmGuardSV1)
	tick(30 * time.Second)

	if pre.Load() != 1 || post.Load() != 1 {
		t.Errorf("pre-sync hooks ran %d times and post-sync hooks %d times, want once each", pre.Load(), post.Load())
	}
}

func TestReplicationController_RunsAtOnceWhenKicked(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	k.addSubVolume(newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10))
	k.addPolicy(newTestReplicationPolicy("prod-to-dr", "prod-pool", "1h"))
	rc := newReplController(k, dc, newFakeNFSOperations())
	rc.SetInterval(time.Hour)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rc.Run(ctx)

	rc.Kick()
	waitForReplJob(t, dc, "prod-to-dr", vmGuardSV1, 3*time.Second)
	rc.Kick()
	rc.Kick() // a second kick while one waits must not block
}

func TestReplicationController_TellsTheBackupControllerAfterAPass(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	k.addPolicy(newTestReplicationPolicy("prod-to-dr", "prod-pool", "1h"))
	rc := newReplController(k, dc, newFakeNFSOperations())
	var told atomic.Int32
	rc.SetOnProcessed(func() { told.Add(1) })

	rc.processOnce(context.Background())
	if told.Load() != 1 {
		t.Errorf("told %d times after a pass that looked at a policy, want 1", told.Load())
	}
}

func TestBackupController_RunsAtOnceWhenKicked(t *testing.T) {
	e := newBackupEnv(t)
	e.bc.interval = time.Hour
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()
	go e.bc.Run(ctx)
	time.Sleep(300 * time.Millisecond) // the first pass finds nothing

	e.addClaim("apps", "db", "data-a", map[string]string{"backup": "yes"})
	e.policy("nightly", nil)
	e.bc.Kick()
	e.bc.Kick() // a second kick while one waits must not block

	deadline := time.Now().Add(3 * time.Second)
	for len(e.repls()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("a kick must make the controller look at its policies now")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestJobFinishHandler(t *testing.T) {
	job := func(active, succeeded, failed int32) *batchv1.Job {
		return &batchv1.Job{Status: batchv1.JobStatus{Active: active, Succeeded: succeeded, Failed: failed}}
	}
	tests := []struct {
		name     string
		old, new *batchv1.Job
		want     bool
	}{
		{"finished", job(1, 0, 0), job(0, 1, 0), true},
		{"a pod failed", job(1, 0, 0), job(1, 0, 1), true},
		{"started only", job(0, 0, 0), job(1, 0, 0), false},
		{"resync, no change", job(0, 1, 0), job(0, 1, 0), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var kicks, other int
			h := NewJobFinishHandler(func() { kicks++ }, func() { other++ })
			h.OnUpdate(tt.old, tt.new)
			if (kicks == 1) != tt.want || kicks != other {
				t.Errorf("kicks = %d, %d; want a kick: %v", kicks, other, tt.want)
			}
		})
	}
	t.Run("add and delete do not kick", func(t *testing.T) {
		kicks := 0
		h := NewJobFinishHandler(func() { kicks++ })
		h.OnAdd(job(0, 1, 0), true)
		h.OnDelete(job(0, 1, 0))
		h.OnDelete(cache.DeletedFinalStateUnknown{Key: "kube-system/x", Obj: job(0, 1, 0)})
		if kicks != 0 {
			t.Errorf("kicks = %d, want 0", kicks)
		}
	})
}
