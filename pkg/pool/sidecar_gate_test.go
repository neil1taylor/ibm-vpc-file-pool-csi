package pool

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

const testGateOp = `{"op":"add","path":"/spec/configuration/developerConfiguration/featureGates/-","value":"Sidecar"}`

// hcoWithJSONPatch is a HyperConverged object with the given jsonpatch
// annotation (none when empty) and one other annotation.
func hcoWithJSONPatch(jsonpatch string) *unstructured.Unstructured {
	hc := hyperConverged(nil)
	ann := map[string]string{"other": "kept"}
	if jsonpatch != "" {
		ann[jsonPatchAnnotation] = jsonpatch
	}
	hc.SetAnnotations(ann)
	return hc
}

func readJSONPatch(t *testing.T, c client.Client) []any {
	t.Helper()
	u := readHyperConverged(t, c)
	var ops []any
	if err := json.Unmarshal([]byte(u.GetAnnotations()[jsonPatchAnnotation]), &ops); err != nil {
		t.Fatalf("annotation is not JSON: %v", err)
	}
	return ops
}

func mustJSON(t *testing.T, s string) []any {
	t.Helper()
	var v []any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestEnableSidecarGate(t *testing.T) {
	other := `{"op":"add","path":"/spec/configuration/cpuModel","value":"host-passthrough"}`
	reordered := `{"value":"Sidecar","op":"add","path":"/spec/configuration/developerConfiguration/featureGates/-"}`
	tests := []struct {
		name   string
		before string
		want   string
	}{
		{"no annotation", "", "[" + testGateOp + "]"},
		{"empty list", "[]", "[" + testGateOp + "]"},
		{"other entries are kept", "[" + other + "]", "[" + other + "," + testGateOp + "]"},
		{"already there: no duplicate", "[" + other + "," + testGateOp + "]", "[" + other + "," + testGateOp + "]"},
		{"already there in another key order: no duplicate", "[" + reordered + "]", "[" + reordered + "]"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := fake.NewClientBuilder().WithScheme(hcoScheme()).WithObjects(hcoWithJSONPatch(tt.before)).Build()
			if err := EnableSidecarGate(context.Background(), c); err != nil {
				t.Fatalf("EnableSidecarGate: %v", err)
			}
			if got := readJSONPatch(t, c); !reflect.DeepEqual(got, mustJSON(t, tt.want)) {
				t.Errorf("jsonpatch = %v, want %s", got, tt.want)
			}
		})
	}
}

func TestEnableSidecarGate_KeepsOtherAnnotations(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(hcoScheme()).WithObjects(hcoWithJSONPatch("[]")).Build()
	if err := EnableSidecarGate(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	u := readHyperConverged(t, c)
	if u.GetAnnotations()["other"] != "kept" {
		t.Errorf("annotations = %v, want the other annotation kept", u.GetAnnotations())
	}
}

func TestEnableSidecarGate_InvalidAnnotationIsLeftAlone(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(hcoScheme()).WithObjects(hcoWithJSONPatch("not json")).Build()
	if err := EnableSidecarGate(context.Background(), c); err == nil {
		t.Fatal("an annotation that is not JSON must be an error, not overwritten")
	}
	u := readHyperConverged(t, c)
	if got := u.GetAnnotations()[jsonPatchAnnotation]; got != "not json" {
		t.Errorf("annotation = %q, want it unchanged", got)
	}
}

func TestEnableSidecarGate_NoHyperConverged(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(hcoScheme()).Build()
	if err := EnableSidecarGate(context.Background(), c); err == nil {
		t.Fatal("a cluster without OpenShift Virtualization must be an error")
	}
}

// The chart gives the gate Job get and update on the one HyperConverged
// object only, so the Job must not need to list HyperConverged objects.
func TestEnableSidecarGate_NeedsNoListRight(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(hcoScheme()).WithObjects(hcoWithJSONPatch("")).
		WithInterceptorFuncs(interceptor.Funcs{
			List: func(ctx context.Context, c client.WithWatch, list client.ObjectList, opts ...client.ListOption) error {
				return apierrors.NewForbidden(schema.GroupResource{Group: "hco.kubevirt.io", Resource: "hyperconvergeds"}, "", nil)
			},
		}).Build()
	if err := EnableSidecarGate(context.Background(), c); err != nil {
		t.Fatalf("EnableSidecarGate: %v", err)
	}
	if got := readJSONPatch(t, c); !reflect.DeepEqual(got, mustJSON(t, "["+testGateOp+"]")) {
		t.Errorf("jsonpatch = %v, want the gate entry", got)
	}
}
