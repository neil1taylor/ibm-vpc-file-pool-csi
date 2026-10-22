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
			if newJob.Status.Succeeded > oldJob.Status.Succeeded || newJob.Status.Failed > oldJob.Status.Failed {
				for _, k := range kick {
					k()
				}
			}
		},
	}
}
