//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	storagev1 "k8s.io/api/storage/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	driverName = "vpc-file-pool.csi.ibm.io"
)

// resourceName returns a unique e2e resource name.
func resourceName(base string) string {
	return fmt.Sprintf("e2e-%s-%s", base, testRunID)
}

// buildPool creates a FileSharePool spec for testing.
func buildPool(name string, accessorZones []v1alpha1.AccessorZone) *v1alpha1.FileSharePool {
	pool := &v1alpha1.FileSharePool{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Spec: v1alpha1.FileSharePoolSpec{
			Zone:                  homeZone,
			Profile:               "dp2",
			ShareSizeGB:           100,
			MaxShares:             2,
			InitialShares:         1,
			AutoExpand:            true,
			ExpandThresholdPercent: 80,
			AllocationStrategy:    "spread",
			DefaultPermissions:    "0755",
		},
	}
	if resourceGroupID != "" {
		pool.Spec.ResourceGroup = resourceGroupID
	}
	if len(accessorZones) > 0 {
		pool.Spec.AccessorZones = accessorZones
	}
	return pool
}

// buildStorageClass creates a StorageClass pointing to a pool.
func buildStorageClass(name, poolName string) *storagev1.StorageClass {
	reclaimPolicy := corev1.PersistentVolumeReclaimDelete
	bindingMode := storagev1.VolumeBindingImmediate
	return &storagev1.StorageClass{
		ObjectMeta: metav1.ObjectMeta{
			Name: name,
		},
		Provisioner:       driverName,
		ReclaimPolicy:     &reclaimPolicy,
		VolumeBindingMode: &bindingMode,
		Parameters: map[string]string{
			"pool": poolName,
		},
		MountOptions: []string{
			"nfsvers=4.1",
			"soft",
			"timeo=600",
			"retrans=3",
		},
	}
}

// buildPVC creates a PVC using the given StorageClass.
func buildPVC(name, scName string, sizeGi int) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{
				corev1.ReadWriteMany,
			},
			StorageClassName: &scName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse(fmt.Sprintf("%dGi", sizeGi)),
				},
			},
		},
	}
}

// buildTestPod creates a pod that mounts a PVC and writes a marker file.
func buildTestPod(name, pvcName string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testNamespace,
		},
		Spec: corev1.PodSpec{
			RestartPolicy: corev1.RestartPolicyNever,
			Containers: []corev1.Container{
				{
					Name:  "writer",
					Image: "busybox:1.36",
					Command: []string{
						"sh", "-c",
						"echo e2e-ok > /data/marker.txt && cat /data/marker.txt && sleep 5",
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "data",
							MountPath: "/data",
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: pvcName,
						},
					},
				},
			},
		},
	}
}

// waitForPoolReady polls until the FileSharePool reaches Ready phase.
func waitForPoolReady(t *testing.T, ctx context.Context, poolName string) *v1alpha1.FileSharePool {
	t.Helper()
	deadline := time.Now().Add(poolReadyTimeout)

	for time.Now().Before(deadline) {
		pool := &v1alpha1.FileSharePool{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: poolName}, pool)
		if err == nil && pool.Status.Phase == "Ready" {
			t.Logf("Pool %s is Ready (shares=%d, capacity=%dGB)", poolName, pool.Status.ShareCount, pool.Status.TotalCapacityGB)
			return pool
		}
		phase := ""
		if err == nil {
			phase = pool.Status.Phase
		}
		t.Logf("Waiting for pool %s to be Ready (current: %s, err: %v)", poolName, phase, err)
		time.Sleep(pollInterval)
	}

	t.Fatalf("Pool %s did not become Ready within %v", poolName, poolReadyTimeout)
	return nil
}

// waitForPVCBound polls until the PVC is Bound.
func waitForPVCBound(t *testing.T, ctx context.Context, pvcName string) *corev1.PersistentVolumeClaim {
	t.Helper()
	deadline := time.Now().Add(pvcBoundTimeout)

	for time.Now().Before(deadline) {
		pvc := &corev1.PersistentVolumeClaim{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: pvcName, Namespace: testNamespace}, pvc)
		if err == nil && pvc.Status.Phase == corev1.ClaimBound {
			t.Logf("PVC %s is Bound (PV: %s)", pvcName, pvc.Spec.VolumeName)
			return pvc
		}
		phase := ""
		if err == nil {
			phase = string(pvc.Status.Phase)
		}
		t.Logf("Waiting for PVC %s to be Bound (current: %s, err: %v)", pvcName, phase, err)
		time.Sleep(pollInterval)
	}

	t.Fatalf("PVC %s did not become Bound within %v", pvcName, pvcBoundTimeout)
	return nil
}

// waitForPodRunning polls until the Pod is Running.
func waitForPodRunning(t *testing.T, ctx context.Context, podName string) *corev1.Pod {
	t.Helper()
	deadline := time.Now().Add(podReadyTimeout)

	for time.Now().Before(deadline) {
		pod := &corev1.Pod{}
		err := k8sClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: testNamespace}, pod)
		if err == nil && pod.Status.Phase == corev1.PodRunning {
			t.Logf("Pod %s is Running", podName)
			return pod
		}
		// Also accept Succeeded (short-lived pods)
		if err == nil && pod.Status.Phase == corev1.PodSucceeded {
			t.Logf("Pod %s Succeeded", podName)
			return pod
		}
		phase := ""
		if err == nil {
			phase = string(pod.Status.Phase)
		}
		t.Logf("Waiting for pod %s to be Running (current: %s, err: %v)", podName, phase, err)
		time.Sleep(pollInterval)
	}

	t.Fatalf("Pod %s did not become Running within %v", podName, podReadyTimeout)
	return nil
}

// getPV returns the PV bound to a PVC.
func getPV(t *testing.T, ctx context.Context, pvName string) *corev1.PersistentVolume {
	t.Helper()
	pv := &corev1.PersistentVolume{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: pvName}, pv); err != nil {
		t.Fatalf("Failed to get PV %s: %v", pvName, err)
	}
	return pv
}

// getPool returns a FileSharePool by name.
func getPool(t *testing.T, ctx context.Context, poolName string) *v1alpha1.FileSharePool {
	t.Helper()
	pool := &v1alpha1.FileSharePool{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: poolName}, pool); err != nil {
		t.Fatalf("Failed to get pool %s: %v", poolName, err)
	}
	return pool
}

// getSubVolumesForPool lists all SubVolumes belonging to a pool.
func getSubVolumesForPool(t *testing.T, ctx context.Context, poolName string) []v1alpha1.SubVolume {
	t.Helper()
	var svList v1alpha1.SubVolumeList
	if err := k8sClient.List(ctx, &svList); err != nil {
		t.Fatalf("Failed to list SubVolumes: %v", err)
	}
	var result []v1alpha1.SubVolume
	for _, sv := range svList.Items {
		if sv.Spec.PoolName == poolName {
			result = append(result, sv)
		}
	}
	return result
}

// pvVolumeAttributes returns the CSI volumeAttributes from a PV.
func pvVolumeAttributes(pv *corev1.PersistentVolume) map[string]string {
	if pv.Spec.CSI == nil {
		return nil
	}
	return pv.Spec.CSI.VolumeAttributes
}

// hasServerZoneKeys checks whether the PV has server.<zone> keys in its volumeAttributes.
func hasServerZoneKeys(attrs map[string]string) map[string]string {
	result := make(map[string]string)
	for k, v := range attrs {
		if strings.HasPrefix(k, "server.") {
			result[k] = v
		}
	}
	return result
}

// cleanupResource deletes a kubernetes object, logging but not failing on error.
func cleanupResource(t *testing.T, ctx context.Context, obj client.Object) {
	t.Helper()
	if err := k8sClient.Delete(ctx, obj); err != nil {
		t.Logf("Cleanup: failed to delete %T %s: %v", obj, obj.GetName(), err)
	}
}

// cleanupPod deletes a pod and waits for it to be fully gone so that PVC
// pvc-protection finalizers are released.
func cleanupPod(t *testing.T, ctx context.Context, podName string) {
	t.Helper()
	pod := &corev1.Pod{}
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: testNamespace}, pod); err != nil {
		return // already gone
	}
	_ = k8sClient.Delete(ctx, pod)

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		err := k8sClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: testNamespace}, pod)
		if err != nil {
			t.Logf("Pod %s fully deleted", podName)
			return
		}
		time.Sleep(pollInterval)
	}
	t.Logf("Warning: pod %s not fully deleted", podName)
}

// waitForSubVolumesGone polls until no SubVolumes exist for a pool.
func waitForSubVolumesGone(t *testing.T, ctx context.Context, poolName string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Minute)

	for time.Now().Before(deadline) {
		svs := getSubVolumesForPool(t, ctx, poolName)
		if len(svs) == 0 {
			t.Logf("All SubVolumes for pool %s deleted", poolName)
			return
		}
		t.Logf("Waiting for %d SubVolumes to be deleted for pool %s", len(svs), poolName)
		time.Sleep(pollInterval)
	}

	t.Logf("Warning: SubVolumes for pool %s not fully cleaned up", poolName)
}
