package poolbench

import (
	"strings"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func int32p(v int32) *int32 { return &v }

// testBench is a small run: steps of 3 VMs up to 6, one VM in 3 Windows.
func testBench() *v1alpha1.PoolBench {
	return &v1alpha1.PoolBench{
		ObjectMeta: metav1.ObjectMeta{Name: "t10k", UID: "uid-1"},
		Spec: v1alpha1.PoolBenchSpec{
			Profile: "dp2", ShareSizeGB: 3900, IOPS: 10000, StepVMs: 3, MaxVMs: 6,
			WindowsEvery: int32p(3), SettleMinutes: int32p(15), MeasureMinutes: 45,
			Load: defaultLoad(),
			Knee: v1alpha1.PoolBenchKnee{LatencyLimitMs: 20, SlowVMPercent: 25, StallSeconds: 5},
		},
	}
}

func get(t *testing.T, u *unstructured.Unstructured, path ...string) any {
	t.Helper()
	v, ok, err := unstructured.NestedFieldNoCopy(u.Object, path...)
	if err != nil || !ok {
		t.Fatalf("%s: no %s", u.GetName(), strings.Join(path, "."))
	}
	return v
}

// at walks into nested maps and lists: a string key for a map, an int for a list.
func at(t *testing.T, v any, path ...any) any {
	t.Helper()
	for _, p := range path {
		switch k := p.(type) {
		case string:
			m, ok := v.(map[string]any)
			if !ok {
				t.Fatalf("not a map at %q", k)
			}
			v = m[k]
		case int:
			l, ok := v.([]any)
			if !ok || k >= len(l) {
				t.Fatalf("not a list of %d at [%d]", k+1, k)
			}
			v = l[k]
		}
	}
	return v
}

func TestWithDefaults(t *testing.T) {
	s := withDefaults(testBench().Spec)
	if s.Linux.CPU != 1 || s.Linux.MemoryGiB != 2 || s.Linux.Image != "centos-stream9" || s.Linux.Preference != "centos.stream9" ||
		s.Linux.ImageNamespace != "openshift-virtualization-os-images" || s.Linux.RootDiskGiB != 30 {
		t.Errorf("linux defaults: %+v", s.Linux)
	}
	if s.Windows.CPU != 2 || s.Windows.MemoryGiB != 4 || s.Windows.Image != "win2k22-perf" || s.Windows.Preference != "windows.2k22.virtio" {
		t.Errorf("windows defaults: %+v", s.Windows)
	}
	b := testBench()
	b.Spec.Linux.CPU = 4
	if withDefaults(b.Spec).Linux.CPU != 4 {
		t.Error("a set value must win over the default")
	}
}

func TestVMNamesAndOS(t *testing.T) {
	b := testBench()
	if vmName(7) != "vm-007" {
		t.Errorf("got %s", vmName(7))
	}
	var got []bool
	for i := int32(1); i <= 6; i++ {
		got = append(got, isWindows(&b.Spec, i))
	}
	if want := []bool{false, false, true, false, false, true}; !equalBools(got, want) {
		t.Errorf("windows: got %v, want %v", got, want)
	}
	b.Spec.WindowsEvery = int32p(0)
	if isWindows(&b.Spec, 3) {
		t.Error("windowsEvery 0 must make Linux only")
	}
}

func equalBools(a, b []bool) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLinuxVM(t *testing.T) {
	b := testBench()
	vm := buildVM(b, 1)
	if vm.GetName() != "vm-001" || vm.GetNamespace() != "poolbench-t10k" {
		t.Fatalf("name %s/%s", vm.GetNamespace(), vm.GetName())
	}
	if vm.GetLabels()[RunLabel] != "t10k" {
		t.Error("the VM lacks the run label")
	}
	tl := get(t, vm, "spec", "template", "metadata", "labels")
	if at(t, tl, RunLabel) != "t10k" || at(t, tl, OSLabel) != "linux" {
		t.Errorf("template labels: %v", tl)
	}
	if get(t, vm, "spec", "runStrategy") != "Always" {
		t.Error("runStrategy")
	}
	if get(t, vm, "spec", "preference", "name") != "centos.stream9" {
		t.Error("preference")
	}
	dv := get(t, vm, "spec", "dataVolumeTemplates")
	if at(t, dv, 0, "spec", "sourceRef", "name") != "centos-stream9" || at(t, dv, 0, "spec", "storage", "storageClassName") != "poolbench-t10k" {
		t.Errorf("root disk: %v", dv)
	}
	if at(t, dv, 0, "spec", "storage", "resources", "requests", "storage") != "30Gi" {
		t.Errorf("root size: %v", dv)
	}
	vols := get(t, vm, "spec", "template", "spec", "volumes")
	if at(t, vols, 2, "cloudInitNoCloud", "secretRef", "name") != UserDataSecret {
		t.Errorf("cloud-init: %v", vols)
	}
	if get(t, vm, "spec", "template", "spec", "domain", "memory", "guest") != "2Gi" {
		t.Error("memory")
	}
}

func TestWindowsVM(t *testing.T) {
	vm := buildVM(testBench(), 3)
	if at(t, get(t, vm, "spec", "template", "metadata", "labels"), OSLabel) != "windows" {
		t.Error("os label")
	}
	disks := get(t, vm, "spec", "template", "spec", "domain", "devices", "disks")
	if at(t, disks, 1, "serial") != WindowsDiskSerial {
		t.Errorf("load disk serial: %v", disks)
	}
	vols := get(t, vm, "spec", "template", "spec", "volumes")
	if at(t, vols, 2, "secret", "secretName") != WindowsSecret || at(t, vols, 2, "secret", "volumeLabel") != "PERFCFG" {
		t.Errorf("config disk: %v", vols)
	}
	if get(t, vm, "spec", "template", "spec", "domain", "cpu", "sockets") != int64(2) {
		t.Error("cpu")
	}
}

func TestDataPVC(t *testing.T) {
	pvc := buildDataPVC(testBench(), 2)
	if pvc.Name != "vm-002-data" || *pvc.Spec.StorageClassName != "poolbench-t10k" {
		t.Fatalf("%s %v", pvc.Name, pvc.Spec.StorageClassName)
	}
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "4Gi" {
		t.Errorf("size %s, want the area plus 1 GiB", got)
	}
	if pvc.Labels[RunLabel] != "t10k" {
		t.Error("run label")
	}
}

func TestPool(t *testing.T) {
	p := buildPool(testBench(), "eu-de-2")
	if p.Name != "poolbench-t10k" || p.Spec.Zone != "eu-de-2" || p.Spec.ShareSizeGB != 3900 || *p.Spec.IOPS != 10000 {
		t.Fatalf("%+v", p.Spec)
	}
	if p.Spec.MaxShares != 1 || p.Spec.InitialShares != 1 || p.Spec.AutoExpand {
		t.Errorf("one fixed share: %+v", p.Spec)
	}
	if *p.Spec.DefaultUID != 107 || p.Labels[RunLabel] != "t10k" {
		t.Errorf("uid or label: %+v", p)
	}
}
