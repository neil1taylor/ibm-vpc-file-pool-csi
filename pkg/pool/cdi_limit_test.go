package pool

import (
	"context"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	dto "github.com/prometheus/client_model/go"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func hcoScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	s.AddKnownTypeWithName(hyperConvergedGVK, &unstructured.Unstructured{})
	list := hyperConvergedGVK
	list.Kind += "List"
	s.AddKnownTypeWithName(list, &unstructured.UnstructuredList{})
	return s
}

// hyperConverged is an OpenShift Virtualization HyperConverged object with
// the given spec.resourceRequirements.
func hyperConverged(resourceRequirements map[string]interface{}) *unstructured.Unstructured {
	hc := &unstructured.Unstructured{}
	hc.SetGroupVersionKind(hyperConvergedGVK)
	hc.SetNamespace("openshift-cnv")
	hc.SetName("kubevirt-hyperconverged")
	_ = unstructured.SetNestedMap(hc.Object, resourceRequirements, "spec", "resourceRequirements")
	_ = unstructured.SetNestedField(hc.Object, true, "spec", "enableCommonBootImageImport")
	return hc
}

func readHyperConverged(t *testing.T, c client.Client) *unstructured.Unstructured {
	t.Helper()
	hc := &unstructured.Unstructured{}
	hc.SetGroupVersionKind(hyperConvergedGVK)
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: "openshift-cnv", Name: "kubevirt-hyperconverged"}, hc); err != nil {
		t.Fatalf("get HyperConverged: %v", err)
	}
	return hc
}

func nested(hc *unstructured.Unstructured, path ...string) string {
	v, _, _ := unstructured.NestedFieldNoCopy(hc.Object, path...)
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	}
	return ""
}

func TestCDILimit_SetsDefaultsWithTheTargetMemoryWhenUnset(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(hcoScheme()).
		WithObjects(hyperConverged(map[string]interface{}{"vmiCPUAllocationRatio": int64(10)})).Build()
	before := raisedCount(t)

	raised, err := NewCDILimitKeeper(c, "2Gi").EnsureOnce(context.Background())
	if err != nil || !raised {
		t.Fatalf("EnsureOnce() = %v, %v; want true, nil", raised, err)
	}
	hc := readHyperConverged(t, c)
	sw := []string{"spec", "resourceRequirements", "storageWorkloads"}
	for path, want := range map[string]string{
		"limits.memory": "2Gi", "limits.cpu": "750m", "requests.memory": "60M", "requests.cpu": "100m",
	} {
		p := append(append([]string{}, sw...), splitDot(path)...)
		if got := nested(hc, p...); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
	if ratio, _, _ := unstructured.NestedInt64(hc.Object, "spec", "resourceRequirements", "vmiCPUAllocationRatio"); ratio != 10 {
		t.Errorf("vmiCPUAllocationRatio = %d, want 10 (other fields must not change)", ratio)
	}
	if on, _, _ := unstructured.NestedBool(hc.Object, "spec", "enableCommonBootImageImport"); !on {
		t.Error("an unrelated spec field was changed")
	}
	if got := raisedCount(t) - before; got != 1 {
		t.Errorf("raised metric grew by %v, want 1", got)
	}
}

func TestCDILimit_RaisesOnlyTheMemoryLimit(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(hcoScheme()).
		WithObjects(hyperConverged(map[string]interface{}{"storageWorkloads": map[string]interface{}{
			"limits":   map[string]interface{}{"cpu": "1", "memory": "600M"},
			"requests": map[string]interface{}{"cpu": "200m", "memory": "100M"},
		}})).Build()

	raised, err := NewCDILimitKeeper(c, "2Gi").EnsureOnce(context.Background())
	if err != nil || !raised {
		t.Fatalf("EnsureOnce() = %v, %v; want true, nil", raised, err)
	}
	hc := readHyperConverged(t, c)
	for path, want := range map[string]string{
		"limits.memory": "2Gi", "limits.cpu": "1", "requests.memory": "100M", "requests.cpu": "200m",
	} {
		p := append([]string{"spec", "resourceRequirements", "storageWorkloads"}, splitDot(path)...)
		if got := nested(hc, p...); got != want {
			t.Errorf("%s = %q, want %q", path, got, want)
		}
	}
}

func TestCDILimit_LeavesAHigherLimitAlone(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(hcoScheme()).
		WithObjects(hyperConverged(map[string]interface{}{"storageWorkloads": map[string]interface{}{
			"limits": map[string]interface{}{"memory": "4Gi"},
		}})).Build()

	raised, err := NewCDILimitKeeper(c, "2Gi").EnsureOnce(context.Background())
	if err != nil || raised {
		t.Fatalf("EnsureOnce() = %v, %v; want false, nil", raised, err)
	}
	if got := nested(readHyperConverged(t, c), "spec", "resourceRequirements", "storageWorkloads", "limits", "memory"); got != "4Gi" {
		t.Errorf("limits.memory = %q, want 4Gi", got)
	}
}

func TestCDILimit_NoOpenShiftVirtualization(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(runtime.NewScheme()).Build()
	raised, err := NewCDILimitKeeper(c, "2Gi").EnsureOnce(context.Background())
	if err != nil || raised {
		t.Fatalf("EnsureOnce() = %v, %v; want false, nil without the HyperConverged API", raised, err)
	}
}

func TestCDILimit_RejectsABadTarget(t *testing.T) {
	if _, err := NewCDILimitKeeper(nil, "two gigs").EnsureOnce(context.Background()); err == nil {
		t.Error("expected an invalid target to be an error")
	}
}

func raisedCount(t *testing.T) float64 {
	t.Helper()
	var m dto.Metric
	if err := metrics.CDICopyLimitRaisedTotal.Write(&m); err != nil {
		t.Fatalf("read metric: %v", err)
	}
	return m.GetCounter().GetValue()
}

func splitDot(s string) []string {
	for i := range s {
		if s[i] == '.' {
			return []string{s[:i], s[i+1:]}
		}
	}
	return []string{s}
}
