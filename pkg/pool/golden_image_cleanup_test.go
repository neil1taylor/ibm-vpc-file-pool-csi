package pool

import (
	"context"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	vpcfake "github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud/fake"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func goldenCleanupScheme() *runtime.Scheme {
	s := newTestScheme()
	for _, gvk := range []schema.GroupVersionKind{templateGVK, dataSourceGVK, dataVolumeGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		list := gvk
		list.Kind += "List"
		s.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
	}
	return s
}

func goldenLabels(pool string) map[string]string {
	return map[string]string{goldenImageLabel: "true", goldenPoolLabel: pool, goldenNameLabel: "centos-stream9"}
}

func labelledUnstructured(gvk schema.GroupVersionKind, ns, name string, labels map[string]string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(gvk)
	u.SetNamespace(ns)
	u.SetName(name)
	u.SetLabels(labels)
	return u
}

func exists(t *testing.T, c client.Client, obj client.Object) bool {
	t.Helper()
	err := c.Get(context.Background(), types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}, obj)
	return err == nil
}

func TestRemovePoolImages_RemovesOnlyThePoolsObjects(t *testing.T) {
	ours := []client.Object{
		goldenDataVolume("app-a", "golden-centos-stream9", "Succeeded"),
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "golden-centos-stream9", Namespace: "app-a", Labels: goldenLabels("my-pool")}},
		labelledUnstructured(templateGVK, "app-a", "centos-stream9-nfs-pool", goldenLabels("my-pool")),
		labelledUnstructured(dataSourceGVK, "openshift-virtualization-os-images", "centos-stream9-app-a", goldenLabels("my-pool")),
	}
	others := []client.Object{
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "user-data", Namespace: "app-a"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "golden-centos-stream9", Namespace: "app-b", Labels: goldenLabels("other-pool")}},
		labelledUnstructured(dataSourceGVK, "openshift-virtualization-os-images", "centos-stream9-app-b", goldenLabels("other-pool")),
		labelledUnstructured(dataSourceGVK, "openshift-virtualization-os-images", "centos-stream9", nil),
	}
	c := fake.NewClientBuilder().WithScheme(goldenCleanupScheme()).WithObjects(append(ours, others...)...).Build()
	syncer := NewGoldenImageSyncer(newFakeK8sClient(), c)

	done, err := syncer.RemovePoolImages(context.Background(), "my-pool")
	if err != nil {
		t.Fatalf("RemovePoolImages: %v", err)
	}
	if done {
		t.Error("expected done=false while the pool's objects were still there")
	}
	for _, obj := range ours {
		if exists(t, c, obj) {
			t.Errorf("%T %s/%s was not removed", obj, obj.GetNamespace(), obj.GetName())
		}
	}
	for _, obj := range others {
		if !exists(t, c, obj) {
			t.Errorf("%T %s/%s was removed, but it is not the pool's", obj, obj.GetNamespace(), obj.GetName())
		}
	}

	done, err = syncer.RemovePoolImages(context.Background(), "my-pool")
	if err != nil {
		t.Fatalf("second RemovePoolImages: %v", err)
	}
	if !done {
		t.Error("expected done=true once nothing of the pool's is left")
	}
}

func TestRemovePoolImages_NoTemplateOrCDIAPI(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: "golden-centos-stream9", Namespace: "app-a", Labels: goldenLabels("my-pool")}}
	c := fake.NewClientBuilder().WithScheme(newTestScheme()).WithObjects(pvc).Build()
	syncer := NewGoldenImageSyncer(newFakeK8sClient(), c)

	if _, err := syncer.RemovePoolImages(context.Background(), "my-pool"); err != nil {
		t.Fatalf("RemovePoolImages without the Template and CDI APIs: %v", err)
	}
	if exists(t, c, pvc) {
		t.Error("golden claim was not removed")
	}
}

func TestProcessOnce_PoolBeingDeleted_Skipped(t *testing.T) {
	pool := newPoolWithGoldenImages("my-pool", true, []string{"test-ns"}, nil)
	now := metav1.Now()
	pool.DeletionTimestamp = &now
	pool.Finalizers = []string{FinalizerName}

	dic := newDataImportCron("centos-stream9", "docker://quay.io/containerdisks/centos-stream:9")
	scheme := newTestScheme()
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataImportCron"}, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(schema.GroupVersionKind{Group: "cdi.kubevirt.io", Version: "v1beta1", Kind: "DataImportCronList"}, &unstructured.UnstructuredList{})
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(dic).Build()

	fk := newFakeK8sClient()
	fk.pools["my-pool"] = pool
	NewGoldenImageSyncer(fk, c).processOnce(context.Background())

	pvcs := &corev1.PersistentVolumeClaimList{}
	_ = c.List(context.Background(), pvcs)
	if len(pvcs.Items) > 0 {
		t.Errorf("expected no golden claims for a pool being deleted, got %d", len(pvcs.Items))
	}
}

type fakeGoldenImageRemover struct {
	calls int
	done  bool
}

func (f *fakeGoldenImageRemover) RemovePoolImages(_ context.Context, _ string) (bool, error) {
	f.calls++
	return f.done, nil
}

func TestReconcile_Deletion_WaitsForGoldenImages(t *testing.T) {
	k := newFakeK8sClient()
	vpc := vpcfake.NewFakeVPCClient()
	shareInfo, _ := vpc.CreateFileShare(context.Background(), ibmcloudInput("test-pool", 1000))
	now := metav1.Now()
	pool := newTestPool("test-pool", "spread", 1000, v1alpha1.PoolShareStatus{
		ShareID: shareInfo.ID, ShareName: shareInfo.Name, TotalGB: 1000, State: "stable", Zone: "us-south-1",
	})
	pool.Spec.GoldenImages = newPoolWithGoldenImages("x", true, []string{"app-a"}, nil).Spec.GoldenImages
	pool.DeletionTimestamp = &now
	pool.Finalizers = []string{FinalizerName}
	k.addPool(pool)

	remover := &fakeGoldenImageRemover{}
	r := newReconciler(k, vpc)
	r.SetGoldenImageRemover(remover)

	result, err := reconcilePool(r, "test-pool")
	if err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
	if remover.calls != 1 {
		t.Errorf("expected the pool's golden images to be removed, got %d calls", remover.calls)
	}
	if result.RequeueAfter == 0 || !hasFinalizer(k.getPool("test-pool")) {
		t.Error("expected deletion to wait while golden images are being removed")
	}
	if vpc.DeleteCalls != 0 {
		t.Errorf("expected no share deleted yet, got %d", vpc.DeleteCalls)
	}

	remover.done = true
	if _, err := reconcilePool(r, "test-pool"); err != nil {
		t.Fatalf("second Reconcile: %v", err)
	}
	if vpc.DeleteCalls != 1 {
		t.Errorf("expected the share to be deleted once the golden images are gone, got %d", vpc.DeleteCalls)
	}
}
