package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
)

const (
	// defaultVMCopySpeedLimitMBps applies when a policy does not set one.
	defaultVMCopySpeedLimitMBps = 20

	// defaultRestorePointKeepHours applies when a policy does not set one.
	defaultRestorePointKeepHours = 24

	vmCopyPhaseCopying = "Copying"
	vmCopyPhaseSending = "Sending"

	consistencyFileSystem = "FileSystemConsistent"
	consistencyCrash      = "CrashConsistent"

	// qemuUID is the user QEMU runs as in a virt-launcher pod. Staging files
	// belong to it, and KubeVirt needs disk images it can read.
	qemuUID = 107

	vmMovingMessage = "copy waits: the VM is moving to another worker"
	vmBusyMessage   = "copy waits: another copy of this VM is running"
)

// VMCopier takes copies of running VM disks. pkg/vmcopy implements it.
type VMCopier interface {
	Start(ctx context.Context, vm vmcopy.VM, volumes []string, opts vmcopy.StartOptions) (*vmcopy.Copy, error)
	Progress(ctx context.Context, vm vmcopy.VM, cp *vmcopy.Copy) (vmcopy.State, string, error)
	Finish(ctx context.Context, vm vmcopy.VM, cp *vmcopy.Copy, removeStaging bool) error
	Commit(ctx context.Context, vm vmcopy.VM, cp *vmcopy.Copy) error
	Cancel(ctx context.Context, vm vmcopy.VM, cp *vmcopy.Copy) error
	RemoveStale(ctx context.Context, vm vmcopy.VM, volumes []string, keepBitmap string) error
	// SetSpeed changes the speed limit of a copy that is running.
	SetSpeed(ctx context.Context, vm vmcopy.VM, cp *vmcopy.Copy, speed int64) error
	// CleanStaleCopies removes staging files and nodes that an earlier copy
	// left, keeps every change list, and returns vmcopy.ErrBusy while a copy
	// runs in the VM.
	CleanStaleCopies(ctx context.Context, vm vmcopy.VM, volumes []string) error
}

// ShareLatency gives the average latency of the shares of a pool, in
// milliseconds, over the last intervals. ok is false when there is no reading.
type ShareLatency interface {
	Latency(ctx context.Context, pool string) (ms float64, ok bool)
}

// SetShareLatency lets copies of running VM disks follow the load on the share
// they read. Without it, every copy runs at a fixed speed.
func (rc *ReplicationController) SetShareLatency(l ShareLatency) {
	rc.shareLatency = l
}

const (
	// adaptStartMBps is the speed an adaptive copy begins at, and the lowest it
	// goes to. adaptStepMBps is how much it grows each time the share is quiet.
	adaptStartMBps = 15
	adaptStepMBps  = 5
	// defaultVMCopyMaxLatencyMs is the share latency above which an adaptive
	// copy slows down.
	defaultVMCopyMaxLatencyMs = 15
)

// vmCopyAdaptive says whether the copy of this policy adapts its speed: the
// policy allows it (the default) and the driver can read the share's latency.
func (rc *ReplicationController) vmCopyAdaptive(policy *v1alpha1.ReplicationPolicy) bool {
	return rc.shareLatency != nil && (policy.Spec.VMCopyAdaptive == nil || *policy.Spec.VMCopyAdaptive)
}

// vmCopyFixedSpeed is the speed of a copy that does not adapt, in bytes per
// second. 0 means no limit.
func vmCopyFixedSpeed(policy *v1alpha1.ReplicationPolicy) int64 {
	if policy.Spec.VMCopySpeedLimitMBps != nil {
		return int64(*policy.Spec.VMCopySpeedLimitMBps) << 20
	}
	return int64(defaultVMCopySpeedLimitMBps) << 20
}

func vmCopyAdaptConfig(policy *v1alpha1.ReplicationPolicy) vmcopy.AdaptConfig {
	cfg := vmcopy.AdaptConfig{Start: adaptStartMBps << 20, Step: adaptStepMBps << 20, LimitMs: defaultVMCopyMaxLatencyMs}
	if policy.Spec.VMCopySpeedLimitMBps != nil && *policy.Spec.VMCopySpeedLimitMBps > 0 {
		cfg.Max = int64(*policy.Spec.VMCopySpeedLimitMBps) << 20
	}
	if policy.Spec.VMCopyMaxLatencyMs != nil {
		cfg.LimitMs = float64(*policy.Spec.VMCopyMaxLatencyMs)
	}
	return cfg
}

// adaptVMCopySpeed moves the speed of a running copy one step, by the latency
// of the share it reads. A change that fails leaves the copy as it was.
func (rc *ReplicationController) adaptVMCopySpeed(ctx context.Context, policy *v1alpha1.ReplicationPolicy, vm vmcopy.VM, cp *vmcopy.Copy, g *vmGroup) {
	if !rc.vmCopyAdaptive(policy) {
		return
	}
	next, changed := stepVMCopySpeed(ctx, rc.shareLatency, rc.vmCopier, vmCopyAdaptConfig(policy), vm, cp, policy.Spec.SourcePoolName, g.states[0].SpeedMBps)
	if !changed {
		return
	}
	for _, st := range g.states {
		st.SpeedMBps = next
	}
}

// stepVMCopySpeed moves the speed of a running copy one step by the latency of
// the pool it reads, and returns the new speed in MB/s. changed is false when
// the speed stayed, or when QEMU would not take the change.
func stepVMCopySpeed(ctx context.Context, latency ShareLatency, copier VMCopier, cfg vmcopy.AdaptConfig, vm vmcopy.VM, cp *vmcopy.Copy, pool string, curMBps int32) (nextMBps int32, changed bool) {
	ms, ok := latency.Latency(ctx, pool)
	cur := int64(curMBps) << 20
	next := cfg.Next(cur, ms, ok)
	if next == cur {
		return curMBps, false
	}
	if err := copier.SetSpeed(ctx, vm, cp, next); err != nil {
		klog.V(2).InfoS("Could not change the speed of a VM disk copy", "vm", klog.KRef(vm.Namespace, vm.Name), "err", err)
		return curMBps, false
	}
	klog.V(4).InfoS("Changed the speed of a VM disk copy", "vm", klog.KRef(vm.Namespace, vm.Name),
		"fromMBps", cur>>20, "toMBps", next>>20, "latencyMs", ms)
	return int32(next >> 20), true // #nosec G115 -- a speed in MB/s fits an int32
}

// SetVMCopier turns on copies of running VM disks in direct NFS mode. Without
// it, the volumes of running VMs are skipped.
func (rc *ReplicationController) SetVMCopier(c VMCopier) {
	rc.vmCopier = c
}

// vmCopyResult is what processVMVolumes did.
type vmCopyResult struct {
	statuses   []v1alpha1.SubVolumeReplicationStatus
	inProgress bool
	failed     int // VM disks whose copy failed in this cycle
}

// vmGroup is the disks of one VM in a policy.
type vmGroup struct {
	members []*v1alpha1.SubVolume
	// For a copy in progress: its state for each member.
	states []*v1alpha1.VMCopyState
	// For a new copy: the VM pod and each member's pod volume name.
	pod     *corev1.Pod
	volumes []string
}

// processVMVolumes handles the SubVolumes that are disks of running VMs: it
// advances copies in progress and, if allowStart, starts new ones. It returns
// the SubVolumes it did not handle, for the file copy path.
func (rc *ReplicationController) processVMVolumes(ctx context.Context, policy *v1alpha1.ReplicationPolicy, svs []v1alpha1.SubVolume, allowStart bool) ([]v1alpha1.SubVolume, vmCopyResult) {
	var res vmCopyResult
	if rc.vmCopier == nil {
		return svs, res
	}
	prev := map[string]v1alpha1.SubVolumeReplicationStatus{}
	for _, s := range policy.Status.SubVolumeStatuses {
		prev[s.SubVolumeName] = s
	}

	var rest []v1alpha1.SubVolume
	inflight := map[string]*vmGroup{}
	fresh := map[types.UID]*vmGroup{}
	var order []*vmGroup
	for i := range svs {
		sv := &svs[i]
		if p, ok := prev[sv.Name]; ok && p.VMCopy != nil {
			g := inflight[p.VMCopy.ID]
			if g == nil {
				g = &vmGroup{}
				inflight[p.VMCopy.ID] = g
				order = append(order, g)
			}
			g.members = append(g.members, sv)
			g.states = append(g.states, p.VMCopy.DeepCopy())
			continue
		}
		if !allowStart {
			rest = append(rest, *sv)
			continue
		}
		pod, vol, err := rc.vmPodForClaim(ctx, sv.Spec.PVCNamespace, sv.Spec.PVCName)
		if err != nil {
			res.statuses = append(res.statuses, vmStatus(prev, sv.Name, fmt.Sprintf("check for a running VM: %v", err)))
			res.failed++
			continue
		}
		if pod == nil {
			rest = append(rest, *sv)
			continue
		}
		if pod.Labels[kubevirtPodLabel] != "virt-launcher" {
			res.statuses = append(res.statuses, vmStatus(prev, sv.Name, fmt.Sprintf(
				"%s %s/%s; hot-plugged disks cannot be copied yet", skippedRunningVMPrefix, pod.Namespace, pod.Name)))
			res.failed++
			continue
		}
		g := fresh[pod.UID]
		if g == nil {
			g = &vmGroup{pod: pod}
			fresh[pod.UID] = g
			order = append(order, g)
		}
		g.members = append(g.members, sv)
		g.volumes = append(g.volumes, vol)
	}

	for _, g := range order {
		if g.pod != nil {
			rc.startVMCopy(ctx, policy, g, prev, &res)
		} else {
			rc.advanceVMCopy(ctx, policy, g, prev, &res)
		}
	}
	return rest, res
}

// vmStatus returns a member's status with the last good copy kept and the
// given error set.
func vmStatus(prev map[string]v1alpha1.SubVolumeReplicationStatus, svName, lastError string) v1alpha1.SubVolumeReplicationStatus {
	s := prev[svName]
	s.SubVolumeName = svName
	s.LastError = lastError
	s.VMCopy = nil
	return s
}

// byMemberName sorts a new copy's members and volumes by SubVolume name.
type byMemberName struct{ g *vmGroup }

func (b byMemberName) Len() int { return len(b.g.members) }
func (b byMemberName) Less(i, j int) bool {
	return b.g.members[i].Name < b.g.members[j].Name
}
func (b byMemberName) Swap(i, j int) {
	b.g.members[i], b.g.members[j] = b.g.members[j], b.g.members[i]
	b.g.volumes[i], b.g.volumes[j] = b.g.volumes[j], b.g.volumes[i]
}

func vmiName(pod *corev1.Pod) string {
	for _, o := range pod.OwnerReferences {
		if o.Kind == "VirtualMachineInstance" {
			return o.Name
		}
	}
	return ""
}

func (rc *ReplicationController) startVMCopy(ctx context.Context, policy *v1alpha1.ReplicationPolicy, g *vmGroup, prev map[string]v1alpha1.SubVolumeReplicationStatus, res *vmCopyResult) {
	fail := func(msg string) {
		for _, sv := range g.members {
			res.statuses = append(res.statuses, vmStatus(prev, sv.Name, msg))
		}
		res.failed += len(g.members)
	}
	// A stable disk order, so the same disk gets the same place every time.
	sort.Sort(byMemberName{g})
	vm := vmcopy.VM{Namespace: g.pod.Namespace, Pod: g.pod.Name, Name: vmiName(g.pod)}
	if vm.Name == "" {
		fail(fmt.Sprintf("VM pod %s/%s has no VirtualMachineInstance owner", vm.Namespace, vm.Pod))
		return
	}
	moving, err := rc.vmMoving(ctx, vm.Namespace, vm.Name)
	if err != nil {
		fail(fmt.Sprintf("check for a VM move: %v", err))
		return
	}
	if moving {
		for _, sv := range g.members {
			res.statuses = append(res.statuses, vmStatus(prev, sv.Name, vmMovingMessage))
		}
		return
	}
	// The change list of the last good copy, if all disks share it.
	base := prev[g.members[0].Name].VMChangeList
	for _, sv := range g.members[1:] {
		if prev[sv.Name].VMChangeList != base {
			base = ""
		}
	}
	if err = rc.vmCopier.RemoveStale(ctx, vm, g.volumes, base); err != nil {
		if errors.Is(err, vmcopy.ErrBusy) {
			// A snapshot copies this VM now. Wait; this is not a failed cycle.
			for _, sv := range g.members {
				res.statuses = append(res.statuses, vmStatus(prev, sv.Name, vmBusyMessage))
			}
			return
		}
		fail(fmt.Sprintf("clear an earlier copy: %v", err))
		return
	}

	speed := vmCopyFixedSpeed(policy)
	if rc.vmCopyAdaptive(policy) {
		speed = vmCopyAdaptConfig(policy).Start
	}
	id := strconv.FormatInt(rc.nowFunc().UnixNano(), 36)
	opts := vmcopy.StartOptions{Speed: speed, ID: id, Base: base}
	cp, err := rc.vmCopier.Start(ctx, vm, g.volumes, opts)
	if errors.Is(err, vmcopy.ErrBaseLost) {
		// The VM restarted or moved, so QEMU lost the change list.
		klog.V(2).InfoS("Change list lost; making a full copy", "policy", policy.Name, "vm", klog.KRef(vm.Namespace, vm.Name), "base", base)
		opts.Base = ""
		cp, err = rc.vmCopier.Start(ctx, vm, g.volumes, opts)
	}
	if err != nil {
		fail(fmt.Sprintf("start the VM disk copy: %v", err))
		return
	}
	klog.V(2).InfoS("Started VM disk copy", "policy", policy.Name, "vm", klog.KRef(vm.Namespace, vm.Name),
		"copy", id, "disks", len(cp.Disks), "fileSystemConsistent", cp.FileSystemConsistent, "freezeError", cp.FreezeError)
	for i, sv := range g.members {
		s := vmStatus(prev, sv.Name, "")
		s.VMCopy = &v1alpha1.VMCopyState{
			ID:                   id,
			Phase:                vmCopyPhaseCopying,
			PodName:              g.pod.Name,
			PodUID:               string(g.pod.UID),
			VMIName:              vm.Name,
			Volume:               g.volumes[i],
			DiskIndex:            int32(i), // #nosec G115 -- a VM has far fewer than 2^31 disks
			StagingFile:          cp.Disks[i].StagingFile,
			SpeedMBps:            int32(speed >> 20), // #nosec G115 -- a speed in MB/s fits an int32
			Moment:               metav1.NewTime(cp.Moment),
			FileSystemConsistent: cp.FileSystemConsistent,
			FreezeError:          cp.FreezeError,
			Bitmap:               cp.Bitmap,
			Base:                 cp.Base,
			Incremental:          cp.Incremental,
		}
		res.statuses = append(res.statuses, s)
	}
	res.inProgress = true
}

func (rc *ReplicationController) advanceVMCopy(ctx context.Context, policy *v1alpha1.ReplicationPolicy, g *vmGroup, prev map[string]v1alpha1.SubVolumeReplicationStatus, res *vmCopyResult) {
	st := g.states[0]
	ns := g.members[0].Spec.PVCNamespace
	vm := vmcopy.VM{Namespace: ns, Pod: st.PodName, Name: st.VMIName}
	cp := &vmcopy.Copy{ID: st.ID, Moment: st.Moment.Time, FileSystemConsistent: st.FileSystemConsistent, FreezeError: st.FreezeError,
		Bitmap: st.Bitmap, Base: st.Base, Incremental: st.Incremental}
	idx := make([]int, len(g.members))
	for i := range idx {
		idx[i] = i
	}
	sort.Slice(idx, func(a, b int) bool { return g.states[idx[a]].DiskIndex < g.states[idx[b]].DiskIndex })
	for _, i := range idx {
		cp.Disks = append(cp.Disks, vmcopy.Disk{Volume: g.states[i].Volume, StagingFile: g.states[i].StagingFile})
	}

	keep := func(lastError string) {
		for i, sv := range g.members {
			s := prev[sv.Name]
			s.LastError = lastError
			s.VMCopy = g.states[i]
			res.statuses = append(res.statuses, s)
		}
		res.inProgress = true
	}
	end := func(msg string, failed bool) {
		for _, sv := range g.members {
			res.statuses = append(res.statuses, vmStatus(prev, sv.Name, msg))
		}
		if failed {
			res.failed += len(g.members)
		}
	}

	switch st.Phase {
	case vmCopyPhaseCopying:
		pod := &corev1.Pod{}
		err := rc.directClient.Get(ctx, types.NamespacedName{Namespace: ns, Name: st.PodName}, pod)
		if apierrors.IsNotFound(err) || (err == nil && string(pod.UID) != st.PodUID) {
			end("copy lost: the VM restarted or moved during the copy; the next cycle starts a new copy", false)
			return
		}
		if err != nil {
			keep(fmt.Sprintf("check the VM pod: %v", err))
			return
		}
		if moving, merr := rc.vmMoving(ctx, ns, st.VMIName); merr != nil {
			klog.ErrorS(merr, "Failed to check for a VM move", "vm", klog.KRef(ns, st.VMIName))
		} else if moving {
			if err = rc.vmCopier.Cancel(ctx, vm, cp); err != nil {
				keep(fmt.Sprintf("cancel the copy for a VM move: %v", err))
				return
			}
			klog.V(2).InfoS("Cancelled VM disk copy for a VM move", "policy", policy.Name, "vm", klog.KRef(ns, st.VMIName), "copy", st.ID)
			end(vmMovingMessage, false)
			return
		}
		state, msg, err := rc.vmCopier.Progress(ctx, vm, cp)
		if err != nil {
			keep(fmt.Sprintf("check the copy: %v", err))
			return
		}
		switch state {
		case vmcopy.Running:
			rc.adaptVMCopySpeed(ctx, policy, vm, cp, g)
			keep("")
		case vmcopy.Failed, vmcopy.Lost:
			if err := rc.vmCopier.Finish(ctx, vm, cp, true); err != nil {
				klog.ErrorS(err, "Failed to clean up a failed VM disk copy", "vm", klog.KRef(ns, st.VMIName), "copy", st.ID)
			}
			end(fmt.Sprintf("copy failed: %s", msg), true)
		case vmcopy.Done:
			if err := rc.vmCopier.Finish(ctx, vm, cp, false); err != nil {
				keep(fmt.Sprintf("release the finished copy: %v", err))
				return
			}
			for i, sv := range g.members {
				if err := rc.createVMApplyResources(ctx, policy, sv, g.states[i], prev[sv.Name].CopyMoment); err != nil {
					for _, m := range g.members {
						rc.cleanupReplicationResources(ctx, policy, m.Name)
					}
					end(fmt.Sprintf("create apply Job for %s: %v", sv.Name, err), true)
					return
				}
				g.states[i].Phase = vmCopyPhaseSending
			}
			keep("")
		}

	case vmCopyPhaseSending:
		allDone := true
		var failedNames, wrongBase, gone []string
		var failures []string
		results := make([]replJobResult, len(g.members))
		for i, sv := range g.members {
			done, ok, jr, err := rc.checkReplicationJob(ctx, policy.Name, sv.Name)
			if err != nil {
				keep(fmt.Sprintf("check apply Job: %v", err))
				return
			}
			if !done {
				allDone = false
				// The Job was made when the copy moved to Sending. If it is
				// missing now, it finished and expired (or was deleted)
				// before its result was read. Running it again is safe:
				// the apply step sees that the disk already shows the copy.
				if exists, gerr := rc.replicationJobExists(ctx, policy.Name, sv.Name); gerr == nil && !exists {
					gone = append(gone, sv.Name)
				}
				// Recreate anything that went missing; this does nothing if
				// the Job exists.
				if err := rc.createVMApplyResources(ctx, policy, sv, g.states[i], prev[sv.Name].CopyMoment); err != nil {
					keep(fmt.Sprintf("create apply Job for %s: %v", sv.Name, err))
					return
				}
				continue
			}
			if !ok {
				failedNames = append(failedNames, sv.Name)
				if jr.Failure != "" {
					failures = append(failures, jr.Failure)
				}
			} else if jr.WrongBase {
				failedNames = append(failedNames, sv.Name)
				wrongBase = append(wrongBase, fmt.Sprintf("%s shows %s", sv.Name, jr.DiskMoment))
			}
			results[i] = jr
		}
		if !allDone {
			note := ""
			if len(gone) > 0 {
				note = fmt.Sprintf("apply Job was gone before its result was read (%s); running it again", strings.Join(gone, ", "))
				klog.InfoS("VM disk apply Job missing; running it again", "policy", policy.Name, "vm", klog.KRef(ns, st.VMIName), "copy", st.ID, "subVolumes", gone)
			}
			keep(note)
			return
		}
		// The new change list becomes the base only when every disk of the VM
		// holds the copy. Otherwise it is dropped, and the next copy reads
		// the old base again, which covers every disk.
		changeList := prev[g.members[0].Name].VMChangeList
		if rc.sameVMPod(ctx, ns, st) {
			if len(failedNames) == 0 {
				if err := rc.vmCopier.Commit(ctx, vm, cp); err != nil {
					klog.ErrorS(err, "Failed to commit VM disk copy", "vm", klog.KRef(ns, st.VMIName), "copy", st.ID)
				}
				changeList = st.Bitmap
			} else if err := rc.vmCopier.Finish(ctx, vm, cp, true); err != nil {
				klog.ErrorS(err, "Failed to drop the change list of a failed copy", "vm", klog.KRef(ns, st.VMIName), "copy", st.ID)
			}
		} else {
			// The VM restarted or moved: its change lists are gone.
			changeList = ""
		}
		if len(wrongBase) > 0 {
			// A far-site disk is not where this copy starts from, so no
			// change copy can be applied to it. The next copy is a full copy.
			changeList = ""
		}
		for i, sv := range g.members {
			rc.cleanupReplicationResources(ctx, policy, sv.Name)
			s := vmStatus(prev, sv.Name, "")
			s.VMChangeList = changeList
			if len(failedNames) > 0 {
				why := ""
				if len(failures) > 0 {
					why = " (" + failures[0] + ")"
				}
				s.LastError = fmt.Sprintf("apply Job failed for %s%s; the destination disks of this VM may now show different moments",
					strings.Join(failedNames, ", "), why)
			}
			if len(wrongBase) > 0 {
				s.LastError = fmt.Sprintf("the far-site disk is not at the moment of the last copy (%s); the next copy is a full copy",
					strings.Join(wrongBase, ", "))
			}
			if !slices.Contains(failedNames, sv.Name) {
				moment := st.Moment
				s.LastSyncTime = &moment
				s.CopyMoment = &moment
				s.Consistency, s.ConsistencyReason = consistencyFileSystem, ""
				if !st.FileSystemConsistent {
					s.Consistency, s.ConsistencyReason = consistencyCrash, st.FreezeError
				}
				if n := results[i].BytesWritten; n > 0 {
					s.BytesSynced = &n
				}
				s.RestorePoints, s.OldestRestorePoint = results[i].RestorePoints, nil
				if o := results[i].OldestRestorePoint; o != nil {
					t := metav1.NewTime(*o)
					s.OldestRestorePoint = &t
				}
				rc.recordSubVolumeSyncTime(policy.Name, sv.Name)
			}
			res.statuses = append(res.statuses, s)
		}
		if len(failedNames) > 0 {
			res.failed += len(g.members)
		}
	}
}

// sameVMPod reports whether the VM pod of a copy still runs, so QEMU still
// has the copy's change lists.
func (rc *ReplicationController) sameVMPod(ctx context.Context, ns string, st *v1alpha1.VMCopyState) bool {
	return samePod(ctx, rc.directClient, ns, st.PodName, st.PodUID)
}

// vmMoving reports whether a live migration of the VMI is requested or running.
func (rc *ReplicationController) vmMoving(ctx context.Context, namespace, vmi string) (bool, error) {
	return vmInstanceMoving(ctx, rc.directClient, namespace, vmi)
}

// advanceVMCopies moves copies of running VM disks forward between scheduled
// cycles, so a copy does not wait a whole schedule interval for each step.
func (rc *ReplicationController) advanceVMCopies(ctx context.Context, policy *v1alpha1.ReplicationPolicy) {
	hasCopy := false
	for _, s := range policy.Status.SubVolumeStatuses {
		if s.VMCopy != nil {
			hasCopy = true
			break
		}
	}
	if !hasCopy || rc.vmCopier == nil {
		return
	}
	subVolumes, err := rc.k8sClient.ListSubVolumes(ctx, policy.Spec.SourcePoolName)
	if err != nil {
		klog.ErrorS(err, "Failed to list SubVolumes for VM disk copies", "policy", policy.Name)
		return
	}
	_, res := rc.processVMVolumes(ctx, policy, filterSubVolumes(subVolumes, policy.Spec.SubVolumeSelector), false)
	if len(res.statuses) == 0 {
		return
	}
	byName := map[string]int{}
	for i, s := range policy.Status.SubVolumeStatuses {
		byName[s.SubVolumeName] = i
	}
	if res.failed > 0 {
		policy.Status.LastError = vmProblemsError(0, res.failed)
	}
	synced := false
	for _, s := range res.statuses {
		if s.VMCopy == nil && s.LastError == "" && s.LastSyncTime != nil {
			synced = true
		}
		if i, ok := byName[s.SubVolumeName]; ok {
			policy.Status.SubVolumeStatuses[i] = s
		} else {
			policy.Status.SubVolumeStatuses = append(policy.Status.SubVolumeStatuses, s)
		}
	}
	if synced {
		now := metav1.NewTime(rc.nowFunc())
		policy.Status.LastSyncTime = &now
		// A good copy replaces the error of an earlier try, unless some
		// volume still reports one.
		clean := true
		for _, s := range policy.Status.SubVolumeStatuses {
			if s.LastError != "" {
				clean = false
				break
			}
		}
		if clean {
			policy.Status.LastError = ""
		}
	}
	if err := rc.k8sClient.UpdateReplicationPolicyStatus(ctx, policy); err != nil {
		klog.ErrorS(err, "Failed to update policy status for VM disk copies", "policy", policy.Name)
	}
}

// createVMApplyResources creates the PVs, PVCs and Job that apply a staged VM
// disk copy at the destination. Each step does nothing if its object exists.
func (rc *ReplicationController) createVMApplyResources(ctx context.Context, policy *v1alpha1.ReplicationPolicy, sv *v1alpha1.SubVolume, st *v1alpha1.VMCopyState, from *metav1.Time) error {
	if err := rc.ensureReplicationSourcePV(ctx, sv, policy.Name); err != nil {
		return fmt.Errorf("source PV: %w", err)
	}
	if err := rc.ensureReplicationPVC(ctx, replSourcePVName(policy.Name, sv.Name), replSourcePVCName(policy.Name, sv.Name), policy.Name, sv.Name, "source"); err != nil {
		return fmt.Errorf("source PVC: %w", err)
	}
	if isReceiverMode(policy) {
		return rc.ensureVMSendJob(ctx, policy, sv, st, from)
	}
	if err := rc.ensureReplicationDestPV(ctx, policy, sv.Name); err != nil {
		return fmt.Errorf("dest PV: %w", err)
	}
	if err := rc.ensureReplicationPVC(ctx, replDestPVName(policy.Name, sv.Name), replDestPVCName(policy.Name, sv.Name), policy.Name, sv.Name, "dest"); err != nil {
		return fmt.Errorf("dest PVC: %w", err)
	}
	return rc.ensureVMApplyJob(ctx, policy, sv, st, from)
}

// vmCopyJobFlags are the Job flags that name the copy: its moment, where it
// starts from, and whether it holds the whole disk. A change copy needs a
// destination disk with a known moment; without one the copy must be full,
// and it is (the change list is new).
func vmCopyJobFlags(policy *v1alpha1.ReplicationPolicy, st *v1alpha1.VMCopyState, from *metav1.Time) []string {
	keep := int32(defaultRestorePointKeepHours)
	if policy.Spec.RestorePointKeepHours != nil {
		keep = *policy.Spec.RestorePointKeepHours
	}
	flags := []string{
		"--to=" + st.Moment.UTC().Format(time.RFC3339Nano),
		"--keep=" + (time.Duration(keep) * time.Hour).String(),
	}
	if from != nil {
		flags = append(flags, "--from="+from.UTC().Format(time.RFC3339Nano))
	}
	if !st.Incremental || from == nil {
		flags = append(flags, "--full")
	}
	return flags
}

// ensureVMApplyJob creates the Job that replaces the destination disk with
// the staged copy. An init container, as root, makes the destination folder
// and writes the metadata file that failover reads. The main container runs
// as the QEMU user, so it can delete the staging file and the new disk image
// belongs to the user KubeVirt runs QEMU as.
// from is the moment the destination disk shows now, nil if it has no copy.
func (rc *ReplicationController) ensureVMApplyJob(ctx context.Context, policy *v1alpha1.ReplicationPolicy, sv *v1alpha1.SubVolume, st *v1alpha1.VMCopyState, from *metav1.Time) error {
	jobName := replJobName(policy.Name, sv.Name)
	existing := &batchv1.Job{}
	if err := rc.directClient.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, existing); err == nil {
		return nil
	}
	if rc.driverImage == "" {
		return fmt.Errorf("no driver image set for VM disk apply Jobs")
	}

	meta := &SubVolumeMetadata{
		SubVolumeName:     sv.Name,
		Spec:              sv.Spec,
		Labels:            sv.Labels,
		LastSyncTime:      st.Moment.Time,
		ReplicationPolicy: policy.Name,
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding metadata: %w", err)
	}
	destDir := "/dest" + policy.Spec.DestinationBasePath + sv.Spec.SubPath
	prepare := fmt.Sprintf(`set -eu
mkdir -p "%s"
chmod 0777 "%s"
cat > "%s/.subvolume-metadata.json" <<'METADATA'
%s
METADATA
`, destDir, destDir, destDir, string(metaJSON))

	labels := map[string]string{
		replTempLabel:      "true",
		replPolicyLabel:    policy.Name,
		replSubVolumeLabel: sv.Name,
	}
	backoffLimit := int32(3)
	ttl := int32(600)
	resources := corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("64Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	// Mount only this volume's folder from the source share, so a link in it
	// cannot reach another volume on the same share.
	mounts := []corev1.VolumeMount{
		{Name: "source", MountPath: "/source", SubPath: strings.TrimPrefix(sv.Spec.SubPath, "/")},
		{Name: "dest", MountPath: "/dest"},
	}
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: replJobNamespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:  int64Ptr(qemuUID),
						RunAsGroup: int64Ptr(qemuUID),
					},
					InitContainers: []corev1.Container{{
						Name:            "prepare",
						Image:           rc.driverImage,
						Command:         []string{"/bin/sh", "-c", prepare},
						SecurityContext: &corev1.SecurityContext{RunAsUser: int64Ptr(0)},
						Resources:       resources,
						VolumeMounts:    []corev1.VolumeMount{{Name: "dest", MountPath: "/dest"}},
					}},
					Containers: []corev1.Container{{
						Name:         "apply",
						Image:        rc.driverImage,
						Command:      vmApplyCommand(policy, st, from, destDir),
						Resources:    resources,
						VolumeMounts: mounts,
					}},
					Volumes: []corev1.Volume{
						{Name: "source", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: replSourcePVCName(policy.Name, sv.Name)}}},
						{Name: "dest", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: replDestPVCName(policy.Name, sv.Name)}}},
					},
				},
			},
		},
	}
	if err := rc.directClient.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating VM disk apply Job %s/%s: %w", replJobNamespace, jobName, err)
	}
	klog.V(2).InfoS("Created VM disk apply Job", "job", klog.KRef(replJobNamespace, jobName), "policy", policy.Name, "subVolume", sv.Name)
	return nil
}

// vmApplyCommand is the command line of a VM disk apply Job.
func vmApplyCommand(policy *v1alpha1.ReplicationPolicy, st *v1alpha1.VMCopyState, from *metav1.Time, destDir string) []string {
	cmd := []string{"/usr/local/bin/vpc-file-pool-csi",
		"--mode=apply-vm-disk",
		"--staging-path=/source/" + st.StagingFile,
		"--disk-path=" + destDir + "/disk.img",
	}
	return append(cmd, vmCopyJobFlags(policy, st, from)...)
}

// ensureVMSendJob creates the Job that sends the staged copy to the receiver
// at the far site, which applies it. The Job runs as the QEMU user, so it can
// delete the staging file. It mounts only this volume's folder from the source
// share, and the receiver's token and CA certificate.
func (rc *ReplicationController) ensureVMSendJob(ctx context.Context, policy *v1alpha1.ReplicationPolicy, sv *v1alpha1.SubVolume, st *v1alpha1.VMCopyState, from *metav1.Time) error {
	jobName := replJobName(policy.Name, sv.Name)
	existing := &batchv1.Job{}
	if err := rc.directClient.Get(ctx, types.NamespacedName{Namespace: replJobNamespace, Name: jobName}, existing); err == nil {
		return nil
	}
	if rc.driverImage == "" {
		return fmt.Errorf("no driver image set for VM disk send Jobs")
	}
	meta := &SubVolumeMetadata{
		SubVolumeName:     sv.Name,
		Spec:              sv.Spec,
		Labels:            sv.Labels,
		LastSyncTime:      st.Moment.Time,
		ReplicationPolicy: policy.Name,
	}
	metaJSON, err := json.MarshalIndent(meta, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding metadata: %w", err)
	}

	cmd := []string{"/usr/local/bin/vpc-file-pool-csi",
		"--mode=send-vm-disk",
		"--receiver-endpoint=" + policy.Spec.DestinationEndpoint,
		"--auth-token-file=/etc/replication/token",
		"--staging-path=/source/" + st.StagingFile,
		"--dest-base-path=" + policy.Spec.DestinationBasePath,
		"--sub-path=" + sv.Spec.SubPath,
		"--subvolume-name=" + sv.Name,
		"--vm-group=" + sv.Spec.PVCNamespace + "/" + st.VMIName,
		"--copy-id=" + st.ID,
		"--metadata-json=" + string(metaJSON),
	}
	cmd = append(cmd, vmCopyJobFlags(policy, st, from)...)

	mounts := []corev1.VolumeMount{
		{Name: "source", MountPath: "/source", SubPath: strings.TrimPrefix(sv.Spec.SubPath, "/")},
		{Name: "auth-token", MountPath: "/etc/replication", ReadOnly: true},
	}
	volumes := []corev1.Volume{
		{Name: "source", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
			ClaimName: replSourcePVCName(policy.Name, sv.Name)}}},
		{Name: "auth-token", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: policy.Spec.DestinationAuthSecretRef}}},
	}
	if policy.Spec.DestinationCACertSecretRef != "" {
		cmd = append(cmd, "--ca-cert-file=/etc/replication-ca/ca.crt")
		mounts = append(mounts, corev1.VolumeMount{Name: "ca-cert", MountPath: "/etc/replication-ca", ReadOnly: true})
		volumes = append(volumes, corev1.Volume{Name: "ca-cert", VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: policy.Spec.DestinationCACertSecretRef}}})
	}

	labels := map[string]string{
		replTempLabel:      "true",
		replPolicyLabel:    policy.Name,
		replSubVolumeLabel: sv.Name,
	}
	backoffLimit := int32(3)
	ttl := int32(600)
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: jobName, Namespace: replJobNamespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoffLimit,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyOnFailure,
					SecurityContext: &corev1.PodSecurityContext{
						RunAsUser:  int64Ptr(qemuUID),
						RunAsGroup: int64Ptr(qemuUID),
					},
					Containers: []corev1.Container{{
						Name:    "send",
						Image:   rc.driverImage,
						Command: cmd,
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("100m"),
								corev1.ResourceMemory: resource.MustParse("128Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("1"),
								corev1.ResourceMemory: resource.MustParse("512Mi"),
							},
						},
						VolumeMounts: mounts,
					}},
					Volumes: volumes,
				},
			},
		},
	}
	if err := rc.directClient.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("creating VM disk send Job %s/%s: %w", replJobNamespace, jobName, err)
	}
	klog.V(2).InfoS("Created VM disk send Job", "job", klog.KRef(replJobNamespace, jobName), "policy", policy.Name, "subVolume", sv.Name, "endpoint", policy.Spec.DestinationEndpoint)
	return nil
}
