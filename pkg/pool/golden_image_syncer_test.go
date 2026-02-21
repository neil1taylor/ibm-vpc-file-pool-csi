package pool

import (
	"context"
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
}
