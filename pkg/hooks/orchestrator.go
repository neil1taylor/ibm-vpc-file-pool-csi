package hooks

import (
	"context"
	"fmt"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"k8s.io/klog/v2"
)

// Orchestrator runs lists of hooks sequentially, respecting OnError policies.
type Orchestrator struct {
	executor ExecutorFunc
}

// NewOrchestrator creates an Orchestrator with real exec and HTTP executors.
func NewOrchestrator(execHook *ExecHook, httpHook *HTTPHook) *Orchestrator {
	return &Orchestrator{
		executor: dispatchExecutor(execHook, httpHook),
	}
}

// NewOrchestratorWithExecutor creates an Orchestrator with a custom executor (for testing).
func NewOrchestratorWithExecutor(executor Executor) *Orchestrator {
	return &Orchestrator{
		executor: func(ctx context.Context, hook v1alpha1.Hook) (*v1alpha1.HookResult, error) {
			return executor.Execute(ctx, hook)
		},
	}
}

// RunPreHooks executes a list of hooks in sequence, labeling results with phase "Pre".
// Returns all results collected so far and an error if any hook with OnError=Abort failed.
func (o *Orchestrator) RunPreHooks(ctx context.Context, hooks []v1alpha1.Hook) ([]v1alpha1.HookResult, error) {
	return o.runHooks(ctx, hooks, "Pre")
}

// RunPostHooks executes a list of hooks in sequence, labeling results with phase "Post".
// Returns all results collected so far and an error if any hook with OnError=Abort failed.
func (o *Orchestrator) RunPostHooks(ctx context.Context, hooks []v1alpha1.Hook) ([]v1alpha1.HookResult, error) {
	return o.runHooks(ctx, hooks, "Post")
}

func (o *Orchestrator) runHooks(ctx context.Context, hooks []v1alpha1.Hook, phase string) ([]v1alpha1.HookResult, error) {
	if len(hooks) == 0 {
		return nil, nil
	}

	var results []v1alpha1.HookResult

	for _, hook := range hooks {
		klog.V(4).InfoS("Running hook", "name", hook.Name, "type", hook.Type, "phase", phase)

		result, err := o.executor(ctx, hook)
		if result != nil {
			result.Phase = phase
			results = append(results, *result)
		}

		if err != nil {
			klog.ErrorS(err, "Hook failed", "name", hook.Name, "phase", phase, "onError", hook.OnError)

			if hook.OnError == v1alpha1.HookOnErrorAbort {
				return results, fmt.Errorf("hook %q (%s) failed: %w", hook.Name, phase, err)
			}
			// OnError=Continue: log and continue to next hook
			continue
		}

		klog.V(4).InfoS("Hook succeeded", "name", hook.Name, "phase", phase)
	}

	return results, nil
}
