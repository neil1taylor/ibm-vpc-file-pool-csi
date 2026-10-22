package consoleplugin

import (
	"context"
	"reflect"
	"testing"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func consoleWithPlugins(plugins ...string) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(consoleGVK)
	u.SetName(consoleName)
	if plugins != nil {
		list := make([]interface{}, len(plugins))
		for i, p := range plugins {
			list[i] = p
		}
		_ = unstructured.SetNestedSlice(u.Object, list, "spec", "plugins")
	}
	return u
}

func readPlugins(t *testing.T, cl client.Client) []string {
	t.Helper()
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(consoleGVK)
	if err := cl.Get(context.Background(), client.ObjectKey{Name: consoleName}, u); err != nil {
		t.Fatalf("get console: %v", err)
	}
	list, _, _ := unstructured.NestedStringSlice(u.Object, "spec", "plugins")
	return list
}

func TestRegister(t *testing.T) {
	tests := []struct {
		name   string
		before []string
		want   []string
	}{
		{"no plugins yet", nil, []string{"pool"}},
		{"other plugins are kept", []string{"kubevirt-plugin", "monitoring"}, []string{"kubevirt-plugin", "monitoring", "pool"}},
		{"already registered: no duplicate", []string{"kubevirt-plugin", "pool"}, []string{"kubevirt-plugin", "pool"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithObjects(consoleWithPlugins(tt.before...)).Build()
			if err := Register(context.Background(), cl, "pool"); err != nil {
				t.Fatalf("Register: %v", err)
			}
			if got := readPlugins(t, cl); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("plugins = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestUnregister(t *testing.T) {
	tests := []struct {
		name   string
		before []string
		want   []string
	}{
		{"only ours is removed", []string{"kubevirt-plugin", "pool", "monitoring"}, []string{"kubevirt-plugin", "monitoring"}},
		{"not registered: others untouched", []string{"kubevirt-plugin"}, []string{"kubevirt-plugin"}},
		{"no plugins at all", nil, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cl := fake.NewClientBuilder().WithObjects(consoleWithPlugins(tt.before...)).Build()
			if err := Unregister(context.Background(), cl, "pool"); err != nil {
				t.Fatalf("Unregister: %v", err)
			}
			if got := readPlugins(t, cl); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("plugins = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestRegister_NoConsole(t *testing.T) {
	cl := fake.NewClientBuilder().Build()
	if err := Register(context.Background(), cl, "pool"); err == nil {
		t.Fatal("Register without a console operator config must fail")
	}
}

func TestUnregister_NoConsoleIsFine(t *testing.T) {
	cl := fake.NewClientBuilder().Build()
	if err := Unregister(context.Background(), cl, "pool"); err != nil {
		t.Fatalf("Unregister without a console operator config: %v", err)
	}
}
