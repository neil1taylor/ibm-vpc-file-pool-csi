package poolbench

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = rbacv1.AddToScheme(s)
	_ = discoveryv1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	for _, gvk := range []schema.GroupVersionKind{vmGVK, vmiGVK, dataSourceGVK, dataVolumeGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return s
}

func worker(name, zone string, memGiB int64) *corev1.Node {
	return &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{Name: name, Labels: map[string]string{
			"node-role.kubernetes.io/worker": "", "topology.kubernetes.io/zone": zone,
		}},
		Status: corev1.NodeStatus{Allocatable: corev1.ResourceList{
			corev1.ResourceMemory: *resource.NewQuantity(memGiB<<30, resource.BinarySI),
		}},
	}
}

func dataSource(name string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(dataSourceGVK)
	u.SetName(name)
	u.SetNamespace(imagesNamespace)
	return u
}

type fakeShares struct {
	mu    sync.Mutex
	reads []PoolBenchCountersReading
}

// PoolBenchCountersReading is one reading the fake hands out in turn.
type PoolBenchCountersReading struct{ ops, secs float64 }

func (f *fakeShares) Totals(_ context.Context, _ string) (float64, float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reads) == 0 {
		return 0, 0, fmt.Errorf("no reading")
	}
	r := f.reads[0]
	f.reads = f.reads[1:]
	return r.ops, r.secs, nil
}

type fakeStarter struct {
	mu      sync.Mutex
	started []string
}

func (f *fakeStarter) StartWindowsLoad(_ context.Context, ns, vm string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.started = append(f.started, ns+"/"+vm)
	return nil
}

type harness struct {
	t       *testing.T
	c       client.Client
	r       *Runner
	now     time.Time
	shares  *fakeShares
	starter *fakeStarter
}

func newHarness(t *testing.T, pb *v1alpha1.PoolBench, extra ...client.Object) *harness {
	t.Helper()
	objs := []client.Object{pb,
		worker("w1", "eu-de-2", 64), worker("w2", "eu-de-2", 64), worker("w3", "eu-de-1", 64),
		dataSource("centos-stream9"), dataSource("win2k22-perf"),
	}
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(append(objs, extra...)...).
		WithStatusSubresource(&v1alpha1.PoolBench{}, &v1alpha1.FileSharePool{}).Build()
	store, err := NewStore(t.TempDir(), NewMetrics(prometheus.NewRegistry()))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{t: t, c: c, now: time.Date(2026, 10, 25, 10, 0, 0, 0, time.UTC), shares: &fakeShares{}, starter: &fakeStarter{}}
	h.r = &Runner{Client: c, Store: store, CollectorURL: "http://pb.example:8080", Shares: h.shares, Starter: h.starter,
		RunRole: "pb-run", ServiceAccount: "pb-sa", ServiceNamespace: "kube-system",
		Now: func() time.Time { return h.now }}
	return h
}

func (h *harness) pass() {
	h.t.Helper()
	h.r.Pass(context.Background())
}

func (h *harness) bench() *v1alpha1.PoolBench {
	h.t.Helper()
	pb := &v1alpha1.PoolBench{}
	if err := h.c.Get(context.Background(), client.ObjectKey{Name: "t10k"}, pb); err != nil {
		h.t.Fatal(err)
	}
	return pb
}

func (h *harness) update(mutate func(*v1alpha1.PoolBench)) {
	h.t.Helper()
	pb := h.bench()
	mutate(pb)
	if err := h.c.Update(context.Background(), pb); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) poolReady() {
	h.t.Helper()
	p := &v1alpha1.FileSharePool{}
	if err := h.c.Get(context.Background(), client.ObjectKey{Name: "poolbench-t10k"}, p); err != nil {
		h.t.Fatal(err)
	}
	p.Status.Phase = "Ready"
	if err := h.c.Status().Update(context.Background(), p); err != nil {
		h.t.Fatal(err)
	}
}

// ready makes VMs from..to say PERF-READY.
func (h *harness) ready(from, to int32) {
	for n := from; n <= to; n++ {
		_ = h.r.Store.Add("t10k", VM{Name: vmName(n), OS: "linux"}, "PERF-READY", h.now)
	}
}

// results sends one steady and one burst line from VMs 1..to, ending at the
// current time, with the given steady p95.
func (h *harness) results(to int32, p95ns int64) {
	for n := int32(1); n <= to; n++ {
		vm := VM{Name: vmName(n), OS: "linux"}
		_ = h.r.Store.Add("t10k", vm, "PERF "+itoa(h.now.Unix()-45)+" steady "+fioJSON("lat_ns", 20, 9, 1000000, p95ns, p95ns, 12000000, 2000000, 3000000), h.now)
		_ = h.r.Store.Add("t10k", vm, "PERF "+itoa(h.now.Unix())+" burst "+fioJSON("lat_ns", 200, 90, 1000000, 4000000, 9000000, 12000000, 2000000, 3000000), h.now)
	}
}

func (h *harness) advance(d time.Duration) { h.now = h.now.Add(d) }

func (h *harness) expect(phase, stage string) *v1alpha1.PoolBench {
	h.t.Helper()
	pb := h.bench()
	if pb.Status.Phase != phase || pb.Status.Stage != stage {
		h.t.Fatalf("want %s/%s, got %s/%s: %s", phase, stage, pb.Status.Phase, pb.Status.Stage, pb.Status.Message)
	}
	return pb
}

func (h *harness) count(list client.ObjectList) int {
	h.t.Helper()
	if err := h.c.List(context.Background(), list, client.InNamespace("poolbench-t10k")); err != nil {
		h.t.Fatal(err)
	}
	items, _ := metaItems(list)
	return items
}

func metaItems(list client.ObjectList) (int, error) {
	switch l := list.(type) {
	case *unstructured.UnstructuredList:
		return len(l.Items), nil
	case *corev1.PersistentVolumeClaimList:
		return len(l.Items), nil
	}
	return 0, fmt.Errorf("unknown list")
}

func vmList() *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(vmGVK.GroupVersion().WithKind("VirtualMachineList"))
	return l
}

// startToMeasuring takes a new run to the measuring of step 1.
func (h *harness) startToMeasuring() {
	h.t.Helper()
	h.pass()
	h.expect(v1alpha1.PoolBenchChecking, "")
	h.pass()
	h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStagePreparing)
	h.poolReady()
	h.pass()
	h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageCreating)
	h.pass() // makes the VMs and starts the Windows load
	h.ready(1, 3)
	h.advance(5 * time.Minute)
	h.pass()
	h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageSettling)
	h.advance(15 * time.Minute)
	h.shares.reads = append(h.shares.reads, PoolBenchCountersReading{ops: 1000, secs: 1})
	h.pass()
	h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageMeasuring)
}

func TestRunner_FullRun(t *testing.T) {
	h := newHarness(t, testBench())
	h.startToMeasuring()
	pb := h.bench()
	if pb.Status.Namespace != "poolbench-t10k" || pb.Status.PoolName != "poolbench-t10k" || pb.Status.TotalSteps != 2 || pb.Status.Step != 1 {
		t.Fatalf("status: %+v", pb.Status)
	}
	if !strings.Contains(strings.Join(pb.Finalizers, ","), Finalizer) {
		t.Fatal("no finalizer")
	}
	if n := h.count(vmList()); n != 3 {
		t.Fatalf("want 3 VMs at step 1, got %d", n)
	}
	if n := h.count(&corev1.PersistentVolumeClaimList{}); n != 3 {
		t.Fatalf("want 3 load disks, got %d", n)
	}
	if len(pb.Status.Steps) != 1 || pb.Status.Steps[0].VMs != 3 || pb.Status.Steps[0].WindowsVMs != 1 || pb.Status.Steps[0].ReadySeconds != 300 {
		t.Fatalf("step 1 row: %+v", pb.Status.Steps)
	}
	if pb.Status.VMs != 3 || pb.Status.WindowsVMs != 1 || pb.Status.ReadyVMs != 3 {
		t.Fatalf("VM counts: %+v", pb.Status)
	}

	// The measuring ends 45 minutes later, plus a short wait for the last lines.
	h.advance(45 * time.Minute)
	h.results(3, 4000000)
	h.shares.reads = append(h.shares.reads, PoolBenchCountersReading{ops: 1000 + 2700*60*45, secs: 1 + 2700*60*45*0.0004})
	h.pass()
	h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageMeasuring)
	h.advance(2 * time.Minute)
	h.pass()
	pb = h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageCreating)
	row := pb.Status.Steps[0]
	if !row.Pass || row.SlowPercent != 0 || row.ShareIOPS != 2700 || row.ShareLatencyMicros != 400 || row.GuestIOPS == 0 || row.IBMOps == 0 {
		t.Fatalf("step 1 result: %+v", row)
	}
	if pb.Status.Step != 2 {
		t.Fatalf("want step 2, got %d", pb.Status.Step)
	}
	h.pass()
	if n := h.count(vmList()); n != 6 {
		t.Fatalf("want 6 VMs at step 2, got %d", n)
	}

	h.ready(4, 6)
	h.pass()
	h.advance(15 * time.Minute)
	h.shares.reads = append(h.shares.reads, PoolBenchCountersReading{ops: 0, secs: 0})
	h.pass()
	h.advance(45 * time.Minute)
	h.results(6, 4000000)
	h.advance(2 * time.Minute)
	h.shares.reads = append(h.shares.reads, PoolBenchCountersReading{ops: 0, secs: 0})
	h.pass()
	h.expect(v1alpha1.PoolBenchCleaningUp, "")
	h.pass()
	pb = h.expect(v1alpha1.PoolBenchComplete, "")
	if !pb.Status.CleanedUp || pb.Status.EndTime == nil || pb.Status.VMs != 0 || pb.Status.ReadyVMs != 0 {
		t.Fatalf("cleanup: %+v", pb.Status)
	}
	if pb.Status.KneeVMs != 0 || pb.Status.RecommendedVMs != 6 {
		t.Fatalf("knee %d recommended %d", pb.Status.KneeVMs, pb.Status.RecommendedVMs)
	}
	ns := &corev1.Namespace{}
	if err := h.c.Get(context.Background(), client.ObjectKey{Name: "poolbench-t10k"}, ns); err == nil {
		t.Fatal("the namespace is still there")
	}
	if err := h.c.Get(context.Background(), client.ObjectKey{Name: "poolbench-t10k"}, &v1alpha1.FileSharePool{}); err == nil {
		t.Fatal("the pool is still there")
	}
	if len(h.starter.started) == 0 || h.starter.started[0] != "poolbench-t10k/vm-003" {
		t.Fatalf("the Windows load of vm-003 was not started: %v", h.starter.started)
	}
}

func TestRunner_MadeObjects(t *testing.T) {
	h := newHarness(t, testBench())
	h.pass()
	h.pass()
	ctx := context.Background()
	ns := &corev1.Namespace{}
	if err := h.c.Get(ctx, client.ObjectKey{Name: "poolbench-t10k"}, ns); err != nil || ns.Labels[RunLabel] != "t10k" {
		t.Fatalf("namespace: %v %v", err, ns.Labels)
	}
	tok := &corev1.Secret{}
	if err := h.c.Get(ctx, client.ObjectKey{Namespace: "poolbench-t10k", Name: TokenSecret}, tok); err != nil || len(tok.Data["token"]) < 32 {
		t.Fatalf("token: %v", err)
	}
	ud := &corev1.Secret{}
	if err := h.c.Get(ctx, client.ObjectKey{Namespace: "poolbench-t10k", Name: UserDataSecret}, ud); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(ud.Data["userdata"]), "http://pb.example:8080/v1/runs/t10k/results") ||
		!strings.Contains(string(ud.Data["userdata"]), string(tok.Data["token"])) {
		t.Fatalf("user data does not post to the collector with the token")
	}
	win := &corev1.Secret{}
	if err := h.c.Get(ctx, client.ObjectKey{Namespace: "poolbench-t10k", Name: WindowsSecret}, win); err != nil || len(win.Data["load.ps1"]) == 0 {
		t.Fatalf("windows config: %v", err)
	}
	pool := &v1alpha1.FileSharePool{}
	if err := h.c.Get(ctx, client.ObjectKey{Name: "poolbench-t10k"}, pool); err != nil || pool.Spec.Zone != "eu-de-2" {
		t.Fatalf("pool: %v %+v", err, pool.Spec)
	}
	rb := &rbacv1.RoleBinding{}
	if err := h.c.Get(ctx, client.ObjectKey{Namespace: "poolbench-t10k", Name: "poolbench"}, rb); err != nil {
		t.Fatalf("the service's rights in the run namespace: %v", err)
	}
	if rb.RoleRef.Kind != "ClusterRole" || rb.RoleRef.Name != "pb-run" || len(rb.Subjects) != 1 ||
		rb.Subjects[0].Name != "pb-sa" || rb.Subjects[0].Namespace != "kube-system" {
		t.Fatalf("role binding: %+v", rb)
	}
	// The collector can now check the run's token and find its VMs.
	got, err := h.r.Token(ctx, "t10k")
	if err != nil || got != string(tok.Data["token"]) {
		t.Fatalf("Token: %q %v", got, err)
	}
}

func TestRunner_NoWindowsSecretForLinuxOnly(t *testing.T) {
	pb := testBench()
	pb.Spec.WindowsEvery = int32p(0)
	h := newHarness(t, pb)
	h.pass()
	h.pass()
	if err := h.c.Get(context.Background(), client.ObjectKey{Namespace: "poolbench-t10k", Name: WindowsSecret}, &corev1.Secret{}); err == nil {
		t.Fatal("a Linux-only run needs no Windows config")
	}
}

func TestRunner_KneeStopsTheRun(t *testing.T) {
	h := newHarness(t, testBench())
	h.startToMeasuring()
	h.advance(45 * time.Minute)
	h.results(3, 40000000) // 40 ms p95 on every VM
	h.advance(2 * time.Minute)
	h.shares.reads = append(h.shares.reads, PoolBenchCountersReading{})
	h.pass()
	pb := h.expect(v1alpha1.PoolBenchCleaningUp, "")
	if pb.Status.KneeVMs != 3 || pb.Status.RecommendedVMs != 0 || pb.Status.Steps[0].Pass {
		t.Fatalf("knee %d recommended %d steps %+v", pb.Status.KneeVMs, pb.Status.RecommendedVMs, pb.Status.Steps)
	}
	h.pass()
	h.expect(v1alpha1.PoolBenchComplete, "")
}

func TestRunner_KneeWithoutStop(t *testing.T) {
	pb := testBench()
	no := false
	pb.Spec.Knee.StopAtKnee = &no
	h := newHarness(t, pb)
	h.startToMeasuring()
	h.advance(45 * time.Minute)
	h.results(3, 40000000)
	h.advance(2 * time.Minute)
	h.shares.reads = append(h.shares.reads, PoolBenchCountersReading{})
	h.pass()
	h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageCreating)
}

func TestRunner_Stop(t *testing.T) {
	h := newHarness(t, testBench())
	h.startToMeasuring()
	h.update(func(pb *v1alpha1.PoolBench) { pb.Spec.Control.Stop = true })
	h.pass()
	h.expect(v1alpha1.PoolBenchCleaningUp, "")
	h.pass()
	pb := h.expect(v1alpha1.PoolBenchStopped, "")
	if !pb.Status.CleanedUp || !hasLog(pb, "Control", "stop") {
		t.Fatalf("status: %+v", pb.Status)
	}
}

func hasLog(pb *v1alpha1.PoolBench, kind, text string) bool {
	for _, l := range pb.Status.Log {
		if l.Kind == kind && strings.Contains(strings.ToLower(l.Message), text) {
			return true
		}
	}
	return false
}

func TestRunner_SkipStep(t *testing.T) {
	h := newHarness(t, testBench())
	h.startToMeasuring()
	h.advance(10 * time.Minute)
	h.update(func(pb *v1alpha1.PoolBench) { pb.Spec.Control.SkipStep = 1 })
	h.results(3, 4000000)
	h.shares.reads = append(h.shares.reads, PoolBenchCountersReading{})
	h.pass()
	pb := h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageCreating)
	if !pb.Status.Steps[0].CutShort {
		t.Fatalf("step 1 should be cut short: %+v", pb.Status.Steps[0])
	}
}

func TestRunner_HoldAndExtend(t *testing.T) {
	h := newHarness(t, testBench())
	h.startToMeasuring()
	h.update(func(pb *v1alpha1.PoolBench) { pb.Spec.Control.ExtendStep, pb.Spec.Control.ExtendMinutes = 1, 30 })
	h.pass()
	if ends := h.bench().Status.StageEnds; !ends.Time.Equal(h.now.Add(75 * time.Minute)) {
		t.Fatalf("stage ends %v, want 75 minutes after the start", ends)
	}
	h.update(func(pb *v1alpha1.PoolBench) { pb.Spec.Control.Hold = true })
	h.advance(3 * time.Hour)
	h.pass()
	h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageMeasuring)
	h.update(func(pb *v1alpha1.PoolBench) { pb.Spec.Control.Hold = false })
	h.results(3, 4000000)
	h.shares.reads = append(h.shares.reads, PoolBenchCountersReading{})
	h.pass()
	h.advance(2 * time.Minute)
	h.pass()
	h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageCreating)
}

func TestRunner_PauseMovesTheEnd(t *testing.T) {
	h := newHarness(t, testBench())
	h.startToMeasuring()
	ends := h.bench().Status.StageEnds.Time
	h.update(func(pb *v1alpha1.PoolBench) { pb.Spec.Control.Paused = true })
	h.pass()
	h.expect(v1alpha1.PoolBenchPaused, v1alpha1.PoolBenchStageMeasuring)
	h.advance(20 * time.Minute)
	h.pass()
	h.update(func(pb *v1alpha1.PoolBench) { pb.Spec.Control.Paused = false })
	h.pass()
	pb := h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageMeasuring)
	if !pb.Status.StageEnds.Time.Equal(ends.Add(20 * time.Minute)) {
		t.Fatalf("stage ends %v, want %v", pb.Status.StageEnds.Time, ends.Add(20*time.Minute))
	}
}

func TestRunner_VMsNeverReady(t *testing.T) {
	h := newHarness(t, testBench())
	h.pass()
	h.pass()
	h.poolReady()
	h.pass()
	h.ready(1, 2)
	h.advance(61 * time.Minute)
	h.pass()
	pb := h.bench()
	if pb.Status.Phase != v1alpha1.PoolBenchFailed || !strings.Contains(pb.Status.Message, "vm-003") {
		t.Fatalf("want Failed naming vm-003, got %s: %s", pb.Status.Phase, pb.Status.Message)
	}
	h.pass()
	pb = h.bench()
	if pb.Status.Phase != v1alpha1.PoolBenchFailed || !pb.Status.CleanedUp {
		t.Fatalf("a failed run must still clean up: %+v", pb.Status)
	}
}

func TestRunner_StuckVMStateIsLogged(t *testing.T) {
	vmi := &unstructured.Unstructured{Object: map[string]interface{}{
		"status": map[string]interface{}{
			"phase":    "Running",
			"nodeName": "worker-7",
			"conditions": []interface{}{
				map[string]interface{}{"type": "AgentConnected", "status": "False"},
				map[string]interface{}{"type": "Ready", "status": "True"},
			},
		},
	}}
	vmi.SetGroupVersionKind(vmiGVK)
	vmi.SetName("vm-003")
	vmi.SetNamespace("poolbench-t10k")
	h := newHarness(t, testBench(), vmi)
	h.pass()
	h.pass()
	h.poolReady()
	h.pass()
	h.ready(1, 2)
	h.advance(61 * time.Minute)
	h.pass()
	var found string
	for _, e := range h.bench().Status.Log {
		if strings.Contains(e.Message, "vm-003") && strings.Contains(e.Message, "worker-7") {
			found = e.Message
		}
	}
	for _, want := range []string{"Running", "worker-7", "guest agent not connected"} {
		if !strings.Contains(found, want) {
			t.Fatalf("the log must say %q about the stuck VM, got %q", want, found)
		}
	}
}

func TestRunner_StuckVMWithoutInstance(t *testing.T) {
	h := newHarness(t, testBench())
	h.pass()
	h.pass()
	h.poolReady()
	h.pass()
	h.ready(1, 2)
	h.advance(61 * time.Minute)
	h.pass()
	for _, e := range h.bench().Status.Log {
		if strings.Contains(e.Message, "vm-003") && strings.Contains(e.Message, "no running instance") {
			return
		}
	}
	t.Fatal("the log must say that vm-003 has no running instance")
}

func TestRunner_SummaryExplainsTheRecommendation(t *testing.T) {
	h := newHarness(t, testBench())
	pb := testBench()
	pb.Status.Steps = []v1alpha1.PoolBenchStepResult{{VMs: 15}, {VMs: 30}, {VMs: 45}}
	pb.Status.KneeVMs, pb.Status.RecommendedVMs = 45, 30
	want := "Knee at 45 VMs: the first step where users would notice. " +
		"Recommended: 30 VMs, the largest step at or below 80% of the knee, to leave room to spare."
	if got := h.r.summary(pb); got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
}

// toEndOfMeasuring runs step 1 of an event test up to the pass that ends its
// measuring, leaving the run in the Event stage if an event is due.
func (h *harness) toEndOfMeasuring() {
	h.t.Helper()
	h.startToMeasuring()
	h.advance(45 * time.Minute)
	h.results(3, 4000000)
	h.shares.reads = append(h.shares.reads, PoolBenchCountersReading{ops: 1000 + 2700*60*45, secs: 1 + 2700*60*45*0.0004})
	h.pass()
	h.advance(2 * time.Minute)
	h.pass()
}

func benchWithEvent(typ string, atVMs int32) *v1alpha1.PoolBench {
	pb := testBench()
	pb.Spec.Events = []v1alpha1.PoolBenchEvent{{Type: typ, AtVMs: atVMs}}
	return pb
}

func instances(names ...string) []client.Object {
	var out []client.Object
	for _, n := range names {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(vmiGVK)
		u.SetName(n)
		u.SetNamespace("poolbench-t10k")
		out = append(out, u)
	}
	return out
}

func TestRunner_BootStorm(t *testing.T) {
	h := newHarness(t, benchWithEvent(v1alpha1.PoolBenchEventBootStorm, 3), instances("vm-001", "vm-002", "vm-003")...)
	h.toEndOfMeasuring()
	pb := h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageEvent)
	if pb.Status.Step != 1 || len(pb.Status.Events) != 1 || pb.Status.Events[0].Type != v1alpha1.PoolBenchEventBootStorm ||
		pb.Status.Events[0].AtVMs != 3 || pb.Status.Events[0].Started == nil || pb.Status.Events[0].Ended != nil {
		t.Fatalf("event start: %+v", pb.Status)
	}
	if !strings.Contains(pb.Status.Message, "boot storm") {
		t.Fatalf("the message must name the event: %q", pb.Status.Message)
	}
	if n := h.count(instancesList()); n != 0 {
		t.Fatalf("the boot storm must restart every VM, %d instances are left", n)
	}

	// Ten minutes later one VM has not sent results again: the event waits,
	// and the Windows VM is given its load again.
	before := len(h.starter.started)
	h.advance(10 * time.Minute)
	_ = h.r.Store.Add("t10k", VM{Name: "vm-001", OS: "linux"}, "PERF "+itoa(h.now.Unix())+" steady "+fioJSON("lat_ns", 20, 9, 1000000, 4000000, 4000000, 12000000, 2000000, 3000000), h.now)
	h.pass()
	h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageEvent)
	if len(h.starter.started) <= before {
		t.Fatal("the Windows VM was not given its load again")
	}

	h.advance(2 * time.Minute)
	h.results(3, 4000000)
	h.pass()
	pb = h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageCreating)
	ev := pb.Status.Events[0]
	if ev.Ended == nil || ev.Seconds != 720 || !strings.Contains(ev.Message, "3 VMs") || pb.Status.Step != 2 {
		t.Fatalf("event result: %+v step %d", ev, pb.Status.Step)
	}
}

func TestRunner_BootStormGivesUp(t *testing.T) {
	h := newHarness(t, benchWithEvent(v1alpha1.PoolBenchEventBootStorm, 3), instances("vm-001", "vm-002", "vm-003")...)
	h.toEndOfMeasuring()
	h.advance(31 * time.Minute)
	h.pass()
	pb := h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageCreating)
	ev := pb.Status.Events[0]
	if ev.Ended == nil || !strings.Contains(ev.Message, "not all") || pb.Status.Step != 2 {
		t.Fatalf("a boot storm that never ends must say so and let the run go on: %+v", ev)
	}
}

func TestRunner_EventOnlyAtItsStep(t *testing.T) {
	h := newHarness(t, benchWithEvent(v1alpha1.PoolBenchEventBootStorm, 6), instances("vm-001")...)
	h.toEndOfMeasuring()
	h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageCreating)
	if len(h.bench().Status.Events) != 0 {
		t.Fatal("the event is for step 2")
	}
}

func instancesList() *unstructured.UnstructuredList {
	l := &unstructured.UnstructuredList{}
	l.SetGroupVersionKind(vmiGVK.GroupVersion().WithKind("VirtualMachineInstanceList"))
	return l
}

func TestRunner_MissingImage(t *testing.T) {
	h := newHarness(t, testBench())
	if err := h.c.Delete(context.Background(), dataSource("win2k22-perf")); err != nil {
		t.Fatal(err)
	}
	h.pass()
	h.pass()
	pb := h.bench()
	if pb.Status.Phase != v1alpha1.PoolBenchFailed || !strings.Contains(pb.Status.Message, "win2k22-perf") {
		t.Fatalf("got %s: %s", pb.Status.Phase, pb.Status.Message)
	}
	if err := h.c.Get(context.Background(), client.ObjectKey{Name: "poolbench-t10k"}, &corev1.Namespace{}); err == nil {
		t.Fatal("a run that failed its checks must make nothing")
	}
}

func TestRunner_NotEnoughRoomWithOneNodeDown(t *testing.T) {
	pb := testBench()
	pb.Spec.StepVMs, pb.Spec.MaxVMs = 30, 60 // 60 VMs need about 170 GiB; 2 of 3 nodes have 128
	h := newHarness(t, pb)
	h.pass()
	h.pass()
	got := h.bench()
	if got.Status.Phase != v1alpha1.PoolBenchFailed || !strings.Contains(got.Status.Message, "one node down") {
		t.Fatalf("got %s: %s", got.Status.Phase, got.Status.Message)
	}
}

func TestRunner_KeepAfterEnd(t *testing.T) {
	pb := testBench()
	pb.Spec.KeepAfterEnd = true
	pb.Spec.MaxVMs = 3
	h := newHarness(t, pb)
	h.startToMeasuring()
	h.advance(45 * time.Minute)
	h.results(3, 4000000)
	h.advance(2 * time.Minute)
	h.shares.reads = append(h.shares.reads, PoolBenchCountersReading{})
	h.pass()
	h.pass()
	got := h.expect(v1alpha1.PoolBenchComplete, "")
	if got.Status.CleanedUp {
		t.Fatal("keepAfterEnd must keep the run's objects")
	}
	h.update(func(pb *v1alpha1.PoolBench) { pb.Spec.Control.Stop = true })
	h.pass()
	h.pass()
	if got := h.bench(); !got.Status.CleanedUp || got.Status.Phase != v1alpha1.PoolBenchComplete {
		t.Fatalf("a stop after the end cleans up and keeps the result: %+v", got.Status)
	}
}

func TestRunner_DeleteCleansUpAndForgets(t *testing.T) {
	h := newHarness(t, testBench())
	h.startToMeasuring()
	h.results(3, 4000000)
	if err := h.c.Delete(context.Background(), h.bench()); err != nil {
		t.Fatal(err)
	}
	h.pass()
	h.pass()
	if err := h.c.Get(context.Background(), client.ObjectKey{Name: "t10k"}, &v1alpha1.PoolBench{}); err == nil {
		t.Fatal("the run is still there after its cleanup")
	}
	if len(h.r.Store.Results("t10k")) != 0 {
		t.Fatal("the store still holds the run's results")
	}
	if err := h.c.Get(context.Background(), client.ObjectKey{Name: "poolbench-t10k"}, &v1alpha1.FileSharePool{}); err == nil {
		t.Fatal("the pool is still there")
	}
}

// TestRunner_Restart checks that a new runner (after a restart of the
// service) goes on from the stage in the status.
func TestRunner_Restart(t *testing.T) {
	h := newHarness(t, testBench())
	h.startToMeasuring()
	h.r = &Runner{Client: h.c, Store: h.r.Store, CollectorURL: h.r.CollectorURL, Shares: h.shares, Starter: h.starter,
		Now: func() time.Time { return h.now }}
	h.advance(45 * time.Minute)
	h.results(3, 4000000)
	h.advance(2 * time.Minute)
	h.shares.reads = append(h.shares.reads, PoolBenchCountersReading{ops: 1000 + 100*60*45, secs: 1})
	h.pass()
	pb := h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageCreating)
	// 270,000 operations from the start reading to the end reading 47 minutes later.
	if pb.Status.Steps[0].ShareIOPS != 96 {
		t.Fatalf("the share counters of the start were lost: %+v", pb.Status.Steps[0])
	}
}

func TestRunner_VMByIP(t *testing.T) {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(vmiGVK)
	vmi.SetName("vm-003")
	vmi.SetNamespace("poolbench-t10k")
	vmi.SetLabels(map[string]string{RunLabel: "t10k", OSLabel: "windows"})
	_ = unstructured.SetNestedSlice(vmi.Object, []any{map[string]any{"ipAddress": "10.128.0.6", "ipAddresses": []any{"10.128.0.6"}}}, "status", "interfaces")
	h := newHarness(t, testBench(), vmi)
	vm, ok := h.r.VMByIP(context.Background(), "t10k", "10.128.0.6")
	if !ok || vm.Name != "vm-003" || vm.OS != "windows" {
		t.Fatalf("got %+v %v", vm, ok)
	}
	if _, ok := h.r.VMByIP(context.Background(), "t10k", "10.128.0.7"); ok {
		t.Fatal("an unknown IP must not match")
	}
	if _, ok := h.r.VMByIP(context.Background(), "other", "10.128.0.6"); ok {
		t.Fatal("a VM of another run must not match")
	}
}

func TestRunner_PausedForTheVMs(t *testing.T) {
	h := newHarness(t, testBench())
	h.startToMeasuring()
	if h.r.Paused(context.Background(), "t10k") {
		t.Fatal("a running run is not paused")
	}
	h.update(func(pb *v1alpha1.PoolBench) { pb.Spec.Control.Paused = true })
	h.pass()
	if !h.r.Paused(context.Background(), "t10k") {
		t.Fatal("the VMs must see the pause")
	}
}

// TestRunner_PauseWhileCreating checks that a pause does not count against
// the time the VMs have to become ready.
func TestRunner_PauseWhileCreating(t *testing.T) {
	h := newHarness(t, testBench())
	h.pass()
	h.pass()
	h.poolReady()
	h.pass()
	h.update(func(pb *v1alpha1.PoolBench) { pb.Spec.Control.Paused = true })
	h.pass()
	h.advance(2 * time.Hour)
	h.pass()
	h.update(func(pb *v1alpha1.PoolBench) { pb.Spec.Control.Paused = false })
	h.pass()
	h.expect(v1alpha1.PoolBenchRunning, v1alpha1.PoolBenchStageCreating)
}

// TestRunner_PublishesTheVMCount checks the run gauges that the console's
// "VMs running" graph reads.
func TestRunner_PublishesTheVMCount(t *testing.T) {
	h := newHarness(t, testBench())
	h.startToMeasuring()
	m := h.r.Store.metrics
	if got := testutil.ToFloat64(m.runVMs.WithLabelValues("t10k", "linux")); got != 2 {
		t.Fatalf("linux VMs: got %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.runVMs.WithLabelValues("t10k", "windows")); got != 1 {
		t.Fatalf("windows VMs: got %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.runReady.WithLabelValues("t10k")); got != 3 {
		t.Fatalf("ready VMs: got %v, want 3", got)
	}
	if got := testutil.ToFloat64(m.runStep.WithLabelValues("t10k")); got != 1 {
		t.Fatalf("step: got %v, want 1", got)
	}
}

// TestRunner_StartAfter checks that a run waits for the run it names to end,
// so the three tiers of a series do not load the cluster at the same time.
func TestRunner_StartAfter(t *testing.T) {
	first := testBench()
	first.Name = "t3000"
	first.Status.Phase = v1alpha1.PoolBenchRunning
	second := testBench()
	second.Spec.StartAfter = "t3000"
	h := newHarness(t, second, first)
	h.pass()
	got := h.bench()
	if got.Status.Phase != v1alpha1.PoolBenchPending || !strings.Contains(got.Status.Message, "t3000") {
		t.Fatalf("want Pending, waiting for t3000; got %s: %s", got.Status.Phase, got.Status.Message)
	}
	f := &v1alpha1.PoolBench{}
	if err := h.c.Get(context.Background(), client.ObjectKey{Name: "t3000"}, f); err != nil {
		t.Fatal(err)
	}
	f.Status.Phase = v1alpha1.PoolBenchComplete
	f.Status.CleanedUp = true
	if err := h.c.Status().Update(context.Background(), f); err != nil {
		t.Fatal(err)
	}
	h.pass()
	if got := h.bench(); got.Status.Phase != v1alpha1.PoolBenchChecking {
		t.Fatalf("want Checking after t3000 ended, got %s", got.Status.Phase)
	}
}

func TestRunner_StartAfterAGoneRun(t *testing.T) {
	pb := testBench()
	pb.Spec.StartAfter = "deleted-run"
	h := newHarness(t, pb)
	h.pass()
	if got := h.bench(); got.Status.Phase != v1alpha1.PoolBenchChecking {
		t.Fatalf("a run that is gone does not hold this one: got %s", got.Status.Phase)
	}
}

func TestRunner_StepErrorShowsInStatus(t *testing.T) {
	h := newHarness(t, testBench())
	watch, ok := h.c.(client.WithWatch)
	if !ok {
		t.Fatal("the fake client can watch")
	}
	h.r.Client = interceptor.NewClient(watch, interceptor.Funcs{
		Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
			if _, ok := obj.(*v1alpha1.FileSharePool); ok {
				return errors.New("pool refused")
			}
			return c.Create(ctx, obj, opts...)
		},
	})
	h.pass()
	h.pass()
	h.pass()
	pb := h.bench()
	if !strings.Contains(pb.Status.Message, "pool refused") {
		t.Fatalf("the owner must see why the run waits: %q", pb.Status.Message)
	}
	n := 0
	for _, e := range pb.Status.Log {
		if strings.Contains(e.Message, "pool refused") {
			n++
		}
	}
	if n != 1 {
		t.Errorf("the same error is logged once, got %d", n)
	}
}
