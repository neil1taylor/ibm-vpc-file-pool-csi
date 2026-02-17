package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HookType is the type of hook to execute.
// +kubebuilder:validation:Enum=exec;http
type HookType string

const (
	HookTypeExec HookType = "exec"
	HookTypeHTTP HookType = "http"
)

// HookOnError defines the behavior when a hook fails.
// +kubebuilder:validation:Enum=Continue;Abort
type HookOnError string

const (
	HookOnErrorContinue HookOnError = "Continue"
	HookOnErrorAbort    HookOnError = "Abort"
)

// Hook defines a pre or post action to execute around snapshot/replication operations.
type Hook struct {
	// Name is a unique identifier for this hook within the hook list.
	// +kubebuilder:validation:Required
	Name string `json:"name"`

	// Type is the hook execution type: "exec" (pod exec) or "http" (HTTP call).
	// +kubebuilder:validation:Required
	Type HookType `json:"type"`

	// Exec specifies a command to run in a pod container.
	// Required when type is "exec".
	// +optional
	Exec *ExecHookSpec `json:"exec,omitempty"`

	// HTTP specifies an HTTP endpoint to call.
	// Required when type is "http".
	// +optional
	HTTP *HTTPHookSpec `json:"http,omitempty"`

	// TimeoutSeconds is the maximum time to wait for the hook to complete.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=30
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`

	// OnError controls behavior when this hook fails.
	// "Continue" proceeds with the operation; "Abort" stops it.
	// +kubebuilder:default=Abort
	OnError HookOnError `json:"onError,omitempty"`
}

// ExecHookSpec defines a command to execute in a pod container via the Kubernetes exec API.
type ExecHookSpec struct {
	// PodSelector selects pods by labels. The hook runs in the first matching pod.
	// +kubebuilder:validation:Required
	PodSelector map[string]string `json:"podSelector"`

	// Namespace is the namespace to search for matching pods.
	// +kubebuilder:validation:Required
	Namespace string `json:"namespace"`

	// Container is the container name to exec into. If empty, the first container is used.
	// +optional
	Container string `json:"container,omitempty"`

	// Command is the command and arguments to execute.
	// +kubebuilder:validation:MinItems=1
	Command []string `json:"command"`
}

// HTTPHookSpec defines an HTTP request to make as a hook action.
type HTTPHookSpec struct {
	// URL is the HTTP endpoint to call.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:Pattern=`^https?://`
	URL string `json:"url"`

	// Method is the HTTP method. Defaults to POST.
	// +kubebuilder:validation:Enum=GET;POST;PUT
	// +kubebuilder:default=POST
	Method string `json:"method,omitempty"`

	// Headers are additional HTTP headers to include in the request.
	// +optional
	Headers map[string]string `json:"headers,omitempty"`
}

// HookResult records the outcome of a single hook execution.
type HookResult struct {
	// Name is the hook name that was executed.
	Name string `json:"name"`

	// Phase is whether this was a "Pre" or "Post" hook.
	Phase string `json:"phase"`

	// Success indicates whether the hook completed without error.
	Success bool `json:"success"`

	// Message contains details about the hook outcome or error.
	// +optional
	Message string `json:"message,omitempty"`

	// StartedAt is when the hook execution began.
	StartedAt metav1.Time `json:"startedAt"`

	// Duration is how long the hook took as a Go duration string.
	Duration string `json:"duration"`
}
