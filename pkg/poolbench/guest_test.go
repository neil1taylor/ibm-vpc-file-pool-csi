package poolbench

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf16"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

type execCall struct {
	ns, pod, container string
	cmd                []string
}

type fakeExec struct {
	calls []execCall
	out   map[string]string // by the command's first two words
}

func (f *fakeExec) Exec(_ context.Context, ns, pod, container string, cmd []string) (string, error) {
	f.calls = append(f.calls, execCall{ns, pod, container, cmd})
	return f.out[strings.Join(cmd[:2], " ")], nil
}

func launcherPod(vm string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "virt-launcher-" + vm + "-abcde", Namespace: "poolbench-t10k",
			Labels: map[string]string{"vm.kubevirt.io/name": vm}},
		Status: corev1.PodStatus{Phase: corev1.PodRunning},
	}
}

func vmi(name string, agent bool) *unstructured.Unstructured {
	u := &unstructured.Unstructured{}
	u.SetGroupVersionKind(vmiGVK)
	u.SetName(name)
	u.SetNamespace("poolbench-t10k")
	status := "False"
	if agent {
		status = "True"
	}
	_ = unstructured.SetNestedSlice(u.Object, []any{map[string]any{"type": "AgentConnected", "status": status}}, "status", "conditions")
	return u
}

func decodePS(t *testing.T, b64 string) string {
	t.Helper()
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil || len(raw)%2 != 0 {
		t.Fatalf("not UTF-16LE base64: %v", err)
	}
	u := make([]uint16, len(raw)/2)
	for i := range u {
		u[i] = uint16(raw[2*i]) | uint16(raw[2*i+1])<<8
	}
	return string(utf16.Decode(u))
}

func TestVirshStarter_StartsTheLoad(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(launcherPod("vm-003"), vmi("vm-003", true)).Build()
	ex := &fakeExec{out: map[string]string{"virsh list": "poolbench-t10k_vm-003\n"}}
	s := &VirshStarter{Client: c, Exec: ex}
	if err := s.StartWindowsLoad(context.Background(), "poolbench-t10k", "vm-003"); err != nil {
		t.Fatal(err)
	}
	if len(ex.calls) != 2 {
		t.Fatalf("want 2 execs (find the domain, send the command), got %d", len(ex.calls))
	}
	call := ex.calls[1]
	if call.pod != "virt-launcher-vm-003-abcde" || call.container != "compute" ||
		call.cmd[0] != "virsh" || call.cmd[1] != "qemu-agent-command" || call.cmd[2] != "poolbench-t10k_vm-003" {
		t.Fatalf("exec: %+v", call)
	}
	var req struct {
		Execute   string `json:"execute"`
		Arguments struct {
			Path string   `json:"path"`
			Arg  []string `json:"arg"`
		} `json:"arguments"`
	}
	if err := json.Unmarshal([]byte(call.cmd[3]), &req); err != nil {
		t.Fatal(err)
	}
	if req.Execute != "guest-exec" || !strings.HasSuffix(req.Arguments.Path, `powershell.exe`) {
		t.Fatalf("request: %+v", req)
	}
	script := decodePS(t, req.Arguments.Arg[len(req.Arguments.Arg)-1])
	if !strings.Contains(script, "PERFCFG") || !strings.Contains(script, "load.ps1") {
		t.Fatalf("the script is not start.ps1:\n%s", script)
	}
}

func TestVirshStarter_WaitsForTheAgent(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(launcherPod("vm-003"), vmi("vm-003", false)).Build()
	ex := &fakeExec{}
	err := (&VirshStarter{Client: c, Exec: ex}).StartWindowsLoad(context.Background(), "poolbench-t10k", "vm-003")
	if err == nil || len(ex.calls) != 0 {
		t.Fatalf("want an error and no exec, got %v and %d execs", err, len(ex.calls))
	}
}

func TestVirshStarter_NoPod(t *testing.T) {
	c := fake.NewClientBuilder().WithScheme(testScheme()).WithObjects(vmi("vm-003", true)).Build()
	err := (&VirshStarter{Client: c, Exec: &fakeExec{}}).StartWindowsLoad(context.Background(), "poolbench-t10k", "vm-003")
	if err == nil {
		t.Fatal("want an error without a launcher pod")
	}
}
