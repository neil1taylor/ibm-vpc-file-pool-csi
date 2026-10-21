// Command pool-snapshots is a kubectl plugin for the instant VM snapshots of
// the pool driver (see VM-INSTANT-SNAPSHOTS.md).
//
// Usage:
//
//	kubectl pool-snapshots flatten <vm> [-n namespace] [--yes]
//
// flatten deletes every snapshot of the VM's pool disks. Each delete merges
// the snapshot's file back, so afterwards every disk is one plain disk.img
// again and the VM starts without the driver's hook, for example when the
// KubeVirt Sidecar feature gate has been turned off.
package main

import (
	"bufio"
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

var version = "dev"

func gvk(group, ver, kind string) schema.GroupVersionKind {
	return schema.GroupVersionKind{Group: group, Version: ver, Kind: kind}
}

var (
	vmGVK         = gvk("kubevirt.io", "v1", "VirtualMachine")
	vmSnapListGVK = gvk("snapshot.kubevirt.io", "v1beta1", "VirtualMachineSnapshotList")
	vsListGVK     = gvk("snapshot.storage.k8s.io", "v1", "VolumeSnapshotList")
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "flatten" {
		fmt.Fprintf(os.Stderr, "kubectl pool-snapshots %s\n\nUsage:\n  kubectl pool-snapshots flatten <vm> [-n namespace] [--yes]\n", version)
		os.Exit(2)
	}
	if err := runFlatten(os.Args[2:], os.Stdin, os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func runFlatten(args []string, in io.Reader, out io.Writer) error {
	fs := flag.NewFlagSet("flatten", flag.ContinueOnError)
	ns := fs.String("n", "", "namespace of the VM (default: the current namespace)")
	yes := fs.Bool("yes", false, "do not ask for confirmation")
	timeout := fs.Duration("timeout", 30*time.Minute, "how long to wait for the merges")
	var vmName string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		vmName, args = args[0], args[1:]
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	if vmName == "" && fs.NArg() == 1 {
		vmName = fs.Arg(0)
	}
	if vmName == "" {
		return fmt.Errorf("name the VM: kubectl pool-snapshots flatten <vm>")
	}
	loader := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(clientcmd.NewDefaultClientConfigLoadingRules(), &clientcmd.ConfigOverrides{})
	if *ns == "" {
		cur, _, err := loader.Namespace()
		if err != nil {
			return err
		}
		*ns = cur
	}
	cfg, err := loader.ClientConfig()
	if err != nil {
		return err
	}
	s := runtime.NewScheme()
	_ = corev1.AddToScheme(s)
	_ = v1alpha1.AddToScheme(s)
	c, err := client.New(cfg, client.Options{Scheme: s})
	if err != nil {
		return err
	}
	ctx := context.Background()
	plan, err := planFlatten(ctx, c, *ns, vmName)
	if err != nil {
		return err
	}
	if len(plan.disks) == 0 {
		_, _ = fmt.Fprintf(out, "No disk of VM %s/%s has instant snapshots. Nothing to do.\n", *ns, vmName)
		return nil
	}
	_, _ = fmt.Fprintf(out, "VM %s/%s: disks with instant snapshots: %s\n", *ns, vmName, strings.Join(plan.disks, ", "))
	_, _ = fmt.Fprintf(out, "These snapshots will be deleted. Their data is merged into the disks and cannot be restored:\n")
	for _, n := range plan.vmSnapshots {
		_, _ = fmt.Fprintf(out, "  VirtualMachineSnapshot %s\n", n)
	}
	for _, n := range plan.volumeSnapshots {
		_, _ = fmt.Fprintf(out, "  VolumeSnapshot %s\n", n)
	}
	if !*yes {
		_, _ = fmt.Fprintf(out, "Type the VM name (%s) to go on: ", vmName)
		if !confirm(in, out, vmName) {
			return fmt.Errorf("not confirmed; nothing was deleted")
		}
	}
	if err = deletePlan(ctx, c, *ns, plan); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Deleted. Waiting for the merges...\n")
	deadline := time.Now().Add(*timeout)
	for {
		left, err := planFlatten(ctx, c, *ns, vmName)
		if err != nil {
			return err
		}
		if len(left.disks) == 0 {
			_, _ = fmt.Fprintf(out, "Done: every disk of %s/%s is a plain disk.img.\n", *ns, vmName)
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("disks %s still have snapshot files after %s; see the VolumeSnapshot events", strings.Join(left.disks, ", "), *timeout)
		}
		time.Sleep(5 * time.Second)
	}
}

// flattenPlan is what flatten deletes.
type flattenPlan struct {
	disks           []string // VM volumes whose pool disk has a chain
	vmSnapshots     []string // VirtualMachineSnapshots of the VM
	volumeSnapshots []string // VolumeSnapshots of those disks that no VM snapshot owns
}

func planFlatten(ctx context.Context, c client.Client, ns, vmName string) (*flattenPlan, error) {
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: vmName}, vm); err != nil {
		return nil, fmt.Errorf("read VM %s/%s: %w", ns, vmName, err)
	}
	vols, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
	plan := &flattenPlan{}
	claims := map[string]bool{}
	for _, v := range vols {
		vol, _ := v.(map[string]any)
		name, _ := vol["name"].(string)
		claim := claimOf(vol)
		if claim == "" {
			continue
		}
		pvc := &corev1.PersistentVolumeClaim{}
		if err := c.Get(ctx, client.ObjectKey{Namespace: ns, Name: claim}, pvc); err != nil || pvc.Spec.VolumeName == "" {
			continue
		}
		sv := &v1alpha1.SubVolume{}
		if err := c.Get(ctx, client.ObjectKey{Name: pvc.Spec.VolumeName}, sv); err != nil {
			if apierrors.IsNotFound(err) {
				continue // not a pool disk
			}
			return nil, err
		}
		if len(sv.Status.DiskChain) > 1 {
			plan.disks = append(plan.disks, name)
			claims[claim] = true
		}
	}
	vmSnaps := &unstructured.UnstructuredList{}
	vmSnaps.SetGroupVersionKind(vmSnapListGVK)
	if err := c.List(ctx, vmSnaps, client.InNamespace(ns)); err != nil && !isNoKind(err) {
		return nil, fmt.Errorf("list VM snapshots: %w", err)
	}
	for _, s := range vmSnaps.Items {
		kind, _, _ := unstructured.NestedString(s.Object, "spec", "source", "kind")
		src, _, _ := unstructured.NestedString(s.Object, "spec", "source", "name")
		if kind == "VirtualMachine" && src == vmName {
			plan.vmSnapshots = append(plan.vmSnapshots, s.GetName())
		}
	}
	vss := &unstructured.UnstructuredList{}
	vss.SetGroupVersionKind(vsListGVK)
	if err := c.List(ctx, vss, client.InNamespace(ns)); err != nil && !isNoKind(err) {
		return nil, fmt.Errorf("list VolumeSnapshots: %w", err)
	}
	for _, s := range vss.Items {
		claim, _, _ := unstructured.NestedString(s.Object, "spec", "source", "persistentVolumeClaimName")
		if !claims[claim] || ownedByVMSnapshot(&s) {
			continue
		}
		plan.volumeSnapshots = append(plan.volumeSnapshots, s.GetName())
	}
	sort.Strings(plan.disks)
	sort.Strings(plan.vmSnapshots)
	sort.Strings(plan.volumeSnapshots)
	return plan, nil
}

func ownedByVMSnapshot(u *unstructured.Unstructured) bool {
	for _, o := range u.GetOwnerReferences() {
		if o.Kind == "VirtualMachineSnapshotContent" {
			return true
		}
	}
	return false
}

func deletePlan(ctx context.Context, c client.Client, ns string, plan *flattenPlan) error {
	del := func(kind schema.GroupVersionKind, name string) error {
		u := &unstructured.Unstructured{}
		u.SetGroupVersionKind(kind)
		u.SetNamespace(ns)
		u.SetName(name)
		if err := c.Delete(ctx, u); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete %s %s: %w", kind.Kind, name, err)
		}
		return nil
	}
	for _, n := range plan.vmSnapshots {
		if err := del(gvk("snapshot.kubevirt.io", "v1beta1", "VirtualMachineSnapshot"), n); err != nil {
			return err
		}
	}
	for _, n := range plan.volumeSnapshots {
		if err := del(gvk("snapshot.storage.k8s.io", "v1", "VolumeSnapshot"), n); err != nil {
			return err
		}
	}
	return nil
}

func confirm(in io.Reader, out io.Writer, want string) bool {
	line, _ := bufio.NewReader(in).ReadString('\n')
	ok := strings.TrimSpace(line) == want
	if !ok {
		fmt.Fprintln(out)
	}
	return ok
}

func claimOf(vol map[string]any) string {
	if pvc, ok := vol["persistentVolumeClaim"].(map[string]any); ok {
		s, _ := pvc["claimName"].(string)
		return s
	}
	if dv, ok := vol["dataVolume"].(map[string]any); ok {
		s, _ := dv["name"].(string)
		return s
	}
	return ""
}

// isNoKind reports a resource type the cluster does not have.
func isNoKind(err error) bool {
	return strings.Contains(err.Error(), "no matches for kind")
}
