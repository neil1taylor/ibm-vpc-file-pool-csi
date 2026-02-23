package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// --- Test helpers ---

func newTestScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(s)
	_ = corev1.AddToScheme(s)
	_ = storagev1.AddToScheme(s)
	_ = batchv1.AddToScheme(s)
	return s
}

func newPoolWithGoldenImages(name string, enabled bool, namespaces []string, filters []string) *v1alpha1.FileSharePool {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: v1alpha1.FileSharePoolSpec{
			Zone:               "us-south-1",
			Profile:            "dp2",
			ShareSizeGB:        1000,
			MaxShares:          10,
			InitialShares:      1,
			AllocationStrategy: "spread",
			DefaultPermissions: "0755",
		},
	}
	if enabled {
		pool.Spec.GoldenImages = &v1alpha1.GoldenImageConfig{
			Enabled:          true,
			TargetNamespaces: namespaces,
			ImageFilter:      filters,
			ConverterImage:   "quay.io/centos/centos:stream9",
			PVCSizeGB:        15,
		}
	}
	return pool
}

func newDataImportCron(name, registryURL string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataImportCron",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "openshift-virtualization-os-images",
			},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"source": map[string]interface{}{
							"registry": map[string]interface{}{
								"url": registryURL,
							},
						},
					},
				},
			},
		},
	}
}

func newPoolStorageClass(name, poolName string, isDefault bool) *storagev1.StorageClass {
	annotations := map[string]string{}
	if isDefault {
		annotations["storageclass.kubernetes.io/is-default-class"] = "true"
	}
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
			Labels: map[string]string{
				"storage.ibmcloud.io/managed-by": "ibm-vpc-file-pool-csi",
				"storage.ibmcloud.io/pool":       poolName,
			},
			Annotations: annotations,
		},
		Provisioner: "vpc-file-pool.csi.ibm.io",
	}
}

// --- Tests ---

func TestIsPoolSCDefault_Yes(t *testing.T) {
	sc := newPoolStorageClass("my-pool", "my-pool", true)
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(sc).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	if !syncer.isPoolSCDefault(context.Background()) {
		t.Error("expected isPoolSCDefault to return true when pool SC is default")
	}
}

func TestIsPoolSCDefault_No(t *testing.T) {
	sc := newPoolStorageClass("my-pool", "my-pool", false)
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(sc).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	if syncer.isPoolSCDefault(context.Background()) {
		t.Error("expected isPoolSCDefault to return false when pool SC is not default")
	}
}

func TestIsPoolSCDefault_NoPoolSC(t *testing.T) {
	// Non-pool SC that is default should not trigger native mode.
	sc := &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "other-sc",
			Annotations: map[string]string{"storageclass.kubernetes.io/is-default-class": "true"},
		},
		Provisioner: "other-csi-driver",
	}
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(sc).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	if syncer.isPoolSCDefault(context.Background()) {
		t.Error("expected isPoolSCDefault to return false for non-pool default SC")
	}
}

func TestDiscoverImages_ParsesDataImportCrons(t *testing.T) {
	dic1 := newDataImportCron("centos-stream9", "docker://quay.io/containerdisks/centos-stream:9")
	dic2 := newDataImportCron("fedora-40", "docker://quay.io/containerdisks/fedora:40")

	// Register the GVK so the fake client knows about DataImportCron.
	scheme := newTestScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataImportCron"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataImportCronList"},
		&unstructured.UnstructuredList{},
	)

	directClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dic1, dic2).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	images, err := syncer.discoverImages(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("expected 2 images, got %d", len(images))
	}

	found := map[string]bool{}
	for _, img := range images {
		found[img.Name] = true
	}
	if !found["centos-stream9"] || !found["fedora-40"] {
		t.Errorf("expected centos-stream9 and fedora-40, got %v", images)
	}
}

func TestDiscoverImages_AppliesFilter(t *testing.T) {
	dic1 := newDataImportCron("centos-stream9", "docker://quay.io/containerdisks/centos-stream:9")
	dic2 := newDataImportCron("fedora-40", "docker://quay.io/containerdisks/fedora:40")

	scheme := newTestScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataImportCron"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataImportCronList"},
		&unstructured.UnstructuredList{},
	)

	directClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dic1, dic2).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	images, err := syncer.discoverImages(context.Background(), []string{"centos"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image after filter, got %d", len(images))
	}
	if images[0].Name != "centos-stream9" {
		t.Errorf("expected centos-stream9, got %s", images[0].Name)
	}
}

func TestDiscoverImages_NoCDI(t *testing.T) {
	// No DataImportCron GVK registered — should gracefully return nil.
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	images, err := syncer.discoverImages(context.Background(), nil)
	if err != nil {
		t.Fatalf("expected nil error when CDI not installed, got: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("expected 0 images, got %d", len(images))
	}
}

func TestEnsureGoldenPVC_Creates(t *testing.T) {
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	ready, err := syncer.ensureGoldenPVC(context.Background(), "test-ns", "golden-centos", "centos-stream9", "my-pool", "my-pool", 15)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready {
		t.Error("expected PVC to not be ready on creation")
	}

	// Verify PVC was created.
	pvc := &corev1.PersistentVolumeClaim{}
	if err := directClient.Get(context.Background(), types.NamespacedName{
		Namespace: "test-ns", Name: "golden-centos",
	}, pvc); err != nil {
		t.Fatalf("expected PVC to exist: %v", err)
	}
	if pvc.Labels[goldenImageLabel] != "true" {
		t.Errorf("expected golden-image label")
	}
	if pvc.Labels[goldenPoolLabel] != "my-pool" {
		t.Errorf("expected pool label")
	}
	expectedSize := resource.MustParse("15Gi")
	if !pvc.Spec.Resources.Requests.Storage().Equal(expectedSize) {
		t.Errorf("expected 15Gi size, got %v", pvc.Spec.Resources.Requests.Storage())
	}
}

func TestEnsureGoldenPVC_SkipsExisting(t *testing.T) {
	existingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "golden-centos",
			Namespace: "test-ns",
			Labels:    map[string]string{goldenImageLabel: "true"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("15Gi"),
				},
			},
		},
	}
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(existingPVC).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	ready, err := syncer.ensureGoldenPVC(context.Background(), "test-ns", "golden-centos", "centos-stream9", "my-pool", "my-pool", 15)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready {
		t.Error("expected existing PVC without ready annotation to not be ready")
	}
}

func TestEnsureGoldenPVC_ReadyAnnotation(t *testing.T) {
	existingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "golden-centos",
			Namespace:   "test-ns",
			Labels:      map[string]string{goldenImageLabel: "true"},
			Annotations: map[string]string{"pool.storage.ibmcloud.io/golden-ready": "true"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("15Gi"),
				},
			},
		},
	}
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(existingPVC).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	ready, err := syncer.ensureGoldenPVC(context.Background(), "test-ns", "golden-centos", "centos-stream9", "my-pool", "my-pool", 15)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ready {
		t.Error("expected PVC with ready annotation to be ready")
	}
}

func TestEnsureConverterJob_Creates(t *testing.T) {
	// Pre-create the PVC so the Job has something to reference.
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "golden-centos",
			Namespace: "test-ns",
			Labels:    map[string]string{goldenImageLabel: "true"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("15Gi")},
			},
		},
	}
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(pvc).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	img := goldenImageInfo{Name: "centos-stream9", RegistryURL: "docker://quay.io/containerdisks/centos-stream:9"}

	done, err := syncer.ensureConverterJob(context.Background(), "test-ns", img, "my-pool", "golden-centos", "quay.io/centos/centos:stream9", 15)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Error("expected job to not be done on creation")
	}

	// Verify Job was created.
	job := &batchv1.Job{}
	if err := directClient.Get(context.Background(), types.NamespacedName{
		Namespace: "test-ns", Name: "golden-convert-centos-stream9",
	}, job); err != nil {
		t.Fatalf("expected job to exist: %v", err)
	}
	if job.Labels[goldenImageLabel] != "true" {
		t.Errorf("expected golden-image label on job")
	}
	if job.Labels[goldenNameLabel] != "centos-stream9" {
		t.Errorf("expected image name label on job")
	}
	if *job.Spec.BackoffLimit != 3 {
		t.Errorf("expected backoffLimit 3, got %d", *job.Spec.BackoffLimit)
	}
}

func TestEnsureConverterJob_SkipsActive(t *testing.T) {
	existingJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "golden-convert-centos-stream9",
			Namespace: "test-ns",
			Labels:    map[string]string{goldenImageLabel: "true"},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:    []corev1.Container{{Name: "converter", Image: "centos:stream9"}},
					RestartPolicy: corev1.RestartPolicyOnFailure,
				},
			},
		},
		Status: batchv1.JobStatus{
			Active: 1,
		},
	}
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(existingJob).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	img := goldenImageInfo{Name: "centos-stream9", RegistryURL: "docker://quay.io/containerdisks/centos-stream:9"}

	done, err := syncer.ensureConverterJob(context.Background(), "test-ns", img, "my-pool", "golden-centos", "quay.io/centos/centos:stream9", 15)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Error("expected active job to not be done")
	}
}

func TestEnsureConverterJob_Completed(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "golden-centos",
			Namespace: "test-ns",
			Labels:    map[string]string{goldenImageLabel: "true"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("15Gi")},
			},
		},
	}
	completedJob := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "golden-convert-centos-stream9",
			Namespace: "test-ns",
			Labels:    map[string]string{goldenImageLabel: "true"},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					Containers:    []corev1.Container{{Name: "converter", Image: "centos:stream9"}},
					RestartPolicy: corev1.RestartPolicyOnFailure,
				},
			},
		},
		Status: batchv1.JobStatus{
			Succeeded: 1,
		},
	}
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(pvc, completedJob).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	img := goldenImageInfo{Name: "centos-stream9", RegistryURL: "docker://quay.io/containerdisks/centos-stream:9"}

	done, err := syncer.ensureConverterJob(context.Background(), "test-ns", img, "my-pool", "golden-centos", "quay.io/centos/centos:stream9", 15)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !done {
		t.Error("expected completed job to be done")
	}

	// Verify PVC was marked ready.
	updatedPVC := &corev1.PersistentVolumeClaim{}
	if err := directClient.Get(context.Background(), types.NamespacedName{Namespace: "test-ns", Name: "golden-centos"}, updatedPVC); err != nil {
		t.Fatalf("failed to get PVC: %v", err)
	}
	if updatedPVC.Annotations["pool.storage.ibmcloud.io/golden-ready"] != "true" {
		t.Error("expected PVC to have golden-ready annotation")
	}
}

func TestEnsureTemplate_Creates(t *testing.T) {
	// Register Template GVK.
	scheme := newTestScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "template.openshift.io", Version: "v1", Kind: "Template"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "template.openshift.io", Version: "v1", Kind: "TemplateList"},
		&unstructured.UnstructuredList{},
	)

	directClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	err := syncer.ensureTemplate(context.Background(), "test-ns", "centos-stream9",
		"centos-stream9-nfs-pool", "my-pool", "my-pool", "golden-centos-stream9")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify Template was created.
	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "template.openshift.io", Version: "v1", Kind: "Template",
	})
	if err := directClient.Get(context.Background(), types.NamespacedName{
		Namespace: "test-ns", Name: "centos-stream9-nfs-pool",
	}, tmpl); err != nil {
		t.Fatalf("expected template to exist: %v", err)
	}

	labels := tmpl.GetLabels()
	if labels["template.kubevirt.io/type"] != "base" {
		t.Errorf("expected template type label")
	}
	if labels["os.template.kubevirt.io/centos-stream9"] != "true" {
		t.Errorf("expected OS template label")
	}
}

func TestProcessOnce_NativeMode_SkipsSyncer(t *testing.T) {
	pool := newPoolWithGoldenImages("my-pool", true, []string{"test-ns"}, nil)
	sc := newPoolStorageClass("my-pool", "my-pool", true) // default SC

	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(sc).
		Build()

	fk := newFakeK8sClient()
	fk.pools["my-pool"] = pool

	syncer := NewGoldenImageSyncer(fk, directClient)
	syncer.processOnce(context.Background())

	// In native mode, no PVCs should be created.
	pvcList := &corev1.PersistentVolumeClaimList{}
	_ = directClient.List(context.Background(), pvcList)
	if len(pvcList.Items) > 0 {
		t.Error("expected no PVCs in native CDI mode, but found some")
	}
}

func TestProcessOnce_DisabledPool_Skipped(t *testing.T) {
	pool := newPoolWithGoldenImages("my-pool", false, nil, nil)

	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		Build()

	fk := newFakeK8sClient()
	fk.pools["my-pool"] = pool

	syncer := NewGoldenImageSyncer(fk, directClient)
	syncer.processOnce(context.Background())

	pvcList := &corev1.PersistentVolumeClaimList{}
	_ = directClient.List(context.Background(), pvcList)
	if len(pvcList.Items) > 0 {
		t.Error("expected no PVCs for disabled pool")
	}
}

func TestMatchesFilter(t *testing.T) {
	tests := []struct {
		name    string
		filters []string
		match   bool
	}{
		{"centos-stream9", []string{"centos"}, true},
		{"fedora-40", []string{"centos"}, false},
		{"centos-stream9", []string{"centos", "fedora"}, true},
		{"ubuntu-22", []string{"centos", "fedora"}, false},
		{"centos-stream9", nil, false}, // nil filters = no match (caller checks len)
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := matchesFilter(tt.name, tt.filters); got != tt.match {
				t.Errorf("matchesFilter(%q, %v) = %v, want %v", tt.name, tt.filters, got, tt.match)
			}
		})
	}
}

func TestGoldenImageSyncer_GracefulShutdown(t *testing.T) {
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	syncer.SetInterval(50 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		syncer.Run(ctx)
		close(done)
	}()

	// Let it tick once.
	time.Sleep(100 * time.Millisecond)
	cancel()

	select {
	case <-done:
		// OK — shut down gracefully.
	case <-time.After(2 * time.Second):
		t.Fatal("syncer did not shut down within timeout")
	}
}

func TestBuildVMTemplate(t *testing.T) {
	tmpl := buildVMTemplate("centos-stream9", "centos-stream9-nfs-pool", "test-ns", "my-pool", "my-pool", "golden-centos-stream9")

	if tmpl.GetName() != "centos-stream9-nfs-pool" {
		t.Errorf("expected template name centos-stream9-nfs-pool, got %s", tmpl.GetName())
	}
	if tmpl.GetNamespace() != "test-ns" {
		t.Errorf("expected namespace test-ns, got %s", tmpl.GetNamespace())
	}

	labels := tmpl.GetLabels()
	if labels["template.kubevirt.io/type"] != "base" {
		t.Error("expected base template type label")
	}
	if labels["workload.template.kubevirt.io/server"] != "true" {
		t.Error("expected server workload label")
	}
	if labels[goldenPoolLabel] != "my-pool" {
		t.Error("expected pool label")
	}

	// Verify objects exist.
	objects, found, err := unstructured.NestedSlice(tmpl.Object, "objects")
	if err != nil || !found {
		t.Fatal("expected objects in template")
	}
	if len(objects) != 3 {
		t.Errorf("expected 3 objects (boot PVC, data PVC, VM), got %d", len(objects))
	}

	// Verify parameters exist.
	params, found, err := unstructured.NestedSlice(tmpl.Object, "parameters")
	if err != nil || !found {
		t.Fatal("expected parameters in template")
	}
	if len(params) != 4 {
		t.Errorf("expected 4 parameters, got %d", len(params))
	}
}

func TestSyncImageInNamespace_EndToEnd(t *testing.T) {
	// Set up completed Job and PVC.
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "golden-centos-stream9",
			Namespace:   "test-ns",
			Labels:      map[string]string{goldenImageLabel: "true"},
			Annotations: map[string]string{"pool.storage.ibmcloud.io/golden-ready": "true"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("15Gi")},
			},
		},
	}

	// Register Template and DataSource GVKs.
	scheme := newTestScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "template.openshift.io", Version: "v1", Kind: "Template"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "template.openshift.io", Version: "v1", Kind: "TemplateList"},
		&unstructured.UnstructuredList{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataSource"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataSourceList"},
		&unstructured.UnstructuredList{},
	)

	directClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pvc).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	img := goldenImageInfo{Name: "centos-stream9", RegistryURL: "docker://quay.io/containerdisks/centos-stream:9"}

	status := syncer.syncImageInNamespace(context.Background(), "my-pool", "my-pool", "test-ns", img,
		"quay.io/centos/centos:stream9", 15)

	if status.Phase != "Ready" {
		t.Errorf("expected Ready phase, got %s: %s", status.Phase, status.Message)
	}
	if status.PVCName != "golden-centos-stream9" {
		t.Errorf("expected PVC name golden-centos-stream9, got %s", status.PVCName)
	}
	if status.TemplateName != "centos-stream9-nfs-pool" {
		t.Errorf("expected template name centos-stream9-nfs-pool, got %s", status.TemplateName)
	}
	if status.DataSourceName != "centos-stream9-nfs-pool" {
		t.Errorf("expected DataSource name centos-stream9-nfs-pool, got %s", status.DataSourceName)
	}
}

func TestOsPreference(t *testing.T) {
	tests := []struct {
		cleanName string
		want      string
	}{
		{"centos-stream9", "centos.stream9"},
		{"centos-stream10", "centos.stream10"},
		{"fedora-40", "fedora.40"},
		{"rhel-9", "rhel.9"},
		{"ubuntu-22.04", "ubuntu.22.04"},
		{"windows-2022", "windows.2022"},
	}
	for _, tt := range tests {
		t.Run(tt.cleanName, func(t *testing.T) {
			got := osPreference(tt.cleanName)
			if got != tt.want {
				t.Errorf("osPreference(%q) = %q, want %q", tt.cleanName, got, tt.want)
			}
		})
	}
}

func TestEnsureDataSource_Creates(t *testing.T) {
	scheme := newTestScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataSource"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataSourceList"},
		&unstructured.UnstructuredList{},
	)

	directClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	dsName, err := syncer.ensureDataSource(context.Background(),
		"centos-stream9", "my-pool", "golden-centos-stream9", "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dsName != "centos-stream9-nfs-pool" {
		t.Errorf("expected DataSource name centos-stream9-nfs-pool, got %s", dsName)
	}

	// Verify DataSource was created.
	ds := &unstructured.Unstructured{}
	ds.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataSource",
	})
	if err := directClient.Get(context.Background(), types.NamespacedName{
		Namespace: "openshift-virtualization-os-images", Name: "centos-stream9-nfs-pool",
	}, ds); err != nil {
		t.Fatalf("expected DataSource to exist: %v", err)
	}

	labels := ds.GetLabels()
	if labels["instancetype.kubevirt.io/default-instancetype"] != "u1.medium" {
		t.Error("expected default-instancetype label")
	}
	if labels["instancetype.kubevirt.io/default-preference"] != "centos.stream9" {
		t.Errorf("expected default-preference centos.stream9, got %s", labels["instancetype.kubevirt.io/default-preference"])
	}
	if labels["app.kubernetes.io/part-of"] != "hyperconverged-cluster" {
		t.Error("expected hyperconverged-cluster part-of label")
	}
	if labels[goldenImageLabel] != "true" {
		t.Error("expected golden-image label")
	}
	if labels[goldenPoolLabel] != "my-pool" {
		t.Error("expected pool label")
	}

	// Verify PVC reference.
	pvcName, _, _ := unstructured.NestedString(ds.Object, "spec", "source", "pvc", "name")
	pvcNS, _, _ := unstructured.NestedString(ds.Object, "spec", "source", "pvc", "namespace")
	if pvcName != "golden-centos-stream9" {
		t.Errorf("expected PVC name golden-centos-stream9, got %s", pvcName)
	}
	if pvcNS != "test-ns" {
		t.Errorf("expected PVC namespace test-ns, got %s", pvcNS)
	}
}

func TestEnsureDataSource_SkipsExisting(t *testing.T) {
	existingDS := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataSource",
			"metadata": map[string]interface{}{
				"name":      "centos-stream9-nfs-pool",
				"namespace": "openshift-virtualization-os-images",
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"pvc": map[string]interface{}{
						"name":      "golden-centos-stream9",
						"namespace": "test-ns",
					},
				},
			},
		},
	}

	scheme := newTestScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataSource"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataSourceList"},
		&unstructured.UnstructuredList{},
	)

	directClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existingDS).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	dsName, err := syncer.ensureDataSource(context.Background(),
		"centos-stream9", "my-pool", "golden-centos-stream9", "test-ns")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dsName != "centos-stream9-nfs-pool" {
		t.Errorf("expected DataSource name centos-stream9-nfs-pool, got %s", dsName)
	}
}

func TestEnsureDataSource_NoCDI(t *testing.T) {
	// No DataSource GVK registered — should gracefully return without error.
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	dsName, err := syncer.ensureDataSource(context.Background(),
		"centos-stream9", "my-pool", "golden-centos-stream9", "test-ns")
	if err != nil {
		t.Fatalf("expected nil error when CDI not installed, got: %v", err)
	}
	if dsName != "centos-stream9-nfs-pool" {
		t.Errorf("expected DataSource name centos-stream9-nfs-pool, got %s", dsName)
	}
}

// --- ImageStream support tests ---

func newDataImportCronWithImageStream(name, imageStreamName string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "cdi.kubevirt.io/v1beta1",
			"kind":       "DataImportCron",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "openshift-virtualization-os-images",
			},
			"spec": map[string]interface{}{
				"template": map[string]interface{}{
					"spec": map[string]interface{}{
						"source": map[string]interface{}{
							"registry": map[string]interface{}{
								"imageStream": imageStreamName,
								"pullMethod":  "node",
							},
						},
					},
				},
			},
		},
	}
}

func newImageStream(name, dockerImageRepository string) *unstructured.Unstructured {
	return &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "image.openshift.io/v1",
			"kind":       "ImageStream",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": "openshift-virtualization-os-images",
			},
			"status": map[string]interface{}{
				"dockerImageRepository": dockerImageRepository,
			},
		},
	}
}

func TestDiscoverImages_ResolvesImageStream(t *testing.T) {
	// RHEL DataImportCron uses imageStream instead of url
	dic := newDataImportCronWithImageStream("rhel9-image-cron", "rhel9-guest")
	is := newImageStream("rhel9-guest",
		"image-registry.openshift-image-registry.svc:5000/openshift-virtualization-os-images/rhel9-guest")

	scheme := newTestScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataImportCron"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataImportCronList"},
		&unstructured.UnstructuredList{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "image.openshift.io", Version: "v1", Kind: "ImageStream"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "image.openshift.io", Version: "v1", Kind: "ImageStreamList"},
		&unstructured.UnstructuredList{},
	)

	directClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dic, is).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	images, err := syncer.discoverImages(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 1 {
		t.Fatalf("expected 1 image, got %d", len(images))
	}
	if images[0].Name != "rhel9-image-cron" {
		t.Errorf("expected rhel9-image-cron, got %s", images[0].Name)
	}
	expectedURL := "docker://image-registry.openshift-image-registry.svc:5000/openshift-virtualization-os-images/rhel9-guest:latest"
	if images[0].RegistryURL != expectedURL {
		t.Errorf("expected URL %s, got %s", expectedURL, images[0].RegistryURL)
	}
}

func TestDiscoverImages_MixedURLAndImageStream(t *testing.T) {
	// Mix of URL-based (centos) and ImageStream-based (rhel) DataImportCrons
	dicURL := newDataImportCron("centos-stream9", "docker://quay.io/containerdisks/centos-stream:9")
	dicIS := newDataImportCronWithImageStream("rhel9-image-cron", "rhel9-guest")
	is := newImageStream("rhel9-guest",
		"image-registry.openshift-image-registry.svc:5000/openshift-virtualization-os-images/rhel9-guest")

	scheme := newTestScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataImportCron"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataImportCronList"},
		&unstructured.UnstructuredList{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "image.openshift.io", Version: "v1", Kind: "ImageStream"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "image.openshift.io", Version: "v1", Kind: "ImageStreamList"},
		&unstructured.UnstructuredList{},
	)

	directClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dicURL, dicIS, is).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	images, err := syncer.discoverImages(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 2 {
		t.Fatalf("expected 2 images, got %d", len(images))
	}

	found := map[string]bool{}
	for _, img := range images {
		found[img.Name] = true
	}
	if !found["centos-stream9"] {
		t.Error("expected centos-stream9 in results")
	}
	if !found["rhel9-image-cron"] {
		t.Error("expected rhel9-image-cron in results")
	}
}

func TestDiscoverImages_ImageStreamNotFound(t *testing.T) {
	// DataImportCron references an ImageStream that doesn't exist — should skip gracefully
	dic := newDataImportCronWithImageStream("rhel9-image-cron", "rhel9-guest")

	scheme := newTestScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataImportCron"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataImportCronList"},
		&unstructured.UnstructuredList{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "image.openshift.io", Version: "v1", Kind: "ImageStream"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "image.openshift.io", Version: "v1", Kind: "ImageStreamList"},
		&unstructured.UnstructuredList{},
	)

	directClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(dic).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	images, err := syncer.discoverImages(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("expected 0 images when ImageStream not found, got %d", len(images))
	}
}

func TestEnsureImagePullerRoleBinding_Creates(t *testing.T) {
	scheme := newTestScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBindingList"},
		&unstructured.UnstructuredList{},
	)

	directClient := fake.NewClientBuilder().
		WithScheme(scheme).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	err := syncer.ensureImagePullerRoleBinding(context.Background(), "latest-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify RoleBinding was created.
	rb := &unstructured.Unstructured{}
	rb.SetGroupVersionKind(schema.GroupVersionKind{
		Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding",
	})
	if err := directClient.Get(context.Background(), types.NamespacedName{
		Namespace: "openshift-virtualization-os-images",
		Name:      "pool-csi-image-puller-latest-test",
	}, rb); err != nil {
		t.Fatalf("expected RoleBinding to exist: %v", err)
	}

	// Verify roleRef
	roleRefName, _, _ := unstructured.NestedString(rb.Object, "roleRef", "name")
	if roleRefName != "system:image-puller" {
		t.Errorf("expected roleRef name system:image-puller, got %s", roleRefName)
	}

	// Verify subject
	subjects, _, _ := unstructured.NestedSlice(rb.Object, "subjects")
	if len(subjects) != 1 {
		t.Fatalf("expected 1 subject, got %d", len(subjects))
	}
	subj := subjects[0].(map[string]interface{})
	if subj["name"] != "system:serviceaccounts:latest-test" {
		t.Errorf("expected subject name system:serviceaccounts:latest-test, got %s", subj["name"])
	}
}

func TestEnsureImagePullerRoleBinding_SkipsExisting(t *testing.T) {
	existingRB := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "rbac.authorization.k8s.io/v1",
			"kind":       "RoleBinding",
			"metadata": map[string]interface{}{
				"name":      "pool-csi-image-puller-latest-test",
				"namespace": "openshift-virtualization-os-images",
			},
			"roleRef": map[string]interface{}{
				"apiGroup": "rbac.authorization.k8s.io",
				"kind":     "ClusterRole",
				"name":     "system:image-puller",
			},
		},
	}

	scheme := newTestScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBinding"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "rbac.authorization.k8s.io", Version: "v1", Kind: "RoleBindingList"},
		&unstructured.UnstructuredList{},
	)

	directClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(existingRB).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	err := syncer.ensureImagePullerRoleBinding(context.Background(), "latest-test")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestIsInternalRegistryURL(t *testing.T) {
	tests := []struct {
		url      string
		expected bool
	}{
		{"docker://image-registry.openshift-image-registry.svc:5000/ns/img:latest", true},
		{"docker://quay.io/containerdisks/centos-stream:9", false},
		{"docker://registry.redhat.io/rhel9/rhel-guest-image:latest", false},
		{"", false},
	}
	for _, tt := range tests {
		t.Run(tt.url, func(t *testing.T) {
			if got := isInternalRegistryURL(tt.url); got != tt.expected {
				t.Errorf("isInternalRegistryURL(%q) = %v, want %v", tt.url, got, tt.expected)
			}
		})
	}
}

func TestConverterScript_InternalRegistryAuth(t *testing.T) {
	// Verify that the converter script includes auth handling for internal registry URLs.
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "golden-rhel9",
			Namespace: "test-ns",
			Labels:    map[string]string{goldenImageLabel: "true"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("15Gi")},
			},
		},
	}
	directClient := fake.NewClientBuilder().
		WithScheme(newTestScheme()).
		WithObjects(pvc).
		Build()
	fk := newFakeK8sClient()

	syncer := NewGoldenImageSyncer(fk, directClient)
	internalURL := "docker://image-registry.openshift-image-registry.svc:5000/openshift-virtualization-os-images/rhel9-guest:latest"
	img := goldenImageInfo{Name: "rhel9", RegistryURL: internalURL}

	done, err := syncer.ensureConverterJob(context.Background(), "test-ns", img, "my-pool", "golden-rhel9", "quay.io/centos/centos:stream9", 15)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if done {
		t.Error("expected job to not be done on creation")
	}

	// Verify Job was created with internal registry auth in the script.
	job := &batchv1.Job{}
	if err := directClient.Get(context.Background(), types.NamespacedName{
		Namespace: "test-ns", Name: "golden-convert-rhel9",
	}, job); err != nil {
		t.Fatalf("expected job to exist: %v", err)
	}

	script := job.Spec.Template.Spec.Containers[0].Command[2]
	if !strings.Contains(script, "image-registry.openshift-image-registry.svc") {
		t.Error("expected script to contain internal registry URL")
	}
	if !strings.Contains(script, "serviceaccount/token") {
		t.Error("expected script to contain SA token auth logic")
	}
	if !strings.Contains(script, "--authfile") {
		t.Error("expected script to contain --authfile flag")
	}
}
