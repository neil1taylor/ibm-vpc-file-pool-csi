package pool

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

const rebindVolume = "pvc-1c4937a8-de80-44e1-b7c1-c97f5db749d8"

func rebindPV(driver string, phase corev1.PersistentVolumePhase, claimNS, claimName string) *corev1.PersistentVolume {
	return &corev1.PersistentVolume{
		ObjectMeta: metav1.ObjectMeta{Name: rebindVolume},
		Spec: corev1.PersistentVolumeSpec{
			PersistentVolumeSource: corev1.PersistentVolumeSource{CSI: &corev1.CSIPersistentVolumeSource{
				Driver:       driver,
				VolumeHandle: "console-check/r010-share/" + rebindVolume,
			}},
			ClaimRef: &corev1.ObjectReference{Namespace: claimNS, Name: claimName},
		},
		Status: corev1.PersistentVolumeStatus{Phase: phase},
	}
}

func rebindSubVolume() *v1alpha1.SubVolume {
	return &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: rebindVolume},
		Spec: v1alpha1.SubVolumeSpec{
			PoolName:     "console-check",
			PVCName:      "tmp-pvc-f582beb4-ac84-41a7-9af0-f9f20189308a",
			PVCNamespace: "openshift-virtualization-os-images",
		},
	}
}

func reconcileRebind(t *testing.T, pv *corev1.PersistentVolume, k8s *fakeK8sClient) {
	t.Helper()
	r := &ClaimRebindReconciler{
		Reader:     fake.NewClientBuilder().WithObjects(pv).WithStatusSubresource(pv).Build(),
		K8sClient:  k8s,
		DriverName: "vpc-file-pool.csi.ibm.io",
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Name: pv.Name}}); err != nil {
		t.Fatalf("Reconcile: %v", err)
	}
}

// A clone made by CDI gets its volume through a temporary claim, which CDI
// then hands to the real claim. The SubVolume must follow the real claim, or
// the driver cannot find the VM that uses the disk.
func TestClaimRebind_FollowsTheClaimTheVolumeIsBoundTo(t *testing.T) {
	k8s := newFakeK8sClient()
	k8s.subVolumes[rebindVolume] = rebindSubVolume()

	reconcileRebind(t, rebindPV("vpc-file-pool.csi.ibm.io", corev1.VolumeBound, "console-check", "nrt-1-volume"), k8s)

	got := k8s.subVolumes[rebindVolume].Spec
	if got.PVCNamespace != "console-check" || got.PVCName != "nrt-1-volume" {
		t.Errorf("SubVolume claim = %s/%s, want console-check/nrt-1-volume", got.PVCNamespace, got.PVCName)
	}
}

func TestClaimRebind_LeavesOtherVolumesAlone(t *testing.T) {
	tests := []struct {
		name string
		pv   *corev1.PersistentVolume
	}{
		{"another driver", rebindPV("other.csi.example.com", corev1.VolumeBound, "console-check", "nrt-1-volume")},
		{"not bound yet", rebindPV("vpc-file-pool.csi.ibm.io", corev1.VolumeReleased, "console-check", "nrt-1-volume")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			k8s := newFakeK8sClient()
			k8s.subVolumes[rebindVolume] = rebindSubVolume()

			reconcileRebind(t, tt.pv, k8s)

			if got := k8s.subVolumes[rebindVolume].Spec.PVCName; got != rebindSubVolume().Spec.PVCName {
				t.Errorf("SubVolume claim changed to %s", got)
			}
		})
	}
}

func TestClaimRebind_NoSubVolumeIsNotAnError(t *testing.T) {
	reconcileRebind(t, rebindPV("vpc-file-pool.csi.ibm.io", corev1.VolumeBound, "console-check", "nrt-1-volume"), newFakeK8sClient())
}
