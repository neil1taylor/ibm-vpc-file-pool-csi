package failover

import (
	"math/rand"
	"os"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakeShare is a destination share with one VM of two disks (both show m2 and
// can show m1) and one plain volume. It returns the mount path.
func fakeShare(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"pvc-a", "pvc-b"} {
		sv := vmDisk(t, root, name, "ns/vm", step{m1, 1}, step{m2, 2})
		writeTestMetadata(t, sv.LocalDir, &pool.SubVolumeMetadata{
			SubVolumeName: name,
			Spec: v1alpha1.SubVolumeSpec{
				PoolName: "prod", SubPath: "/pvcs/" + name, RequestedGB: 10,
				PVCName: "disk-" + name, PVCNamespace: "ns",
			},
			LastSyncTime:      m2,
			ReplicationPolicy: "prod-to-dr",
		})
	}
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

func TestRunJob_PlanListsVMsMomentsAndVolumes(t *testing.T) {
	root := fakeShare(t)
	res, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Volumes) != 3 {
		t.Fatalf("volumes = %d, want 3", len(res.Volumes))
	}
	if len(res.VMs) != 1 || res.VMs[0].Name != "ns/vm" {
		t.Fatalf("vms = %+v", res.VMs)
	}
	vm := res.VMs[0]
	if !vm.Moment.Time.Equal(m2) {
		t.Errorf("moment = %v, want %v", vm.Moment.Time, m2)
	}
	if len(vm.Moments) != 2 || !vm.Moments[0].Time.Equal(m2) || !vm.Moments[1].Time.Equal(m1) {
		t.Errorf("moments = %+v, want [m2 m1]", vm.Moments)
	}
	if len(vm.Disks) != 2 || vm.Disks[0].Rewind || vm.Disks[1].Rewind {
		t.Errorf("disks = %+v, want two in place", vm.Disks)
	}
	if res.RPOSeconds != int64(m3.Sub(m2).Seconds()) {
		t.Errorf("rpo = %d, want %d", res.RPOSeconds, int64(m3.Sub(m2).Seconds()))
	}
}

func TestRunJob_PlanWithRewindToMarksDisksToRewindAndChangesNothing(t *testing.T) {
	root := fakeShare(t)
	at := m1.Add(time.Minute)
	res, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, RewindTo: &at, Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	vm := res.VMs[0]
	if !vm.Moment.Time.Equal(m1) || !vm.Disks[0].Rewind || !vm.Disks[1].Rewind {
		t.Errorf("vm = %+v, want moment m1 and both disks rewound", vm)
	}
	entries, _ := os.ReadDir(root + "/pvcs")
	if len(entries) != 3 {
		t.Errorf("a Plan changed the share: %d entries in pvcs", len(entries))
	}
}

func TestRunJob_ExecuteRewindsAndReportsTheNewFolder(t *testing.T) {
	root := fakeShare(t)
	at := m1.Add(time.Minute)
	res, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionExecute, RewindTo: &at, Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range res.Volumes {
		if v.SubVolume == "pvc-a" && !strings.HasPrefix(v.SubPath, "/pvcs/pvc-a.rewind-") {
			t.Errorf("sub path = %q, want the rewound folder", v.SubPath)
		}
	}
}

func TestRunJob_ExecuteRefusesWhenThePlanChanged(t *testing.T) {
	root := fakeShare(t)
	// The plan that was confirmed showed m1; the share now shows m2.
	expect := &Expectation{
		VMs:     map[string]time.Time{"ns/vm": m1},
		Volumes: []string{"pvc-a", "pvc-b", "pvc-plain"},
	}
	_, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionExecute, Now: m3, Expect: expect})
	if err == nil || !strings.Contains(err.Error(), "ns/vm") {
		t.Fatalf("err = %v, want a refusal that names the VM", err)
	}
	entries, _ := os.ReadDir(root + "/pvcs")
	if len(entries) != 3 {
		t.Errorf("a refused Execute changed the share: %d entries in pvcs", len(entries))
	}
}

func TestRunJob_ExecuteRefusesWhenAVolumeAppeared(t *testing.T) {
	root := fakeShare(t)
	expect := &Expectation{
		VMs:     map[string]time.Time{"ns/vm": m2},
		Volumes: []string{"pvc-a", "pvc-b"},
	}
	_, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionExecute, Now: m3, Expect: expect})
	if err == nil || !strings.Contains(err.Error(), "pvc-plain") {
		t.Fatalf("err = %v, want a refusal that names the new volume", err)
	}
}

func TestRunJob_ExecuteRefusesWhenAVolumeChangedWhereItGoes(t *testing.T) {
	root := fakeShare(t)
	good := func() map[string]VolumeTarget {
		return map[string]VolumeTarget{
			"pvc-a":     {PVCName: "disk-pvc-a", PVCNamespace: "ns", SubPath: "/pvcs/pvc-a"},
			"pvc-b":     {PVCName: "disk-pvc-b", PVCNamespace: "ns", SubPath: "/pvcs/pvc-b"},
			"pvc-plain": {PVCName: "data", PVCNamespace: "ns", SubPath: "/pvcs/pvc-plain"},
		}
	}
	// The names and moments are as confirmed, but the share now sends the claim
	// somewhere else than the operator saw.
	for name, change := range map[string]func(map[string]VolumeTarget){
		"namespace":   func(m map[string]VolumeTarget) { v := m["pvc-plain"]; v.PVCNamespace = "other"; m["pvc-plain"] = v },
		"claim name":  func(m map[string]VolumeTarget) { v := m["pvc-plain"]; v.PVCName = "elsewhere"; m["pvc-plain"] = v },
		"folder":      func(m map[string]VolumeTarget) { v := m["pvc-plain"]; v.SubPath = "/pvcs/pvc-b"; m["pvc-plain"] = v },
		"permissions": func(m map[string]VolumeTarget) { v := m["pvc-plain"]; v.Permissions = "0777"; m["pvc-plain"] = v },
	} {
		targets := good()
		change(targets)
		expect := &Expectation{
			VMs:     map[string]time.Time{"ns/vm": m2},
			Volumes: []string{"pvc-a", "pvc-b", "pvc-plain"},
			Targets: targets,
		}
		_, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionExecute, Now: m3, Expect: expect})
		if err == nil || !strings.Contains(err.Error(), "pvc-plain") {
			t.Errorf("%s changed: err = %v, want a refusal that names the volume", name, err)
		}
	}
	// And the unchanged targets go ahead.
	expect := &Expectation{VMs: map[string]time.Time{"ns/vm": m2}, Volumes: []string{"pvc-a", "pvc-b", "pvc-plain"}, Targets: good()}
	if _, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionExecute, Now: m3, Expect: expect}); err != nil {
		t.Fatalf("unchanged targets: %v", err)
	}
}

func TestRunJob_ExecuteWithTheConfirmedPlanGoesAhead(t *testing.T) {
	root := fakeShare(t)
	expect := &Expectation{
		VMs:     map[string]time.Time{"ns/vm": m2},
		Volumes: []string{"pvc-a", "pvc-b", "pvc-plain"},
	}
	if _, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionExecute, Now: m3, Expect: expect}); err != nil {
		t.Fatal(err)
	}
}

func TestRunJob_UnknownActionIsAnError(t *testing.T) {
	if _, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: t.TempDir(), Action: "Destroy"}); err == nil {
		t.Fatal("expected an error")
	}
}

func TestResult_PlanKeepsWhatTheExecutorNeeds(t *testing.T) {
	root := fakeShare(t)
	res, err := RunJob(pool.NewRealNFSOperations(), Request{MountPath: root, Action: v1alpha1.FailoverActionPlan, Now: m3})
	if err != nil {
		t.Fatal(err)
	}
	plan := PlanFromVolumes(res.Volumes)
	if len(plan.SubVolumes) != 3 {
		t.Fatalf("sub volumes = %d", len(plan.SubVolumes))
	}
	sv := byName(plan, "pvc-a")
	if sv.PVCName != "disk-pvc-a" || sv.PVCNamespace != "ns" || sv.RequestedGB != 10 || sv.SubPath != "/pvcs/pvc-a" || sv.PolicyName != "prod-to-dr" {
		t.Errorf("sub volume = %+v", sv)
	}
}

func TestOutcome_RoundTripsInPlainJSONWhenSmall(t *testing.T) {
	in := Outcome{Result: &Result{RPOSeconds: 5, Volumes: []v1alpha1.FailoverVolume{{SubVolume: "pvc-a", LastSync: metav1.NewTime(m1)}}}}
	msg, err := EncodeOutcome(in)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(msg, "{") {
		t.Errorf("small outcome should be plain JSON, got %q", msg)
	}
	out, err := DecodeOutcome(msg)
	if err != nil {
		t.Fatal(err)
	}
	if out.Result == nil || out.Result.RPOSeconds != 5 || out.Result.Volumes[0].SubVolume != "pvc-a" {
		t.Errorf("out = %+v", out)
	}
}

func TestOutcome_ErrorRoundTrips(t *testing.T) {
	msg, err := EncodeOutcome(Outcome{Error: "a copy is still being applied"})
	if err != nil {
		t.Fatal(err)
	}
	out, err := DecodeOutcome(msg)
	if err != nil || out.Error != "a copy is still being applied" || out.Result != nil {
		t.Errorf("out = %+v, err = %v", out, err)
	}
}

func bigResult(vms, moments int) *Result {
	rng := rand.New(rand.NewSource(1)) //nolint:gosec // G404: test data
	res := &Result{}
	for i := 0; i < vms; i++ {
		vm := v1alpha1.FailoverVM{
			Name:   "ns/vm-" + string(rune('a'+i%26)) + string(rune('a'+i/26)),
			Moment: metav1.NewTime(m3),
			Disks:  []v1alpha1.FailoverDisk{{SubVolume: "pvc-x"}},
		}
		for j := 0; j < moments; j++ {
			// Random gaps, so that compression cannot make the list vanish.
			vm.Moments = append(vm.Moments, metav1.NewTime(m3.Add(-time.Duration(j*3600+rng.Intn(3600))*time.Second)))
		}
		res.VMs = append(res.VMs, vm)
	}
	return res
}

func TestOutcome_LargeResultStaysUnderTheTerminationMessageLimit(t *testing.T) {
	res := bigResult(20, 96)
	msg, err := EncodeOutcome(Outcome{Result: res})
	if err != nil {
		t.Fatal(err)
	}
	if len(msg) > MaxMessageBytes {
		t.Fatalf("message is %d bytes, limit %d", len(msg), MaxMessageBytes)
	}
	out, err := DecodeOutcome(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !out.Result.MomentsTruncated {
		t.Error("expected MomentsTruncated, the moments cannot all fit")
	}
	for i, vm := range out.Result.VMs {
		if len(vm.Moments) == 0 || !vm.Moments[0].Time.Equal(res.VMs[i].Moments[0].Time) {
			t.Errorf("VM %s lost its newest moment", vm.Name)
		}
	}
}

func TestOutcome_ResultThatCannotBeShrunkIsAnError(t *testing.T) {
	res := &Result{}
	for i := 0; i < 400; i++ {
		res.Volumes = append(res.Volumes, v1alpha1.FailoverVolume{
			SubVolume: "pvc-" + strings.Repeat(string(rune('a'+i%26)), 8) + strings.Repeat(string(rune('a'+(i/26)%26)), 8) + string(rune('0'+i%10)),
			PVCName:   "claim-" + strings.Repeat(string(rune('a'+i%7)), 20) + string(rune('0'+i%10)) + strings.Repeat(string(rune('a'+(i/7)%26)), 12),
			SubPath:   "/pvcs/" + strings.Repeat(string(rune('a'+(i*7)%26)), 30) + string(rune('0'+(i/3)%10)),
			LastSync:  metav1.NewTime(m1.Add(time.Duration(i*i) * time.Second)),
		})
	}
	if _, err := EncodeOutcome(Outcome{Result: res}); err == nil {
		t.Fatal("expected an error: the volumes alone do not fit")
	}
}
