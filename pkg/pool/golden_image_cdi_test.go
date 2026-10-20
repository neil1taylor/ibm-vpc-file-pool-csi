package pool

import (
	"context"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func addDataVolumeKinds(s *runtime.Scheme) {
	s.AddKnownTypeWithName(dataVolumeGVK, &unstructured.Unstructured{})
	list := dataVolumeGVK
	list.Kind = "DataVolumeList"
	s.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
}

// goldenDataVolume is the CentOS Stream 9 golden image DataVolume that pool
// my-pool (StorageClass my-pool) makes, in the given phase.
func goldenDataVolume(ns, name, phase string) *unstructured.Unstructured {
	dv := labelledUnstructured(dataVolumeGVK, ns, name, goldenLabels("my-pool"))
	_ = unstructured.SetNestedField(dv.Object, "docker://quay.io/containerdisks/centos-stream:9", "spec", "source", "registry", "url")
	_ = unstructured.SetNestedField(dv.Object, "my-pool", "spec", "pvc", "storageClassName")
	if phase != "" {
		_ = unstructured.SetNestedField(dv.Object, phase, "status", "phase")
	}
	return dv
}

// newDataVolumeClient returns a fake client that knows DataVolumes and holds objs.
func newDataVolumeClient(objs ...*unstructured.Unstructured) client.Client {
	scheme := newTestScheme()
	addDataVolumeKinds(scheme)
	b := fake.NewClientBuilder().WithScheme(scheme)
	for _, o := range objs {
		b = b.WithObjects(o)
	}
	return b.Build()
}

func TestEnsureGoldenDataVolume_ImportsFromTheRegistryOntoThePool(t *testing.T) {
	c := newDataVolumeClient()
	syncer := NewGoldenImageSyncer(newFakeK8sClient(), c)
	img := goldenImageInfo{Name: "centos-stream9", RegistryURL: "docker://quay.io/containerdisks/centos-stream:9"}

	phase, err := syncer.ensureGoldenDataVolume(context.Background(), "app-a", "golden-centos-stream9", img, "my-pool", "my-sc", 15)
	if err != nil {
		t.Fatalf("ensureGoldenDataVolume: %v", err)
	}
	if phase == "Succeeded" {
		t.Error("a new DataVolume must not be reported as ready")
	}

	dv := &unstructured.Unstructured{}
	dv.SetGroupVersionKind(dataVolumeGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "app-a", Name: "golden-centos-stream9"}, dv); err != nil {
		t.Fatalf("DataVolume not created: %v", err)
	}
	str := func(path ...string) string { v, _, _ := unstructured.NestedString(dv.Object, path...); return v }
	checks := map[string][2]string{
		"registry URL":  {str("spec", "source", "registry", "url"), img.RegistryURL},
		"pull method":   {str("spec", "source", "registry", "pullMethod"), "node"},
		"storage class": {str("spec", "pvc", "storageClassName"), "my-sc"},
		"volume mode":   {str("spec", "pvc", "volumeMode"), "Filesystem"},
		// spec.pvc gives the claim exactly this size, so VM disks of the
		// same size can be copied from it. spec.storage would add CDI's
		// file system room on top.
		"size": {str("spec", "pvc", "resources", "requests", "storage"), "15Gi"},
	}
	for what, got := range checks {
		if got[0] != got[1] {
			t.Errorf("%s = %q, want %q", what, got[0], got[1])
		}
	}
	if _, found, _ := unstructured.NestedMap(dv.Object, "spec", "storage"); found {
		t.Error("spec.storage must not be set: CDI would make the claim bigger than the VM template's boot disk")
	}
	modes, _, _ := unstructured.NestedStringSlice(dv.Object, "spec", "pvc", "accessModes")
	if len(modes) != 1 || modes[0] != "ReadWriteMany" {
		t.Errorf("access modes = %v, want [ReadWriteMany] so VMs can live-migrate", modes)
	}
	if l := dv.GetLabels(); l[goldenImageLabel] != "true" || l[goldenPoolLabel] != "my-pool" {
		t.Errorf("labels = %v, want the golden image and pool labels", l)
	}
}

func TestEnsureGoldenDataVolume_ReportsThePhase(t *testing.T) {
	for _, phase := range []string{"ImportInProgress", "Succeeded", "Failed"} {
		syncer := NewGoldenImageSyncer(newFakeK8sClient(), newDataVolumeClient(goldenDataVolume("app-a", "golden-centos-stream9", phase)))
		img := goldenImageInfo{Name: "centos-stream9", RegistryURL: "docker://quay.io/containerdisks/centos-stream:9"}

		got, err := syncer.ensureGoldenDataVolume(context.Background(), "app-a", "golden-centos-stream9", img, "my-pool", "my-pool", 15)
		if err != nil || got != phase {
			t.Errorf("phase %s: ensureGoldenDataVolume() = %q, %v", phase, got, err)
		}
	}
}

func TestSyncImageInNamespace_NotReadyWhileImporting(t *testing.T) {
	syncer := NewGoldenImageSyncer(newFakeK8sClient(), newDataVolumeClient(goldenDataVolume("app-a", "golden-centos-stream9", "ImportInProgress")))
	img := goldenImageInfo{Name: "centos-stream9", RegistryURL: "docker://quay.io/containerdisks/centos-stream:9"}

	status := syncer.syncImageInNamespace(context.Background(), "my-pool", "my-pool", "app-a", img, 15)
	if status.Phase != "Syncing" {
		t.Errorf("phase = %s, want Syncing while CDI imports", status.Phase)
	}
}

func TestSyncImageInNamespace_FailedImport(t *testing.T) {
	syncer := NewGoldenImageSyncer(newFakeK8sClient(), newDataVolumeClient(goldenDataVolume("app-a", "golden-centos-stream9", "Failed")))
	img := goldenImageInfo{Name: "centos-stream9", RegistryURL: "docker://quay.io/containerdisks/centos-stream:9"}

	status := syncer.syncImageInNamespace(context.Background(), "my-pool", "my-pool", "app-a", img, 15)
	if status.Phase != "Failed" {
		t.Errorf("phase = %s, want Failed", status.Phase)
	}
}

func TestEnsureGoldenDataVolume_RefusesADataVolumeItDidNotMake(t *testing.T) {
	img := goldenImageInfo{Name: "centos-stream9", RegistryURL: "docker://quay.io/containerdisks/centos-stream:9"}
	unlabelled := labelledUnstructured(dataVolumeGVK, "app-a", "golden-centos-stream9", nil)
	_ = unstructured.SetNestedField(unlabelled.Object, "Succeeded", "status", "phase")
	otherSource := goldenDataVolume("app-a", "golden-centos-stream9", "Succeeded")
	_ = unstructured.SetNestedField(otherSource.Object, "docker://evil.example/disk:1", "spec", "source", "registry", "url")

	for name, dv := range map[string]*unstructured.Unstructured{"no pool labels": unlabelled, "another source": otherSource} {
		syncer := NewGoldenImageSyncer(newFakeK8sClient(), newDataVolumeClient(dv))
		if _, err := syncer.ensureGoldenDataVolume(context.Background(), "app-a", "golden-centos-stream9", img, "my-pool", "my-pool", 15); err == nil {
			t.Errorf("%s: expected the DataVolume to be refused", name)
		}
		status := syncer.syncImageInNamespace(context.Background(), "my-pool", "my-pool", "app-a", img, 15)
		if status.Phase != "Failed" || status.DataSourceName != "" {
			t.Errorf("%s: status = %s with catalog entry %q, want Failed and no catalog entry", name, status.Phase, status.DataSourceName)
		}
	}
}

func TestBuildVMTemplate_BootDiskIsTheGoldenImageSize(t *testing.T) {
	tmpl := buildVMTemplate("centos-stream9", "centos-stream9-nfs-pool", "app-a", "my-pool", "my-pool", "golden-centos-stream9", 20)
	params, _, _ := unstructured.NestedSlice(tmpl.Object, "parameters")
	for _, p := range params {
		if m, ok := p.(map[string]interface{}); ok && m["name"] == "BOOT_DISK_SIZE" {
			if m["value"] != "20Gi" {
				t.Errorf("BOOT_DISK_SIZE default = %v, want 20Gi (a copy cannot be smaller than the golden image)", m["value"])
			}
			return
		}
	}
	t.Fatal("no BOOT_DISK_SIZE parameter")
}
