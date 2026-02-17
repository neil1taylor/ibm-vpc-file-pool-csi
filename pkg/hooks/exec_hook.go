package hooks

import (
	"bytes"
	"context"
	"fmt"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/klog/v2"
)

// PodFinder finds the first pod matching labels in a namespace.
type PodFinder interface {
	FindPod(ctx context.Context, namespace string, labels map[string]string) (*corev1.Pod, error)
}

// PodExecer executes a command in a pod container.
type PodExecer interface {
	Exec(ctx context.Context, namespace, podName, container string, command []string) (stdout, stderr string, err error)
}

// ExecHook executes commands in pod containers via the Kubernetes exec API.
type ExecHook struct {
	podFinder PodFinder
	podExecer PodExecer
}

// NewExecHook creates an ExecHook with real Kubernetes clients.
func NewExecHook(clientset kubernetes.Interface, config *rest.Config) *ExecHook {
	return &ExecHook{
		podFinder: &realPodFinder{clientset: clientset},
		podExecer: &realPodExecer{clientset: clientset, config: config},
	}
}

// NewExecHookWithDeps creates an ExecHook with injectable dependencies for testing.
func NewExecHookWithDeps(finder PodFinder, execer PodExecer) *ExecHook {
	return &ExecHook{
		podFinder: finder,
		podExecer: execer,
	}
}

func (e *ExecHook) Execute(ctx context.Context, hook v1alpha1.Hook) (*v1alpha1.HookResult, error) {
	start := time.Now()
	spec := hook.Exec

	pod, err := e.podFinder.FindPod(ctx, spec.Namespace, spec.PodSelector)
	if err != nil {
		return newHookResult(hook.Name, "", false, fmt.Sprintf("find pod: %v", err), start, time.Since(start)), err
	}

	container := spec.Container
	if container == "" && len(pod.Spec.Containers) > 0 {
		container = pod.Spec.Containers[0].Name
	}

	klog.V(4).InfoS("Executing hook in pod",
		"hook", hook.Name,
		"pod", pod.Name,
		"namespace", spec.Namespace,
		"container", container,
		"command", spec.Command,
	)

	stdout, stderr, err := e.podExecer.Exec(ctx, spec.Namespace, pod.Name, container, spec.Command)
	duration := time.Since(start)

	if err != nil {
		msg := fmt.Sprintf("exec failed: %v; stderr: %s", err, stderr)
		return newHookResult(hook.Name, "", false, msg, start, duration), err
	}

	msg := stdout
	if len(msg) > 256 {
		msg = msg[:256] + "..."
	}
	return newHookResult(hook.Name, "", true, msg, start, duration), nil
}

// realPodFinder finds pods using the Kubernetes API.
type realPodFinder struct {
	clientset kubernetes.Interface
}

func (f *realPodFinder) FindPod(ctx context.Context, namespace string, labels map[string]string) (*corev1.Pod, error) {
	selector := metav1.FormatLabelSelector(&metav1.LabelSelector{MatchLabels: labels})
	pods, err := f.clientset.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{
		LabelSelector: selector,
		Limit:         1,
	})
	if err != nil {
		return nil, fmt.Errorf("list pods in %s with labels %v: %w", namespace, labels, err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("no pod found in %s matching labels %v", namespace, labels)
	}
	return &pods.Items[0], nil
}

// realPodExecer executes commands via the Kubernetes exec API.
type realPodExecer struct {
	clientset kubernetes.Interface
	config    *rest.Config
}

func (e *realPodExecer) Exec(ctx context.Context, namespace, podName, container string, command []string) (string, string, error) {
	req := e.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Namespace(namespace).
		Name(podName).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(e.config, "POST", req.URL())
	if err != nil {
		return "", "", fmt.Errorf("create executor: %w", err)
	}

	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return stdout.String(), stderr.String(), err
	}
	return stdout.String(), stderr.String(), nil
}
