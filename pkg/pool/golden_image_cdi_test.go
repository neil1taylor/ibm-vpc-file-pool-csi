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

// goldenDataVolume is a golden image DataVolume of my-pool in the given phase.
func goldenDataVolume(ns, name, phase string) *unstructured.Unstructured {
	dv := labelledUnstructured(dataVolumeGVK, ns, name, goldenLabels("my-pool"))
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
		"storage class": {str("spec", "storage", "storageClassName"), "my-sc"},
		"volume mode":   {str("spec", "storage", "volumeMode"), "Filesystem"},
		"size":          {str("spec", "storage", "resources", "requests", "storage"), "15Gi"},
	}
	for what, got := range checks {
		if got[0] != got[1] {
			t.Errorf("%s = %q, want %q", what, got[0], got[1])
		}
	}
	modes, _, _ := unstructured.NestedStringSlice(dv.Object, "spec", "storage", "accessModes")
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

		got, err := syncer.ensureGoldenDataVolume(context.Background(), "app-a", "golden-centos-stream9", img, "my-pool", "my-sc", 15)
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
