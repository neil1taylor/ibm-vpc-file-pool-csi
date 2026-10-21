package pool

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var (
	vmimListGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstanceMigrationList"}
	vmiListGVK  = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstanceList"}
)

// Helpers that find the KubeVirt pod of a claim and watch it. Both the
// replication controller and the snapshot code use them.

// findVMPodForClaim returns a KubeVirt pod that is running or starting and mounts
// the claim, and the pod's volume name for it, or nil if there is none. Both a
// VM pod and a hot-plug disk pod count, because both mean a running VM writes
// to the disk; a VM pod is preferred.
func findVMPodForClaim(ctx context.Context, c client.Client, namespace, claim string) (*corev1.Pod, string, error) {
	podList := &corev1.PodList{}
	if err := c.List(ctx, podList,
		client.InNamespace(namespace),
		client.HasLabels{kubevirtPodLabel},
	); err != nil {
		return nil, "", fmt.Errorf("listing pods in %s: %w", namespace, err)
	}
	var found *corev1.Pod
	var foundVol string
	for i := range podList.Items {
		pod := &podList.Items[i]
		kind := pod.Labels[kubevirtPodLabel]
		if kind != "virt-launcher" && kind != "hotplug-disk" {
			continue
		}
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		for _, vol := range pod.Spec.Volumes {
			if vol.PersistentVolumeClaim == nil || vol.PersistentVolumeClaim.ClaimName != claim {
				continue
			}
			if kind == "virt-launcher" {
				return pod, vol.Name, nil
			}
			found, foundVol = pod, vol.Name
		}
	}
	return found, foundVol, nil
}

// findVMInstanceForClaim returns the name of a VirtualMachineInstance that uses
// the claim and has not finished, or "". A VMI exists before its pod does, so
// it shows a VM that starts.
func findVMInstanceForClaim(ctx context.Context, c client.Client, namespace, claim string) (string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmiListGVK)
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return "", fmt.Errorf("listing VM instances in %s: %w", namespace, err)
	}
	for _, vmi := range list.Items {
		if phase, _, _ := unstructured.NestedString(vmi.Object, "status", "phase"); phase == "Succeeded" || phase == "Failed" {
			continue
		}
		vols, _, _ := unstructured.NestedSlice(vmi.Object, "spec", "volumes")
		for _, v := range vols {
			if vol, _ := v.(map[string]any); claimOfVolume(vol) == claim {
				return vmi.GetName(), nil
			}
		}
	}
	return "", nil
}

// vmInstanceMoving reports whether a live migration of the VMI is requested or running.
func vmInstanceMoving(ctx context.Context, c client.Client, namespace, vmi string) (bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmimListGVK)
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return false, fmt.Errorf("listing VM migrations in %s: %w", namespace, err)
	}
	for _, m := range list.Items {
		name, _, _ := unstructured.NestedString(m.Object, "spec", "vmiName")
		phase, _, _ := unstructured.NestedString(m.Object, "status", "phase")
		if name == vmi && phase != "Succeeded" && phase != "Failed" {
			return true, nil
		}
	}
	return false, nil
}

// samePod reports whether the pod still exists with the same UID. A VM pod
// that changed means the VM restarted or moved, and QEMU lost its copies.
func samePod(ctx context.Context, c client.Client, ns, name, uid string) bool {
	pod := &corev1.Pod{}
	if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, pod); err != nil {
		return false
	}
	return string(pod.UID) == uid
}
