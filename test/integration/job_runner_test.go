//go:build integration

package integration

import (
	"context"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// ---------------------------------------------------------------------------
// Fake Kubernetes Job execution (clone worker and replication controller)
// ---------------------------------------------------------------------------

// newFakeDirectClient returns a controller-runtime fake client that holds the
// PVs, PVCs and Jobs the clone worker and replication controller create.
func newFakeDirectClient() client.Client {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	return fakeclient.NewClientBuilder().
		WithScheme(s).
		WithStatusSubresource(&batchv1.Job{}).
		Build()
}

// fakeJobRunner stands in for the Job controller and kubelet: it finds Jobs
// that have no result yet and marks them succeeded or failed.
type fakeJobRunner struct {
	dc client.Client

	mu     sync.Mutex
	failIf func(job *batchv1.Job) bool // nil means every Job succeeds
	ran    int                         // Jobs given a result, counting a re-created Job each time
	names  map[string]bool             // distinct Job names given a result
}

// startFakeJobRunner starts a runner that stops when the test ends.
func startFakeJobRunner(t *testing.T, dc client.Client) *fakeJobRunner {
	t.Helper()
	r := &fakeJobRunner{dc: dc, names: make(map[string]bool)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		r.run(ctx)
	}()
	t.Cleanup(func() {
		cancel()
		<-done
	})
	return r
}

// failJobsWhere makes Jobs that match the predicate fail. Jobs already given
// a result are not changed.
func (r *fakeJobRunner) failJobsWhere(pred func(job *batchv1.Job) bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failIf = pred
}

// failAllJobs makes every new Job fail.
func (r *fakeJobRunner) failAllJobs() {
	r.failJobsWhere(func(*batchv1.Job) bool { return true })
}

// succeedAllJobs makes every new Job succeed.
func (r *fakeJobRunner) succeedAllJobs() {
	r.failJobsWhere(nil)
}

// jobCount returns how many Jobs have been given a result. A Job that is
// deleted and created again counts each time.
func (r *fakeJobRunner) jobCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.ran
}

// distinctJobCount returns how many differently named Jobs have been given a
// result.
func (r *fakeJobRunner) distinctJobCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.names)
}

func (r *fakeJobRunner) run(ctx context.Context) {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.runPendingJobs(ctx)
		}
	}
}

func (r *fakeJobRunner) runPendingJobs(ctx context.Context) {
	jobs := &batchv1.JobList{}
	if err := r.dc.List(ctx, jobs); err != nil {
		return
	}
	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
			continue
		}

		r.mu.Lock()
		fail := r.failIf != nil && r.failIf(job)
		r.mu.Unlock()

		if fail {
			backoffLimit := int32(3)
			if job.Spec.BackoffLimit != nil {
				backoffLimit = *job.Spec.BackoffLimit
			}
			job.Status.Failed = backoffLimit + 1
		} else {
			job.Status.Succeeded = 1
		}
		// The controller may delete the Job at the same moment; skip it then.
		if err := r.dc.Status().Update(ctx, job); err != nil {
			continue
		}
		r.mu.Lock()
		r.ran++
		r.names[job.Name] = true
		r.mu.Unlock()
	}
}
