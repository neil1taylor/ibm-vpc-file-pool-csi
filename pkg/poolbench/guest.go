package poolbench

import (
	"context"
	_ "embed" // start.ps1
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf16"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed guest/windows/start.ps1
var windowsStartScript string

// Executor runs a command in a container of a pod and returns what it printed.
type Executor interface {
	Exec(ctx context.Context, namespace, pod, container string, cmd []string) (string, error)
}

// VirshStarter starts the load of a Windows VM by sending start.ps1 to the
// VM's guest agent, with virsh in the VM's launcher pod. A Windows start-up
// task would be simpler, but on this image it stayed "Queued" and never ran.
type VirshStarter struct {
	Client client.Client
	Exec   Executor
}

// powershellCommand is the guest-exec request that runs a script with
// PowerShell. The script goes base64-encoded in UTF-16LE, as -EncodedCommand takes it.
func powershellCommand(script string) string {
	units := utf16.Encode([]rune("$ProgressPreference = \"SilentlyContinue\"\n" + script))
	raw := make([]byte, 0, 2*len(units))
	for _, u := range units {
		raw = append(raw, byte(u), byte(u>>8))
	}
	req := map[string]any{
		"execute": "guest-exec",
		"arguments": map[string]any{
			"path":           `C:\Windows\System32\WindowsPowerShell\v1.0\powershell.exe`,
			"arg":            []string{"-NoProfile", "-ExecutionPolicy", "Bypass", "-EncodedCommand", base64.StdEncoding.EncodeToString(raw)},
			"capture-output": false,
		},
	}
	out, _ := json.Marshal(req) // a map of strings always marshals
	return string(out)
}

// StartWindowsLoad sends start.ps1 to the VM. It fails while the VM's guest
// agent is not connected; the runner tries again on its next pass. The
// script does nothing if the load already runs.
func (s *VirshStarter) StartWindowsLoad(ctx context.Context, namespace, vm string) error {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(vmiGVK)
	if err := s.Client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: vm}, vmi); err != nil {
		return fmt.Errorf("read the VM instance: %w", err)
	}
	if !agentConnected(vmi) {
		return errors.New("the guest agent is not connected yet")
	}
	pods := &corev1.PodList{}
	if err := s.Client.List(ctx, pods, client.InNamespace(namespace), client.MatchingLabels{"vm.kubevirt.io/name": vm}); err != nil {
		return fmt.Errorf("find the launcher pod: %w", err)
	}
	pod := ""
	for _, p := range pods.Items {
		if p.Status.Phase == corev1.PodRunning {
			pod = p.Name
		}
	}
	if pod == "" {
		return errors.New("the VM has no running launcher pod")
	}
	out, err := s.Exec.Exec(ctx, namespace, pod, "compute", []string{"virsh", "list", "--name"})
	if err != nil {
		return fmt.Errorf("find the domain: %w", err)
	}
	domain := strings.TrimSpace(strings.SplitN(out, "\n", 2)[0])
	if domain == "" {
		return errors.New("the launcher pod runs no domain")
	}
	if _, err := s.Exec.Exec(ctx, namespace, pod, "compute",
		[]string{"virsh", "qemu-agent-command", domain, powershellCommand(windowsStartScript)}); err != nil {
		return fmt.Errorf("send the start script: %w", err)
	}
	return nil
}

func agentConnected(vmi *unstructured.Unstructured) bool {
	conds, _, _ := unstructured.NestedSlice(vmi.Object, "status", "conditions")
	for _, c := range conds {
		m, ok := c.(map[string]any)
		if ok && m["type"] == "AgentConnected" && m["status"] == "True" {
			return true
		}
	}
	return false
}
