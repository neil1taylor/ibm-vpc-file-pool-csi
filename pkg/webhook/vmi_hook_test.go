package webhook

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	jsonpatch "gomodules.xyz/jsonpatch/v2"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

var kubevirtGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "KubeVirt"}

func newHookClient(t *testing.T, gateOn bool, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	s.AddKnownTypeWithName(kubevirtGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(kubevirtGVK.GroupVersion().WithKind("KubeVirtList"), &unstructured.UnstructuredList{})
	kv := &unstructured.Unstructured{}
	kv.SetGroupVersionKind(kubevirtGVK)
	kv.SetNamespace("openshift-cnv")
	kv.SetName("kubevirt")
	gates := []any{"Snapshot"}
	if gateOn {
		gates = append(gates, "Sidecar")
	}
	_ = unstructured.SetNestedSlice(kv.Object, gates, "spec", "configuration", "developerConfiguration", "featureGates")
	return fakeclient.NewClientBuilder().WithScheme(s).WithObjects(append(objs, kv)...).Build()
}

func claim(name, pv string) *corev1.PersistentVolumeClaim {
	return &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{Namespace: "vms", Name: name},
		Spec:       corev1.PersistentVolumeClaimSpec{VolumeName: pv},
	}
}

func subVolume(name string, chain []string, op string) *v1alpha1.SubVolume {
	return &v1alpha1.SubVolume{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status:     v1alpha1.SubVolumeStatus{DiskChain: chain, ChainOperation: op},
	}
}

func vmiRequest(t *testing.T, annotations map[string]string) admission.Request {
	t.Helper()
	vmi := map[string]any{
		"apiVersion": "kubevirt.io/v1", "kind": "VirtualMachineInstance",
		"metadata": map[string]any{"name": "web", "namespace": "vms", "annotations": annotations},
		"spec": map[string]any{"volumes": []any{
			map[string]any{"name": "rootdisk", "persistentVolumeClaim": map[string]any{"claimName": "root"}},
			map[string]any{"name": "data", "dataVolume": map[string]any{"name": "data-dv"}},
			map[string]any{"name": "cloudinit", "cloudInitNoCloud": map[string]any{"userData": "#cloud-config"}},
		}},
	}
	raw, err := json.Marshal(vmi)
	if err != nil {
		t.Fatal(err)
	}
	return admission.Request{AdmissionRequest: admissionv1.AdmissionRequest{
		Operation: admissionv1.Create, Namespace: "vms", Object: runtime.RawExtension{Raw: raw},
	}}
}

// patched applies the response's patch to the request's VMI annotations.
func patchedAnnotations(t *testing.T, req admission.Request, resp admission.Response) map[string]string {
	t.Helper()
	if !resp.Allowed {
		t.Fatalf("denied: %v", resp.Result)
	}
	var obj map[string]any
	_ = json.Unmarshal(req.Object.Raw, &obj)
	ann := map[string]string{}
	if md, ok := obj["metadata"].(map[string]any); ok {
		if a, ok := md["annotations"].(map[string]any); ok {
			for k, v := range a {
				ann[k], _ = v.(string)
			}
		}
	}
	for _, p := range resp.Patches {
		applyAnnotationPatch(t, ann, p)
	}
	return ann
}

func applyAnnotationPatch(t *testing.T, ann map[string]string, p jsonpatch.JsonPatchOperation) {
	t.Helper()
	switch {
	case p.Path == "/metadata/annotations":
		for k := range ann {
			delete(ann, k)
		}
		if m, ok := p.Value.(map[string]any); ok {
			for k, v := range m {
				ann[k], _ = v.(string)
			}
		}
	case strings.HasPrefix(p.Path, "/metadata/annotations/"):
		key := strings.ReplaceAll(strings.ReplaceAll(strings.TrimPrefix(p.Path, "/metadata/annotations/"), "~1", "/"), "~0", "~")
		if p.Operation == "remove" {
			delete(ann, key)
		} else {
			ann[key], _ = p.Value.(string)
		}
	default:
		t.Fatalf("unexpected patch %+v", p)
	}
}

func TestVMIHook_AddsTheHookToAVMWithSnapshots(t *testing.T) {
	c := newHookClient(t, true,
		claim("root", "pvc-root"), claim("data-dv", "pvc-data"),
		subVolume("pvc-root", []string{"snap-1.qcow2", "disk.img"}, ""), subVolume("pvc-data", nil, ""))
	h := &VMIHookInjector{Client: c}
	req := vmiRequest(t, nil)
	ann := patchedAnnotations(t, req, h.Handle(context.Background(), req))

	tops := map[string]string{}
	if err := json.Unmarshal([]byte(ann[pool.DiskTopsAnnotation]), &tops); err != nil {
		t.Fatalf("disk tops %q: %v", ann[pool.DiskTopsAnnotation], err)
	}
	if len(tops) != 1 || tops["rootdisk"] != "snap-1.qcow2" {
		t.Errorf("tops = %v, want only rootdisk", tops)
	}
	var sidecars []map[string]any
	if err := json.Unmarshal([]byte(ann[pool.HookSidecarsAnnotation]), &sidecars); err != nil || len(sidecars) != 1 {
		t.Fatalf("hook sidecars %q: %v", ann[pool.HookSidecarsAnnotation], err)
	}
	if cm, _ := sidecars[0]["configMap"].(map[string]any); cm["name"] != pool.VMHookConfigMap {
		t.Errorf("sidecar = %v", sidecars[0])
	}
}

func TestVMIHook_KeepsTheUsersOwnHooks(t *testing.T) {
	c := newHookClient(t, true, claim("root", "pvc-root"), subVolume("pvc-root", []string{"snap-1.qcow2", "disk.img"}, ""))
	h := &VMIHookInjector{Client: c}
	own := `[{"image":"registry.example/my-hook:1","args":["--version","v1alpha2"]}]`
	req := vmiRequest(t, map[string]string{pool.HookSidecarsAnnotation: own, "team": "web"})
	ann := patchedAnnotations(t, req, h.Handle(context.Background(), req))
	var sidecars []map[string]any
	_ = json.Unmarshal([]byte(ann[pool.HookSidecarsAnnotation]), &sidecars)
	if len(sidecars) != 2 || sidecars[0]["image"] != "registry.example/my-hook:1" {
		t.Errorf("sidecars = %v, want the user's hook and ours", sidecars)
	}
	if ann["team"] != "web" {
		t.Error("other annotations must stay")
	}
}

func TestVMIHook_LeavesAVMWithoutSnapshotsAlone(t *testing.T) {
	c := newHookClient(t, false, claim("root", "pvc-root"), subVolume("pvc-root", nil, ""))
	h := &VMIHookInjector{Client: c}
	req := vmiRequest(t, nil)
	resp := h.Handle(context.Background(), req)
	if !resp.Allowed || len(resp.Patches) != 0 {
		t.Errorf("a VM with no snapshots must start unchanged, got %+v", resp)
	}
}

func TestVMIHook_RefusesAVMWithSnapshotsWhenTheGateIsOff(t *testing.T) {
	c := newHookClient(t, false, claim("root", "pvc-root"), subVolume("pvc-root", []string{"snap-1.qcow2", "disk.img"}, ""))
	h := &VMIHookInjector{Client: c}
	resp := h.Handle(context.Background(), vmiRequest(t, nil))
	if resp.Allowed || !strings.Contains(resp.Result.Message, "Sidecar") || !strings.Contains(resp.Result.Message, "flatten") {
		t.Errorf("want a refusal that says what to do, got %+v", resp.Result)
	}
}

func TestVMIHook_RefusesAStartWhileAJobChangesTheDisk(t *testing.T) {
	c := newHookClient(t, true, claim("root", "pvc-root"), subVolume("pvc-root", []string{"disk.img"}, "chain-snap-one"))
	h := &VMIHookInjector{Client: c}
	resp := h.Handle(context.Background(), vmiRequest(t, nil))
	if resp.Allowed {
		t.Error("the VM must not start while a Job changes its disk")
	}
}

func TestVMIHook_IgnoresClaimsOfOtherDrivers(t *testing.T) {
	c := newHookClient(t, true, claim("root", "pvc-ceph"))
	h := &VMIHookInjector{Client: c}
	resp := h.Handle(context.Background(), vmiRequest(t, nil))
	if !resp.Allowed || len(resp.Patches) != 0 {
		t.Errorf("got %+v", resp)
	}
}
