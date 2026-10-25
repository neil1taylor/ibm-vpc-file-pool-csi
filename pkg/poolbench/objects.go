package poolbench

import (
	"fmt"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

const (
	// RunLabel is on every object a run makes. Its value is the run's name.
	RunLabel = "storage.ibmcloud.io/poolbench"
	// OSLabel is on each test VM and its pod: linux or windows.
	OSLabel = "storage.ibmcloud.io/poolbench-os"

	// UserDataSecret holds the cloud-init of the Linux VMs, WindowsSecret the
	// config disk of the Windows VMs, and TokenSecret the run's token. All
	// three are in the run's namespace.
	UserDataSecret = "poolbench-userdata"
	WindowsSecret  = "poolbench-windows"
	TokenSecret    = "poolbench-token"

	imagesNamespace = "openshift-virtualization-os-images"
	rootDiskGiB     = 30
	// qemuUID owns the folders of the test pool, as KubeVirt needs.
	qemuUID = 107
)

// runName is the name of the run's namespace and pool.
func runName(pb *v1alpha1.PoolBench) string { return "poolbench-" + pb.Name }

func runLabels(pb *v1alpha1.PoolBench) map[string]string {
	return map[string]string{RunLabel: pb.Name}
}

// vmName is the name of the nth test VM, from 1.
func vmName(n int32) string { return fmt.Sprintf("vm-%03d", n) }

// isWindows is true when the nth VM runs Windows: every WindowsEvery-th VM.
func isWindows(spec *v1alpha1.PoolBenchSpec, n int32) bool {
	return spec.WindowsEvery != nil && *spec.WindowsEvery > 0 && n%*spec.WindowsEvery == 0
}

func vmDefaults(vm, d v1alpha1.PoolBenchVM) v1alpha1.PoolBenchVM {
	if vm.CPU == 0 {
		vm.CPU = d.CPU
	}
	if vm.MemoryGiB == 0 {
		vm.MemoryGiB = d.MemoryGiB
	}
	if vm.Image == "" {
		vm.Image = d.Image
	}
	if vm.ImageNamespace == "" {
		vm.ImageNamespace = d.ImageNamespace
	}
	if vm.Preference == "" {
		vm.Preference = d.Preference
	}
	if vm.RootDiskGiB == 0 {
		vm.RootDiskGiB = d.RootDiskGiB
	}
	return vm
}

// withDefaults fills in the VM sizes and images the spec leaves out: small
// VMs, so the disk is what is tested and not the CPU.
func withDefaults(spec v1alpha1.PoolBenchSpec) v1alpha1.PoolBenchSpec {
	spec.Linux = vmDefaults(spec.Linux, v1alpha1.PoolBenchVM{
		CPU: 1, MemoryGiB: 2, Image: "centos-stream9", ImageNamespace: imagesNamespace,
		Preference: "centos.stream9", RootDiskGiB: rootDiskGiB,
	})
	spec.Windows = vmDefaults(spec.Windows, v1alpha1.PoolBenchVM{
		CPU: 2, MemoryGiB: 4, Image: "win2k22-perf", ImageNamespace: imagesNamespace,
		Preference: "windows.2k22.virtio", RootDiskGiB: rootDiskGiB,
	})
	return spec
}

// buildPool is the run's own pool: one share of the size and IOPS under test,
// which never grows.
func buildPool(pb *v1alpha1.PoolBench, zone string) *v1alpha1.FileSharePool {
	iops := pb.Spec.IOPS
	uid := int64(qemuUID)
	return &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: runName(pb), Labels: runLabels(pb)},
		Spec: v1alpha1.FileSharePoolSpec{
			Zone: zone, Profile: "dp2", ShareSizeGB: pb.Spec.ShareSizeGB, IOPS: &iops,
			MaxShares: 1, InitialShares: 1, AutoExpand: false, ExpandThresholdPercent: 80, AllocationStrategy: "spread",
			DefaultUID: &uid, DefaultGID: &uid, DefaultPermissions: "0777",
		},
	}
}

// buildDataPVC is the load disk of the nth VM: the test area and 1 GiB.
func buildDataPVC(pb *v1alpha1.PoolBench, n int32) *corev1.PersistentVolumeClaim {
	class := runName(pb)
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Name: vmName(n) + "-data", Namespace: runName(pb), Labels: runLabels(pb)},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes:      []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			StorageClassName: &class,
			Resources: corev1.VolumeResourceRequirements{Requests: corev1.ResourceList{
				corev1.ResourceStorage: *resource.NewQuantity(int64(pb.Spec.Load.AreaGiB+1)<<30, resource.BinarySI),
			}},
		},
	}
}

// buildVM is the nth test VM: its root disk copied from the image onto the
// test pool, its load disk, and its config (cloud-init for Linux, a config
// disk for Windows).
func buildVM(pb *v1alpha1.PoolBench, n int32) *unstructured.Unstructured {
	spec := withDefaults(pb.Spec)
	name, ns := vmName(n), runName(pb)
	size, osName := spec.Linux, "linux"
	if isWindows(&spec, n) {
		size, osName = spec.Windows, "windows"
	}
	labels := map[string]any{RunLabel: pb.Name, OSLabel: osName}
	rootDV := map[string]any{
		"metadata": map[string]any{"name": name + "-root", "labels": map[string]any{RunLabel: pb.Name}},
		"spec": map[string]any{
			"sourceRef": map[string]any{"kind": "DataSource", "name": size.Image, "namespace": size.ImageNamespace},
			"storage": map[string]any{
				"storageClassName": ns,
				"resources":        map[string]any{"requests": map[string]any{"storage": fmt.Sprintf("%dGi", size.RootDiskGiB)}},
			},
		},
	}
	devices := map[string]any{"logSerialConsole": true}
	volumes := []any{
		map[string]any{"name": "rootdisk", "dataVolume": map[string]any{"name": name + "-root"}},
		map[string]any{"name": "loaddisk", "persistentVolumeClaim": map[string]any{"claimName": name + "-data"}},
	}
	if osName == "windows" {
		devices["disks"] = []any{
			map[string]any{"name": "rootdisk", "bootOrder": int64(1), "disk": map[string]any{"bus": "virtio"}},
			map[string]any{"name": "loaddisk", "serial": WindowsDiskSerial, "disk": map[string]any{"bus": "virtio"}},
			map[string]any{"name": "perfcfg", "cdrom": map[string]any{"bus": "sata"}},
		}
		volumes = append(volumes, map[string]any{"name": "perfcfg", "secret": map[string]any{"secretName": WindowsSecret, "volumeLabel": "PERFCFG"}})
	} else {
		volumes = append(volumes, map[string]any{"name": "cloudinit", "cloudInitNoCloud": map[string]any{"secretRef": map[string]any{"name": UserDataSecret}}})
	}
	vm := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "kubevirt.io/v1",
		"kind":       "VirtualMachine",
		"metadata":   map[string]any{"name": name, "namespace": ns, "labels": map[string]any{RunLabel: pb.Name, OSLabel: osName}},
		"spec": map[string]any{
			"runStrategy":         "Always",
			"preference":          map[string]any{"kind": "VirtualMachineClusterPreference", "name": size.Preference},
			"dataVolumeTemplates": []any{rootDV},
			"template": map[string]any{
				"metadata": map[string]any{"labels": labels},
				"spec": map[string]any{
					"domain": map[string]any{
						"cpu":     map[string]any{"sockets": int64(size.CPU)},
						"memory":  map[string]any{"guest": fmt.Sprintf("%dGi", size.MemoryGiB)},
						"devices": devices,
					},
					"volumes": volumes,
				},
			},
		},
	}}
	return vm
}
