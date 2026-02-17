package hooks

import (
	"context"
	"fmt"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// fakePodFinder returns a fixed pod or error.
type fakePodFinder struct {
	pod *corev1.Pod
	err error
}

func (f *fakePodFinder) FindPod(_ context.Context, _ string, _ map[string]string) (*corev1.Pod, error) {
	return f.pod, f.err
}

// fakePodExecer returns fixed exec results.
type fakePodExecer struct {
	stdout string
	stderr string
	err    error
}

func (f *fakePodExecer) Exec(_ context.Context, _, _, _ string, _ []string) (string, string, error) {
	return f.stdout, f.stderr, f.err
}

func TestExecHook_Execute_Success(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}

	hook := &ExecHook{
		podFinder: &fakePodFinder{pod: pod},
		podExecer: &fakePodExecer{stdout: "quiesced", stderr: ""},
	}

	h := v1alpha1.Hook{
		Name:           "quiesce-db",
		Type:           v1alpha1.HookTypeExec,
		TimeoutSeconds: 10,
		Exec: &v1alpha1.ExecHookSpec{
			PodSelector: map[string]string{"app": "postgres"},
			Namespace:   "default",
			Command:     []string{"pg_start_backup", "snap"},
		},
	}

	result, err := hook.Execute(context.Background(), h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got failure: %s", result.Message)
	}
	if result.Name != "quiesce-db" {
		t.Fatalf("expected hook name quiesce-db, got %s", result.Name)
	}
}

func TestExecHook_Execute_PodNotFound(t *testing.T) {
	hook := &ExecHook{
		podFinder: &fakePodFinder{err: fmt.Errorf("no pod found")},
		podExecer: &fakePodExecer{},
	}

	h := v1alpha1.Hook{
		Name: "quiesce-db",
		Type: v1alpha1.HookTypeExec,
		Exec: &v1alpha1.ExecHookSpec{
			PodSelector: map[string]string{"app": "postgres"},
			Namespace:   "default",
			Command:     []string{"pg_start_backup"},
		},
	}

	result, err := hook.Execute(context.Background(), h)
	if err == nil {
		t.Fatal("expected error for missing pod")
	}
	if result.Success {
		t.Fatal("expected failure result")
	}
}

func TestExecHook_Execute_ExecError(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "app"}}},
	}

	hook := &ExecHook{
		podFinder: &fakePodFinder{pod: pod},
		podExecer: &fakePodExecer{stderr: "permission denied", err: fmt.Errorf("exit code 1")},
	}

	h := v1alpha1.Hook{
		Name: "quiesce-db",
		Type: v1alpha1.HookTypeExec,
		Exec: &v1alpha1.ExecHookSpec{
			PodSelector: map[string]string{"app": "postgres"},
			Namespace:   "default",
			Command:     []string{"pg_start_backup"},
		},
	}

	result, err := hook.Execute(context.Background(), h)
	if err == nil {
		t.Fatal("expected error for exec failure")
	}
	if result.Success {
		t.Fatal("expected failure result")
	}
	if result.Message == "" {
		t.Fatal("expected error message in result")
	}
}

func TestExecHook_Execute_DefaultContainer(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "test-pod", Namespace: "default"},
		Spec: corev1.PodSpec{Containers: []corev1.Container{
			{Name: "main-container"},
			{Name: "sidecar"},
		}},
	}

	execer := &fakePodExecer{stdout: "ok"}
	hook := &ExecHook{
		podFinder: &fakePodFinder{pod: pod},
		podExecer: execer,
	}

	h := v1alpha1.Hook{
		Name: "test",
		Type: v1alpha1.HookTypeExec,
		Exec: &v1alpha1.ExecHookSpec{
			PodSelector: map[string]string{"app": "test"},
			Namespace:   "default",
			Container:   "", // should default to first container
			Command:     []string{"echo", "hello"},
		},
	}

	result, err := hook.Execute(context.Background(), h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success: %s", result.Message)
	}
}
