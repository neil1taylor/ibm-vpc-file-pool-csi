package pool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmsnap"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// An instant snapshot of a VM disk is a new top file in the disk's chain
// (pkg/vmsnap): nothing is copied, and the snapshot is ready when
// CreateSnapshot returns. The design is in
// docs/specs/2026-10-21-instant-vm-snapshots.md.

const (
	// snapshotMethodInstant marks an instant snapshot, in snapshotMethodAnnotation.
	snapshotMethodInstant = "instant"

	// DiskTopsAnnotation on a VirtualMachineInstance maps each pod volume
	// with a chain to its top file, as JSON. The VM hook opens that file.
	DiskTopsAnnotation = "storage.ibmcloud.io/disk-tops"

	// VMHookConfigMap holds the hook script, in every namespace with a VM
	// that has instant snapshots.
	VMHookConfigMap = "ibm-vpc-file-pool-vm-hook"
	VMHookScriptKey = "hook.py"

	// HookSidecarsAnnotation is KubeVirt's hook sidecar annotation.
	HookSidecarsAnnotation = "hooks.kubevirt.io/hookSidecars"

	// sidecarFeatureGate is the KubeVirt feature gate that hooks need.
	sidecarFeatureGate = "Sidecar"
)

// VMHookScript is the onDefineDomain hook. KubeVirt runs it in the VM pod
// before the VM starts. It opens each disk named in the disk-tops annotation
// as the top qcow2 file of its chain instead of the raw disk.img.
const VMHookScript = `#!/usr/bin/env python3
# Written by the ibm-vpc-file-pool-csi driver. See VM-INSTANT-SNAPSHOTS.md.
import json, re, sys
args = dict(zip(sys.argv[1::2], sys.argv[2::2]))
vmi, domain = json.loads(args["--vmi"]), args["--domain"]
ann = vmi.get("metadata", {}).get("annotations", {})
tops = json.loads(ann.get("` + DiskTopsAnnotation + `", "{}"))
name_ok = re.compile(r"^snap-[a-z0-9]{1,32}\.qcow2$")
def fix(m):
    d = m.group(0)
    src = re.search(r"/var/run/kubevirt-private/vmi-disks/([a-z0-9-]+)/disk\.img", d)
    top = tops.get(src.group(1), "") if src else ""
    if not name_ok.match(top):
        return d
    sys.stderr.write("vm-disk-hook: %s opens %s\n" % (src.group(1), top))
    d = d.replace(src.group(0), "/var/run/kubevirt-private/vmi-disks/%s/%s" % (src.group(1), top))
    return re.sub(r'(<driver\b[^>]*\btype=)"raw"', r'\1"qcow2"', d)
print(re.sub(r"<disk\b.*?</disk>", fix, domain, flags=re.S), end="")
`

// VMHookSidecar is the entry of KubeVirt's hookSidecars annotation for the hook.
var VMHookSidecar = map[string]any{
	"args":      []string{"--version", "v1alpha2"},
	"configMap": map[string]any{"name": VMHookConfigMap, "key": VMHookScriptKey, "hookPath": "/usr/bin/onDefineDomain"},
}

var (
	kubevirtListGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "KubeVirtList"}

	vmiObjGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"}
)

// InstantSnapshotter changes the chain of a running VM's disk. pkg/vmsnap
// implements it.
type InstantSnapshotter interface {
	Take(ctx context.Context, vm vmcopy.VM, volume, newFile string, freeze bool) (*vmsnap.Result, error)
	Merge(ctx context.Context, vm vmcopy.VM, volume, file, child string) ([]vmsnap.File, error)
	Chain(ctx context.Context, vm vmcopy.VM, volume string) ([]vmsnap.File, error)
}

// ChainJobs runs a pkg/vmsnap script in a Job, as the QEMU user, in an image
// that has qemu-img. initScript, if set, runs first as root. Folder i is
// mounted at /vol<i>, and nothing else of the shares, so a file planted in a
// volume folder cannot reach another volume. It reports done=true once the
// Job has finished; it is idempotent for a key.
type ChainJobs interface {
	RunChainScript(ctx context.Context, key string, folders []ShareFolder, script, initScript string) (bool, error)
}

type instantSnapshots struct {
	snap InstantSnapshotter
	jobs ChainJobs
	kube client.Client
}

// SetInstantSnapshots turns on instant snapshots of VM disks. Without it,
// every VM disk snapshot is a copy.
func (m *Manager) SetInstantSnapshots(s InstantSnapshotter, jobs ChainJobs, kube client.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.instant = &instantSnapshots{snap: s, jobs: jobs, kube: kube}
}

// instantFileName is the chain file that a snapshot starts. The same snapshot
// always gets the same name, so a repeated call finds its own file.
func instantFileName(snapshotName string) string {
	sum := sha256.Sum256([]byte(snapshotName))
	return "snap-" + hex.EncodeToString(sum[:])[:16] + ".qcow2"
}

// instantChargeGB is the share space an instant snapshot is charged: a tenth
// of the disk, at least 1 GB. The file grows with the changes after it.
func instantChargeGB(sizeGB int64) int64 {
	if c := (sizeGB + 9) / 10; c > 1 {
		return c
	}
	return 1
}

// instantPlan says how to take an instant snapshot of a volume.
type instantPlan struct {
	pod    *corev1.Pod // the running VM's pod; nil for a stopped VM
	volume string      // the pod or VM volume name of the disk
}

// planInstant decides whether a snapshot of sv can be instant. It returns a
// plan, or a reason why not when the volume is a VM disk, or neither when the
// volume is not a VM disk.
func (m *Manager) planInstant(ctx context.Context, sv *v1alpha1.SubVolume) (*instantPlan, string, error) {
	in := m.instant
	pod, vol, err := findVMPodForClaim(ctx, in.kube, sv.Spec.PVCNamespace, sv.Spec.PVCName)
	if err != nil {
		return nil, "", fmt.Errorf("check for a running VM: %w", err)
	}
	if pod == nil {
		vmName, vmVol, vmErr := findVMForClaim(ctx, in.kube, sv.Spec.PVCNamespace, sv.Spec.PVCName)
		if vmErr != nil {
			return nil, "", vmErr
		}
		if vmName == "" {
			return nil, "", nil
		}
		vol = vmVol
	} else if pod.Labels[kubevirtPodLabel] != "virt-launcher" {
		return nil, "", fmt.Errorf("%w: %s/%s is a hot-plugged disk of a running VM; stop the VM or detach the disk",
			ErrSnapshotUnsupported, sv.Spec.PVCNamespace, sv.Spec.PVCName)
	}
	on, err := SidecarGateOn(ctx, in.kube)
	if err != nil {
		return nil, "", err
	}
	if !on {
		return nil, "the KubeVirt Sidecar feature gate is off, so the snapshot is a copy", nil
	}
	return &instantPlan{pod: pod, volume: vol}, "", nil
}

// SidecarGateOn reports whether a KubeVirt resource turns the Sidecar feature
// gate on.
func SidecarGateOn(ctx context.Context, c client.Reader) (bool, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(kubevirtListGVK)
	if err := c.List(ctx, list); err != nil {
		return false, fmt.Errorf("read the KubeVirt feature gates: %w", err)
	}
	for _, kv := range list.Items {
		gates, _, _ := unstructured.NestedStringSlice(kv.Object, "spec", "configuration", "developerConfiguration", "featureGates")
		for _, g := range gates {
			if g == sidecarFeatureGate {
				return true, nil
			}
		}
	}
	return false, nil
}

// findVMForClaim returns the VirtualMachine whose spec uses the claim, and the
// VM's volume name for it, or "" when there is none.
func findVMForClaim(ctx context.Context, c client.Client, namespace, claim string) (string, string, error) {
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(vmListGVK)
	if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
		return "", "", fmt.Errorf("listing VMs in %s: %w", namespace, err)
	}
	for _, vm := range list.Items {
		vols, _, _ := unstructured.NestedSlice(vm.Object, "spec", "template", "spec", "volumes")
		for _, v := range vols {
			vol, _ := v.(map[string]any)
			name, _ := vol["name"].(string)
			if claimOfVolume(vol) == claim && name != "" {
				return vm.GetName(), name, nil
			}
		}
	}
	return "", "", nil
}

// claimOfVolume returns the claim of a KubeVirt volume, or "".
func claimOfVolume(vol map[string]any) string {
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

func (m *Manager) getVMI(ctx context.Context, namespace, name string) (*unstructured.Unstructured, error) {
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(vmiObjGVK)
	if err := m.instant.kube.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, vmi); err != nil {
		return nil, err
	}
	return vmi, nil
}

// guestFrozen reports whether OpenShift has frozen the VM's guest, as it does
// for a VirtualMachineSnapshot.
func (m *Manager) guestFrozen(ctx context.Context, namespace, vmi string) bool {
	obj, err := m.getVMI(ctx, namespace, vmi)
	if err != nil {
		return false
	}
	s, _, _ := unstructured.NestedString(obj.Object, "status", "fsFreezeStatus")
	return s == "frozen"
}

// setDiskTop records volume's top file on the running VMI, for its next start.
// An empty top removes the entry. A failure is logged: the next snapshot or
// merge writes it again, and a VM that starts without it fails safe.
func (m *Manager) setDiskTop(ctx context.Context, namespace, vmiName, volume, top string) {
	vmi, err := m.getVMI(ctx, namespace, vmiName)
	if err != nil {
		klog.ErrorS(err, "Failed to read the VMI to record its disk top", "vmi", klog.KRef(namespace, vmiName))
		return
	}
	base := vmi.DeepCopy()
	ann := vmi.GetAnnotations()
	tops := map[string]string{}
	if v := ann[DiskTopsAnnotation]; v != "" {
		_ = json.Unmarshal([]byte(v), &tops)
	}
	if top == "" {
		delete(tops, volume)
	} else {
		tops[volume] = top
	}
	if ann == nil {
		ann = map[string]string{}
	}
	if len(tops) == 0 {
		delete(ann, DiskTopsAnnotation)
	} else {
		b, _ := json.Marshal(tops)
		ann[DiskTopsAnnotation] = string(b)
	}
	vmi.SetAnnotations(ann)
	if err := m.instant.kube.Patch(ctx, vmi, client.MergeFrom(base)); err != nil {
		klog.ErrorS(err, "Failed to record the disk top on the VMI", "vmi", klog.KRef(namespace, vmiName))
	}
}

// ensureVMHook makes or updates the hook ConfigMap in namespace.
func (m *Manager) ensureVMHook(ctx context.Context, namespace string) error {
	cm := &corev1.ConfigMap{}
	err := m.instant.kube.Get(ctx, types.NamespacedName{Namespace: namespace, Name: VMHookConfigMap}, cm)
	switch {
	case apierrors.IsNotFound(err):
		cm = &corev1.ConfigMap{
			ObjectMeta: metav1.ObjectMeta{Name: VMHookConfigMap, Namespace: namespace,
				Labels: map[string]string{"app.kubernetes.io/managed-by": "ibm-vpc-file-pool-csi"}},
			Data: map[string]string{VMHookScriptKey: VMHookScript},
		}
		if err = m.instant.kube.Create(ctx, cm); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create the VM hook ConfigMap in %s: %w", namespace, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("read the VM hook ConfigMap in %s: %w", namespace, err)
	case cm.Data[VMHookScriptKey] != VMHookScript:
		if cm.Data == nil {
			cm.Data = map[string]string{}
		}
		cm.Data[VMHookScriptKey] = VMHookScript
		if err = m.instant.kube.Update(ctx, cm); err != nil {
			return fmt.Errorf("update the VM hook ConfigMap in %s: %w", namespace, err)
		}
	}
	return nil
}

// chainFiles turns a chain of names into files.
func chainFiles(names []string) []vmsnap.File {
	out := make([]vmsnap.File, len(names))
	for i, n := range names {
		out[i] = vmsnap.File{Name: n, Format: vmsnap.FormatOf(n)}
	}
	return out
}

// chainNames returns the names of a chain, or nil for a plain disk.
func chainNames(chain []vmsnap.File) []string {
	if len(chain) <= 1 {
		return nil
	}
	out := make([]string, len(chain))
	for i, f := range chain {
		out[i] = f.Name
	}
	return out
}

// diskChainOf returns a SubVolume's chain, disk.img alone for a plain disk.
func diskChainOf(sv *v1alpha1.SubVolume) []string {
	if len(sv.Status.DiskChain) == 0 {
		return []string{vmsnap.BaseFile}
	}
	return sv.Status.DiskChain
}

func indexOfName(chain []string, name string) int {
	for i, n := range chain {
		if n == name {
			return i
		}
	}
	return -1
}

// saveDiskChain records a SubVolume's chain on a fresh copy of the SubVolume.
func (m *Manager) saveDiskChain(ctx context.Context, name string, chain []string, operation string) error {
	sv, err := m.k8sClient.GetSubVolume(ctx, name)
	if err != nil {
		return fmt.Errorf("read SubVolume %s: %w", name, err)
	}
	sv.Status.DiskChain = chain
	sv.Status.ChainOperation = operation
	if err = m.k8sClient.UpdateSubVolumeStatus(ctx, sv); err != nil {
		return fmt.Errorf("record the disk chain of %s: %w", name, err)
	}
	return nil
}

// markInstantReady records a taken snapshot.
func (m *Manager) markInstantReady(ctx context.Context, snap *v1alpha1.Snapshot, res *vmsnap.Result) error {
	snap.Status.Phase = "Ready"
	snap.Status.ReadyToUse = true
	snap.Status.ChainFile = res.Frozen
	if !res.Moment.IsZero() {
		t := metav1.NewTime(res.Moment)
		snap.Status.CreationTime = &t
	}
	moment := *snap.Status.CreationTime
	snap.Status.CopyMoment = &moment
	snap.Status.Consistency, snap.Status.ConsistencyReason = consistencyFileSystem, ""
	if !res.FileSystemConsistent {
		snap.Status.Consistency, snap.Status.ConsistencyReason = consistencyCrash, res.FreezeError
	}
	snap.Status.Message = ""
	if err := m.k8sClient.UpdateSnapshotStatus(ctx, snap); err != nil {
		return fmt.Errorf("mark snapshot ready: %w", err)
	}
	return nil
}

// takeInstant takes or goes on with an instant snapshot. It returns nil once
// the snapshot is ready or a Job is still running (the snapshot is not ready
// yet then), or an error to retry.
func (m *Manager) takeInstant(ctx context.Context, snap *v1alpha1.Snapshot, sv *v1alpha1.SubVolume) error {
	newFile := instantFileName(snap.Name)
	key := "chain-" + snap.Name
	if sv.Status.ChainOperation == key {
		return m.takeInstantOffline(ctx, snap, sv, key, newFile)
	}
	plan, reason, err := m.planInstant(ctx, sv)
	if err != nil {
		return err
	}
	if plan == nil {
		return fmt.Errorf("%w: the snapshot cannot be instant now: %s", ErrOperationInProgress, orDefault(reason, "no VM uses the disk"))
	}
	if plan.pod == nil {
		return m.takeInstantOffline(ctx, snap, sv, key, newFile)
	}
	pod := plan.pod
	vmi := vmiName(pod)
	if vmi == "" {
		return fmt.Errorf("VM pod %s/%s has no VirtualMachineInstance owner", pod.Namespace, pod.Name)
	}
	if err = m.ensureVMHook(ctx, pod.Namespace); err != nil {
		return err
	}
	target := vmcopy.VM{Namespace: pod.Namespace, Pod: pod.Name, Name: vmi}
	freeze := !m.guestFrozen(ctx, pod.Namespace, vmi)
	res, err := m.instant.snap.Take(ctx, target, plan.volume, newFile, freeze)
	if errors.Is(err, vmsnap.ErrChainFull) {
		m.failVMSnapshot(ctx, snap, err.Error())
		return fmt.Errorf("%w: %s", ErrSnapshotFailed, err)
	}
	if err != nil {
		return fmt.Errorf("take the instant snapshot: %w", err)
	}
	if err = m.saveDiskChain(ctx, sv.Name, chainNames(res.Chain), ""); err != nil {
		return err
	}
	m.setDiskTop(ctx, pod.Namespace, vmi, plan.volume, newFile)
	if err = m.markInstantReady(ctx, snap, res); err != nil {
		return err
	}
	klog.V(2).InfoS("Took an instant snapshot", "snapshot", snap.Name, "vm", klog.KRef(pod.Namespace, vmi),
		"volume", plan.volume, "file", newFile, "moment", snap.Status.CreationTime.Time, "consistency", snap.Status.Consistency)
	return nil
}

// takeInstantOffline adds newFile to the chain of a stopped VM's disk with a
// Job. While the Job runs, the SubVolume names it, so the VM is not started.
func (m *Manager) takeInstantOffline(ctx context.Context, snap *v1alpha1.Snapshot, sv *v1alpha1.SubVolume, key, newFile string) error {
	chain := diskChainOf(sv)
	if chain[0] != newFile {
		if len(chain) > vmsnap.MaxSnapshots {
			m.failVMSnapshot(ctx, snap, vmsnap.ErrChainFull.Error())
			return fmt.Errorf("%w: %s", ErrSnapshotFailed, vmsnap.ErrChainFull)
		}
		if sv.Status.ChainOperation != key {
			if sv.Status.ChainOperation != "" {
				return fmt.Errorf("%w: %s changes the disk now", ErrOperationInProgress, sv.Status.ChainOperation)
			}
			if err := m.saveDiskChain(ctx, sv.Name, sv.Status.DiskChain, key); err != nil {
				return err
			}
			// The VM may have started before the mark was saved.
			if pod, _, err := findVMPodForClaim(ctx, m.instant.kube, sv.Spec.PVCNamespace, sv.Spec.PVCName); err != nil || pod != nil {
				if clearErr := m.saveDiskChain(ctx, sv.Name, sv.Status.DiskChain, ""); clearErr != nil {
					return clearErr
				}
				return fmt.Errorf("%w: the VM started", ErrOperationInProgress)
			}
		}
		script, err := vmsnap.OfflineTakeScript("/vol0", chainFiles(chain)[0], newFile)
		if err != nil {
			return err
		}
		done, err := m.instant.jobs.RunChainScript(ctx, key, []ShareFolder{{Share: subVolumeShareMount(sv), Path: sv.Spec.SubPath}}, script, "")
		if err != nil {
			return fmt.Errorf("run the snapshot Job: %w", err)
		}
		if !done {
			return nil
		}
		chain = append([]string{newFile}, chain...)
	}
	if err := m.saveDiskChain(ctx, sv.Name, chain, ""); err != nil {
		return err
	}
	return m.markInstantReady(ctx, snap, &vmsnap.Result{Moment: m.timeNow(), Frozen: chain[1], FileSystemConsistent: true})
}

// deleteInstant merges an instant snapshot away. It returns nil once the
// Snapshot CR may be deleted, ErrOperationInProgress while a Job or a VM move
// runs, or another error to retry.
func (m *Manager) deleteInstant(ctx context.Context, snap *v1alpha1.Snapshot, pool *v1alpha1.FileSharePool) error {
	if snap.Status.ChainFile == "" {
		return nil // never taken
	}
	sv, err := m.k8sClient.GetSubVolume(ctx, snap.Spec.SourceSubVolume)
	if err != nil {
		return m.deleteInstantOfGoneVolume(ctx, snap, pool)
	}
	if busy, busyErr := m.chainRestoreRunning(ctx, snap.Spec.PoolName, sv.Name); busyErr != nil {
		return busyErr
	} else if busy {
		return fmt.Errorf("%w: a restore reads the disk's snapshots", ErrOperationInProgress)
	}
	chain := diskChainOf(sv)
	file := snap.Status.ChainFile
	child := snap.Status.MergeChild
	if child == "" {
		fi := indexOfName(chain, file)
		if fi < 1 {
			klog.ErrorS(nil, "An instant snapshot's file is not in its disk's chain; deleting the record only",
				"snapshot", snap.Name, "file", file, "chain", chain)
			return nil
		}
		child = chain[fi-1]
		snap.Status.MergeChild = child
		snap.Status.Phase = "Deleting"
		if err = m.k8sClient.UpdateSnapshotStatus(ctx, snap); err != nil {
			return fmt.Errorf("record the merge: %w", err)
		}
	}
	key := "chain-rm-" + snap.Name
	var newChain []string
	if sv.Status.ChainOperation == key {
		newChain, err = m.mergeOffline(ctx, sv, key, file, child)
	} else {
		var pod *corev1.Pod
		var vol string
		pod, vol, err = findVMPodForClaim(ctx, m.instant.kube, sv.Spec.PVCNamespace, sv.Spec.PVCName)
		switch {
		case err != nil:
			return fmt.Errorf("check for a running VM: %w", err)
		case pod == nil:
			newChain, err = m.mergeOffline(ctx, sv, key, file, child)
		default:
			newChain, err = m.mergeRunning(ctx, pod, vol, file, child)
		}
	}
	if err != nil {
		return err
	}
	if newChain == nil {
		return ErrOperationInProgress // the Job runs
	}
	// The snapshot that was held in child is held in file now.
	others, err := m.k8sClient.ListSnapshotsBySource(ctx, snap.Spec.SourceSubVolume)
	if err != nil {
		return fmt.Errorf("list the disk's snapshots: %w", err)
	}
	for i := range others {
		o := &others[i]
		if o.Name != snap.Name && o.Status.ChainFile == child {
			o.Status.ChainFile = file
			if err = m.k8sClient.UpdateSnapshotStatus(ctx, o); err != nil {
				return fmt.Errorf("move snapshot %s to %s: %w", o.Name, file, err)
			}
		}
	}
	return m.saveDiskChain(ctx, sv.Name, chainNamesOf(newChain), "")
}

func chainNamesOf(chain []string) []string {
	if len(chain) <= 1 {
		return nil
	}
	return chain
}

// mergeRunning merges child into file in a running VM.
func (m *Manager) mergeRunning(ctx context.Context, pod *corev1.Pod, vol, file, child string) ([]string, error) {
	vmi := vmiName(pod)
	moving, err := vmInstanceMoving(ctx, m.instant.kube, pod.Namespace, vmi)
	if err != nil {
		return nil, err
	}
	if moving {
		return nil, fmt.Errorf("%w: the VM is moving to another worker", ErrOperationInProgress)
	}
	chain, err := m.instant.snap.Merge(ctx, vmcopy.VM{Namespace: pod.Namespace, Pod: pod.Name, Name: vmi}, vol, file, child)
	if err != nil {
		return nil, fmt.Errorf("merge the snapshot: %w", err)
	}
	m.setDiskTop(ctx, pod.Namespace, vmi, vol, topOrEmpty(chain))
	names := make([]string, len(chain))
	for i, f := range chain {
		names[i] = f.Name
	}
	return names, nil
}

// topOrEmpty is the top file of a chain, or "" for a plain disk.
func topOrEmpty(chain []vmsnap.File) string {
	if len(chain) <= 1 {
		return ""
	}
	return chain[0].Name
}

// mergeOffline merges child into file with a Job. It returns a nil chain
// while the Job runs.
func (m *Manager) mergeOffline(ctx context.Context, sv *v1alpha1.SubVolume, key, file, child string) ([]string, error) {
	chain := diskChainOf(sv)
	ci := indexOfName(chain, child)
	if ci < 0 {
		// Merged already; the chain was saved without child.
		return chain, nil
	}
	grandchild := ""
	if ci > 0 {
		grandchild = chain[ci-1]
	}
	if sv.Status.ChainOperation != key {
		if sv.Status.ChainOperation != "" {
			return nil, fmt.Errorf("%w: %s changes the disk now", ErrOperationInProgress, sv.Status.ChainOperation)
		}
		if err := m.saveDiskChain(ctx, sv.Name, sv.Status.DiskChain, key); err != nil {
			return nil, err
		}
		if pod, _, err := findVMPodForClaim(ctx, m.instant.kube, sv.Spec.PVCNamespace, sv.Spec.PVCName); err != nil || pod != nil {
			if clearErr := m.saveDiskChain(ctx, sv.Name, sv.Status.DiskChain, ""); clearErr != nil {
				return nil, clearErr
			}
			return nil, fmt.Errorf("%w: the VM started", ErrOperationInProgress)
		}
	}
	script, err := vmsnap.OfflineMergeScript("/vol0", file, child, grandchild)
	if err != nil {
		return nil, err
	}
	done, err := m.instant.jobs.RunChainScript(ctx, key, []ShareFolder{{Share: subVolumeShareMount(sv), Path: sv.Spec.SubPath}}, script, "")
	if err != nil {
		return nil, fmt.Errorf("run the merge Job: %w", err)
	}
	if !done {
		return nil, nil
	}
	out := append(append([]string{}, chain[:ci]...), chain[ci+1:]...)
	return out, nil
}

// deleteInstantOfGoneVolume deletes a snapshot whose disk was deleted. Its
// files stay for the disk's other snapshots; the last one removes the folder.
func (m *Manager) deleteInstantOfGoneVolume(ctx context.Context, snap *v1alpha1.Snapshot, pool *v1alpha1.FileSharePool) error {
	others, err := m.k8sClient.ListSnapshotsBySource(ctx, snap.Spec.SourceSubVolume)
	if err != nil {
		return fmt.Errorf("list the disk's snapshots: %w", err)
	}
	for _, o := range others {
		if o.Name != snap.Name {
			return nil
		}
	}
	share, err := poolShareMount(pool, snap.Spec.ShareID)
	if err != nil {
		return err
	}
	done, err := m.shareData.RemoveDir(ctx, "rm-"+snap.Spec.SourceSubVolume, share, snap.Spec.SourceSubPath)
	if err != nil {
		return fmt.Errorf("remove the deleted disk's folder: %w", err)
	}
	if !done {
		return ErrOperationInProgress
	}
	return nil
}

// hasInstantSnapshots reports whether any instant snapshot was taken of a SubVolume.
func (m *Manager) hasInstantSnapshots(ctx context.Context, svName string) (bool, error) {
	snaps, err := m.k8sClient.ListSnapshotsBySource(ctx, svName)
	if err != nil {
		return false, fmt.Errorf("list the volume's snapshots: %w", err)
	}
	for _, s := range snaps {
		if s.Annotations[snapshotMethodAnnotation] == snapshotMethodInstant && s.Status.ChainFile != "" {
			return true, nil
		}
	}
	return false, nil
}

// chainRestoreRunning reports whether a restore from any snapshot of a disk
// still reads the disk's chain.
func (m *Manager) chainRestoreRunning(ctx context.Context, poolName, svName string) (bool, error) {
	svs, err := m.k8sClient.ListSubVolumes(ctx, poolName)
	if err != nil {
		return false, fmt.Errorf("list SubVolumes: %w", err)
	}
	for _, r := range svs {
		snapName := r.Labels[restoreSnapshotLabel]
		if snapName == "" || r.Status.Phase == "Bound" {
			continue
		}
		s, err := m.k8sClient.GetSnapshot(ctx, snapName)
		if err == nil && s.Spec.SourceSubVolume == svName && s.Annotations[snapshotMethodAnnotation] == snapshotMethodInstant {
			return true, nil
		}
	}
	return false, nil
}

// restoreInstant converts an instant snapshot's file into the restored
// volume's disk.img with a Job.
func (m *Manager) restoreInstant(ctx context.Context, sv *v1alpha1.SubVolume, snap *v1alpha1.Snapshot, src ShareMount) (bool, error) {
	if m.instant == nil {
		return false, errors.New("instant snapshots are not configured")
	}
	if source, err := m.k8sClient.GetSubVolume(ctx, snap.Spec.SourceSubVolume); err == nil && source.Status.ChainOperation != "" {
		return false, nil // a Job changes the chain now
	}
	folders := []ShareFolder{{Share: src, Path: snap.Spec.SourceSubPath}, {Share: subVolumeShareMount(sv), Path: sv.Spec.SubPath}}
	script, err := vmsnap.ConvertScript("/vol0", snap.Status.ChainFile, "/vol1")
	if err != nil {
		return false, err
	}
	// The kubelet makes the new folder for the mount. The share squashes
	// root, so root cannot give the folder to the QEMU user; it opens the
	// folder to all, as the snapshot apply Job does. KubeVirt gives it to the
	// QEMU user when a VM starts.
	return m.instant.jobs.RunChainScript(ctx, "restore-"+sv.Name, folders, script, "set -eu\nchmod 0777 /vol1\n")
}

// snapshotChargeGB is the share space a snapshot is charged.
func snapshotChargeGB(snap *v1alpha1.Snapshot) int64 {
	if snap.Annotations[snapshotMethodAnnotation] == snapshotMethodInstant {
		return instantChargeGB(snap.Spec.SizeGB)
	}
	return snap.Spec.SizeGB
}

// deleteInstantSnapshot merges an instant snapshot away, then deletes its
// record and gives its space back.
func (m *Manager) deleteInstantSnapshot(ctx context.Context, snap *v1alpha1.Snapshot) error {
	if m.instant == nil {
		return errors.New("instant snapshots are not configured")
	}
	if m.shareData == nil {
		return errShareDataNotSet
	}
	pool, err := m.k8sClient.GetFileSharePool(ctx, snap.Spec.PoolName)
	if err != nil {
		return fmt.Errorf("%w: %s", ErrPoolNotFound, snap.Spec.PoolName)
	}
	if err = m.deleteInstant(ctx, snap, pool); err != nil {
		return err
	}
	if err = m.k8sClient.DeleteSnapshot(ctx, snap.Name); err != nil {
		return fmt.Errorf("delete Snapshot CR: %w", err)
	}
	m.updateShareDeallocation(pool, snap.Spec.ShareID, snapshotChargeGB(snap))
	if err := m.k8sClient.UpdateFileSharePoolStatus(ctx, pool); err != nil {
		klog.ErrorS(err, "Failed to update pool status after snapshot deletion", "pool", pool.Name)
	}
	klog.V(2).InfoS("Deleted instant snapshot", "snapshot", snap.Name, "pool", pool.Name)
	return nil
}
