package poolbench

import (
	"context"
	"fmt"
	"math"
	"sort"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// eventTimeout is the longest an event may take. After it the event ends with
// what it has and the run goes on.
const eventTimeout = 30 * time.Minute

// backupTimeout is the longest a backup may take. A backup copies whole VM
// disks at the backup policy's copy speed limit, so it takes much longer than
// the other events.
const backupTimeout = 60 * time.Minute

// timeoutOf is the longest the event of this type may take.
func timeoutOf(eventType string) time.Duration {
	if eventType == v1alpha1.PoolBenchEventBackup {
		return backupTimeout
	}
	return eventTimeout
}

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
	case v1alpha1.PoolBenchEventMigration:
		err = r.startMigration(ctx, pb)
	case v1alpha1.PoolBenchEventSnapshot:
		err = r.startSnapshot(ctx, pb)
	case v1alpha1.PoolBenchEventBackup:
		err = r.startBackup(ctx, pb)
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
	timedOut := now.Sub(st.StageStarted.Time) > timeoutOf(res.Type)
	var done bool
	var msg string
	switch res.Type {
	case v1alpha1.PoolBenchEventBootStorm:
		done, msg = r.pollBootStorm(ctx, pb, res.Started.Time, now, timedOut)
	case v1alpha1.PoolBenchEventPatchWindow:
		done, msg = r.pollPatchWindow(pb, res.Started.Time, now, timedOut)
	case v1alpha1.PoolBenchEventMigration:
		done, msg = r.pollMigration(ctx, pb, timedOut)
	case v1alpha1.PoolBenchEventSnapshot:
		done, msg = r.pollSnapshot(ctx, pb, timedOut)
	case v1alpha1.PoolBenchEventBackup:
		r.sampleCopySpeed(ctx, pb, res)
		done, msg = r.pollBackup(ctx, pb, res.Started.Time, timedOut)
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
	if res.Type == v1alpha1.PoolBenchEventBackup {
		r.stopBackup(ctx, pb, now)
		msg += r.backupNumbers(pb, res, now)
	}
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

// eventLabel marks the objects an event makes in the run's namespace.
const eventLabel = "storage.ibmcloud.io/poolbench-event"

// vmsText is "1 VM" or "3 VMs".
func vmsText(n int) string {
	if n == 1 {
		return "1 VM"
	}
	return fmt.Sprintf("%d VMs", n)
}

// eventTargets are the VMs a migration, snapshot or backup works on: 10 percent
// of the running VMs (at least 1, at most 10), the lowest-numbered ones.
func (r *Runner) eventTargets(ctx context.Context, pb *v1alpha1.PoolBench) ([]string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmiGVK.GroupVersion().WithKind("VirtualMachineInstanceList"))
	if err := r.Client.List(ctx, list, client.InNamespace(runName(pb))); err != nil {
		return nil, fmt.Errorf("cannot list the VMs: %w", err)
	}
	if len(list.Items) == 0 {
		return nil, fmt.Errorf("there is no running VM to work on")
	}
	names := make([]string, 0, len(list.Items))
	for _, vmi := range list.Items {
		names = append(names, vmi.GetName())
	}
	sort.Strings(names)
	return names[:backupTargetCount(len(names))], nil
}

// backupTargetCount is how many of n VMs a migration, snapshot or backup works
// on: 10 percent, at least 1, at most 10.
func backupTargetCount(n int) int { return max(1, min(10, n/10)) }

// startMigration live-migrates the target VMs at once.
func (r *Runner) startMigration(ctx context.Context, pb *v1alpha1.PoolBench) error {
	vms, err := r.eventTargets(ctx, pb)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		m := &unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{"vmiName": vm},
		}}
		m.SetGroupVersionKind(vmimGVK)
		m.SetName("pb-mig-" + vm)
		m.SetNamespace(runName(pb))
		m.SetLabels(map[string]string{eventLabel: v1alpha1.PoolBenchEventMigration})
		if err := r.createIfMissing(ctx, m); err != nil {
			return fmt.Errorf("cannot move %s: %w", vm, err)
		}
	}
	return nil
}

// pollMigration is done when every migration the event made has ended.
func (r *Runner) pollMigration(ctx context.Context, pb *v1alpha1.PoolBench, timedOut bool) (bool, string) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmimGVK.GroupVersion().WithKind("VirtualMachineInstanceMigrationList"))
	if err := r.Client.List(ctx, list, client.InNamespace(runName(pb)), client.MatchingLabels{eventLabel: v1alpha1.PoolBenchEventMigration}); err != nil {
		return false, ""
	}
	var ok, failed int
	var slowest time.Duration
	for _, m := range list.Items {
		phase, _, _ := unstructured.NestedString(m.Object, "status", "phase")
		switch phase {
		case "Succeeded":
			ok++
			vm, _, _ := unstructured.NestedString(m.Object, "spec", "vmiName")
			slowest = max(slowest, r.migrationTime(ctx, pb, vm))
		case "Failed":
			failed++
		}
	}
	total := len(list.Items)
	switch {
	case ok+failed == total && failed == 0:
		msg := fmt.Sprintf("Moved %s at once; all finished.", vmsText(total))
		if slowest > 0 {
			msg = fmt.Sprintf("Moved %s at once; all finished, the slowest took %d s.", vmsText(total), int64(slowest.Seconds()))
		}
		return true, msg
	case ok+failed == total:
		return true, fmt.Sprintf("Moved %s at once: %d finished, %d failed.", vmsText(total), ok, failed)
	case timedOut:
		return true, fmt.Sprintf("Moved %s at once; not all finished within %d minutes (%d did, %d failed).", vmsText(total), int(eventTimeout.Minutes()), ok, failed)
	}
	return false, ""
}

// migrationTime is how long the last migration of a VM took, from the VM's own
// record. It is zero when the record has no times.
func (r *Runner) migrationTime(ctx context.Context, pb *v1alpha1.PoolBench, vm string) time.Duration {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(vmiGVK)
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: runName(pb), Name: vm}, vmi); err != nil {
		return 0
	}
	start, _, _ := unstructured.NestedString(vmi.Object, "status", "migrationState", "startTimestamp")
	end, _, _ := unstructured.NestedString(vmi.Object, "status", "migrationState", "endTimestamp")
	a, errA := time.Parse(time.RFC3339, start)
	b, errB := time.Parse(time.RFC3339, end)
	if errA != nil || errB != nil || b.Before(a) {
		return 0
	}
	return b.Sub(a)
}

// startSnapshot takes an OpenShift snapshot of each target VM while it runs.
// The driver makes the disk snapshots, instantly when the cluster allows it.
func (r *Runner) startSnapshot(ctx context.Context, pb *v1alpha1.PoolBench) error {
	vms, err := r.eventTargets(ctx, pb)
	if err != nil {
		return err
	}
	for _, vm := range vms {
		snap := &unstructured.Unstructured{Object: map[string]interface{}{
			"spec": map[string]interface{}{"source": map[string]interface{}{
				"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": vm,
			}},
		}}
		snap.SetGroupVersionKind(vmsnapGVK)
		snap.SetName("pb-snap-" + vm)
		snap.SetNamespace(runName(pb))
		snap.SetLabels(map[string]string{eventLabel: v1alpha1.PoolBenchEventSnapshot})
		if err := r.createIfMissing(ctx, snap); err != nil {
			return fmt.Errorf("cannot snapshot %s: %w", vm, err)
		}
	}
	return nil
}

// pollSnapshot is done when every snapshot the event made is ready or failed.
func (r *Runner) pollSnapshot(ctx context.Context, pb *v1alpha1.PoolBench, timedOut bool) (bool, string) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmsnapGVK.GroupVersion().WithKind("VirtualMachineSnapshotList"))
	if err := r.Client.List(ctx, list, client.InNamespace(runName(pb)), client.MatchingLabels{eventLabel: v1alpha1.PoolBenchEventSnapshot}); err != nil {
		return false, ""
	}
	var ok, failed int
	for _, snap := range list.Items {
		ready, _, _ := unstructured.NestedBool(snap.Object, "status", "readyToUse")
		phase, _, _ := unstructured.NestedString(snap.Object, "status", "phase")
		switch {
		case ready:
			ok++
		case phase == "Failed":
			failed++
		}
	}
	total := len(list.Items)
	switch {
	case ok+failed == total && failed == 0:
		return true, fmt.Sprintf("Snapshots of %s, taken while the VMs ran; all are ready.", vmsText(total))
	case ok+failed == total:
		return true, fmt.Sprintf("Snapshots of %s, taken while the VMs ran: %d ready, %d failed.", vmsText(total), ok, failed)
	case timedOut:
		return true, fmt.Sprintf("Snapshots of %s; not all were ready within %d minutes (%d were, %d failed).", vmsText(total), int(eventTimeout.Minutes()), ok, failed)
	}
	return false, ""
}

// backupLabel is on the load disks the backup event backs up. Its value is the
// run's name; the event's policy selects the disks by it.
const backupLabel = "storage.ibmcloud.io/poolbench-backup"

// backupPolicyLabel is on the replication policies a backup policy makes. It is
// the same label as in pkg/pool, which this package does not import.
const backupPolicyLabel = "storage.ibmcloud.io/backup-policy"

// startBackup labels the load disks of the target VMs and makes a backup policy
// that selects them. The policy copies them into the run's own backup pool, so
// the backup load is not on the share under test but the VMs' disks are read
// from it. The policy goes when the event ends, so it backs up once.
func (r *Runner) startBackup(ctx context.Context, pb *v1alpha1.PoolBench) error {
	vms, err := r.eventTargets(ctx, pb)
	if err != nil {
		return err
	}
	if err := r.ensureBackupPool(ctx, pb); err != nil {
		return err
	}
	for _, vm := range vms {
		pvc := &corev1.PersistentVolumeClaim{}
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: runName(pb), Name: vm + "-data"}, pvc); err != nil {
			return fmt.Errorf("cannot find the disk of %s: %w", vm, err)
		}
		patch := client.MergeFrom(pvc.DeepCopy())
		if pvc.Labels == nil {
			pvc.Labels = map[string]string{}
		}
		pvc.Labels[backupLabel] = pb.Name
		if err := r.Client.Patch(ctx, pvc, patch); err != nil {
			return fmt.Errorf("cannot mark the disk of %s: %w", vm, err)
		}
	}
	bp := &v1alpha1.BackupPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: backupPolicyName(pb), Labels: runLabels(pb)},
		Spec: v1alpha1.BackupPolicySpec{
			SnapshotPool: backupPoolName(pb),
			Schedule:     "1h",
			KeepFor:      "1h",
			Selector:     &metav1.LabelSelector{MatchLabels: map[string]string{backupLabel: pb.Name}},
			Namespaces:   []string{runName(pb)},
		},
	}
	if err := r.createIfMissing(ctx, bp); err != nil {
		return fmt.Errorf("cannot start the backup: %w", err)
	}
	return nil
}

// ensureBackupPool makes the pool the backups go to, if the run does not have it.
func (r *Runner) ensureBackupPool(ctx context.Context, pb *v1alpha1.PoolBench) error {
	return r.ensurePool(ctx, pb, backupPoolName(pb), buildBackupPool)
}

func backupPolicyName(pb *v1alpha1.PoolBench) string { return runName(pb) + "-backup" }

// pollBackup is done when every target disk has a backup made after the event
// began. The policy's own message is added when time runs out, to say why.
func (r *Runner) pollBackup(ctx context.Context, pb *v1alpha1.PoolBench, started time.Time, timedOut bool) (bool, string) {
	pvcs := &corev1.PersistentVolumeClaimList{}
	if err := r.Client.List(ctx, pvcs, client.InNamespace(runName(pb)), client.MatchingLabels{backupLabel: pb.Name}); err != nil {
		return false, ""
	}
	bp := &v1alpha1.BackupPolicy{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: backupPolicyName(pb)}, bp); err != nil {
		if apierrors.IsNotFound(err) {
			return true, "The backup policy is gone, so the backup did not finish."
		}
		return false, ""
	}
	total := len(pvcs.Items)
	done := 0
	for _, v := range bp.Status.Volumes {
		if v.PVCNamespace == runName(pb) && v.LastBackupTime != nil && !v.LastBackupTime.Time.Before(started) {
			done++
		}
	}
	switch {
	case done >= total:
		return true, fmt.Sprintf("Backed up %s while they ran; all finished.", vmsText(total))
	case timedOut:
		msg := fmt.Sprintf("Backed up %s; not all finished within %d minutes (%d did).", vmsText(total), int(backupTimeout.Minutes()), done)
		if bp.Status.Phase == "Error" && bp.Status.Message != "" {
			msg += " The backup said: " + bp.Status.Message
		}
		return true, msg
	}
	return false, ""
}

// sampleCopySpeed adds a reading of the speed of every copy of a running VM
// disk that the event's backup policy has now to the event's result.
func (r *Runner) sampleCopySpeed(ctx context.Context, pb *v1alpha1.PoolBench, res *v1alpha1.PoolBenchEventResult) {
	policies := &v1alpha1.ReplicationPolicyList{}
	if err := r.Client.List(ctx, policies, client.MatchingLabels{backupPolicyLabel: backupPolicyName(pb)}); err != nil {
		klog.ErrorS(err, "PoolBench: cannot read the copy speed of the backup", "run", pb.Name)
		return
	}
	for _, rp := range policies.Items {
		for _, sv := range rp.Status.SubVolumeStatuses {
			if sv.VMCopy == nil || sv.VMCopy.SpeedMBps <= 0 {
				continue
			}
			res.CopySpeedSumMBps += int64(sv.VMCopy.SpeedMBps)
			res.CopySpeedSamples++
			res.CopySpeedMaxMBps = max(res.CopySpeedMaxMBps, sv.VMCopy.SpeedMBps)
		}
	}
}

// backupNumbers sets the copy speed average and the VMs' latency in the result
// of the backup event that ends now, and returns the sentences that say them.
func (r *Runner) backupNumbers(pb *v1alpha1.PoolBench, res *v1alpha1.PoolBenchEventResult, now time.Time) string {
	var text string
	if res.CopySpeedSamples > 0 {
		res.CopySpeedAvgMBps = int32(math.Round(float64(res.CopySpeedSumMBps) / float64(res.CopySpeedSamples))) // #nosec G115 -- an average of int32 speeds
		text += fmt.Sprintf(" The copy speed averaged %d MB/s and reached %d MB/s.", res.CopySpeedAvgMBps, res.CopySpeedMaxMBps)
	}
	st := &pb.Status
	n := r.target(pb)
	all := r.Store.Results(pb.Name)
	results := make(map[string][]Result, n)
	for i := int32(1); i <= n; i++ {
		results[vmName(i)] = all[vmName(i)]
	}
	during := stepRules(pb).Step(results, res.Started.Unix(), now.Unix())
	if during.P95Ms > 0 && len(st.Steps) > 0 {
		res.P95Micros = int64(during.P95Ms * 1000)
		res.P95BeforeMicros = st.Steps[len(st.Steps)-1].P95Micros
		text += fmt.Sprintf(" The VMs' p95 latency was %.1f ms during the backup and %.1f ms before.",
			float64(res.P95Micros)/1000, float64(res.P95BeforeMicros)/1000)
	}
	return text
}

// stopBackup deletes the event's backup policy, so it does not back up again.
func (r *Runner) stopBackup(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time) {
	bp := &v1alpha1.BackupPolicy{ObjectMeta: metav1.ObjectMeta{Name: backupPolicyName(pb)}}
	if err := r.Client.Delete(ctx, bp); err != nil && !apierrors.IsNotFound(err) {
		r.log(pb, now, "Warn", fmt.Sprintf("Could not delete the backup policy: %v", err))
	}
}
