package hooks

import (
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// Executor runs a single hook and returns the result.
type Executor interface {
	Execute(ctx context.Context, hook v1alpha1.Hook) (*v1alpha1.HookResult, error)
}

// ExecutorFunc adapts a function to the Executor interface.
type ExecutorFunc func(ctx context.Context, hook v1alpha1.Hook) (*v1alpha1.HookResult, error)

func (f ExecutorFunc) Execute(ctx context.Context, hook v1alpha1.Hook) (*v1alpha1.HookResult, error) {
	return f(ctx, hook)
}

// newHookResult creates a HookResult with the given parameters.
func newHookResult(name, phase string, success bool, message string, start time.Time, duration time.Duration) *v1alpha1.HookResult {
	return &v1alpha1.HookResult{
		Name:      name,
		Phase:     phase,
		Success:   success,
		Message:   message,
		StartedAt: metav1.NewTime(start),
		Duration:  duration.String(),
	}
}

// dispatchExecutor selects the right executor based on hook type.
func dispatchExecutor(execExecutor, httpExecutor Executor) ExecutorFunc {
	return func(ctx context.Context, hook v1alpha1.Hook) (*v1alpha1.HookResult, error) {
		timeout := time.Duration(hook.TimeoutSeconds) * time.Second
		if timeout <= 0 {
			timeout = 30 * time.Second
		}
		ctx, cancel := context.WithTimeout(ctx, timeout)
		defer cancel()

		switch hook.Type {
		case v1alpha1.HookTypeExec:
			if hook.Exec == nil {
				return nil, fmt.Errorf("hook %q: type is exec but exec spec is nil", hook.Name)
			}
			return execExecutor.Execute(ctx, hook)
		case v1alpha1.HookTypeHTTP:
			if hook.HTTP == nil {
				return nil, fmt.Errorf("hook %q: type is http but http spec is nil", hook.Name)
			}
			return httpExecutor.Execute(ctx, hook)
		default:
			return nil, fmt.Errorf("hook %q: unknown type %q", hook.Name, hook.Type)
		}
	}
}
