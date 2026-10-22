package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
)

// A Job whose pod restarts in place uses up its retries without the failed
// count reaching the limit: Kubernetes marks the Job Failed, deletes the pod,
// and the count stays at 0 or 1. The controller waited for ever, the policy
// stayed "Sending" with no error, and the Job was never retried (#213).

func markJobFailedByCondition(t *testing.T, dc client.Client, job *batchv1.Job) {
	t.Helper()
	job.Status.Failed = 1
	job.Status.Conditions = []batchv1.JobCondition{{
		Type: batchv1.JobFailed, Status: corev1.ConditionTrue,
		Reason: "BackoffLimitExceeded", Message: "Job has reached the specified backoff limit",
	}}
	if err := dc.Status().Update(context.Background(), job); err != nil {
		t.Fatal(err)
	}
}

func TestVMCopy_ApplyJobFailedCondition_ShowsErrorAndIsRetried(t *testing.T) {
	e := newVMCopyEnv(t)
	e.tick()
	e.cop.state = vmcopy.Done
	e.tick()
	for _, sv := range []string{vmGuardSV1, vmGuardSV2} {
		job := e.job(t, sv)
		if job == nil {
			t.Fatalf("no apply Job for %s", sv)
		}
		markJobFailedByCondition(t, e.dc, job)
	}
	e.tick()
	s := e.status(t, vmGuardSV1)
	if s.VMCopy != nil || s.LastSyncTime != nil {
		t.Errorf("status = %+v", s)
	}
	if !strings.Contains(s.LastError, "apply Job failed") || !strings.Contains(s.LastError, "BackoffLimitExceeded") {
		t.Errorf("LastError = %q, want the failure and its reason", s.LastError)
	}
	if p := e.k.getPolicy(e.policy); p.Status.LastError == "" {
		t.Error("the policy shows no error")
	}
	for _, sv := range []string{vmGuardSV1, vmGuardSV2} {
		if e.job(t, sv) != nil {
			t.Errorf("the failed Job of %s was not removed, so it can never be retried", sv)
		}
	}
	// The next scheduled cycle (every hour) starts a new copy.
	before := len(e.cop.starts)
	e.cop.state = vmcopy.Running
	e.clock = e.clock.Add(time.Hour)
	e.tick()
	if len(e.cop.starts) <= before {
		t.Error("no new copy was started after the failure")
	}
}

func TestCheckReplicationJob_FailedConditionIsDone(t *testing.T) {
	e := newVMCopyEnv(t)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: replJobName(e.policy, vmGuardSV1), Namespace: replJobNamespace},
		Spec:       batchv1.JobSpec{BackoffLimit: int32Ptr(3)},
	}
	if err := e.dc.Create(e.ctx, job); err != nil {
		t.Fatal(err)
	}
	if done, _, _, err := e.rc.checkReplicationJob(e.ctx, e.policy, vmGuardSV1); err != nil || done {
		t.Fatalf("a running Job: done = %v err = %v", done, err)
	}
	markJobFailedByCondition(t, e.dc, job)
	done, ok, res, err := e.rc.checkReplicationJob(e.ctx, e.policy, vmGuardSV1)
	if err != nil || !done || ok {
		t.Fatalf("done = %v ok = %v err = %v, want done and failed", done, ok, err)
	}
	if !strings.Contains(res.Failure, "BackoffLimitExceeded") {
		t.Errorf("failure = %q", res.Failure)
	}
}

func TestJobFinishHandler_KicksWhenAJobGetsItsFailedCondition(t *testing.T) {
	kicks := 0
	h := NewJobFinishHandler(func() { kicks++ })
	old := &batchv1.Job{Status: batchv1.JobStatus{Failed: 1}}
	nw := old.DeepCopy()
	nw.Status.Conditions = []batchv1.JobCondition{{Type: batchv1.JobFailed, Status: corev1.ConditionTrue}}
	h.OnUpdate(old, nw)
	if kicks != 1 {
		t.Fatalf("kicks = %d, want 1", kicks)
	}
}
