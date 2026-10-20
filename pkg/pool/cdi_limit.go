package pool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DefaultCDILimitInterval is how often the CDI copy limit is checked.
const DefaultCDILimitInterval = 5 * time.Minute

var hyperConvergedGVK = schema.GroupVersionKind{Group: "hco.kubevirt.io", Version: "v1beta1", Kind: "HyperConverged"}

// cdiDefaultWorkloads are CDI's own pod requests and limits, used when the
// HyperConverged object sets none. Only the memory limit is raised.
var cdiDefaultWorkloads = map[string]interface{}{
	"limits":   map[string]interface{}{"cpu": "750m"},
	"requests": map[string]interface{}{"cpu": "100m", "memory": "60M"},
}

// CDILimitKeeper keeps the memory limit of CDI's copy pods at or above a
// target. CDI's default of 600 MB is too small for copies onto NFS: the
// file cache counts against the limit and the pod is killed.
type CDILimitKeeper struct {
	client   client.Client
	target   string
	interval time.Duration
}

// NewCDILimitKeeper creates a keeper that raises the limit to target, such as "2Gi".
func NewCDILimitKeeper(c client.Client, target string) *CDILimitKeeper {
	return &CDILimitKeeper{client: c, target: target, interval: DefaultCDILimitInterval}
}

// EnsureOnce raises the CDI copy pod memory limit in every HyperConverged
// object where it is missing or below the target. It changes no other field.
// It reports whether it raised a limit.
func (k *CDILimitKeeper) EnsureOnce(ctx context.Context) (bool, error) {
	target, err := resource.ParseQuantity(k.target)
	if err != nil {
		return false, fmt.Errorf("CDI copy pod memory limit %q: %w", k.target, err)
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(hyperConvergedGVK.GroupVersion().WithKind(hyperConvergedGVK.Kind + "List"))
	if err := k.client.List(ctx, list); err != nil {
		if isNoMatchError(err) {
			return false, nil
		}
		return false, fmt.Errorf("listing HyperConverged: %w", err)
	}

	raised := false
	for i := range list.Items {
		hc := &list.Items[i]
		workloads, found, _ := unstructured.NestedMap(hc.Object, "spec", "resourceRequirements", "storageWorkloads")
		current, _, _ := unstructured.NestedString(hc.Object, "spec", "resourceRequirements", "storageWorkloads", "limits", "memory")
		if q, err := resource.ParseQuantity(current); err == nil && q.Cmp(target) >= 0 {
			continue
		}
		if !found {
			workloads = runtime.DeepCopyJSON(cdiDefaultWorkloads)
		}
		limits, _, _ := unstructured.NestedMap(workloads, "limits")
		if limits == nil {
			limits = map[string]interface{}{}
		}
		limits["memory"] = k.target
		workloads["limits"] = limits

		patch, _ := json.Marshal(map[string]interface{}{
			"spec": map[string]interface{}{"resourceRequirements": map[string]interface{}{"storageWorkloads": workloads}},
		})
		obj := &unstructured.Unstructured{}
		obj.SetGroupVersionKind(hyperConvergedGVK)
		obj.SetNamespace(hc.GetNamespace())
		obj.SetName(hc.GetName())
		if err := k.client.Patch(ctx, obj, client.RawPatch(types.MergePatchType, patch)); err != nil {
			return raised, fmt.Errorf("raising CDI copy pod memory limit in %s/%s: %w", hc.GetNamespace(), hc.GetName(), err)
		}
		raised = true
		metrics.CDICopyLimitRaisedTotal.Inc()
		klog.InfoS("Raised the CDI copy pod memory limit; if this repeats, something resets it",
			"hyperConverged", hc.GetNamespace()+"/"+hc.GetName(), "from", current, "to", k.target)
	}
	return raised, nil
}

// Start checks the limit now and then every interval until ctx ends. It
// implements manager.Runnable, so it runs only on the leader.
func (k *CDILimitKeeper) Start(ctx context.Context) error {
	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	for {
		if _, err := k.EnsureOnce(ctx); err != nil {
			klog.ErrorS(err, "Failed to check the CDI copy pod memory limit")
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}
