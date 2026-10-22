package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// jsonPatchAnnotation is the HyperConverged annotation whose JSON patches the
// operator applies to the KubeVirt configuration. Red Hat does not support it.
const jsonPatchAnnotation = "kubevirt.kubevirt.io/jsonpatch"

// sidecarGatePatch is the jsonpatch entry that adds the Sidecar feature gate,
// the same entry hack/sidecar-gate.sh writes.
var sidecarGatePatch = map[string]any{
	"op":    "add",
	"path":  "/spec/configuration/developerConfiguration/featureGates/-",
	"value": sidecarFeatureGate,
}

// EnableSidecarGate adds the Sidecar feature gate entry to the jsonpatch
// annotation of every HyperConverged object, keeping the other entries. It
// never removes the gate: VMs with instant snapshots cannot start without it.
func EnableSidecarGate(ctx context.Context, c client.Client) error {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(hyperConvergedGVK.GroupVersion().WithKind(hyperConvergedGVK.Kind + "List"))
	if err := c.List(ctx, list); err != nil {
		return fmt.Errorf("listing HyperConverged: %w", err)
	}
	if len(list.Items) == 0 {
		return fmt.Errorf("no HyperConverged object found; is OpenShift Virtualization installed?")
	}
	for _, item := range list.Items {
		key := client.ObjectKeyFromObject(&item)
		err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			hc := &unstructured.Unstructured{}
			hc.SetGroupVersionKind(hyperConvergedGVK)
			if err := c.Get(ctx, key, hc); err != nil {
				return err
			}
			ann := hc.GetAnnotations()
			var ops []any
			if cur := ann[jsonPatchAnnotation]; cur != "" {
				if err := json.Unmarshal([]byte(cur), &ops); err != nil {
					return fmt.Errorf("the %s annotation is not a JSON list, so it is left alone: %w", jsonPatchAnnotation, err)
				}
			}
			for _, op := range ops {
				if reflect.DeepEqual(op, sidecarGatePatch) {
					return nil
				}
			}
			out, err := json.Marshal(append(ops, sidecarGatePatch))
			if err != nil {
				return err
			}
			if ann == nil {
				ann = map[string]string{}
			}
			ann[jsonPatchAnnotation] = string(out)
			hc.SetAnnotations(ann)
			return c.Update(ctx, hc)
		})
		if err != nil {
			return fmt.Errorf("HyperConverged %s: %w", key, err)
		}
	}
	return nil
}
