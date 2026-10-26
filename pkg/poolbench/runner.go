package poolbench

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"math"
	"reflect"
	"sort"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

// Finalizer keeps a PoolBench until its namespace and pool are gone.
const Finalizer = "storage.ibmcloud.io/poolbench"

const (
	// readyTimeout is how long the VMs of a step may take to say PERF-READY.
	readyTimeout = 60 * time.Minute
	// poolTimeout is how long the run's pool may take to become Ready.
	poolTimeout = 30 * time.Minute
	// resultGrace is the wait after the measuring for the last results to arrive.
	resultGrace = 90 * time.Second
	// maxLogEntries is the number of log entries the status keeps.
	maxLogEntries = 200
	// vmOverheadBytes is the memory KubeVirt's launcher pod needs on top of the guest.
	vmOverheadBytes = 300 << 20
	// limitShare is the share of the IOPS limit at which the share counts as at its limit.
	limitShare = 0.95
)

var (
	vmGVK         = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachine"}
	vmiGVK        = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"}
	dataSourceGVK = schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataSource"}
	dataVolumeGVK = schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataVolume"}
)

// ShareCounter reads the NFS counters of a pool's share from the node agents:
// the reads and writes done, and the seconds they took, added up over every node.
type ShareCounter interface {
	Totals(ctx context.Context, pool string) (ops, seconds float64, err error)
}

// GuestStarter starts the load of a Windows VM through its guest agent.
// Starting twice does nothing.
type GuestStarter interface {
	StartWindowsLoad(ctx context.Context, namespace, vm string) error
}

// Runner moves each PoolBench through its stages, one small step a pass, and
// records every stage in the status. A new Runner goes on from the status, so
// a restart of the service loses nothing.
type Runner struct {
	Client client.Client
	Store  *Store
	// CollectorURL is where the test VMs reach the collector, for example
	// http://<service>.<namespace>.svc:8080.
	CollectorURL string
	Shares       ShareCounter
	Starter      GuestStarter
	// RunRole is the ClusterRole that gives the service its rights inside a
	// run's namespace. The runner binds it there to its own service account
	// (ServiceAccount in ServiceNamespace), so the service has no right to
	// VMs, disks or pod exec in any other namespace. Empty binds nothing.
	RunRole          string
	ServiceAccount   string
	ServiceNamespace string
	Now              func() time.Time
}

// Pass advances every run once.
func (r *Runner) Pass(ctx context.Context) {
	list := &v1alpha1.PoolBenchList{}
	if err := r.Client.List(ctx, list); err != nil {
		klog.ErrorS(err, "PoolBench: cannot list runs")
		return
	}
	for i := range list.Items {
		if err := r.advance(ctx, &list.Items[i]); err != nil {
			klog.ErrorS(err, "PoolBench: pass failed", "run", list.Items[i].Name)
		}
	}
}

func (r *Runner) advance(ctx context.Context, pb *v1alpha1.PoolBench) error {
	now := r.Now()
	if !pb.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, pb)
	}
	if !controllerutil.ContainsFinalizer(pb, Finalizer) {
		controllerutil.AddFinalizer(pb, Finalizer)
		if err := r.Client.Update(ctx, pb); err != nil {
			return fmt.Errorf("add finalizer: %w", err)
		}
	}
	before := pb.Status.DeepCopy()
	err := r.step(ctx, pb, now)
	st := &pb.Status
	r.Store.metrics.setRun(pb.Name, st.VMs-st.WindowsVMs, st.WindowsVMs, st.ReadyVMs, st.Step)
	if err != nil {
		klog.ErrorS(err, "PoolBench: step failed", "run", pb.Name, "phase", pb.Status.Phase, "stage", pb.Status.Stage)
		if msg := "A step failed and will be tried again: " + err.Error(); st.Message != msg {
			st.Message = msg
			r.log(pb, now, "Warning", msg)
		}
	}
	if !reflect.DeepEqual(*before, pb.Status) {
		if uerr := r.Client.Status().Update(ctx, pb); uerr != nil {
			return fmt.Errorf("save status: %w", uerr)
		}
	}
	return err
}

func (r *Runner) step(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time) error {
	st := &pb.Status
	active := st.Phase == "" || st.Phase == v1alpha1.PoolBenchPending || st.Phase == v1alpha1.PoolBenchChecking ||
		st.Phase == v1alpha1.PoolBenchRunning || st.Phase == v1alpha1.PoolBenchPaused
	if active && pb.Spec.Control.Stop {
		r.log(pb, now, "Control", "Stop asked: the run ends and cleans up")
		r.endRun(pb, now, true)
		return nil
	}
	switch st.Phase {
	case "", v1alpha1.PoolBenchPending:
		if waitFor, err := r.waitingFor(ctx, pb); err != nil || waitFor != "" {
			if waitFor != "" {
				st.Phase = v1alpha1.PoolBenchPending
				st.Message = fmt.Sprintf("Waits for the run %s to end", waitFor)
			}
			return err
		}
		st.Phase = v1alpha1.PoolBenchChecking
		st.Namespace, st.PoolName = runName(pb), runName(pb)
		st.TotalSteps = pb.Spec.MaxVMs / max(pb.Spec.StepVMs, 1)
		st.StartTime = &metav1.Time{Time: now}
		st.Message = "Checking the images and the room on the nodes"
		r.log(pb, now, "Info", fmt.Sprintf("Run started: %d GB share at %d IOPS, up to %d VMs in steps of %d",
			pb.Spec.ShareSizeGB, pb.Spec.IOPS, pb.Spec.MaxVMs, pb.Spec.StepVMs))
		return nil
	case v1alpha1.PoolBenchChecking:
		return r.check(ctx, pb, now)
	case v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchPaused:
		return r.run(ctx, pb, now)
	case v1alpha1.PoolBenchCleaningUp:
		done, err := r.cleanup(ctx, pb, now)
		if done {
			st.Phase = v1alpha1.PoolBenchComplete
			if pb.Spec.Control.Stop {
				st.Phase = v1alpha1.PoolBenchStopped
			}
			st.Message = r.summary(pb)
		}
		return err
	default: // Complete, Stopped, Failed
		if st.CleanedUp || (st.Phase != v1alpha1.PoolBenchFailed && pb.Spec.KeepAfterEnd && !pb.Spec.Control.Stop) {
			return nil
		}
		_, err := r.cleanup(ctx, pb, now)
		return err
	}
}

// waitingFor is the run that this run must wait for: its spec.startAfter, while
// that run exists and has not ended. Empty means this run may start.
func (r *Runner) waitingFor(ctx context.Context, pb *v1alpha1.PoolBench) (string, error) {
	if pb.Spec.StartAfter == "" {
		return "", nil
	}
	other := &v1alpha1.PoolBench{}
	err := r.Client.Get(ctx, client.ObjectKey{Name: pb.Spec.StartAfter}, other)
	if apierrors.IsNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	switch other.Status.Phase {
	case v1alpha1.PoolBenchComplete, v1alpha1.PoolBenchStopped, v1alpha1.PoolBenchFailed:
		// An ended run that still cleans up holds its share and VMs: wait for it too.
		if other.Status.CleanedUp || other.Spec.KeepAfterEnd {
			return "", nil
		}
	}
	return other.Name, nil
}

// log adds an entry to the run's log, which keeps the newest entries.
func (r *Runner) log(pb *v1alpha1.PoolBench, now time.Time, kind, msg string) {
	pb.Status.Log = append(pb.Status.Log, v1alpha1.PoolBenchLogEntry{Time: metav1.Time{Time: now}, Kind: kind, Message: msg})
	if n := len(pb.Status.Log); n > maxLogEntries {
		pb.Status.Log = pb.Status.Log[n-maxLogEntries:]
	}
	klog.V(2).InfoS("PoolBench", "run", pb.Name, "kind", kind, "message", msg)
}

func (r *Runner) fail(pb *v1alpha1.PoolBench, now time.Time, msg string) {
	pb.Status.Phase, pb.Status.Stage, pb.Status.Message = v1alpha1.PoolBenchFailed, "", msg
	pb.Status.EndTime = &metav1.Time{Time: now}
	r.log(pb, now, "Error", msg)
}

// endRun ends a run after its last step, at the knee, or on a stop.
func (r *Runner) endRun(pb *v1alpha1.PoolBench, now time.Time, stopped bool) {
	st := &pb.Status
	st.Stage, st.StageEnds, st.PausedAt = "", nil, nil
	st.EndTime = &metav1.Time{Time: now}
	if pb.Spec.KeepAfterEnd && !stopped {
		st.Phase = v1alpha1.PoolBenchComplete
		st.Message = r.summary(pb) + " The VMs and the pool are kept until you clean up."
		return
	}
	st.Phase = v1alpha1.PoolBenchCleaningUp
	st.Message = "Cleaning up: deleting the test VMs, the namespace and the pool"
}

func (r *Runner) summary(pb *v1alpha1.PoolBench) string {
	st := &pb.Status
	switch {
	case len(st.Steps) == 0:
		return "Ended before any step was measured."
	case st.KneeVMs == 0:
		return fmt.Sprintf("No knee up to %d VMs: the share carried every step.", st.Steps[len(st.Steps)-1].VMs)
	case st.RecommendedVMs == 0:
		return fmt.Sprintf("Knee at %d VMs, the first step: users would notice already.", st.KneeVMs)
	}
	return fmt.Sprintf("Knee at %d VMs: the first step where users would notice. "+
		"Recommended: %d VMs, the largest step at or below 80%% of the knee, to leave room to spare.", st.KneeVMs, st.RecommendedVMs)
}

// check makes sure the images exist and the largest step fits the nodes with
// one node down, then makes the run's namespace, secrets and pool.
func (r *Runner) check(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time) error {
	spec := withDefaults(pb.Spec)
	images := []v1alpha1.PoolBenchVM{spec.Linux}
	windows := windowsCount(&spec, spec.MaxVMs)
	if windows > 0 {
		images = append(images, spec.Windows)
	}
	for _, img := range images {
		ds := &unstructured.Unstructured{}
		ds.SetGroupVersionKind(dataSourceGVK)
		err := r.Client.Get(ctx, client.ObjectKey{Namespace: img.ImageNamespace, Name: img.Image}, ds)
		if apierrors.IsNotFound(err) {
			r.fail(pb, now, fmt.Sprintf("The image %s/%s does not exist. Make it first, or choose another image.", img.ImageNamespace, img.Image))
			pb.Status.CleanedUp = true
			return nil
		}
		if err != nil {
			return fmt.Errorf("read image %s: %w", img.Image, err)
		}
	}
	linux := int64(spec.MaxVMs - windows)
	need := linux*(int64(spec.Linux.MemoryGiB)<<30+vmOverheadBytes) + int64(windows)*(int64(spec.Windows.MemoryGiB)<<30+vmOverheadBytes)
	free, nodes, err := r.freeMemory(ctx)
	if err != nil {
		return err
	}
	if nodes < 2 || free < need {
		r.fail(pb, now, fmt.Sprintf("The nodes do not have room for %d VMs with one node down: they need %d GiB of memory and %d GiB is free.",
			spec.MaxVMs, need>>30, max(free, 0)>>30))
		pb.Status.CleanedUp = true
		return nil
	}
	if err := r.prepare(ctx, pb); err != nil {
		return err
	}
	pb.Status.Phase, pb.Status.Stage = v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStagePreparing
	pb.Status.StageStarted = &metav1.Time{Time: now}
	pb.Status.Message = "Making the test pool and waiting for its share"
	r.log(pb, now, "Info", "Checks passed; making the namespace and the pool")
	return nil
}

// freeMemory is the memory free for new pods on the schedulable workers,
// less the node with the most free memory, so the VMs still fit with one node
// down. nodes is the number of schedulable workers.
func (r *Runner) freeMemory(ctx context.Context) (free int64, nodes int, err error) {
	nodeList := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodeList, client.HasLabels{"node-role.kubernetes.io/worker"}); err != nil {
		return 0, 0, fmt.Errorf("list nodes: %w", err)
	}
	pods := &corev1.PodList{}
	if err := r.Client.List(ctx, pods); err != nil {
		return 0, 0, fmt.Errorf("list pods: %w", err)
	}
	used := map[string]int64{}
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodSucceeded || p.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, c := range p.Spec.Containers {
			used[p.Spec.NodeName] += c.Resources.Requests.Memory().Value()
		}
	}
	var most int64
	for _, n := range nodeList.Items {
		if n.Spec.Unschedulable {
			continue
		}
		f := n.Status.Allocatable.Memory().Value() - used[n.Name]
		free += f
		most = max(most, f)
		nodes++
	}
	return free - most, nodes, nil
}

// zone is the spec's zone, or the zone with the most worker nodes.
func (r *Runner) zone(ctx context.Context, pb *v1alpha1.PoolBench) (string, error) {
	if pb.Spec.Zone != "" {
		return pb.Spec.Zone, nil
	}
	nodes := &corev1.NodeList{}
	if err := r.Client.List(ctx, nodes, client.HasLabels{"node-role.kubernetes.io/worker"}); err != nil {
		return "", fmt.Errorf("list nodes: %w", err)
	}
	count := map[string]int{}
	for _, n := range nodes.Items {
		if z := n.Labels["topology.kubernetes.io/zone"]; z != "" {
			count[z]++
		}
	}
	best := ""
	for z, c := range count {
		if best == "" || c > count[best] || (c == count[best] && z < best) {
			best = z
		}
	}
	if best == "" {
		return "", errors.New("no worker node has a zone label; set spec.zone")
	}
	return best, nil
}

func (r *Runner) createIfMissing(ctx context.Context, obj client.Object) error {
	if err := r.Client.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create %s %s: %w", obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), err)
	}
	return nil
}

// prepare makes the run's namespace, its token, the VMs' config and its pool.
// Each exists once; a second call changes nothing.
func (r *Runner) prepare(ctx context.Context, pb *v1alpha1.PoolBench) error {
	ns := runName(pb)
	if err := r.createIfMissing(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: ns, Labels: runLabels(pb)}}); err != nil {
		return err
	}
	if r.RunRole != "" {
		rb := &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{Name: "poolbench", Namespace: ns, Labels: runLabels(pb)},
			RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: r.RunRole},
			Subjects:   []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Name: r.ServiceAccount, Namespace: r.ServiceNamespace}},
		}
		if err := r.createIfMissing(ctx, rb); err != nil {
			return err
		}
	}
	token, err := r.ensureToken(ctx, pb)
	if err != nil {
		return err
	}
	profile, err := NewProfile(pb.Spec.Load)
	if err != nil {
		return err
	}
	url := strings.TrimSuffix(r.CollectorURL, "/") + "/v1/runs/" + pb.Name + "/results"
	ud := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: UserDataSecret, Namespace: ns, Labels: runLabels(pb)},
		Data:       map[string][]byte{"userdata": []byte(profile.LinuxUserData(url, token))},
	}
	if cerr := r.createIfMissing(ctx, ud); cerr != nil {
		return cerr
	}
	if windowsCount(&pb.Spec, pb.Spec.MaxVMs) > 0 {
		win := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: WindowsSecret, Namespace: ns, Labels: runLabels(pb)}, Data: map[string][]byte{}}
		for name, data := range profile.WindowsFiles(url, token) {
			win.Data[name] = []byte(data)
		}
		if cerr := r.createIfMissing(ctx, win); cerr != nil {
			return cerr
		}
	}
	err = r.Client.Get(ctx, client.ObjectKey{Name: ns}, &v1alpha1.FileSharePool{})
	if apierrors.IsNotFound(err) {
		zone, zerr := r.zone(ctx, pb)
		if zerr != nil {
			return zerr
		}
		return r.createIfMissing(ctx, buildPool(pb, zone))
	}
	return err
}

func (r *Runner) ensureToken(ctx context.Context, pb *v1alpha1.PoolBench) (string, error) {
	sec := &corev1.Secret{}
	err := r.Client.Get(ctx, client.ObjectKey{Namespace: runName(pb), Name: TokenSecret}, sec)
	if err == nil {
		return string(sec.Data["token"]), nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := hex.EncodeToString(buf)
	sec = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: TokenSecret, Namespace: runName(pb), Labels: runLabels(pb)},
		Data:       map[string][]byte{"token": []byte(token)},
	}
	if err := r.Client.Create(ctx, sec); err != nil {
		return "", fmt.Errorf("create token: %w", err)
	}
	return token, nil
}

func windowsCount(spec *v1alpha1.PoolBenchSpec, vms int32) int32 {
	var n int32
	for i := int32(1); i <= vms; i++ {
		if isWindows(spec, i) {
			n++
		}
	}
	return n
}

// run advances a running or paused run.
func (r *Runner) run(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time) error {
	st := &pb.Status
	switch {
	case pb.Spec.Control.Paused && st.Phase == v1alpha1.PoolBenchRunning:
		st.Phase, st.PausedAt = v1alpha1.PoolBenchPaused, &metav1.Time{Time: now}
		st.Message = "Paused: the clock of the step stands still"
		r.log(pb, now, "Control", "Paused")
		return nil
	case !pb.Spec.Control.Paused && st.Phase == v1alpha1.PoolBenchPaused:
		if st.PausedAt != nil {
			paused := now.Sub(st.PausedAt.Time)
			if st.StageEnds != nil {
				st.StageEnds = &metav1.Time{Time: st.StageEnds.Add(paused)}
			}
			// The start of a measuring is the start of its window; other
			// stages move their start so the pause does not count as waiting.
			if st.Stage != v1alpha1.PoolBenchStageMeasuring && st.StageStarted != nil {
				st.StageStarted = &metav1.Time{Time: st.StageStarted.Add(paused)}
			}
		}
		st.Phase, st.PausedAt = v1alpha1.PoolBenchRunning, nil
		r.log(pb, now, "Control", "Resumed")
	case st.Phase == v1alpha1.PoolBenchPaused:
		return nil
	}
	switch st.Stage {
	case v1alpha1.PoolBenchStagePreparing:
		return r.preparing(ctx, pb, now)
	case v1alpha1.PoolBenchStageCreating:
		return r.creating(ctx, pb, now)
	case v1alpha1.PoolBenchStageSettling:
		if !now.Before(st.StageEnds.Time) {
			r.startMeasuring(ctx, pb, now)
		}
		return nil
	case v1alpha1.PoolBenchStageMeasuring:
		return r.measuring(ctx, pb, now)
	}
	return nil
}

func (r *Runner) preparing(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time) error {
	if err := r.prepare(ctx, pb); err != nil {
		return err
	}
	pool := &v1alpha1.FileSharePool{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: runName(pb)}, pool); err != nil {
		return err
	}
	if pool.Status.Phase == "Ready" {
		pb.Status.Step = 1
		r.startCreating(pb, now)
		return nil
	}
	if now.Sub(pb.Status.StageStarted.Time) > poolTimeout {
		r.fail(pb, now, fmt.Sprintf("The test pool was not ready after %s (phase %q).", poolTimeout, pool.Status.Phase))
	}
	return nil
}

func (r *Runner) target(pb *v1alpha1.PoolBench) int32 { return pb.Status.Step * pb.Spec.StepVMs }

func (r *Runner) startCreating(pb *v1alpha1.PoolBench, now time.Time) {
	st := &pb.Status
	n := r.target(pb)
	w := windowsCount(&pb.Spec, n)
	st.Stage, st.StageStarted, st.StageEnds = v1alpha1.PoolBenchStageCreating, &metav1.Time{Time: now}, nil
	st.Steps = append(st.Steps, v1alpha1.PoolBenchStepResult{VMs: n, WindowsVMs: w})
	r.log(pb, now, "Info", fmt.Sprintf("Step %d of %d: making VMs up to %d (%d Windows)", st.Step, st.TotalSteps, n, w))
	r.describe(pb, now)
}

// describe sets the plain-words message of the current stage.
func (r *Runner) describe(pb *v1alpha1.PoolBench, now time.Time) {
	st := &pb.Status
	what := map[string]string{
		v1alpha1.PoolBenchStageCreating:  "making VMs and waiting until each one sends results",
		v1alpha1.PoolBenchStageSettling:  "letting the share settle after the disk copies",
		v1alpha1.PoolBenchStageMeasuring: "measuring",
	}[st.Stage]
	msg := fmt.Sprintf("Step %d of %d: %d VMs (%d Windows), %s", st.Step, st.TotalSteps, r.target(pb), windowsCount(&pb.Spec, r.target(pb)), what)
	if st.StageEnds != nil && now.Before(st.StageEnds.Time) {
		msg += fmt.Sprintf(", %d min left", int(math.Ceil(st.StageEnds.Sub(now).Minutes())))
	}
	st.Message = msg
}

func (r *Runner) creating(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time) error {
	st := &pb.Status
	n := r.target(pb)
	for i := int32(1); i <= n; i++ {
		if err := r.createIfMissing(ctx, buildDataPVC(pb, i)); err != nil {
			return err
		}
		if err := r.createIfMissing(ctx, buildVM(pb, i)); err != nil {
			return err
		}
	}
	ready := r.Store.Ready(pb.Name)
	var notReady []string
	var readyVMs int32
	for i := int32(1); i <= n; i++ {
		if _, ok := ready[vmName(i)]; ok {
			readyVMs++
			continue
		}
		notReady = append(notReady, vmName(i))
		if isWindows(&pb.Spec, i) && r.Starter != nil {
			if err := r.Starter.StartWindowsLoad(ctx, runName(pb), vmName(i)); err != nil {
				klog.V(4).InfoS("PoolBench: Windows load not started yet", "run", pb.Name, "vm", vmName(i), "reason", err.Error())
			}
		}
	}
	st.VMs, st.WindowsVMs, st.ReadyVMs = n, windowsCount(&pb.Spec, n), readyVMs
	copying, err := r.copying(ctx, pb)
	if err != nil {
		return err
	}
	if len(notReady) == 0 && copying == 0 {
		row := &st.Steps[len(st.Steps)-1]
		row.ReadySeconds = int64(now.Sub(st.StageStarted.Time).Seconds())
		st.Stage, st.StageStarted = v1alpha1.PoolBenchStageSettling, &metav1.Time{Time: now}
		st.StageEnds = &metav1.Time{Time: now.Add(time.Duration(settleMinutes(pb)) * time.Minute)}
		r.log(pb, now, "Info", fmt.Sprintf("Step %d: all %d VMs ready after %d s; settling", st.Step, n, row.ReadySeconds))
		r.describe(pb, now)
		return nil
	}
	if now.Sub(st.StageStarted.Time) > readyTimeout {
		sort.Strings(notReady)
		r.logStuck(ctx, pb, now, notReady)
		if len(notReady) > 10 {
			notReady = append(notReady[:10], "...")
		}
		r.fail(pb, now, fmt.Sprintf("Step %d: VMs not ready after %s: %s", st.Step, readyTimeout, strings.Join(notReady, ", ")))
		return nil
	}
	r.describe(pb, now)
	return nil
}

// logStuck writes what the cluster says about the first VMs that never became
// ready, so the cause is still known after the cleanup removes them.
func (r *Runner) logStuck(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time, vms []string) {
	if len(vms) > 10 {
		vms = vms[:10]
	}
	for _, name := range vms {
		vmi := &unstructured.Unstructured{}
		vmi.SetGroupVersionKind(vmiGVK)
		if err := r.Client.Get(ctx, client.ObjectKey{Namespace: runName(pb), Name: name}, vmi); err != nil {
			r.log(pb, now, "Warning", fmt.Sprintf("%s has no running instance (%v)", name, err))
			continue
		}
		phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase")
		node, _, _ := unstructured.NestedString(vmi.Object, "status", "nodeName")
		conds, _, _ := unstructured.NestedSlice(vmi.Object, "status", "conditions")
		agent := "guest agent not reported"
		for _, c := range conds {
			m, _ := c.(map[string]interface{})
			if m["type"] == "AgentConnected" {
				agent = "guest agent not connected"
				if m["status"] == "True" {
					agent = "guest agent connected"
				}
			}
		}
		r.log(pb, now, "Warning", fmt.Sprintf("%s never became ready: instance %s on node %s, %s", name, phase, node, agent))
	}
}

func settleMinutes(pb *v1alpha1.PoolBench) int32 {
	if pb.Spec.SettleMinutes == nil {
		return 15
	}
	return *pb.Spec.SettleMinutes
}

// copying is the number of the run's disk copies that have not ended.
func (r *Runner) copying(ctx context.Context, pb *v1alpha1.PoolBench) (int, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(dataVolumeGVK.GroupVersion().WithKind("DataVolumeList"))
	if err := r.Client.List(ctx, list, client.InNamespace(runName(pb))); err != nil {
		return 0, fmt.Errorf("list disk copies: %w", err)
	}
	n := 0
	for _, dv := range list.Items {
		if phase, _, _ := unstructured.NestedString(dv.Object, "status", "phase"); phase != "Succeeded" {
			n++
		}
	}
	return n, nil
}

func (r *Runner) readShare(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time) *v1alpha1.PoolBenchCounters {
	if r.Shares == nil {
		r.log(pb, now, "Warning", "The node agents' metrics are off, so the share's own numbers are missing")
		return nil
	}
	ops, secs, err := r.Shares.Totals(ctx, runName(pb))
	if err != nil {
		r.log(pb, now, "Warning", "Cannot read the share's counters: "+err.Error())
		return nil
	}
	return &v1alpha1.PoolBenchCounters{Ops: int64(ops), Micros: int64(secs * 1e6), At: metav1.Time{Time: now}}
}

func (r *Runner) measureEnd(pb *v1alpha1.PoolBench) time.Time {
	st := &pb.Status
	d := time.Duration(pb.Spec.MeasureMinutes) * time.Minute
	if pb.Spec.Control.ExtendStep == st.Step {
		d += time.Duration(pb.Spec.Control.ExtendMinutes) * time.Minute
	}
	return st.StageStarted.Add(d)
}

func (r *Runner) startMeasuring(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time) {
	st := &pb.Status
	st.Stage, st.StageStarted = v1alpha1.PoolBenchStageMeasuring, &metav1.Time{Time: now}
	st.StageEnds = &metav1.Time{Time: r.measureEnd(pb)}
	st.ShareAtStart, st.ShareAtEnd = r.readShare(ctx, pb, now), nil
	st.Steps[len(st.Steps)-1].Started = &metav1.Time{Time: now}
	r.log(pb, now, "Info", fmt.Sprintf("Step %d: measuring %d VMs", st.Step, r.target(pb)))
	r.describe(pb, now)
}

func (r *Runner) measuring(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time) error {
	st := &pb.Status
	// The end moves later for an extension or a hold, never earlier.
	if end := r.measureEnd(pb); end.After(st.StageEnds.Time) {
		st.StageEnds = &metav1.Time{Time: end}
	}
	if pb.Spec.Control.Hold && now.After(st.StageEnds.Time) {
		st.StageEnds = &metav1.Time{Time: now}
	}
	if pb.Spec.Control.SkipStep == st.Step {
		r.log(pb, now, "Control", fmt.Sprintf("Step %d cut short", st.Step))
		st.ShareAtEnd = r.readShare(ctx, pb, now)
		return r.finishStep(pb, now, now, true)
	}
	if pb.Spec.Control.Hold || now.Before(st.StageEnds.Time) {
		r.describe(pb, now)
		return nil
	}
	if st.ShareAtEnd == nil {
		st.ShareAtEnd = r.readShare(ctx, pb, now)
	}
	if now.Before(st.StageEnds.Add(resultGrace)) {
		st.Message = fmt.Sprintf("Step %d of %d: waiting for the last results", st.Step, st.TotalSteps)
		return nil
	}
	return r.finishStep(pb, st.StageEnds.Time, now, false)
}

// finishStep measures the step that ran from StageStarted to end, then goes
// to the next step or ends the run.
func (r *Runner) finishStep(pb *v1alpha1.PoolBench, end, now time.Time, cutShort bool) error {
	st := &pb.Status
	profile, err := NewProfile(pb.Spec.Load)
	if err != nil {
		return err
	}
	n := r.target(pb)
	all := r.Store.Results(pb.Name)
	results := make(map[string][]Result, n)
	for i := int32(1); i <= n; i++ {
		results[vmName(i)] = all[vmName(i)]
	}
	rules := Rules{
		SteadySecs:     int64(pb.Spec.Load.PeriodSeconds - pb.Spec.Load.BurstSeconds),
		BurstSecs:      int64(pb.Spec.Load.BurstSeconds),
		LatencyLimitMs: float64(pb.Spec.Knee.LatencyLimitMs),
		StallMs:        float64(pb.Spec.Knee.StallSeconds) * 1000,
		TargetIOPS:     int64(pb.Spec.Load.SteadyIOPS),
	}
	s := rules.Step(results, st.StageStarted.Unix(), end.Unix())
	row := &st.Steps[len(st.Steps)-1]
	row.GuestIOPS, row.ThroughputKBps = s.GuestIOPS, s.KBps
	row.IBMOps = int64(math.Round(float64(s.GuestIOPS) * profile.IBMOpsPerIO()))
	row.P50Micros, row.P95Micros, row.P99Micros = int64(s.P50Ms*1000), int64(s.P95Ms*1000), int64(s.P99Ms*1000)
	row.SlowPercent, row.Stalls = s.SlowPercent, s.Stalls
	row.CutShort, row.Ended = cutShort, &metav1.Time{Time: end}
	if a, b := st.ShareAtStart, st.ShareAtEnd; a != nil && b != nil {
		ops := b.Ops - a.Ops
		if secs := b.At.Sub(a.At.Time).Seconds(); ops > 0 && secs > 0 {
			row.ShareIOPS = int64(math.Round(float64(ops) / secs))
			row.ShareLatencyMicros = (b.Micros - a.Micros) / ops
		}
	}
	limit := limitShare * float64(pb.Spec.IOPS)
	row.BurstSlowPercent = s.BurstSlowPercent
	row.AtLimit = float64(row.ShareIOPS) >= limit || float64(row.IBMOps) >= limit
	st.ShareAtStart, st.ShareAtEnd = nil, nil

	rows := make([]StepRow, 0, len(st.Steps))
	for _, done := range st.Steps {
		if done.Ended != nil {
			rows = append(rows, StepRow{VMs: done.VMs, SlowPercent: done.SlowPercent, BurstSlowPercent: done.BurstSlowPercent,
				Stalls: done.Stalls, AtLimit: done.AtLimit})
		}
	}
	row.Pass = !rows[len(rows)-1].Fails(pb.Spec.Knee.SlowVMPercent)
	st.KneeVMs = Knee(rows, pb.Spec.Knee.SlowVMPercent)
	st.RecommendedVMs = Recommended(rows, st.KneeVMs)
	verdict := "pass"
	if !row.Pass {
		verdict = "fail"
	}
	r.log(pb, now, "Info", fmt.Sprintf("Step %d: %d VMs, p95 %.1f ms, %d%% slow, share %d IOPS: %s",
		st.Step, row.VMs, s.P95Ms, s.SlowPercent, row.ShareIOPS, verdict))

	stopAtKnee := pb.Spec.Knee.StopAtKnee == nil || *pb.Spec.Knee.StopAtKnee
	if (st.KneeVMs > 0 && stopAtKnee) || st.Step >= st.TotalSteps {
		r.endRun(pb, now, false)
		return nil
	}
	st.Step++
	r.startCreating(pb, now)
	return nil
}

// cleanup deletes the run's namespace (with its VMs and disks), then its
// pool. done is true once both are gone.
func (r *Runner) cleanup(ctx context.Context, pb *v1alpha1.PoolBench, now time.Time) (done bool, err error) {
	if pb.Status.CleanedUp {
		return true, nil
	}
	ns := &corev1.Namespace{}
	err = r.Client.Get(ctx, client.ObjectKey{Name: runName(pb)}, ns)
	if err == nil && ns.DeletionTimestamp.IsZero() {
		r.log(pb, now, "Info", "Deleting the namespace with the test VMs and their disks")
		if derr := r.Client.Delete(ctx, ns); derr != nil && !apierrors.IsNotFound(derr) {
			return false, fmt.Errorf("delete namespace: %w", derr)
		}
		err = r.Client.Get(ctx, client.ObjectKey{Name: runName(pb)}, ns)
	}
	if err == nil {
		return false, nil // the namespace is still going
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	pool := &v1alpha1.FileSharePool{}
	err = r.Client.Get(ctx, client.ObjectKey{Name: runName(pb)}, pool)
	if err == nil && pool.DeletionTimestamp.IsZero() {
		r.log(pb, now, "Info", "Deleting the test pool and its share")
		if derr := r.Client.Delete(ctx, pool); derr != nil && !apierrors.IsNotFound(derr) {
			return false, fmt.Errorf("delete pool: %w", derr)
		}
		err = r.Client.Get(ctx, client.ObjectKey{Name: runName(pb)}, pool)
	}
	if err == nil {
		return false, nil
	}
	if !apierrors.IsNotFound(err) {
		return false, err
	}
	pb.Status.CleanedUp = true
	pb.Status.VMs, pb.Status.WindowsVMs, pb.Status.ReadyVMs = 0, 0, 0
	r.log(pb, now, "Info", "Cleaned up: the namespace and the pool are gone")
	return true, nil
}

// finalize cleans up a deleted run, forgets its results, and lets it go.
func (r *Runner) finalize(ctx context.Context, pb *v1alpha1.PoolBench) error {
	if !controllerutil.ContainsFinalizer(pb, Finalizer) {
		return nil
	}
	done, err := r.cleanup(ctx, pb, r.Now())
	if err != nil || !done {
		return err
	}
	if err := r.Store.Forget(pb.Name); err != nil {
		return err
	}
	controllerutil.RemoveFinalizer(pb, Finalizer)
	return r.Client.Update(ctx, pb)
}

// Token is the run's secret, for the collector.
func (r *Runner) Token(ctx context.Context, run string) (string, error) {
	sec := &corev1.Secret{}
	if err := r.Client.Get(ctx, client.ObjectKey{Namespace: "poolbench-" + run, Name: TokenSecret}, sec); err != nil {
		return "", err
	}
	if len(sec.Data["token"]) == 0 {
		return "", errors.New("the run has no token")
	}
	return string(sec.Data["token"]), nil
}

// Paused is true while the run is paused, for the collector.
func (r *Runner) Paused(ctx context.Context, run string) bool {
	pb := &v1alpha1.PoolBench{}
	if err := r.Client.Get(ctx, client.ObjectKey{Name: run}, pb); err != nil {
		return false
	}
	return pb.Status.Phase == v1alpha1.PoolBenchPaused
}

// VMByIP finds the test VM of the run whose pod has this IP address, for the collector.
func (r *Runner) VMByIP(ctx context.Context, run, ip string) (VM, bool) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmiGVK.GroupVersion().WithKind("VirtualMachineInstanceList"))
	if err := r.Client.List(ctx, list, client.InNamespace("poolbench-"+run), client.MatchingLabels{RunLabel: run}); err != nil {
		return VM{}, false
	}
	for _, vmi := range list.Items {
		ifaces, _, _ := unstructured.NestedSlice(vmi.Object, "status", "interfaces")
		for _, i := range ifaces {
			m, _ := i.(map[string]any)
			if m["ipAddress"] == ip {
				return VM{Name: vmi.GetName(), OS: vmi.GetLabels()[OSLabel]}, true
			}
			ips, _ := m["ipAddresses"].([]any)
			for _, a := range ips {
				if a == ip {
					return VM{Name: vmi.GetName(), OS: vmi.GetLabels()[OSLabel]}, true
				}
			}
		}
	}
	return VM{}, false
}
