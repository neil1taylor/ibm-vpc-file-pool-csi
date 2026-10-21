package pool

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// A snapshot of a PVC that a running VM uses is made with a QEMU copy, not a
// file copy, so it shows one moment and the guest file system is consistent.
// The design is in docs/specs/2026-10-21-vm-safe-snapshots.md.

const (
	// snapshotMethodAnnotation says how a Snapshot is copied: "vm" for a
	// QEMU copy that AdvanceVMSnapshots drives, "file" for a copy of the
	// volume folder.
	snapshotMethodAnnotation = "storage.ibmcloud.io/snapshot-method"
	snapshotMethodVM         = "vm"
	snapshotMethodFile       = "file"

	// snapshotSpeedAnnotation keeps the speed limit (MB/s) that the
	// VolumeSnapshotClass gave, because the copy loop does not see the class.
	snapshotSpeedAnnotation = "storage.ibmcloud.io/vm-copy-speed-mbps"

	// snapshotSpeedParam is the VolumeSnapshotClass parameter.
	snapshotSpeedParam = "vmCopySpeedLimitMBps"

	// maxVMSnapshotAttempts copies may be cancelled or fail before the
	// snapshot fails.
	maxVMSnapshotAttempts = 3

	// vmSnapshotRetryDelay is the wait before the next try.
	vmSnapshotRetryDelay = time.Minute

	// vmSnapshotMinDeadline is the shortest time a copy may run.
	vmSnapshotMinDeadline = 2 * time.Hour
)

// ErrInvalidSnapshotParameter means a VolumeSnapshotClass parameter has a bad value.
var ErrInvalidSnapshotParameter = errors.New("invalid snapshot parameter")

// vmSnapshots is what the Manager needs to snapshot running VMs.
type vmSnapshots struct {
	copier VMCopier
	kube   client.Client
}

// SetVMSnapshots turns on QEMU copies for snapshots of running VM disks.
// Without it, every snapshot is a file copy.
func (m *Manager) SetVMSnapshots(copier VMCopier, kube client.Client) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.vm = &vmSnapshots{copier: copier, kube: kube}
}

func (m *Manager) timeNow() time.Time {
	if m.vmNow != nil {
		return m.vmNow()
	}
	return time.Now()
}

// snapshotSpeed checks the speed parameter of a snapshot class. It returns ""
// when there is none.
func snapshotSpeed(params map[string]string) (string, error) {
	v, ok := params[snapshotSpeedParam]
	if !ok || v == "" {
		return "", nil
	}
	n, err := strconv.Atoi(v)
	if err != nil || n < 1 {
		return "", fmt.Errorf("%w: %s must be a whole number of at least 1, got %q", ErrInvalidSnapshotParameter, snapshotSpeedParam, v)
	}
	return v, nil
}

// vmSnapshotDeadline is how long one copy try may run: at least two hours, and
// twice the time the disk needs at the speed limit.
func vmSnapshotDeadline(sizeGB, speedMBps int64) time.Duration {
	if speedMBps < 1 {
		speedMBps = defaultVMCopySpeedLimitMBps
	}
	return vmSnapshotMinDeadline + time.Duration(2*sizeGB*1024/speedMBps)*time.Second
}

func snapshotSpeedMBps(snap *v1alpha1.Snapshot) int64 {
	if n, err := strconv.ParseInt(snap.Annotations[snapshotSpeedAnnotation], 10, 64); err == nil && n >= 1 {
		return n
	}
	return defaultVMCopySpeedLimitMBps
}

// runningVMPodForSnapshot returns the VM pod that uses the snapshot's source
// volume, or nil, and the SubVolume.
func (m *Manager) runningVMPodForSnapshot(ctx context.Context, vm *vmSnapshots, snap *v1alpha1.Snapshot) (*corev1.Pod, string, *v1alpha1.SubVolume, error) {
	sv, err := m.k8sClient.GetSubVolume(ctx, snap.Spec.SourceSubVolume)
	if err != nil {
		return nil, "", nil, fmt.Errorf("get source SubVolume %s: %w", snap.Spec.SourceSubVolume, err)
	}
	pod, vol, err := findVMPodForClaim(ctx, vm.kube, sv.Spec.PVCNamespace, sv.Spec.PVCName)
	if err != nil {
		return nil, "", sv, fmt.Errorf("check for a running VM: %w", err)
	}
	return pod, vol, sv, nil
}

// switchToVMPath is called when a file copy has finished. If a VM started
// during the copy, the copy may mix moments, so the snapshot moves to the VM
// path and the finished folder is replaced by the apply Job.
func (m *Manager) switchToVMPath(ctx context.Context, snap *v1alpha1.Snapshot) (bool, error) {
	if m.vm == nil {
		return false, nil
	}
	pod, _, _, err := m.runningVMPodForSnapshot(ctx, m.vm, snap)
	if err != nil {
		return false, err
	}
	if pod == nil {
		return false, nil
	}
	klog.V(2).InfoS("A VM started during a snapshot file copy; copying again with QEMU", "snapshot", snap.Name, "vmPod", klog.KObj(pod))
	if err := m.setSnapshotMethod(ctx, snap, snapshotMethodVM); err != nil {
		return false, err
	}
	snap.Status.Message = "a VM started during the copy; copying again with QEMU"
	return true, m.k8sClient.UpdateSnapshotStatus(ctx, snap)
}

// setSnapshotMethod records how the Snapshot is copied.
func (m *Manager) setSnapshotMethod(ctx context.Context, snap *v1alpha1.Snapshot, method string) error {
	if snap.Annotations == nil {
		snap.Annotations = map[string]string{}
	}
	snap.Annotations[snapshotMethodAnnotation] = method
	if err := m.k8sClient.UpdateSnapshot(ctx, snap); err != nil {
		return fmt.Errorf("record the snapshot method: %w", err)
	}
	return nil
}

// DefaultVMSnapshotInterval is how often the controller moves VM snapshot
// copies forward.
const DefaultVMSnapshotInterval = 10 * time.Second

// RunVMSnapshots calls AdvanceVMSnapshots every interval until ctx ends.
func (m *Manager) RunVMSnapshots(ctx context.Context, interval time.Duration) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.AdvanceVMSnapshots(ctx)
		}
	}
}

// AdvanceVMSnapshots moves every snapshot of a running VM disk forward by one
// step: it starts the QEMU copy, watches it, and starts and watches the Job
// that writes the disk image into the snapshot folder. The controller calls it
// on a timer.
func (m *Manager) AdvanceVMSnapshots(ctx context.Context) {
	m.mu.Lock()
	vm := m.vm
	m.mu.Unlock()
	if vm == nil {
		return
	}
	pools, err := m.k8sClient.ListFileSharePools(ctx)
	if err != nil {
		klog.ErrorS(err, "Failed to list pools for VM snapshots")
		return
	}
	for i := range pools {
		snaps, err := m.k8sClient.ListSnapshots(ctx, pools[i].Name)
		if err != nil {
			klog.ErrorS(err, "Failed to list snapshots", "pool", pools[i].Name)
			continue
		}
		for j := range snaps {
			s := &snaps[j]
			if s.Annotations[snapshotMethodAnnotation] != snapshotMethodVM || s.Status.ReadyToUse || s.Status.Phase == "Failed" {
				continue
			}
			m.advanceVMSnapshot(ctx, vm, s.Name)
		}
	}
}

// advanceVMSnapshot takes the lock, reads the Snapshot again so that it works
// on the newest state, and does one step.
func (m *Manager) advanceVMSnapshot(ctx context.Context, vm *vmSnapshots, name string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	snap, err := m.k8sClient.GetSnapshot(ctx, name)
	if err != nil || snap.Annotations[snapshotMethodAnnotation] != snapshotMethodVM || snap.Status.ReadyToUse || snap.Status.Phase == "Failed" {
		return
	}
	pool, err := m.k8sClient.GetFileSharePool(ctx, snap.Spec.PoolName)
	if err != nil {
		klog.ErrorS(err, "Failed to get the pool of a VM snapshot", "snapshot", name, "pool", snap.Spec.PoolName)
		return
	}
	switch {
	case snap.Status.VMCopy == nil:
		m.startVMSnapshotCopy(ctx, vm, snap, pool)
	case snap.Status.VMCopy.Phase == vmCopyPhaseCopying:
		m.watchVMSnapshotCopy(ctx, vm, snap, pool)
	default:
		m.watchVMSnapshotApply(ctx, vm, snap, pool)
	}
}

func (m *Manager) saveSnapshotStatus(ctx context.Context, snap *v1alpha1.Snapshot) {
	if err := m.k8sClient.UpdateSnapshotStatus(ctx, snap); err != nil {
		klog.ErrorS(err, "Failed to update Snapshot status", "snapshot", snap.Name)
	}
}

// wait records why the snapshot waits. It does not count as a try.
func (m *Manager) waitVMSnapshot(ctx context.Context, snap *v1alpha1.Snapshot, msg string) {
	if snap.Status.Message == msg {
		return
	}
	snap.Status.Message = msg
	m.saveSnapshotStatus(ctx, snap)
}

// vmSnapshotTryFailed counts a failed or cancelled try. After the last try the
// snapshot fails; before it, the next try waits a minute.
func (m *Manager) vmSnapshotTryFailed(ctx context.Context, snap *v1alpha1.Snapshot, reason string) {
	now := m.timeNow()
	snap.Status.VMCopy = nil
	snap.Status.VMCopyStarted = nil
	snap.Status.VMCopyAttempts++
	if snap.Status.VMCopyAttempts >= maxVMSnapshotAttempts {
		snap.Status.Phase = "Failed"
		snap.Status.VMCopyRetryAfter = nil
		snap.Status.Message = fmt.Sprintf("failed after %d tries: %s", snap.Status.VMCopyAttempts, reason)
		klog.ErrorS(nil, "VM snapshot failed", "snapshot", snap.Name, "reason", reason)
	} else {
		retry := metav1.NewTime(now.Add(vmSnapshotRetryDelay))
		snap.Status.VMCopyRetryAfter = &retry
		snap.Status.Message = fmt.Sprintf("%s; try %d of %d failed, trying again", reason, snap.Status.VMCopyAttempts, maxVMSnapshotAttempts)
		klog.V(2).InfoS("VM snapshot try failed", "snapshot", snap.Name, "reason", reason, "attempts", snap.Status.VMCopyAttempts)
	}
	m.saveSnapshotStatus(ctx, snap)
}

func (m *Manager) startVMSnapshotCopy(ctx context.Context, vm *vmSnapshots, snap *v1alpha1.Snapshot, pool *v1alpha1.FileSharePool) {
	now := m.timeNow()
	if snap.Status.VMCopyRetryAfter != nil && now.Before(snap.Status.VMCopyRetryAfter.Time) {
		return
	}
	pod, volume, _, err := m.runningVMPodForSnapshot(ctx, vm, snap)
	if err != nil {
		m.waitVMSnapshot(ctx, snap, err.Error())
		return
	}
	if pod == nil {
		// The VM stopped: the disk is at rest, so a file copy is safe.
		klog.V(2).InfoS("No VM uses the snapshot source any more; using the file copy", "snapshot", snap.Name)
		if err := m.setSnapshotMethod(ctx, snap, snapshotMethodFile); err != nil {
			m.waitVMSnapshot(ctx, snap, err.Error())
			return
		}
		if err := m.copySnapshotData(ctx, snap, pool); err != nil {
			klog.ErrorS(err, "File copy of a snapshot failed", "snapshot", snap.Name)
		}
		return
	}
	vmi := vmiName(pod)
	if vmi == "" {
		m.vmSnapshotTryFailed(ctx, snap, fmt.Sprintf("VM pod %s/%s has no VirtualMachineInstance owner", pod.Namespace, pod.Name))
		return
	}
	moving, err := vmInstanceMoving(ctx, vm.kube, pod.Namespace, vmi)
	if err != nil {
		m.waitVMSnapshot(ctx, snap, fmt.Sprintf("check for a VM move: %v", err))
		return
	}
	if moving {
		m.waitVMSnapshot(ctx, snap, "copy waits: the VM is migrating to another worker")
		return
	}
	target := vmcopy.VM{Namespace: pod.Namespace, Pod: pod.Name, Name: vmi}
	// Clear what an earlier try left, but keep every change list: the one
	// that replication keeps for this VM must stay.
	if err := vm.copier.CleanStaleCopies(ctx, target, []string{volume}); err != nil {
		if errors.Is(err, vmcopy.ErrBusy) {
			m.waitVMSnapshot(ctx, snap, vmBusyMessage)
		} else {
			m.waitVMSnapshot(ctx, snap, fmt.Sprintf("check the VM for other copies: %v", err))
		}
		return
	}
	id := strconv.FormatInt(now.UnixNano(), 36)
	cp, err := vm.copier.Start(ctx, target, []string{volume}, vmcopy.StartOptions{Speed: snapshotSpeedMBps(snap) << 20, ID: id})
	if err != nil {
		m.vmSnapshotTryFailed(ctx, snap, fmt.Sprintf("start the VM disk copy: %v", err))
		return
	}
	klog.V(2).InfoS("Started VM snapshot copy", "snapshot", snap.Name, "vm", klog.KRef(pod.Namespace, vmi), "copy", id,
		"fileSystemConsistent", cp.FileSystemConsistent, "freezeError", cp.FreezeError)
	started := metav1.NewTime(now)
	snap.Status.VMCopy = &v1alpha1.VMCopyState{
		ID:                   id,
		Phase:                vmCopyPhaseCopying,
		PodName:              pod.Name,
		PodUID:               string(pod.UID),
		VMIName:              vmi,
		Volume:               volume,
		StagingFile:          cp.Disks[0].StagingFile,
		Moment:               metav1.NewTime(cp.Moment),
		FileSystemConsistent: cp.FileSystemConsistent,
		FreezeError:          cp.FreezeError,
		Bitmap:               cp.Bitmap,
	}
	snap.Status.VMCopyStarted = &started
	snap.Status.VMCopyRetryAfter = nil
	snap.Status.Message = ""
	m.saveSnapshotStatus(ctx, snap)
}

// vmOfCopy returns the VM and the Copy that a Snapshot's state names.
func vmOfCopy(ns string, st *v1alpha1.VMCopyState) (vmcopy.VM, *vmcopy.Copy) {
	return vmcopy.VM{Namespace: ns, Pod: st.PodName, Name: st.VMIName},
		&vmcopy.Copy{ID: st.ID, Moment: st.Moment.Time, FileSystemConsistent: st.FileSystemConsistent, FreezeError: st.FreezeError,
			Bitmap: st.Bitmap, Disks: []vmcopy.Disk{{Volume: st.Volume, StagingFile: st.StagingFile}}}
}

func (m *Manager) watchVMSnapshotCopy(ctx context.Context, vm *vmSnapshots, snap *v1alpha1.Snapshot, pool *v1alpha1.FileSharePool) {
	st := snap.Status.VMCopy
	sv, err := m.k8sClient.GetSubVolume(ctx, snap.Spec.SourceSubVolume)
	if err != nil {
		m.waitVMSnapshot(ctx, snap, fmt.Sprintf("get source SubVolume: %v", err))
		return
	}
	ns := sv.Spec.PVCNamespace
	target, cp := vmOfCopy(ns, st)

	pod := &corev1.Pod{}
	err = vm.kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: st.PodName}, pod)
	if apierrors.IsNotFound(err) || (err == nil && string(pod.UID) != st.PodUID) {
		m.vmSnapshotTryFailed(ctx, snap, "the VM restarted or moved during the copy")
		return
	}
	if err != nil {
		m.waitVMSnapshot(ctx, snap, fmt.Sprintf("check the VM pod: %v", err))
		return
	}
	cancel := func(reason string) {
		if err := vm.copier.Cancel(ctx, target, cp); err != nil {
			m.waitVMSnapshot(ctx, snap, fmt.Sprintf("cancel the copy (%s): %v", reason, err))
			return
		}
		m.vmSnapshotTryFailed(ctx, snap, reason)
	}
	if moving, merr := vmInstanceMoving(ctx, vm.kube, ns, st.VMIName); merr != nil {
		klog.ErrorS(merr, "Failed to check for a VM move", "vm", klog.KRef(ns, st.VMIName))
	} else if moving {
		cancel("the VM is migrating to another worker, so the copy was cancelled")
		return
	}
	if started := snap.Status.VMCopyStarted; started != nil {
		if limit := vmSnapshotDeadline(snap.Spec.SizeGB, snapshotSpeedMBps(snap)); m.timeNow().Sub(started.Time) > limit {
			cancel(fmt.Sprintf("the copy ran longer than %s, so it was cancelled", limit.Round(time.Minute)))
			return
		}
	}

	state, msg, err := vm.copier.Progress(ctx, target, cp)
	if err != nil {
		m.waitVMSnapshot(ctx, snap, fmt.Sprintf("check the copy: %v", err))
		return
	}
	switch state {
	case vmcopy.Running:
	case vmcopy.Failed, vmcopy.Lost:
		if err := vm.copier.Finish(ctx, target, cp, true); err != nil {
			klog.ErrorS(err, "Failed to clean up a failed VM snapshot copy", "snapshot", snap.Name)
		}
		m.vmSnapshotTryFailed(ctx, snap, fmt.Sprintf("copy failed: %s", orDefault(msg, "QEMU has no record of the copy")))
	case vmcopy.Done:
		if err := vm.copier.Finish(ctx, target, cp, false); err != nil {
			m.waitVMSnapshot(ctx, snap, fmt.Sprintf("release the finished copy: %v", err))
			return
		}
		st.Phase = vmCopyPhaseSending
		snap.Status.Message = ""
		m.saveSnapshotStatus(ctx, snap)
		m.watchVMSnapshotApply(ctx, vm, snap, pool)
	}
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (m *Manager) watchVMSnapshotApply(ctx context.Context, vm *vmSnapshots, snap *v1alpha1.Snapshot, pool *v1alpha1.FileSharePool) {
	st := snap.Status.VMCopy
	sv, err := m.k8sClient.GetSubVolume(ctx, snap.Spec.SourceSubVolume)
	if err != nil {
		m.waitVMSnapshot(ctx, snap, fmt.Sprintf("get source SubVolume: %v", err))
		return
	}
	share, err := poolShareMount(pool, snap.Spec.ShareID)
	if err != nil {
		m.waitVMSnapshot(ctx, snap, err.Error())
		return
	}
	target, cp := vmOfCopy(sv.Spec.PVCNamespace, st)
	done, err := m.shareData.ApplyVMDisk(ctx, "snap-"+snap.Name, share, snap.Spec.SourceSubPath, st.StagingFile, snap.Spec.SnapshotPath, st.Moment.Time)
	if errors.Is(err, ErrShareJobFailed) {
		m.releaseVMSnapshotCopy(ctx, vm, target, cp, st, sv.Spec.PVCNamespace)
		m.vmSnapshotTryFailed(ctx, snap, fmt.Sprintf("writing the snapshot failed: %v", err))
		return
	}
	if err != nil {
		m.waitVMSnapshot(ctx, snap, fmt.Sprintf("write the snapshot: %v", err))
		return
	}
	if !done {
		return
	}
	// The change list of a snapshot is not needed: every snapshot is full.
	m.releaseVMSnapshotCopy(ctx, vm, target, cp, st, sv.Spec.PVCNamespace)

	moment := st.Moment
	snap.Status.Phase = "Ready"
	snap.Status.ReadyToUse = true
	snap.Status.CopyMoment = &moment
	snap.Status.Consistency, snap.Status.ConsistencyReason = consistencyFileSystem, ""
	if !st.FileSystemConsistent {
		snap.Status.Consistency, snap.Status.ConsistencyReason = consistencyCrash, st.FreezeError
	}
	snap.Status.VMCopy = nil
	snap.Status.VMCopyStarted = nil
	snap.Status.VMCopyRetryAfter = nil
	snap.Status.Message = ""
	m.saveSnapshotStatus(ctx, snap)
	klog.V(2).InfoS("VM snapshot ready", "snapshot", snap.Name, "moment", moment.Time, "consistency", snap.Status.Consistency)
}

// releaseVMSnapshotCopy drops what QEMU holds for the copy and deletes the
// staging file, if the VM pod that made it still runs. Errors are logged: a
// later copy of the VM clears anything left.
func (m *Manager) releaseVMSnapshotCopy(ctx context.Context, vm *vmSnapshots, target vmcopy.VM, cp *vmcopy.Copy, st *v1alpha1.VMCopyState, ns string) {
	if !samePod(ctx, vm.kube, ns, st.PodName, st.PodUID) {
		return
	}
	if err := vm.copier.Finish(ctx, target, cp, true); err != nil {
		klog.ErrorS(err, "Failed to release a VM snapshot copy", "vm", klog.KRef(ns, st.VMIName), "copy", st.ID)
	}
}

// cancelVMSnapshotCopy ends a copy in progress, before the snapshot is deleted.
func (m *Manager) cancelVMSnapshotCopy(ctx context.Context, snap *v1alpha1.Snapshot) {
	st := snap.Status.VMCopy
	if m.vm == nil || st == nil {
		return
	}
	sv, err := m.k8sClient.GetSubVolume(ctx, snap.Spec.SourceSubVolume)
	if err != nil {
		klog.ErrorS(err, "Cannot cancel a VM snapshot copy: the source SubVolume is gone", "snapshot", snap.Name)
		return
	}
	ns := sv.Spec.PVCNamespace
	target, cp := vmOfCopy(ns, st)
	if samePod(ctx, m.vm.kube, ns, st.PodName, st.PodUID) {
		var err error
		if st.Phase == vmCopyPhaseCopying {
			err = m.vm.copier.Cancel(ctx, target, cp)
		} else {
			err = m.vm.copier.Finish(ctx, target, cp, true)
		}
		if err != nil {
			klog.ErrorS(err, "Failed to cancel a VM snapshot copy", "snapshot", snap.Name)
		}
	}
	snap.Status.VMCopy = nil
	snap.Status.VMCopyStarted = nil
	m.saveSnapshotStatus(ctx, snap)
}
