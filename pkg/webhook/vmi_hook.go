package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	admissionv1 "k8s.io/api/admission/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// VMIHookPath is where the VMI hook webhook is served.
const VMIHookPath = "/mutate-kubevirt-io-v1-virtualmachineinstance"

// VMIHookInjector adds the driver's VM hook to a VirtualMachineInstance whose
// disks have instant snapshots, as the VM starts. The hook opens each such
// disk at the top of its snapshot chain. A VM without instant snapshots is
// not changed. See VM-INSTANT-SNAPSHOTS.md.
type VMIHookInjector struct {
	Client client.Reader
}

var _ admission.Handler = (*VMIHookInjector)(nil)

type vmiObject struct {
	Metadata struct {
		Name        string            `json:"name"`
		Annotations map[string]string `json:"annotations"`
	} `json:"metadata"`
	Spec struct {
		Volumes []map[string]any `json:"volumes"`
	} `json:"spec"`
}

// Handle implements admission.Handler.
func (h *VMIHookInjector) Handle(ctx context.Context, req admission.Request) admission.Response {
	if req.Operation != admissionv1.Create {
		return admission.Allowed("")
	}
	var vmi vmiObject
	if err := json.Unmarshal(req.Object.Raw, &vmi); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	tops := map[string]string{}
	for _, vol := range vmi.Spec.Volumes {
		name, _ := vol["name"].(string)
		claimName := claimOf(vol)
		if name == "" || claimName == "" {
			continue
		}
		sv, err := h.subVolumeOf(ctx, req.Namespace, claimName)
		if err != nil {
			// Do not block VM starts on a read error; a VM with a chain that
			// starts without the hook fails safe (its frozen disk.img is read-only).
			klog.ErrorS(err, "VMI hook: cannot read a disk's SubVolume", "vmi", klog.KRef(req.Namespace, vmi.Metadata.Name), "claim", claimName)
			continue
		}
		if sv == nil {
			continue
		}
		if sv.Status.ChainOperation != "" {
			return admission.Denied(fmt.Sprintf("disk %s (claim %s): a snapshot Job changes the disk now; the VM starts when it is done", name, claimName))
		}
		if len(sv.Status.DiskChain) > 1 {
			tops[name] = sv.Status.DiskChain[0]
		}
	}
	if len(tops) == 0 && vmi.Metadata.Annotations[pool.DiskTopsAnnotation] == "" {
		return admission.Allowed("")
	}
	if len(tops) > 0 {
		on, err := pool.SidecarGateOn(ctx, h.Client)
		if err != nil {
			return admission.Errored(http.StatusInternalServerError, err)
		}
		if !on {
			return admission.Denied(fmt.Sprintf("disk(s) %s of this VM have instant snapshots, which need the KubeVirt Sidecar feature gate to start the VM; "+
				"turn the gate on, or remove the snapshots with `kubectl pool-snapshots flatten %s -n %s`", strings.Join(sortedKeys(tops), ", "), vmi.Metadata.Name, req.Namespace))
		}
	}
	var obj map[string]any
	if err := json.Unmarshal(req.Object.Raw, &obj); err != nil {
		return admission.Errored(http.StatusBadRequest, err)
	}
	md, _ := obj["metadata"].(map[string]any)
	ann := map[string]any{}
	if a, ok := md["annotations"].(map[string]any); ok {
		ann = a
	}
	if len(tops) == 0 {
		delete(ann, pool.DiskTopsAnnotation)
	} else {
		b, _ := json.Marshal(tops)
		ann[pool.DiskTopsAnnotation] = string(b)
		sidecars, err := withVMHook(vmi.Metadata.Annotations[pool.HookSidecarsAnnotation])
		if err != nil {
			return admission.Denied(err.Error())
		}
		ann[pool.HookSidecarsAnnotation] = sidecars
	}
	md["annotations"] = ann
	obj["metadata"] = md
	patched, err := json.Marshal(obj)
	if err != nil {
		return admission.Errored(http.StatusInternalServerError, err)
	}
	return admission.PatchResponseFromRaw(req.Object.Raw, patched)
}

// subVolumeOf returns the driver's SubVolume behind a claim, or nil when the
// claim is not a volume of this driver.
func (h *VMIHookInjector) subVolumeOf(ctx context.Context, namespace, claimName string) (*v1alpha1.SubVolume, error) {
	pvc := &corev1.PersistentVolumeClaim{}
	if err := h.Client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: claimName}, pvc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	if pvc.Spec.VolumeName == "" {
		return nil, nil
	}
	sv := &v1alpha1.SubVolume{}
	if err := h.Client.Get(ctx, types.NamespacedName{Name: pvc.Spec.VolumeName}, sv); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	return sv, nil
}

// withVMHook adds the driver's hook to a hookSidecars annotation value, keeping
// the VM owner's own hooks.
func withVMHook(existing string) (string, error) {
	var sidecars []map[string]any
	if existing != "" {
		if err := json.Unmarshal([]byte(existing), &sidecars); err != nil {
			return "", fmt.Errorf("the VM's %s annotation is not valid JSON: %v", pool.HookSidecarsAnnotation, err)
		}
	}
	out := sidecars[:0]
	for _, s := range sidecars {
		if cm, _ := s["configMap"].(map[string]any); cm != nil && cm["name"] == pool.VMHookConfigMap {
			continue
		}
		out = append(out, s)
	}
	out = append(out, pool.VMHookSidecar)
	b, err := json.Marshal(out)
	return string(b), err
}

func claimOf(vol map[string]any) string {
	if pvc, ok := vol["persistentVolumeClaim"].(map[string]any); ok {
		s, _ := pvc["claimName"].(string)
		return s
	}
	if dv, ok := vol["dataVolume"].(map[string]any); ok {
		s, _ := dv["name"].(string)
		return s
	}
	return ""
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
