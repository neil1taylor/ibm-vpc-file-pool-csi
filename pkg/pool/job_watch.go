package pool

import (
	batchv1 "k8s.io/api/batch/v1"
	"k8s.io/client-go/tools/cache"
)

// NewJobFinishHandler returns an event handler that calls every kick function
// when a Job finishes, or when one of its pods fails. The controllers that
// wait for Jobs use it to look at their Jobs at once and not at their next
// tick.
func NewJobFinishHandler(kick ...func()) cache.ResourceEventHandler {
	return cache.ResourceEventHandlerFuncs{
		UpdateFunc: func(oldObj, newObj any) {
			oldJob, ok1 := oldObj.(*batchv1.Job)
			newJob, ok2 := newObj.(*batchv1.Job)
			if !ok1 || !ok2 {
				return
			}
			// A Job can fail without its failed count rising: a pod that
			// restarts in place uses up the retries, and the Job gets its
			// Failed condition.
			if newJob.Status.Succeeded > oldJob.Status.Succeeded || newJob.Status.Failed > oldJob.Status.Failed ||
				(jobHasFailed(newJob) && !jobHasFailed(oldJob)) {
				for _, k := range kick {
					k()
				}
			}
		},
	}
}
