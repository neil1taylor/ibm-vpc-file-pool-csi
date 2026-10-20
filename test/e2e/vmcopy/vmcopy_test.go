//go:build e2e

// Package vmcopy_test runs pkg/vmcopy against a real KubeVirt VM.
//
// Set KUBECONFIG and VMCOPY_NAMESPACE, VMCOPY_VMI and VMCOPY_VOLUMES (comma
// separated pod volume names of the VM's filesystem disks). The VM must be
// running and have a guest agent. The test leaves no staging files behind.
//
//	go test -tags e2e ./test/e2e/vmcopy/ -v -timeout 60m
package vmcopy_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/hooks"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
)

func setup(t *testing.T) (*vmcopy.Copier, hooks.PodExecer, vmcopy.VM, []string) {
	t.Helper()
	ns, vmi, vols := os.Getenv("VMCOPY_NAMESPACE"), os.Getenv("VMCOPY_VMI"), os.Getenv("VMCOPY_VOLUMES")
	if ns == "" || vmi == "" || vols == "" {
		t.Skip("VMCOPY_NAMESPACE, VMCOPY_VMI and VMCOPY_VOLUMES are not set")
	}
	cfg, err := clientcmd.BuildConfigFromFlags("", os.Getenv("KUBECONFIG"))
	if err != nil {
		t.Fatal(err)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	pods, err := cs.CoreV1().Pods(ns).List(context.Background(), metav1.ListOptions{
		LabelSelector: "kubevirt.io=virt-launcher,vmi.kubevirt.io/id=" + vmi,
	})
	if err != nil {
		t.Fatal(err)
	}
	var pod string
	for _, p := range pods.Items {
		if p.Status.Phase == "Running" {
			pod = p.Name
		}
	}
	if pod == "" {
		t.Fatalf("no running virt-launcher pod for %s/%s", ns, vmi)
	}
	ex := hooks.NewPodExecer(cs, cfg)
	return vmcopy.NewCopier(ex), ex, vmcopy.VM{Namespace: ns, Pod: pod, Name: vmi}, strings.Split(vols, ",")
}

func waitFor(t *testing.T, c *vmcopy.Copier, vm vmcopy.VM, cp *vmcopy.Copy, timeout time.Duration) vmcopy.State {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		st, msg, err := c.Progress(context.Background(), vm, cp)
		if err != nil {
			t.Fatalf("Progress: %v", err)
		}
		if st != vmcopy.Running {
			if msg != "" {
				t.Logf("Progress: %s: %s", st, msg)
			}
			return st
		}
		time.Sleep(5 * time.Second)
	}
	t.Fatalf("copy still running after %v", timeout)
	return vmcopy.Running
}

func run(t *testing.T, ex hooks.PodExecer, vm vmcopy.VM, args ...string) string {
	t.Helper()
	out, stderr, err := ex.Exec(context.Background(), vm.Namespace, vm.Pod, "compute", args)
	if err != nil {
		t.Fatalf("%v: %v: %s", args, err, stderr)
	}
	return out
}

func TestFullCopy(t *testing.T) {
	c, ex, vm, vols := setup(t)
	ctx := context.Background()
	if err := c.RemoveStale(ctx, vm, vols); err != nil {
		t.Fatalf("RemoveStale: %v", err)
	}

	id := fmt.Sprintf("e2e%x", time.Now().Unix())
	start := time.Now()
	cp, err := c.Start(ctx, vm, vols, 0, id)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("Start took %v; guest paused %v; moment %v; file-system consistent %v %s", time.Since(start), cp.Paused, cp.Moment, cp.FileSystemConsistent, cp.FreezeError)
	if !cp.FileSystemConsistent {
		t.Errorf("expected a file-system consistent copy: %s", cp.FreezeError)
	}
	defer func() {
		if err := c.Finish(ctx, vm, cp, true); err != nil {
			t.Errorf("Finish(remove): %v", err)
		}
	}()

	if st := waitFor(t, c, vm, cp, 45*time.Minute); st != vmcopy.Done {
		t.Fatalf("copy ended %v", st)
	}
	t.Logf("copy took %v", time.Since(start))
	if err := c.Finish(ctx, vm, cp, false); err != nil {
		t.Fatalf("Finish(keep): %v", err)
	}
	for _, d := range cp.Disks {
		p := "/var/run/kubevirt-private/vmi-disks/" + d.Volume + "/" + d.StagingFile
		t.Logf("%s: %s", d.Volume, strings.Join(strings.Fields(run(t, ex, vm, "qemu-img", "info", "-U", p)), " "))
		run(t, ex, vm, "qemu-img", "check", "-U", p)
	}
}

func TestCancel(t *testing.T) {
	c, ex, vm, vols := setup(t)
	ctx := context.Background()
	if err := c.RemoveStale(ctx, vm, vols); err != nil {
		t.Fatalf("RemoveStale: %v", err)
	}
	cp, err := c.Start(ctx, vm, vols, 1<<20, fmt.Sprintf("e2e%x", time.Now().Unix()))
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Logf("guest paused %v", cp.Paused)
	time.Sleep(10 * time.Second)
	start := time.Now()
	if err := c.Cancel(ctx, vm, cp); err != nil {
		t.Fatalf("Cancel: %v", err)
	}
	t.Logf("Cancel took %v", time.Since(start))
	if out := run(t, ex, vm, "virsh", "qemu-monitor-command", vm.Namespace+"_"+vm.Name, `{"execute":"query-jobs"}`); strings.Contains(out, "repl-") {
		t.Errorf("copy jobs left after Cancel: %s", out)
	}
	for _, d := range cp.Disks {
		out := run(t, ex, vm, "ls", "-a", "/var/run/kubevirt-private/vmi-disks/"+d.Volume)
		if strings.Contains(out, vmcopy.StagingPrefix) {
			t.Errorf("staging file left in %s: %s", d.Volume, out)
		}
	}
}
