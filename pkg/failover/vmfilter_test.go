package failover

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// twoVMShare is a share with two VMs (vm1 has two disks, vm2 has one) and one
// plain volume.
func twoVMShare(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	add := func(name, group string, steps ...step) {
		sv := vmDisk(t, root, name, group, steps...)
		writeTestMetadata(t, sv.LocalDir, &pool.SubVolumeMetadata{
			SubVolumeName: name,
			Spec: v1alpha1.SubVolumeSpec{
				PoolName: "prod", SubPath: "/pvcs/" + name, RequestedGB: 10,
				PVCName: "disk-" + name, PVCNamespace: "ns",
			},
			LastSyncTime:      steps[len(steps)-1].at,
			ReplicationPolicy: "prod-to-dr",
		})
	}
	add("pvc-a", "ns/vm1", step{m1, 1}, step{m2, 2})
	add("pvc-b", "ns/vm1", step{m1, 1}, step{m2, 2})
	add("pvc-c", "ns/vm2", step{m1, 1}, step{m2, 2})
	writeTestMetadata(t, root+"/pvcs/pvc-plain", &pool.SubVolumeMetadata{
		SubVolumeName: "pvc-plain",
		Spec: v1alpha1.SubVolumeSpec{
			PoolName: "prod", SubPath: "/pvcs/pvc-plain", RequestedGB: 5,
			PVCName: "data", PVCNamespace: "ns",
		},
		LastSyncTime:      m2,
		ReplicationPolicy: "prod-to-dr",
	})
	return root
}

func volumeNames(res *Result) []string {
	var out []string
	for _, v := range res.Volumes {
		out = append(out, v.SubVolume)
	}
	return out
}

func TestNormalizeVMs_SortsAndRemovesDuplicates(t *testing.T) {
	got := NormalizeVMs([]string{"b/x", "a/y", "b/x", ""})
	if !reflect.DeepEqual(got, []string{"a/y", "b/x"}) {
		t.Errorf("got %v", got)
	}
	if NormalizeVMs(nil) != nil || NormalizeVMs([]string{}) != nil {
		t.Error("an empty filter must stay empty (nil)")
	}
	if !SameVMs([]string{"a/y", "b/x"}, []string{"b/x", "a/y", "a/y"}) || SameVMs([]string{"a/y"}, nil) {
		t.Error("SameVMs compares as sets")
	}
}

func TestValidateVMName(t *testing.T) {
	for _, ok := range []string{"ns/vm", "my-ns/my-vm-1", "ns/vm.1"} {
		if err := ValidateVMName(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
	for _, bad := range []string{"", "vm", "ns/", "/vm", "a/b/c", "NS/vm", "ns/VM", "ns/v m", "ns,x/vm"} {
		if err := ValidateVMName(bad); err == nil {
			t.Errorf("%q: expected an error", bad)
		}
	}
}

func TestRunJob_PlanForOneVMListsOnlyThatVMAndItsDisks(t *testing.T) {
	root := twoVMShare(t)
	res, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, VMs: []string{"ns/vm1"}, Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	if got := volumeNames(res); !reflect.DeepEqual(got, []string{"pvc-a", "pvc-b"}) {
		t.Errorf("volumes = %v, want both disks of vm1 and nothing else (no plain volume)", got)
	}
	if len(res.VMs) != 1 || res.VMs[0].Name != "ns/vm1" || len(res.VMs[0].Disks) != 2 {
		t.Errorf("vms = %+v", res.VMs)
	}
}

func TestRunJob_PlanForASetOfVMs(t *testing.T) {
	root := twoVMShare(t)
	res, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, VMs: []string{"ns/vm2", "ns/vm1"}, Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.VMs) != 2 || len(res.Volumes) != 3 {
		t.Errorf("vms = %d, volumes = %v", len(res.VMs), volumeNames(res))
	}
}

func TestRunJob_PlanWithoutAFilterStillCoversEverything(t *testing.T) {
	root := twoVMShare(t)
	res, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.VMs) != 2 || len(res.Volumes) != 4 {
		t.Errorf("vms = %d, volumes = %v", len(res.VMs), volumeNames(res))
	}
}

func TestRunJob_UnknownVMInTheFilterIsRefusedWithTheVMsFound(t *testing.T) {
	root := twoVMShare(t)
	_, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, VMs: []string{"ns/vm1", "ns/nope"}, Now: m3})
	if err == nil || !strings.Contains(err.Error(), "ns/nope") || !strings.Contains(err.Error(), "ns/vm1") || !strings.Contains(err.Error(), "ns/vm2") {
		t.Fatalf("err = %v, want one that names the unknown VM and the VMs on the copy", err)
	}
}

func TestRunJob_ABusyOrBrokenOtherVMDoesNotBlockTheChosenOne(t *testing.T) {
	root := twoVMShare(t)
	// vm2 has a running commit; vm1 is chosen, so it must not matter.
	lease := filepath.Join(root, "pvcs", "pvc-c", ".repl-lease")
	if err := os.WriteFile(lease, []byte(m3.Add(time.Hour).Format(time.RFC3339Nano)), 0o660); err != nil {
		t.Fatal(err)
	}
	if _, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, Now: m3}); err == nil {
		t.Fatal("without a filter the busy VM must block")
	}
	res, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, VMs: []string{"ns/vm1"}, Now: m3})
	if err != nil {
		t.Fatalf("the busy VM is not chosen: %v", err)
	}
	if len(res.VMs) != 1 {
		t.Errorf("vms = %+v", res.VMs)
	}
	// And a busy chosen VM still blocks.
	if _, err = RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, VMs: []string{"ns/vm2"}, Now: m3}); err == nil || !strings.Contains(err.Error(), "pvc-c") {
		t.Errorf("err = %v, want a refusal that names the busy disk", err)
	}
}

func TestRunJob_RewindToAppliesOnlyToTheChosenVMs(t *testing.T) {
	root := twoVMShare(t)
	// vm2 only has a moment from m1 on; make vm3 that has nothing at or before the rewind point.
	vmDisk(t, root, "pvc-d", "ns/vm3", step{m3, 3})
	writeTestMetadata(t, root+"/pvcs/pvc-d", &pool.SubVolumeMetadata{
		SubVolumeName: "pvc-d",
		Spec:          v1alpha1.SubVolumeSpec{PoolName: "prod", SubPath: "/pvcs/pvc-d", RequestedGB: 1, PVCName: "d", PVCNamespace: "ns"},
		LastSyncTime:  m3,
	})
	at := m1.Add(time.Minute)
	if _, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, RewindTo: &at, Now: m3}); err == nil {
		t.Fatal("without a filter vm3 has no moment at or before the rewind point and must block")
	}
	res, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, RewindTo: &at, VMs: []string{"ns/vm1"}, Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	if !res.VMs[0].Moment.Time.Equal(m1) {
		t.Errorf("moment = %v", res.VMs[0].Moment.Time)
	}
}

func TestRunJob_ExecuteForOneVMRewindsOnlyThatVMAndChecksTheFilteredPlan(t *testing.T) {
	root := twoVMShare(t)
	at := m1.Add(time.Minute)
	vms := []string{"ns/vm1"}
	plan, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, RewindTo: &at, VMs: vms, Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	p := &v1alpha1.Failover{Status: v1alpha1.FailoverStatus{VMs: plan.VMs, Volumes: plan.Volumes}}
	res, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionExecute, RewindTo: &at, VMs: vms, Now: m3, Expect: ExpectationFromPlan(p)})
	if err != nil {
		t.Fatal(err)
	}
	if got := volumeNames(res); !reflect.DeepEqual(got, []string{"pvc-a", "pvc-b"}) {
		t.Errorf("volumes = %v", got)
	}
	entries, _ := os.ReadDir(root + "/pvcs")
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "pvc-c.") || strings.HasPrefix(e.Name(), "pvc-plain.") {
			t.Errorf("another VM was rewound: %s", e.Name())
		}
	}
	// An Execute with another filter than the Plan's finds a different set of volumes and refuses.
	_, err = RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionExecute, RewindTo: &at, VMs: []string{"ns/vm1", "ns/vm2"}, Now: m3, Expect: ExpectationFromPlan(p)})
	if err == nil || !strings.Contains(err.Error(), "ns/vm2") {
		t.Errorf("err = %v, want a refusal that names the VM that was not planned", err)
	}
}

func TestFindPlan_TheVMFilterMustMatchAsASet(t *testing.T) {
	now := m3
	p := planned("p", "recv", now.Add(-time.Minute), nil)
	p.Spec.VMs = []string{"ns/a", "ns/b"}
	exec := execFor("recv", nil)

	exec.Spec.VMs = []string{"ns/b", "ns/a"}
	if got, err := FindPlan([]v1alpha1.Failover{p}, exec, "", now); err != nil || got.Name != "p" {
		t.Fatalf("same set in another order: %v", err)
	}
	for name, vms := range map[string][]string{
		"fewer":    {"ns/a"},
		"more":     {"ns/a", "ns/b", "ns/c"},
		"none":     nil,
		"disjoint": {"ns/c"},
	} {
		exec.Spec.VMs = vms
		_, err := FindPlan([]v1alpha1.Failover{p}, exec, "", now)
		if err == nil || !strings.Contains(err.Error(), "spec.vms") {
			t.Errorf("%s: err = %v, want a refusal that names spec.vms", name, err)
		}
	}
	// A Plan for all VMs does not serve an Execute for one.
	all := planned("all", "recv", now.Add(-time.Minute), nil)
	exec.Spec.VMs = []string{"ns/a"}
	if _, err := FindPlan([]v1alpha1.Failover{all}, exec, "", now); err == nil {
		t.Error("a Plan with no filter must not serve a filtered Execute")
	}
	// And the other way round.
	exec.Spec.VMs = nil
	if _, err := FindPlan([]v1alpha1.Failover{p}, exec, "", now); err == nil {
		t.Error("a filtered Plan must not serve an Execute for all VMs")
	}
}

func TestController_VMFilterIsCarriedToTheJob(t *testing.T) {
	f := newFailover("p1", v1alpha1.FailoverActionPlan)
	f.Spec.VMs = []string{"ns/b", "ns/a"}
	ctl, c := newTestController(t, append(receiverClaim(), f)...)
	if err := ctl.processOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(jobFor(t, c, "p1").Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--vms=ns/a,ns/b") {
		t.Errorf("args %q lack the sorted filter", args)
	}
	f2 := newFailover("p2", v1alpha1.FailoverActionPlan)
	ctl, c = newTestController(t, append(receiverClaim(), f2)...)
	if err := ctl.processOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if args = strings.Join(jobFor(t, c, "p2").Spec.Template.Spec.Containers[0].Args, " "); strings.Contains(args, "--vms") {
		t.Errorf("args %q: no filter, no flag", args)
	}
}

func TestController_ExecuteForOneVMNeedsThePlanWithTheSameFilter(t *testing.T) {
	plan := freshPlan()
	plan.Spec.VMs = []string{"ns/vm"}
	e := newFailover("go", v1alpha1.FailoverActionExecute)
	e.Spec.DRPool = "dr"

	e.Spec.VMs = []string{"ns/other"}
	ctl, c := newTestController(t, append(receiverClaim(), e.DeepCopy(), plan.DeepCopy())...)
	if err := ctl.processOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f := getFailover(t, c, "go"); f.Status.Phase != v1alpha1.FailoverPhaseFailed || !strings.Contains(f.Status.Message, "spec.vms") {
		t.Errorf("status = %+v", f.Status)
	}

	e.Spec.VMs = []string{"ns/vm"}
	ctl, c = newTestController(t, append(receiverClaim(), e.DeepCopy(), plan.DeepCopy())...)
	if err := ctl.processOnce(context.Background()); err != nil {
		t.Fatal(err)
	}
	args := strings.Join(jobFor(t, c, "go").Spec.Template.Spec.Containers[0].Args, " ")
	if !strings.Contains(args, "--vms=ns/vm") || !strings.Contains(args, "--failover-expect=") {
		t.Errorf("args = %q", args)
	}
}

func TestAlreadyFailedOver_OnlyLooksAtTheVolumesOfThePlan(t *testing.T) {
	// A plan for vm2 only: vm1's earlier failover must not block it.
	plan := &v1alpha1.Failover{Status: v1alpha1.FailoverStatus{
		VMs:     []v1alpha1.FailoverVM{{Name: "ns/vm2", Moment: metav1.NewTime(m2), Disks: []v1alpha1.FailoverDisk{{SubVolume: "pvc-c"}}}},
		Volumes: []v1alpha1.FailoverVolume{{SubVolume: "pvc-c"}},
	}}
	done := &v1alpha1.SubVolume{ObjectMeta: metav1.ObjectMeta{Name: "pvc-a", Labels: map[string]string{FailoverSourceLabel: "p"}}}
	ctl, _ := newTestController(t, done)
	if err := AlreadyFailedOver(context.Background(), ctl.client, plan); err != nil {
		t.Errorf("err = %v", err)
	}
}

func TestParseVMs(t *testing.T) {
	got, err := ParseVMs(" ns/b, ns/a ,ns/b,")
	if err != nil || !reflect.DeepEqual(got, []string{"ns/a", "ns/b"}) {
		t.Errorf("got %v, %v", got, err)
	}
	if got, err = ParseVMs(""); err != nil || got != nil {
		t.Errorf("empty: %v, %v", got, err)
	}
	if _, err = ParseVMs("ns/a,bad"); err == nil {
		t.Error("expected an error for a name without a namespace")
	}
}
