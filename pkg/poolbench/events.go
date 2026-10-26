package poolbench

import (
	"context"
	"fmt"
	"math"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// eventTimeout is the longest an event may take. After it the event ends with
// what it has and the run goes on.
const eventTimeout = 30 * time.Minute

// patchWindow is how long all VMs run only bursts in a patch window.
const patchWindow = 10 * time.Minute

// eventNames are the plain names of the events, for messages.
var eventNames = map[string]string{
	v1alpha1.PoolBenchEventBootStorm:   "boot storm",
	v1alpha1.PoolBenchEventPatchWindow: "patch window",
	v1alpha1.PoolBenchEventMigration:   "live migration",
	v1alpha1.PoolBenchEventSnapshot:    "snapshot",
	v1alpha1.PoolBenchEventBackup:      "backup",
}

// pendingEvent is the first event asked for at the VM count of the current
// step that has no result yet.
func (r *Runner) pendingEvent(pb *v1alpha1.PoolBench) *v1alpha1.PoolBenchEvent {
	for i, ev := range pb.Spec.Events {
		if ev.AtVMs != r.target(pb) {
			continue
		}
		done := false
		for _, res := range pb.Status.Events {
			if res.Type == ev.Type && res.AtVMs == ev.AtVMs {
				done = true
			}
		}
		if !done {
			return &pb.Spec.Events[i]
		}
	}
	return nil
}

// startEvent enters the Event stage and starts the event. An event that cannot
// start ends at once with the reason; it never fails the run.
func (r *Runner) startEvent(ctx context.Context, pb *v1alpha1.PoolBench, ev *v1alpha1.PoolBenchEvent, now time.Time) error {
	st := &pb.Status
	st.Stage, st.StageStarted, st.StageEnds = v1alpha1.PoolBenchStageEvent, &metav1.Time{Time: now}, nil
	st.Events = append(st.Events, v1alpha1.PoolBenchEventResult{Type: ev.Type, AtVMs: ev.AtVMs, Started: &metav1.Time{Time: now}})
	r.log(pb, now, "Info", fmt.Sprintf("Step %d: event %s at %d VMs", st.Step, eventNames[ev.Type], ev.AtVMs))
	r.describe(pb, now)
	var err error
	switch ev.Type {
	case v1alpha1.PoolBenchEventBootStorm:
		err = r.startBootStorm(ctx, pb)
	case v1alpha1.PoolBenchEventPatchWindow:
		// The VMs learn of the window from the collector, which reads this stage.
	default:
		return r.endEvent(ctx, pb, now, "This event is not available in this version.")
	}
	if err != nil {
		return r.endEvent(ctx, pb, now, "Not run: "+err.Error())
	}
	return nil
}

// event moves the running event one small step on.
func (r *Runner) event(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time) error {
	st := &pb.Status
	if len(st.Events) == 0 || st.Events[len(st.Events)-1].Ended != nil {
		return r.nextStep(ctx, pb, now)
	}
	res := &st.Events[len(st.Events)-1]
	timedOut := now.Sub(st.StageStarted.Time) > eventTimeout
	var done bool
	var msg string
	switch res.Type {
	case v1alpha1.PoolBenchEventBootStorm:
		done, msg = r.pollBootStorm(ctx, pb, res.Started.Time, now, timedOut)
	case v1alpha1.PoolBenchEventPatchWindow:
		done, msg = r.pollPatchWindow(pb, res.Started.Time, now, timedOut)
	default:
		done, msg = true, "This event is not available in this version."
	}
	if !done {
		r.describe(pb, now)
		return nil
	}
	return r.endEvent(ctx, pb, now, msg)
}

// endEvent writes the result of the running event, then starts the next event
// of this step or goes on to the next step.
func (r *Runner) endEvent(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time, msg string) error {
	st := &pb.Status
	res := &st.Events[len(st.Events)-1]
	res.Ended = &metav1.Time{Time: now}
	res.Seconds = int64(now.Sub(res.Started.Time).Seconds())
	res.Message = msg
	r.log(pb, now, "Info", fmt.Sprintf("Event %s ended after %d s: %s", eventNames[res.Type], res.Seconds, msg))
	return r.nextStep(ctx, pb, now)
}

// nextStep follows a measured step and its events: it ends the run at the
// knee or after the last step, and otherwise starts the next step.
func (r *Runner) nextStep(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time) error {
	st := &pb.Status
	if ev := r.pendingEvent(pb); ev != nil {
		return r.startEvent(ctx, pb, ev, now)
	}
	stopAtKnee := pb.Spec.Knee.StopAtKnee == nil || *pb.Spec.Knee.StopAtKnee
	if (st.KneeVMs > 0 && stopAtKnee) || st.Step >= st.TotalSteps {
		r.endRun(pb, now, false)
		return nil
	}
	st.Step++
	r.startCreating(pb, now)
	return nil
}

// startBootStorm restarts every VM of the run at once by deleting its running
// instance; the VM's run strategy makes a new one.
func (r *Runner) startBootStorm(ctx context.Context, pb *v1alpha1.PoolBench) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmiGVK.GroupVersion().WithKind("VirtualMachineInstanceList"))
	if err := r.Client.List(ctx, list, client.InNamespace(runName(pb))); err != nil {
		return fmt.Errorf("cannot list the VMs: %w", err)
	}
	for i := range list.Items {
		if err := client.IgnoreNotFound(r.Client.Delete(ctx, &list.Items[i])); err != nil {
			return fmt.Errorf("cannot restart %s: %w", list.Items[i].GetName(), err)
		}
	}
	return nil
}

// pollBootStorm is done when every VM of the step has sent a result made after
// the restart. A Windows VM that has not is given its load again, as at the
// start of a step.
func (r *Runner) pollBootStorm(ctx context.Context, pb *v1alpha1.PoolBench, started, now time.Time, timedOut bool) (bool, string) {
	n := r.target(pb)
	results := r.Store.Results(pb.Name)
	var back int32
	for i := int32(1); i <= n; i++ {
		sent := false
		for _, res := range results[vmName(i)] {
			if res.Epoch >= started.Unix() {
				sent = true
				break
			}
		}
		if sent {
			back++
			continue
		}
		if isWindows(&pb.Spec, i) && r.Starter != nil {
			_ = r.Starter.StartWindowsLoad(ctx, runName(pb), vmName(i))
		}
	}
	switch {
	case back == n:
		return true, fmt.Sprintf("Restarted %d VMs; all %d VMs sent results again after %d s.", n, n, int64(now.Sub(started).Seconds()))
	case timedOut:
		return true, fmt.Sprintf("Restarted %d VMs; not all sent results again within %d minutes (%d did).", n, int(eventTimeout.Minutes()), back)
	}
	return false, ""
}

// pollPatchWindow waits out the window, then waits until every VM has sent a
// steady result, made after the window, that is under the latency limit. The
// message gives how the VMs did in the window and how long the recovery took.
func (r *Runner) pollPatchWindow(pb *v1alpha1.PoolBench, started, now time.Time, timedOut bool) (bool, string) {
	windowEnd := started.Add(patchWindow)
	if now.Before(windowEnd) {
		return false, ""
	}
	n := r.target(pb)
	all := r.Store.Results(pb.Name)
	results := make(map[string][]Result, n)
	var back int32
	for i := int32(1); i <= n; i++ {
		vm := vmName(i)
		results[vm] = all[vm]
		for _, res := range all[vm] {
			if res.Phase == "steady" && res.Epoch >= windowEnd.Unix() && math.Max(res.ReadP95Ms, res.WriteP95Ms) <= float64(pb.Spec.Knee.LatencyLimitMs) {
				back++
				break
			}
		}
	}
	rules := stepRules(pb)
	window := rules.Step(results, started.Unix(), windowEnd.Unix())
	sent := false
	for _, vmResults := range results {
		for _, res := range vmResults {
			sent = sent || (res.Phase == "burst" && rules.InWindow(res, started.Unix(), windowEnd.Unix()))
		}
	}
	in := "No VM sent a result in the window. "
	if sent {
		in = fmt.Sprintf("For %d minutes all %d VMs ran only bursts: %d%% of them had a burst p95 over %d ms. ",
			int(patchWindow.Minutes()), n, window.BurstSlowPercent, BurstSlowMs)
	}
	switch {
	case back == n:
		return true, in + fmt.Sprintf("All VMs were back under the latency limit %d s after the window.", int64(now.Sub(windowEnd).Seconds()))
	case timedOut:
		return true, in + fmt.Sprintf("Not back under the latency limit within %d minutes (%d of %d VMs were).", int(eventTimeout.Minutes()), back, n)
	}
	return false, ""
}
