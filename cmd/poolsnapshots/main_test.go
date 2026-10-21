package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func newObj(gvkKind, group, version, ns, name string, fields map[string]any) *unstructured.Unstructured {
	u := &unstructured.Unstructured{Object: map[string]any{}}
	for k, v := range fields {
		u.Object[k] = v
	}
	u.SetAPIVersion(group + "/" + version)
	u.SetKind(gvkKind)
	u.SetNamespace(ns)
	u.SetName(name)
	return u
}

func newTestClient(t *testing.T) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	for _, g := range []struct{ group, version, kind string }{
		{"kubevirt.io", "v1", "VirtualMachine"},
		{"snapshot.kubevirt.io", "v1beta1", "VirtualMachineSnapshot"},
		{"snapshot.storage.k8s.io", "v1", "VolumeSnapshot"},
	} {
		s.AddKnownTypeWithName(gvk(g.group, g.version, g.kind), &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk(g.group, g.version, g.kind+"List"), &unstructured.UnstructuredList{})
	}
	vm := newObj("VirtualMachine", "kubevirt.io", "v1", "vms", "web", map[string]any{
		"spec": map[string]any{"template": map[string]any{"spec": map[string]any{"volumes": []any{
			map[string]any{"name": "rootdisk", "persistentVolumeClaim": map[string]any{"claimName": "root"}},
			map[string]any{"name": "data", "dataVolume": map[string]any{"name": "data-dv"}},
			map[string]any{"name": "other", "persistentVolumeClaim": map[string]any{"claimName": "ceph-disk"}},
		}}}},
	})
	vmsnap := newObj("VirtualMachineSnapshot", "snapshot.kubevirt.io", "v1beta1", "vms", "web-snap-1", map[string]any{
		"spec": map[string]any{"source": map[string]any{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": "web"}},
	})
	otherVMSnap := newObj("VirtualMachineSnapshot", "snapshot.kubevirt.io", "v1beta1", "vms", "db-snap-1", map[string]any{
		"spec": map[string]any{"source": map[string]any{"apiGroup": "kubevirt.io", "kind": "VirtualMachine", "name": "db"}},
	})
	// A VolumeSnapshot that OpenShift made for web-snap-1: deleting the VM
	// snapshot deletes it.
	owned := newObj("VolumeSnapshot", "snapshot.storage.k8s.io", "v1", "vms", "vmsnapshot-x-volume-rootdisk", map[string]any{
		"spec": map[string]any{"source": map[string]any{"persistentVolumeClaimName": "root"}},
	})
	owned.SetOwnerReferences([]metav1.OwnerReference{{APIVersion: "snapshot.kubevirt.io/v1beta1", Kind: "VirtualMachineSnapshotContent", Name: "content-x", UID: "u1"}})
	plain := newObj("VolumeSnapshot", "snapshot.storage.k8s.io", "v1", "vms", "root-by-hand", map[string]any{
		"spec": map[string]any{"source": map[string]any{"persistentVolumeClaimName": "root"}},
	})
	cephSnap := newObj("VolumeSnapshot", "snapshot.storage.k8s.io", "v1", "vms", "ceph-by-hand", map[string]any{
		"spec": map[string]any{"source": map[string]any{"persistentVolumeClaimName": "ceph-disk"}},
	})
	objs := []client.Object{vm, vmsnap, otherVMSnap, owned, plain, cephSnap,
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "vms", Name: "root"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-root"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "vms", Name: "data-dv"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-data"}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Namespace: "vms", Name: "ceph-disk"}, Spec: corev1.PersistentVolumeClaimSpec{VolumeName: "pvc-ceph"}},
		&v1alpha1.SubVolume{ObjectMeta: metav1.ObjectMeta{Name: "pvc-root"}, Status: v1alpha1.SubVolumeStatus{DiskChain: []string{"snap-1.qcow2", "disk.img"}}},
		&v1alpha1.SubVolume{ObjectMeta: metav1.ObjectMeta{Name: "pvc-data"}},
	}
	return fakeclient.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func TestPlanFlatten(t *testing.T) {
	c := newTestClient(t)
	plan, err := planFlatten(context.Background(), c, "vms", "web")
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(plan.disks, ","); got != "rootdisk" {
		t.Errorf("disks with snapshots = %s, want rootdisk", got)
	}
	if got := strings.Join(plan.vmSnapshots, ","); got != "web-snap-1" {
		t.Errorf("VM snapshots = %s", got)
	}
	// The owned VolumeSnapshot goes with its VM snapshot; the Ceph one is not ours.
	if got := strings.Join(plan.volumeSnapshots, ","); got != "root-by-hand" {
		t.Errorf("VolumeSnapshots = %s", got)
	}
}

func TestPlanFlatten_NothingToDo(t *testing.T) {
	c := newTestClient(t)
	sv := &v1alpha1.SubVolume{}
	if err := c.Get(context.Background(), client.ObjectKey{Name: "pvc-root"}, sv); err != nil {
		t.Fatal(err)
	}
	sv.Status.DiskChain = nil
	if err := c.Update(context.Background(), sv); err != nil {
		t.Fatal(err)
	}
	plan, err := planFlatten(context.Background(), c, "vms", "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.disks) != 0 {
		t.Errorf("no disk has a chain, got %v", plan.disks)
	}
}

func TestConfirm(t *testing.T) {
	var out bytes.Buffer
	if confirm(strings.NewReader("web\n"), &out, "web") != true {
		t.Error("typing the VM name confirms")
	}
	if confirm(strings.NewReader("yes\n"), &out, "web") != false {
		t.Error("anything else does not")
	}
}

func TestFlatten_DeletesThePlan(t *testing.T) {
	c := newTestClient(t)
	plan, _ := planFlatten(context.Background(), c, "vms", "web")
	if err := deletePlan(context.Background(), c, "vms", plan); err != nil {
		t.Fatal(err)
	}
	after, _ := planFlatten(context.Background(), c, "vms", "web")
	if len(after.vmSnapshots) != 0 || len(after.volumeSnapshots) != 0 {
		t.Errorf("left: %+v", after)
	}
	// Another VM's snapshot stays.
	left := newObj("VirtualMachineSnapshot", "snapshot.kubevirt.io", "v1beta1", "vms", "db-snap-1", nil)
	if err := c.Get(context.Background(), client.ObjectKeyFromObject(left), left); err != nil {
		t.Errorf("another VM's snapshot was deleted: %v", err)
	}
}
