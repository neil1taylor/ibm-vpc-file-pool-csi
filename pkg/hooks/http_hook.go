package hooks

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"k8s.io/klog/v2"
)

// HTTPHook executes HTTP requests as hooks.
type HTTPHook struct {
	client *http.Client
}

// NewHTTPHook creates an HTTPHook with the given HTTP client.
// If client is nil, a default client with a 30s timeout is used.
func NewHTTPHook(client *http.Client) *HTTPHook {
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	return &HTTPHook{client: client}
}

func (h *HTTPHook) Execute(ctx context.Context, hook v1alpha1.Hook) (*v1alpha1.HookResult, error) {
	start := time.Now()
	spec := hook.HTTP

	method := spec.Method
	if method == "" {
		method = http.MethodPost
	}

	klog.V(4).InfoS("Executing HTTP hook",
		"hook", hook.Name,
		"method", method,
		"url", spec.URL,
	)

	req, err := http.NewRequestWithContext(ctx, method, spec.URL, nil)
	if err != nil {
		return newHookResult(hook.Name, "", false, fmt.Sprintf("create request: %v", err), start, time.Since(start)), err
	}

	for k, v := range spec.Headers {
		req.Header.Set(k, v)
	}

	resp, err := h.client.Do(req)
	if err != nil {
		duration := time.Since(start)
		return newHookResult(hook.Name, "", false, fmt.Sprintf("HTTP %s %s: %v", method, spec.URL, err), start, duration), err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
	duration := time.Since(start)

	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		msg := fmt.Sprintf("HTTP %d", resp.StatusCode)
		if len(body) > 0 {
			bodyStr := string(body)
			if len(bodyStr) > 256 {
				bodyStr = bodyStr[:256] + "..."
			}
			msg += ": " + bodyStr
		}
		return newHookResult(hook.Name, "", true, msg, start, duration), nil
	}

	msg := fmt.Sprintf("HTTP %d: %s", resp.StatusCode, string(body))
	return newHookResult(hook.Name, "", false, msg, start, duration), fmt.Errorf("hook %q: HTTP %s returned %d", hook.Name, method, resp.StatusCode)
}
