package pool

import (
	"context"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func memoryLimitOf(t *testing.T, k *CDILimitKeeper) string {
	t.Helper()
	return nested(readHyperConverged(t, k.client), "spec", "resourceRequirements", "storageWorkloads", "limits", "memory")
}

func waitForMemoryLimit(t *testing.T, k *CDILimitKeeper, want string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for memoryLimitOf(t, k) != want {
		if time.Now().After(deadline) {
			t.Fatalf("memory limit = %q, want %q", memoryLimitOf(t, k), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// The add-on that owns the HyperConverged object can reset the limit later. The keeper must notice and raise it again.
func TestCDILimit_StartChecksAtOnceThenAgainEveryInterval(t *testing.T) {
	hc := hyperConverged(map[string]interface{}{
		"storageWorkloads": map[string]interface{}{
			"limits": map[string]interface{}{"cpu": "750m", "memory": "600M"},
		},
	})
	c := fake.NewClientBuilder().WithScheme(hcoScheme()).WithObjects(hc).Build()
	k := NewCDILimitKeeper(c, "8Gi")
	k.interval = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- k.Start(ctx) }()

	waitForMemoryLimit(t, k, "8Gi")

	// The add-on resets the limit.
	reset := readHyperConverged(t, c)
	if err := unstructured.SetNestedField(reset.Object, "600M", "spec", "resourceRequirements", "storageWorkloads", "limits", "memory"); err != nil {
		t.Fatal(err)
	}
	if err := c.Update(context.Background(), reset); err != nil {
		t.Fatalf("reset the limit: %v", err)
	}
	waitForMemoryLimit(t, k, "8Gi")

	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned %v after the context ended, want nil", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the context ended")
	}
}

func TestCDILimit_StartKeepsRunningAfterACheckFails(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(hcoScheme()).Build()
	k := NewCDILimitKeeper(c, "not-a-size")
	k.interval = 10 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- k.Start(ctx) }()

	select {
	case err := <-done:
		if err != nil {
			t.Errorf("Start returned %v, want nil: a failed check is logged, not fatal", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Start did not return after the context ended")
	}
}
