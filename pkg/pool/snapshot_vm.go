package pool

import (
	"context"
	"errors"
	"fmt"
	"sort"
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

	// vmCopyJoinWindow is how long after a copy starts the snapshots of the
	// VM's other disks may join it. KubeVirt asks for all of them within
	// seconds. A disk that nobody claims in this time is released.
	vmCopyJoinWindow = 10 * time.Minute
)

// ErrInvalidSnapshotParameter means a VolumeSnapshotClass parameter has a bad value.
var ErrInvalidSnapshotParameter = errors.New("invalid snapshot parameter")

// ErrSnapshotUnsupported means the volume is in use in a way that a snapshot
// cannot be made safely. Retrying does not help.
var ErrSnapshotUnsupported = errors.New("snapshot not supported")

// ErrSnapshotFailed means the snapshot ended in the Failed phase. Retrying
// does not help; delete the VolumeSnapshot and make a new one.
var ErrSnapshotFailed = errors.New("snapshot failed")

// snapshotDeletingAnnotation marks a snapshot whose delete has started, so the
// copy loop leaves it alone.
const snapshotDeletingAnnotation = "storage.ibmcloud.io/deleting"

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

// switchToVMPath is called while a file copy runs or when it has finished. If
// a VM uses the disk, the copy may mix moments, so the snapshot moves to the
// VM path and the apply Job replaces the folder. It first stops the file copy
// Job, so two writers never work on the folder. It reports handled=true when
// the caller must not go on with the file copy: the Job is still stopping, or
// the snapshot has moved.
func (m *Manager) switchToVMPath(ctx context.Context, snap *v1alpha1.Snapshot) (bool, error) {
	if m.vm == nil || m.shareData == nil {
		return false, nil
	}
	pod, _, _, err := m.runningVMPodForSnapshot(ctx, m.vm, snap)
	if err != nil {
		return false, err
	}
	if pod == nil {
		return false, nil
	}
	stopped, err := m.shareData.Stop(ctx, "snap-"+snap.Name)
	if err != nil {
		return false, fmt.Errorf("stop the file copy: %w", err)
	}
	if !stopped {
		return true, nil
	}
	klog.V(2).InfoS("A VM uses the snapshot source; copying with QEMU instead of files", "snapshot", snap.Name, "vmPod", klog.KObj(pod))
	if err := m.setSnapshotMethod(ctx, snap, snapshotMethodVM); err != nil {
		return false, err
	}
	snap.Status.Message = "a VM uses the disk; copying again with QEMU"
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
			m.RemoveOrphanSnapshots(ctx)
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
	// One listing per pass: the VM pods that a snapshot copies now, the copy
	// sets that other snapshots may join, and the snapshots to advance.
	pass := &vmPass{active: map[string]string{}, sets: map[string]*v1alpha1.Snapshot{}, claimed: map[string]bool{}, names: map[string]bool{}}
	var todo []string
	for i := range pools {
		snaps, listErr := m.k8sClient.ListSnapshots(ctx, pools[i].Name)
		if listErr != nil {
			klog.ErrorS(listErr, "Failed to list snapshots", "pool", pools[i].Name)
			continue
		}
		for j := range snaps {
			s := &snaps[j]
			pass.names[s.Name] = true
			if s.Status.VMCopy != nil {
				pass.active[vmKey(s.Status.VMCopy)] = s.Name
			}
			if set := s.Status.VMCopySet; set != nil {
				pass.sets[set.PodNamespace+"/"+set.VMIName] = s
			}
			if s.Status.CopyID != "" {
				pass.claimed[s.Status.CopyID+"/"+s.Spec.SourceSubVolume] = true
			}
			if snapshotNeedsVMStep(s) {
				todo = append(todo, s.Name)
			}
		}
	}
	for _, name := range todo {
		m.advanceVMSnapshot(ctx, vm, name, pass)
	}
	m.releaseExpiredCopySets(ctx, vm, pass)
}

// vmPass is what one pass of the copy loop knows about all snapshots.
type vmPass struct {
	// active maps a VM to the snapshot that holds a QEMU copy or an apply Job in it.
	active map[string]string
	// sets maps a VM to the snapshot that owns the newest copy set of it.
	sets map[string]*v1alpha1.Snapshot
	// claimed holds "copy id/SubVolume" for every snapshot made from a copy.
	claimed map[string]bool
	// names holds the name of every snapshot.
	names map[string]bool
}

// vmKey names the VM that a copy runs in. It stays the same when the VM pod is
// replaced, so a snapshot that waits for its apply Job keeps the VM busy.
func vmKey(st *v1alpha1.VMCopyState) string { return st.PodNamespace + "/" + st.VMIName }

// snapshotNeedsVMStep reports whether the VM loop has work on the snapshot.
func snapshotNeedsVMStep(s *v1alpha1.Snapshot) bool {
	return s.Annotations[snapshotMethodAnnotation] == snapshotMethodVM &&
		s.Annotations[snapshotDeletingAnnotation] == "" &&
		!s.Status.ReadyToUse && s.Status.Phase != "Failed"
}

// advanceVMSnapshot reads the Snapshot again, so that it works on the newest
// state, and does one step. It holds vmMu, not mu: commands in a VM pod can be
// slow, and CSI calls must not wait for them.
func (m *Manager) advanceVMSnapshot(ctx context.Context, vm *vmSnapshots, name string, pass *vmPass) {
	m.vmMu.Lock()
	defer m.vmMu.Unlock()
	snap, err := m.k8sClient.GetSnapshot(ctx, name)
	if err != nil || !snapshotNeedsVMStep(snap) {
		return
	}
	switch {
	case snap.Status.VMCopy == nil:
		m.startVMSnapshotCopy(ctx, vm, snap, pass)
	case snap.Status.VMCopy.Phase == vmCopyPhaseCopying:
		m.watchVMSnapshotCopy(ctx, vm, snap, pass)
	default:
		m.watchVMSnapshotApply(ctx, vm, snap)
	}
	// Keep the maps right for the snapshots later in this pass.
	for pod, owner := range pass.active {
		if owner == snap.Name {
			delete(pass.active, pod)
		}
	}
	if snap.Status.VMCopy != nil {
		pass.active[vmKey(snap.Status.VMCopy)] = snap.Name
	}
	for pod, owner := range pass.sets {
		if owner.Name == snap.Name {
			delete(pass.sets, pod)
		}
	}
	if set := snap.Status.VMCopySet; set != nil {
		pass.sets[set.PodNamespace+"/"+set.VMIName] = snap
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

// failVMSnapshot ends the snapshot at once, without more tries.
func (m *Manager) failVMSnapshot(ctx context.Context, snap *v1alpha1.Snapshot, reason string) {
	snap.Status.Phase = "Failed"
	snap.Status.VMCopy = nil
	snap.Status.VMCopyStarted = nil
	snap.Status.CopyID = ""
	snap.Status.Message = reason
	klog.ErrorS(nil, "VM snapshot failed", "snapshot", snap.Name, "reason", reason)
	m.saveSnapshotStatus(ctx, snap)
}

// vmSnapshotTryFailed counts a failed or cancelled try. After the last try the
// snapshot fails; before it, the next try waits a minute.
func (m *Manager) vmSnapshotTryFailed(ctx context.Context, snap *v1alpha1.Snapshot, reason string) {
	now := m.timeNow()
	snap.Status.VMCopy = nil
	snap.Status.VMCopyStarted = nil
	snap.Status.CopyID = ""
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

func (m *Manager) startVMSnapshotCopy(ctx context.Context, vm *vmSnapshots, snap *v1alpha1.Snapshot, pass *vmPass) {
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
		// The VM stopped: the disk is at rest, so a file copy is safe. The
		// file copy runs when the snapshotter next calls CreateSnapshot.
		klog.V(2).InfoS("No VM uses the snapshot source any more; using the file copy", "snapshot", snap.Name)
		if setErr := m.setSnapshotMethod(ctx, snap, snapshotMethodFile); setErr != nil {
			m.waitVMSnapshot(ctx, snap, setErr.Error())
		}
		return
	}
	if pod.Labels[kubevirtPodLabel] != "virt-launcher" {
		m.failVMSnapshot(ctx, snap, "the disk is hot-plugged into the running VM; QEMU cannot copy it yet. Stop the VM or detach the disk, and snapshot again")
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
	key := pod.Namespace + "/" + vmi
	// A snapshot of another disk of this VM may already have started a copy of
	// all the VM's disks. Join it, so both snapshots show one moment.
	if leader, idx := joinableCopySet(pass, key, snap, pod, now); leader != nil {
		m.joinVMCopySet(ctx, snap, leader, idx, key, pass)
		return
	}
	// Snapshots of one VM take turns: a copy that waits for its apply Job has
	// no QEMU job, and the next snapshot's cleanup must not remove its file.
	if other := pass.active[key]; other != "" && other != snap.Name {
		m.waitVMSnapshot(ctx, snap, fmt.Sprintf("copy waits: another snapshot of this VM (%s) is being copied", other))
		return
	}
	// Copy every pool disk of the VM at one moment, so the snapshots of the
	// VM's disks fit each other.
	disks, err := m.vmPoolDisks(ctx, vm, pod)
	if err != nil {
		m.waitVMSnapshot(ctx, snap, fmt.Sprintf("find the disks of the VM: %v", err))
		return
	}
	idx := diskIndexOf(disks, snap.Spec.SourceSubVolume)
	if idx < 0 || disks[idx].Volume != volume {
		m.waitVMSnapshot(ctx, snap, fmt.Sprintf("find the disks of the VM: the source volume %s is not among them", volume))
		return
	}
	volumes := make([]string, len(disks))
	for i := range disks {
		volumes[i] = disks[i].Volume
	}
	// Clear what earlier snapshot copies left. Replication's copies and change
	// lists stay: the change list that replication keeps for this VM must live.
	if cleanErr := vm.copier.CleanStaleCopies(ctx, target, volumes); cleanErr != nil {
		if errors.Is(cleanErr, vmcopy.ErrBusy) {
			m.waitVMSnapshot(ctx, snap, vmBusyMessage)
		} else {
			m.waitVMSnapshot(ctx, snap, fmt.Sprintf("check the VM for other copies: %v", cleanErr))
		}
		return
	}
	id := newSnapshotCopyID(now)
	cp, err := vm.copier.Start(ctx, target, volumes, vmcopy.StartOptions{Speed: snapshotSpeedMBps(snap) << 20, ID: id})
	if err != nil {
		m.vmSnapshotTryFailed(ctx, snap, fmt.Sprintf("start the VM disk copy: %v", err))
		return
	}
	klog.V(2).InfoS("Started VM snapshot copy", "snapshot", snap.Name, "vm", klog.KRef(pod.Namespace, vmi), "copy", id,
		"disks", len(volumes), "fileSystemConsistent", cp.FileSystemConsistent, "freezeError", cp.FreezeError)
	started := metav1.NewTime(now)
	moment := metav1.NewTime(cp.Moment)
	setDisks := make([]v1alpha1.VMCopyDisk, len(cp.Disks))
	for i, d := range cp.Disks {
		setDisks[i] = v1alpha1.VMCopyDisk{Volume: d.Volume, StagingFile: d.StagingFile}
		disks[i].StagingFile = d.StagingFile
	}
	snap.Status.VMCopy = &v1alpha1.VMCopyState{
		ID:                   id,
		Phase:                vmCopyPhaseCopying,
		PodNamespace:         pod.Namespace,
		PodName:              pod.Name,
		PodUID:               string(pod.UID),
		VMIName:              vmi,
		Volume:               volume,
		DiskIndex:            int32(idx), // #nosec G115 -- a VM has far fewer than 2^31 disks
		StagingFile:          cp.Disks[idx].StagingFile,
		Moment:               moment,
		FileSystemConsistent: cp.FileSystemConsistent,
		FreezeError:          cp.FreezeError,
		Bitmap:               cp.Bitmap,
		SetDisks:             setDisks,
	}
	snap.Status.CopyID = id
	snap.Status.VMCopySet = nil
	if len(disks) > 1 {
		snap.Status.VMCopySet = &v1alpha1.VMCopySet{
			ID:                   id,
			PodNamespace:         pod.Namespace,
			PodName:              pod.Name,
			PodUID:               string(pod.UID),
			VMIName:              vmi,
			Moment:               moment,
			FileSystemConsistent: cp.FileSystemConsistent,
			FreezeError:          cp.FreezeError,
			Bitmap:               cp.Bitmap,
			JoinUntil:            metav1.NewTime(now.Add(vmCopyJoinWindow)),
			Disks:                disks,
		}
	}
	snap.Status.VMCopyStarted = &started
	snap.Status.VMCopyRetryAfter = nil
	snap.Status.Message = ""
	if saveErr := m.k8sClient.UpdateSnapshotStatus(ctx, snap); saveErr != nil {
		// Nothing records the copy now, so it would run unseen. End it.
		klog.ErrorS(saveErr, "Failed to record a VM snapshot copy; cancelling it", "snapshot", snap.Name, "copy", id)
		if cancelErr := vm.copier.Cancel(ctx, target, cp); cancelErr != nil {
			klog.ErrorS(cancelErr, "Failed to cancel an unrecorded VM snapshot copy", "snapshot", snap.Name, "copy", id)
		}
		return
	}
	pass.active[key] = snap.Name
	pass.claimed[id+"/"+snap.Spec.SourceSubVolume] = true
	if snap.Status.VMCopySet != nil {
		pass.sets[key] = snap
	}
}

// diskIndexOf returns the place of the SubVolume among the disks, or -1.
func diskIndexOf(disks []v1alpha1.VMCopySetDisk, subVolume string) int {
	for i := range disks {
		if disks[i].SubVolume == subVolume {
			return i
		}
	}
	return -1
}

// vmPoolDisks lists the disks of the VM pod that are volumes of a pool, in the
// order of their SubVolume names. A stable order gives a disk the same place in
// every copy.
func (m *Manager) vmPoolDisks(ctx context.Context, vm *vmSnapshots, pod *corev1.Pod) ([]v1alpha1.VMCopySetDisk, error) {
	pools, err := m.k8sClient.ListFileSharePools(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}
	byClaim := map[string]string{}
	for i := range pools {
		svs, listErr := m.k8sClient.ListSubVolumes(ctx, pools[i].Name)
		if listErr != nil {
			return nil, fmt.Errorf("list the volumes of pool %s: %w", pools[i].Name, listErr)
		}
		for j := range svs {
			byClaim[svs[j].Spec.PVCNamespace+"/"+svs[j].Spec.PVCName] = svs[j].Name
		}
	}
	var disks []v1alpha1.VMCopySetDisk
	for _, vol := range pod.Spec.Volumes {
		if vol.PersistentVolumeClaim == nil {
			continue
		}
		if sv, ok := byClaim[pod.Namespace+"/"+vol.PersistentVolumeClaim.ClaimName]; ok {
			disks = append(disks, v1alpha1.VMCopySetDisk{Volume: vol.Name, SubVolume: sv})
		}
	}
	sort.Slice(disks, func(i, j int) bool { return disks[i].SubVolume < disks[j].SubVolume })
	return disks, nil
}

// joinableCopySet returns the snapshot that owns a copy set that snap may
// join, and snap's place in the set. It returns nil if there is none: no set
// for the VM, the set belongs to an earlier VM pod, its join time has passed,
// the disk is not in it, or another snapshot has the disk.
func joinableCopySet(pass *vmPass, key string, snap *v1alpha1.Snapshot, pod *corev1.Pod, now time.Time) (*v1alpha1.Snapshot, int) {
	leader := pass.sets[key]
	if leader == nil || leader.Name == snap.Name || leader.Annotations[snapshotDeletingAnnotation] != "" {
		return nil, 0
	}
	set := leader.Status.VMCopySet
	if set == nil || set.PodUID != string(pod.UID) || !now.Before(set.JoinUntil.Time) {
		return nil, 0
	}
	idx := diskIndexOf(set.Disks, snap.Spec.SourceSubVolume)
	if idx < 0 || pass.claimed[set.ID+"/"+snap.Spec.SourceSubVolume] {
		return nil, 0
	}
	copying := leader.Status.VMCopy != nil && leader.Status.VMCopy.ID == set.ID
	done := leader.Status.VMCopy == nil && leader.Status.ReadyToUse && leader.Status.CopyID == set.ID
	if !copying && !done {
		return nil, 0
	}
	return leader, idx
}

// joinVMCopySet makes snap a member of the copy set that leader owns. It
// starts no QEMU copy: it shows the same moment as the leader.
func (m *Manager) joinVMCopySet(ctx context.Context, snap, leader *v1alpha1.Snapshot, idx int, key string, pass *vmPass) {
	set := leader.Status.VMCopySet
	phase := vmCopyPhaseSending
	if leader.Status.VMCopy != nil && leader.Status.VMCopy.Phase == vmCopyPhaseCopying {
		phase = vmCopyPhaseCopying
	}
	setDisks := make([]v1alpha1.VMCopyDisk, len(set.Disks))
	for i, d := range set.Disks {
		setDisks[i] = v1alpha1.VMCopyDisk{Volume: d.Volume, StagingFile: d.StagingFile}
	}
	started := metav1.NewTime(m.timeNow())
	snap.Status.VMCopy = &v1alpha1.VMCopyState{
		ID:                   set.ID,
		Phase:                phase,
		PodNamespace:         set.PodNamespace,
		PodName:              set.PodName,
		PodUID:               set.PodUID,
		VMIName:              set.VMIName,
		Volume:               set.Disks[idx].Volume,
		DiskIndex:            int32(idx), // #nosec G115 -- a VM has far fewer than 2^31 disks
		StagingFile:          set.Disks[idx].StagingFile,
		Moment:               set.Moment,
		FileSystemConsistent: set.FileSystemConsistent,
		FreezeError:          set.FreezeError,
		Bitmap:               set.Bitmap,
		Leader:               leader.Name,
		SetDisks:             setDisks,
	}
	snap.Status.CopyID = set.ID
	snap.Status.VMCopyStarted = &started
	snap.Status.VMCopyRetryAfter = nil
	snap.Status.Message = ""
	if err := m.k8sClient.UpdateSnapshotStatus(ctx, snap); err != nil {
		klog.ErrorS(err, "Failed to record that a snapshot joined a VM copy", "snapshot", snap.Name, "leader", leader.Name)
		return
	}
	klog.V(2).InfoS("Snapshot joined the copy of the VM's disks", "snapshot", snap.Name, "leader", leader.Name, "copy", set.ID)
	if pass.active[key] == "" {
		pass.active[key] = snap.Name
	}
	pass.claimed[set.ID+"/"+snap.Spec.SourceSubVolume] = true
}

// newSnapshotCopyID makes the id of a copy. The prefix marks it as a snapshot
// copy, so replication's cleanup leaves it alone. Ids are at most 16 characters.
func newSnapshotCopyID(now time.Time) string {
	t := strconv.FormatInt(now.UnixNano(), 36)
	if n := 16 - len(vmcopy.SnapshotIDPrefix); len(t) > n {
		t = t[len(t)-n:]
	}
	return vmcopy.SnapshotIDPrefix + t
}

// vmOfCopy returns the VM and the Copy that a Snapshot's state names.
//
// While QEMU copies, the owner of a copy that covers several disks gets a Copy
// with all of them, because QEMU numbers its jobs by the place of the disk.
// Once QEMU is done, each snapshot has only its own disk to release.
func vmOfCopy(ns string, st *v1alpha1.VMCopyState) (vmcopy.VM, *vmcopy.Copy) {
	disks := []vmcopy.Disk{{Volume: st.Volume, StagingFile: st.StagingFile}}
	if st.Leader == "" && st.Phase == vmCopyPhaseCopying && len(st.SetDisks) > 1 {
		disks = make([]vmcopy.Disk, len(st.SetDisks))
		for i, d := range st.SetDisks {
			disks[i] = vmcopy.Disk{Volume: d.Volume, StagingFile: d.StagingFile}
		}
	}
	return vmcopy.VM{Namespace: ns, Pod: st.PodName, Name: st.VMIName},
		&vmcopy.Copy{ID: st.ID, Moment: st.Moment.Time, FileSystemConsistent: st.FileSystemConsistent, FreezeError: st.FreezeError,
			Bitmap: st.Bitmap, Disks: disks}
}

func (m *Manager) watchVMSnapshotCopy(ctx context.Context, vm *vmSnapshots, snap *v1alpha1.Snapshot, pass *vmPass) {
	st := snap.Status.VMCopy
	if st.Leader != "" {
		m.watchJoinedVMSnapshot(ctx, vm, snap, pass)
		return
	}
	ns := st.PodNamespace
	target, cp := vmOfCopy(ns, st)

	pod := &corev1.Pod{}
	err := vm.kube.Get(ctx, types.NamespacedName{Namespace: ns, Name: st.PodName}, pod)
	if apierrors.IsNotFound(err) || (err == nil && string(pod.UID) != st.PodUID) {
		snap.Status.VMCopySet = nil
		m.vmSnapshotTryFailed(ctx, snap, "the VM restarted or moved during the copy")
		return
	}
	if err != nil {
		m.waitVMSnapshot(ctx, snap, fmt.Sprintf("check the VM pod: %v", err))
		return
	}
	cancel := func(reason string) {
		if cancelErr := vm.copier.Cancel(ctx, target, cp); cancelErr != nil {
			m.waitVMSnapshot(ctx, snap, fmt.Sprintf("cancel the copy (%s): %v", reason, cancelErr))
			return
		}
		snap.Status.VMCopySet = nil
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
		snap.Status.VMCopySet = nil
		m.vmSnapshotTryFailed(ctx, snap, fmt.Sprintf("copy failed: %s", orDefault(msg, "QEMU has no record of the copy")))
	case vmcopy.Done:
		if err := vm.copier.Finish(ctx, target, cp, false); err != nil {
			m.waitVMSnapshot(ctx, snap, fmt.Sprintf("release the finished copy: %v", err))
			return
		}
		st.Phase = vmCopyPhaseSending
		snap.Status.Message = ""
		m.saveSnapshotStatus(ctx, snap)
		m.watchVMSnapshotApply(ctx, vm, snap)
	}
}

// watchJoinedVMSnapshot follows the snapshot that owns the QEMU copy. It does
// not touch the copy. When the owner's QEMU copy is done, this snapshot starts
// its apply Job. If the owner's copy was cancelled, so is this snapshot's try.
func (m *Manager) watchJoinedVMSnapshot(ctx context.Context, vm *vmSnapshots, snap *v1alpha1.Snapshot, pass *vmPass) {
	st := snap.Status.VMCopy
	leader, err := m.k8sClient.GetSnapshot(ctx, st.Leader)
	if err != nil {
		if pass.names[st.Leader] {
			m.waitVMSnapshot(ctx, snap, fmt.Sprintf("read the snapshot that owns the copy: %v", err))
			return
		}
		m.vmSnapshotTryFailed(ctx, snap, fmt.Sprintf("the snapshot that owned the copy (%s) was deleted", st.Leader))
		return
	}
	switch lc := leader.Status.VMCopy; {
	case lc != nil && lc.ID == st.ID && lc.Phase == vmCopyPhaseCopying:
		m.waitVMSnapshot(ctx, snap, fmt.Sprintf("copy waits: QEMU copies the VM's disks together for %s", st.Leader))
	case lc != nil && lc.ID == st.ID, lc == nil && leader.Status.ReadyToUse && leader.Status.CopyID == st.ID:
		st.Phase = vmCopyPhaseSending
		snap.Status.Message = ""
		m.saveSnapshotStatus(ctx, snap)
		m.watchVMSnapshotApply(ctx, vm, snap)
	default:
		m.vmSnapshotTryFailed(ctx, snap, fmt.Sprintf("the copy of the VM's disks that %s started was cancelled", st.Leader))
	}
}

// claimedDisks returns the SubVolumes of the copy that a snapshot, other than
// the one named except, has claimed.
func (m *Manager) claimedDisks(ctx context.Context, copyID, except string) (map[string]bool, error) {
	pools, err := m.k8sClient.ListFileSharePools(ctx)
	if err != nil {
		return nil, fmt.Errorf("list pools: %w", err)
	}
	claimed := map[string]bool{}
	for i := range pools {
		snaps, listErr := m.k8sClient.ListSnapshots(ctx, pools[i].Name)
		if listErr != nil {
			return nil, fmt.Errorf("list the snapshots of pool %s: %w", pools[i].Name, listErr)
		}
		for j := range snaps {
			if snaps[j].Name != except && snaps[j].Status.CopyID == copyID {
				claimed[snaps[j].Spec.SourceSubVolume] = true
			}
		}
	}
	return claimed, nil
}

// releaseUnclaimedDisks drops the QEMU change list and the staging file of
// each disk in the set that no snapshot (other than except) has claimed. It
// does nothing for a VM pod that has gone: the files are cleared by the next
// copy of the VM.
func (m *Manager) releaseUnclaimedDisks(ctx context.Context, vm *vmSnapshots, set *v1alpha1.VMCopySet, except string) error {
	claimed, err := m.claimedDisks(ctx, set.ID, except)
	if err != nil {
		return err
	}
	if !samePod(ctx, vm.kube, set.PodNamespace, set.PodName, set.PodUID) {
		return nil
	}
	target := vmcopy.VM{Namespace: set.PodNamespace, Pod: set.PodName, Name: set.VMIName}
	for _, d := range set.Disks {
		if claimed[d.SubVolume] {
			continue
		}
		cp := &vmcopy.Copy{ID: set.ID, Bitmap: set.Bitmap, Disks: []vmcopy.Disk{{Volume: d.Volume, StagingFile: d.StagingFile}}}
		if err := vm.copier.Finish(ctx, target, cp, true); err != nil {
			return fmt.Errorf("release disk %s of copy %s: %w", d.Volume, set.ID, err)
		}
	}
	return nil
}

// releaseExpiredCopySets releases the disks that no snapshot claimed, once the
// time to join a copy set has passed and QEMU has finished the copy.
func (m *Manager) releaseExpiredCopySets(ctx context.Context, vm *vmSnapshots, pass *vmPass) {
	seen := map[string]bool{}
	for _, owner := range pass.sets {
		if seen[owner.Name] {
			continue
		}
		seen[owner.Name] = true
		m.releaseExpiredCopySet(ctx, vm, owner.Name)
	}
}

func (m *Manager) releaseExpiredCopySet(ctx context.Context, vm *vmSnapshots, name string) {
	m.vmMu.Lock()
	defer m.vmMu.Unlock()
	owner, err := m.k8sClient.GetSnapshot(ctx, name)
	if err != nil || owner.Annotations[snapshotDeletingAnnotation] != "" {
		return
	}
	set := owner.Status.VMCopySet
	if set == nil || m.timeNow().Before(set.JoinUntil.Time) {
		return
	}
	if vc := owner.Status.VMCopy; vc != nil && vc.Phase == vmCopyPhaseCopying {
		return // QEMU still copies: its staging files are in use
	}
	if err := m.releaseUnclaimedDisks(ctx, vm, set, ""); err != nil {
		klog.ErrorS(err, "Failed to release the unclaimed disks of a VM copy; trying again", "snapshot", name)
		return
	}
	owner.Status.VMCopySet = nil
	m.saveSnapshotStatus(ctx, owner)
}

func orDefault(s, d string) string {
	if s == "" {
		return d
	}
	return s
}

func (m *Manager) watchVMSnapshotApply(ctx context.Context, vm *vmSnapshots, snap *v1alpha1.Snapshot) {
	st := snap.Status.VMCopy
	ns := st.PodNamespace
	pool, err := m.k8sClient.GetFileSharePool(ctx, snap.Spec.PoolName)
	if err != nil {
		m.waitVMSnapshot(ctx, snap, fmt.Sprintf("get the pool: %v", err))
		return
	}
	share, err := poolShareMount(pool, snap.Spec.ShareID)
	if err != nil {
		m.waitVMSnapshot(ctx, snap, err.Error())
		return
	}
	target, cp := vmOfCopy(ns, st)
	done, err := m.shareData.ApplyVMDisk(ctx, "snapvm-"+snap.Name, share, snap.Spec.SourceSubPath, st.StagingFile, snap.Spec.SnapshotPath, st.Moment.Time)
	if errors.Is(err, ErrShareJobFailed) {
		m.releaseVMSnapshotCopy(ctx, vm, target, cp, st, ns)
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
	m.releaseVMSnapshotCopy(ctx, vm, target, cp, st, ns)

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

// cancelVMSnapshotForDelete is called when a snapshot is deleted. It marks the
// snapshot, so the copy loop leaves it alone, waits for a loop step that runs
// now, and cancels a QEMU copy in progress. It returns an error when the copy
// cannot be cancelled; the record of the copy stays, so a later delete tries
// again and the copy is never forgotten while it runs.
func (m *Manager) cancelVMSnapshotForDelete(ctx context.Context, snap *v1alpha1.Snapshot) error {
	if m.vm == nil {
		return nil
	}
	// A loop step that runs now may take long (a slow command in the VM pod).
	// CSI calls must not wait for it: ask the caller to try again.
	if !m.vmMu.TryLock() {
		return ErrOperationInProgress
	}
	defer m.vmMu.Unlock()
	fresh, err := m.k8sClient.GetSnapshot(ctx, snap.Name)
	if err != nil {
		return fmt.Errorf("read the snapshot: %w", err)
	}
	// Mark it, so the loop leaves it alone from now on.
	if fresh.Annotations[snapshotDeletingAnnotation] == "" {
		if fresh.Annotations == nil {
			fresh.Annotations = map[string]string{}
		}
		fresh.Annotations[snapshotDeletingAnnotation] = "true"
		if err = m.k8sClient.UpdateSnapshot(ctx, fresh); err != nil {
			return fmt.Errorf("mark the snapshot as deleting: %w", err)
		}
	}
	if st := fresh.Status.VMCopy; st != nil {
		if ns := st.PodNamespace; samePod(ctx, m.vm.kube, ns, st.PodName, st.PodUID) {
			target, cp := vmOfCopy(ns, st)
			switch {
			case st.Leader != "" && st.Phase == vmCopyPhaseCopying:
				// The owner's QEMU copy runs on; this snapshot only leaves it.
			case st.Phase == vmCopyPhaseCopying:
				err = m.vm.copier.Cancel(ctx, target, cp)
			default:
				err = m.vm.copier.Finish(ctx, target, cp, true)
			}
			if err != nil {
				return fmt.Errorf("cancel the VM snapshot copy: %w", err)
			}
		}
	}
	if set := fresh.Status.VMCopySet; set != nil && (fresh.Status.VMCopy == nil || fresh.Status.VMCopy.Phase != vmCopyPhaseCopying) {
		// The owner goes away: the disks that no other snapshot claimed would
		// stay for good, so release them now. (A copy that was still running
		// has just been cancelled, and that removed every staging file.)
		if relErr := m.releaseUnclaimedDisks(ctx, m.vm, set, fresh.Name); relErr != nil {
			klog.ErrorS(relErr, "Failed to release the unclaimed disks of a VM copy", "snapshot", fresh.Name)
		}
	}
	if fresh.Status.VMCopy == nil && fresh.Status.VMCopySet == nil {
		return nil
	}
	fresh.Status.VMCopy = nil
	fresh.Status.VMCopyStarted = nil
	fresh.Status.VMCopySet = nil
	m.saveSnapshotStatus(ctx, fresh)
	return nil
}
