package k8s

import (
	"context"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add v1alpha1 to scheme: %v", err)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		t.Fatalf("failed to add corev1 to scheme: %v", err)
	}
	return scheme
}

func TestGetFileSharePool_Found(t *testing.T) {
	scheme := newTestScheme(t)
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool"},
		Spec:       v1alpha1.FileSharePoolSpec{Zone: "us-south-1", Profile: "dp2"},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build()
	c := NewClient(fc)

	got, err := c.GetFileSharePool(context.Background(), "test-pool")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Name != "test-pool" {
		t.Errorf("expected name test-pool, got %s", got.Name)
	}
	if got.Spec.Zone != "us-south-1" {
		t.Errorf("expected zone us-south-1, got %s", got.Spec.Zone)
	}
}

func TestGetFileSharePool_NotFound(t *testing.T) {
	scheme := newTestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	c := NewClient(fc)

	_, err := c.GetFileSharePool(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent pool, got nil")
	}
}

func TestUpdateFileSharePoolStatus(t *testing.T) {
	scheme := newTestScheme(t)
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool"},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(pool).
		WithStatusSubresource(&v1alpha1.FileSharePool{}).
		Build()
	c := NewClient(fc)

	// Fetch, modify status, update
	got, err := c.GetFileSharePool(context.Background(), "test-pool")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	got.Status.Phase = "Ready"
	got.Status.ShareCount = 3

	if err := c.UpdateFileSharePoolStatus(context.Background(), got); err != nil {
		t.Fatalf("status update failed: %v", err)
	}

	// Re-fetch and verify
	updated, err := c.GetFileSharePool(context.Background(), "test-pool")
	if err != nil {
		t.Fatalf("re-fetch failed: %v", err)
	}
	if updated.Status.Phase != "Ready" {
		t.Errorf("expected phase Ready, got %s", updated.Status.Phase)
	}
	if updated.Status.ShareCount != 3 {
		t.Errorf("expected shareCount 3, got %d", updated.Status.ShareCount)
	}
}

func TestUpdateFileSharePool(t *testing.T) {
	scheme := newTestScheme(t)
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pool"},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(pool).Build()
	c := NewClient(fc)

	got, err := c.GetFileSharePool(context.Background(), "test-pool")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	got.Finalizers = append(got.Finalizers, "storage.ibmcloud.io/pool-protection")

	if err := c.UpdateFileSharePool(context.Background(), got); err != nil {
		t.Fatalf("update failed: %v", err)
	}

	updated, err := c.GetFileSharePool(context.Background(), "test-pool")
	if err != nil {
		t.Fatalf("re-fetch failed: %v", err)
	}
	if len(updated.Finalizers) != 1 || updated.Finalizers[0] != "storage.ibmcloud.io/pool-protection" {
		t.Errorf("expected finalizer, got %v", updated.Finalizers)
	}
}

func TestGetSubVolume_Found(t *testing.T) {
	scheme := newTestScheme(t)
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-abc123"},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:    "test-pool",
			ShareID:     "r006-share1",
			RequestedGB: 10,
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sv).Build()
	c := NewClient(fc)

	got, err := c.GetSubVolume(context.Background(), "pvc-abc123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Spec.ShareID != "r006-share1" {
		t.Errorf("expected shareID r006-share1, got %s", got.Spec.ShareID)
	}
}

func TestGetSubVolume_NotFound(t *testing.T) {
	scheme := newTestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	c := NewClient(fc)

	_, err := c.GetSubVolume(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent SubVolume, got nil")
	}
}

func TestCreateSubVolume(t *testing.T) {
	scheme := newTestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	c := NewClient(fc)

	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-new",
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":     "test-pool",
				"storage.ibmcloud.io/share-id": "r006-share1",
			},
		},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:    "test-pool",
			ShareID:     "r006-share1",
			RequestedGB: 20,
		},
	}

	if err := c.CreateSubVolume(context.Background(), sv); err != nil {
		t.Fatalf("create failed: %v", err)
	}

	got, err := c.GetSubVolume(context.Background(), "pvc-new")
	if err != nil {
		t.Fatalf("get after create failed: %v", err)
	}
	if got.Spec.RequestedGB != 20 {
		t.Errorf("expected requestedGB 20, got %d", got.Spec.RequestedGB)
	}
}

func TestUpdateSubVolume(t *testing.T) {
	scheme := newTestScheme(t)
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-update"},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:    "test-pool",
			ShareID:     "r006-share1",
			RequestedGB: 10,
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sv).Build()
	c := NewClient(fc)

	got, err := c.GetSubVolume(context.Background(), "pvc-update")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	got.Spec.RequestedGB = 50

	if err := c.UpdateSubVolume(context.Background(), got); err != nil {
		t.Fatalf("update failed: %v", err)
	}

	updated, err := c.GetSubVolume(context.Background(), "pvc-update")
	if err != nil {
		t.Fatalf("re-fetch failed: %v", err)
	}
	if updated.Spec.RequestedGB != 50 {
		t.Errorf("expected requestedGB 50, got %d", updated.Spec.RequestedGB)
	}
}

func TestUpdateSubVolumeStatus(t *testing.T) {
	scheme := newTestScheme(t)
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-status"},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName: "test-pool",
			ShareID:  "r006-share1",
		},
	}

	fc := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(sv).
		WithStatusSubresource(&v1alpha1.SubVolume{}).
		Build()
	c := NewClient(fc)

	got, err := c.GetSubVolume(context.Background(), "pvc-status")
	if err != nil {
		t.Fatalf("get failed: %v", err)
	}
	got.Status.Phase = "Bound"

	if err := c.UpdateSubVolumeStatus(context.Background(), got); err != nil {
		t.Fatalf("status update failed: %v", err)
	}

	updated, err := c.GetSubVolume(context.Background(), "pvc-status")
	if err != nil {
		t.Fatalf("re-fetch failed: %v", err)
	}
	if updated.Status.Phase != "Bound" {
		t.Errorf("expected phase Bound, got %s", updated.Status.Phase)
	}
}

func TestDeleteSubVolume(t *testing.T) {
	scheme := newTestScheme(t)
	sv := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: "pvc-delete"},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName: "test-pool",
			ShareID:  "r006-share1",
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sv).Build()
	c := NewClient(fc)

	if err := c.DeleteSubVolume(context.Background(), "pvc-delete"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	_, err := c.GetSubVolume(context.Background(), "pvc-delete")
	if err == nil {
		t.Fatal("expected error after deletion, got nil")
	}
}

func TestDeleteSubVolume_NotFound(t *testing.T) {
	scheme := newTestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	c := NewClient(fc)

	err := c.DeleteSubVolume(context.Background(), "nonexistent")
	if err == nil {
		t.Fatal("expected error for nonexistent SubVolume, got nil")
	}
}

func TestListSubVolumes_ByPool(t *testing.T) {
	scheme := newTestScheme(t)
	sv1 := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-1",
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":     "pool-a",
				"storage.ibmcloud.io/share-id": "r006-share1",
			},
		},
		Spec: v1alpha1.SubVolumeSpec{PoolName: "pool-a", ShareID: "r006-share1"},
	}
	sv2 := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-2",
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":     "pool-a",
				"storage.ibmcloud.io/share-id": "r006-share2",
			},
		},
		Spec: v1alpha1.SubVolumeSpec{PoolName: "pool-a", ShareID: "r006-share2"},
	}
	sv3 := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-3",
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":     "pool-b",
				"storage.ibmcloud.io/share-id": "r006-share3",
			},
		},
		Spec: v1alpha1.SubVolumeSpec{PoolName: "pool-b", ShareID: "r006-share3"},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sv1, sv2, sv3).Build()
	c := NewClient(fc)

	results, err := c.ListSubVolumes(context.Background(), "pool-a")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 SubVolumes for pool-a, got %d", len(results))
	}
}

func TestListSubVolumes_Empty(t *testing.T) {
	scheme := newTestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	c := NewClient(fc)

	results, err := c.ListSubVolumes(context.Background(), "no-such-pool")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 SubVolumes, got %d", len(results))
	}
}

func TestListSubVolumesByShare(t *testing.T) {
	scheme := newTestScheme(t)
	sv1 := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-1",
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":     "pool-a",
				"storage.ibmcloud.io/share-id": "r006-share1",
			},
		},
		Spec: v1alpha1.SubVolumeSpec{PoolName: "pool-a", ShareID: "r006-share1"},
	}
	sv2 := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-2",
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":     "pool-a",
				"storage.ibmcloud.io/share-id": "r006-share1",
			},
		},
		Spec: v1alpha1.SubVolumeSpec{PoolName: "pool-a", ShareID: "r006-share1"},
	}
	sv3 := &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pvc-3",
			Labels: map[string]string{
				"storage.ibmcloud.io/pool":     "pool-a",
				"storage.ibmcloud.io/share-id": "r006-share2",
			},
		},
		Spec: v1alpha1.SubVolumeSpec{PoolName: "pool-a", ShareID: "r006-share2"},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(sv1, sv2, sv3).Build()
	c := NewClient(fc)

	results, err := c.ListSubVolumesByShare(context.Background(), "r006-share1")
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 SubVolumes for share r006-share1, got %d", len(results))
	}
}

func TestGetConfigMapValue_Found(t *testing.T) {
	scheme := newTestScheme(t)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ibm-cloud-provider-data",
			Namespace: "kube-system",
		},
		Data: map[string]string{
			"vpc_id":         "r006-abc123",
			"vpc_subnet_ids": "subnet-1,subnet-2",
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	c := NewClient(fc)

	val, err := c.GetConfigMapValue(context.Background(), "kube-system", "ibm-cloud-provider-data", "vpc_id")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if val != "r006-abc123" {
		t.Errorf("expected vpc_id r006-abc123, got %s", val)
	}
}

func TestGetConfigMapValue_KeyNotFound(t *testing.T) {
	scheme := newTestScheme(t)
	cm := &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "ibm-cloud-provider-data",
			Namespace: "kube-system",
		},
		Data: map[string]string{
			"vpc_id": "r006-abc123",
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cm).Build()
	c := NewClient(fc)

	_, err := c.GetConfigMapValue(context.Background(), "kube-system", "ibm-cloud-provider-data", "nonexistent_key")
	if err == nil {
		t.Fatal("expected error for missing key, got nil")
	}
}

func TestGetConfigMapValue_ConfigMapNotFound(t *testing.T) {
	scheme := newTestScheme(t)
	fc := fake.NewClientBuilder().WithScheme(scheme).Build()
	c := NewClient(fc)

	_, err := c.GetConfigMapValue(context.Background(), "kube-system", "nonexistent-cm", "key")
	if err == nil {
		t.Fatal("expected error for nonexistent configmap, got nil")
	}
}

func TestGetNodeZone(t *testing.T) {
	scheme := newTestScheme(t)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-1",
			Labels: map[string]string{
				"topology.kubernetes.io/zone": "us-south-1",
			},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	c := NewClient(fc)

	zone, err := c.GetNodeZone(context.Background(), "worker-1")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if zone != "us-south-1" {
		t.Errorf("expected zone us-south-1, got %s", zone)
	}
}

func TestGetNodeZone_LegacyLabel(t *testing.T) {
	scheme := newTestScheme(t)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name: "worker-legacy",
			Labels: map[string]string{
				"failure-domain.beta.kubernetes.io/zone": "eu-de-2",
			},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	c := NewClient(fc)

	zone, err := c.GetNodeZone(context.Background(), "worker-legacy")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if zone != "eu-de-2" {
		t.Errorf("expected zone eu-de-2, got %s", zone)
	}
}

func TestGetNodeZone_NoLabel(t *testing.T) {
	scheme := newTestScheme(t)
	node := &corev1.Node{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "worker-nolabel",
			Labels: map[string]string{},
		},
	}

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(node).Build()
	c := NewClient(fc)

	_, err := c.GetNodeZone(context.Background(), "worker-nolabel")
	if err == nil {
		t.Fatal("expected error for node without zone label, got nil")
	}
}
