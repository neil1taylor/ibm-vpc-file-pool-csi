package hooks

import (
	"context"
	"fmt"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// mockExecutor records calls and returns configured results.
type mockExecutor struct {
	calls   []string
	results map[string]*v1alpha1.HookResult
	errors  map[string]error
}

func newMockExecutor() *mockExecutor {
	return &mockExecutor{
		results: make(map[string]*v1alpha1.HookResult),
		errors:  make(map[string]error),
	}
}

func (m *mockExecutor) Execute(_ context.Context, hook v1alpha1.Hook) (*v1alpha1.HookResult, error) {
	m.calls = append(m.calls, hook.Name)
	result := m.results[hook.Name]
	if result == nil {
		result = &v1alpha1.HookResult{
			Name:      hook.Name,
			Success:   true,
			Message:   "ok",
			StartedAt: metav1.NewTime(time.Now()),
			Duration:  "1ms",
		}
	}
	return result, m.errors[hook.Name]
}

func TestOrchestrator_RunPreHooks_AllSuccess(t *testing.T) {
	mock := newMockExecutor()
	orch := NewOrchestratorWithExecutor(mock)

	hooks := []v1alpha1.Hook{
		{Name: "hook1", Type: v1alpha1.HookTypeExec, OnError: v1alpha1.HookOnErrorAbort},
		{Name: "hook2", Type: v1alpha1.HookTypeHTTP, OnError: v1alpha1.HookOnErrorAbort},
	}

	results, err := orch.RunPreHooks(context.Background(), hooks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
	for _, r := range results {
		if r.Phase != "Pre" {
			t.Errorf("expected phase Pre, got %s", r.Phase)
		}
	}
	if len(mock.calls) != 2 {
		t.Fatalf("expected 2 calls, got %d", len(mock.calls))
	}
}

func TestOrchestrator_RunPostHooks_AllSuccess(t *testing.T) {
	mock := newMockExecutor()
	orch := NewOrchestratorWithExecutor(mock)

	hooks := []v1alpha1.Hook{
		{Name: "hook1", Type: v1alpha1.HookTypeExec, OnError: v1alpha1.HookOnErrorContinue},
	}

	results, err := orch.RunPostHooks(context.Background(), hooks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
	if results[0].Phase != "Post" {
		t.Errorf("expected phase Post, got %s", results[0].Phase)
	}
}

func TestOrchestrator_AbortOnFailure(t *testing.T) {
	mock := newMockExecutor()
	mock.errors["hook1"] = fmt.Errorf("hook1 failed")
	mock.results["hook1"] = &v1alpha1.HookResult{
		Name:      "hook1",
		Success:   false,
		Message:   "failed",
		StartedAt: metav1.NewTime(time.Now()),
		Duration:  "1ms",
	}

	orch := NewOrchestratorWithExecutor(mock)

	hooks := []v1alpha1.Hook{
		{Name: "hook1", Type: v1alpha1.HookTypeExec, OnError: v1alpha1.HookOnErrorAbort},
		{Name: "hook2", Type: v1alpha1.HookTypeExec, OnError: v1alpha1.HookOnErrorAbort},
	}

	results, err := orch.RunPreHooks(context.Background(), hooks)
	if err == nil {
		t.Fatal("expected error for abort")
	}
	// hook2 should NOT have been called
	if len(mock.calls) != 1 {
		t.Fatalf("expected 1 call (abort stops), got %d", len(mock.calls))
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}
}

func TestOrchestrator_ContinueOnFailure(t *testing.T) {
	mock := newMockExecutor()
	mock.errors["hook1"] = fmt.Errorf("hook1 failed")
	mock.results["hook1"] = &v1alpha1.HookResult{
		Name:      "hook1",
		Success:   false,
		Message:   "failed",
		StartedAt: metav1.NewTime(time.Now()),
		Duration:  "1ms",
	}

	orch := NewOrchestratorWithExecutor(mock)

	hooks := []v1alpha1.Hook{
		{Name: "hook1", Type: v1alpha1.HookTypeExec, OnError: v1alpha1.HookOnErrorContinue},
		{Name: "hook2", Type: v1alpha1.HookTypeExec, OnError: v1alpha1.HookOnErrorAbort},
	}

	results, err := orch.RunPreHooks(context.Background(), hooks)
	if err != nil {
		t.Fatalf("unexpected error (continue policy): %v", err)
	}
	// Both hooks should have been called
	if len(mock.calls) != 2 {
		t.Fatalf("expected 2 calls (continue), got %d", len(mock.calls))
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results, got %d", len(results))
	}
}

func TestOrchestrator_EmptyHooks(t *testing.T) {
	mock := newMockExecutor()
	orch := NewOrchestratorWithExecutor(mock)

	results, err := orch.RunPreHooks(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil results, got %v", results)
	}

	results, err = orch.RunPostHooks(context.Background(), []v1alpha1.Hook{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if results != nil {
		t.Fatalf("expected nil results, got %v", results)
	}
}

func TestOrchestrator_ExecutionOrder(t *testing.T) {
	mock := newMockExecutor()
	orch := NewOrchestratorWithExecutor(mock)

	hooks := []v1alpha1.Hook{
		{Name: "first", Type: v1alpha1.HookTypeExec, OnError: v1alpha1.HookOnErrorContinue},
		{Name: "second", Type: v1alpha1.HookTypeHTTP, OnError: v1alpha1.HookOnErrorContinue},
		{Name: "third", Type: v1alpha1.HookTypeExec, OnError: v1alpha1.HookOnErrorContinue},
	}

	_, err := orch.RunPreHooks(context.Background(), hooks)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(mock.calls) != 3 {
		t.Fatalf("expected 3 calls, got %d", len(mock.calls))
	}
	if mock.calls[0] != "first" || mock.calls[1] != "second" || mock.calls[2] != "third" {
		t.Fatalf("expected order [first, second, third], got %v", mock.calls)
	}
}
