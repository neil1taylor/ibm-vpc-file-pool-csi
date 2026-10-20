package pool

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

// fakeVMCopier records calls and returns programmed results.
type fakeVMCopier struct {
	mu sync.Mutex

	starts      []fakeStart
	startErr    error
	freezeErr   string
	state       vmcopy.State
	stateMsg    string
	finishes    []bool // removeStaging of each Finish call
	cancels     int
	removeStale int
	moment      time.Time
}

type fakeStart struct {
	vm      vmcopy.VM
	volumes []string
	speed   int64
	id      string
}

func newFakeVMCopier() *fakeVMCopier {
	return &fakeVMCopier{moment: time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)}
}

func (f *fakeVMCopier) Start(_ context.Context, vm vmcopy.VM, volumes []string, speed int64, id string) (*vmcopy.Copy, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.starts = append(f.starts, fakeStart{vm, volumes, speed, id})
	if f.startErr != nil {
		return nil, f.startErr
	}
	cp := &vmcopy.Copy{ID: id, Moment: f.moment, FileSystemConsistent: f.freezeErr == "", FreezeError: f.freezeErr}
	for _, v := range volumes {
		cp.Disks = append(cp.Disks, vmcopy.Disk{Volume: v, StagingFile: vmcopy.StagingPrefix + id + ".qcow2"})
	}
	return cp, nil
}

func (f *fakeVMCopier) Progress(_ context.Context, _ vmcopy.VM, _ *vmcopy.Copy) (vmcopy.State, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.state, f.stateMsg, nil
}

func (f *fakeVMCopier) Finish(_ context.Context, _ vmcopy.VM, _ *vmcopy.Copy, removeStaging bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishes = append(f.finishes, removeStaging)
	return nil
}

func (f *fakeVMCopier) Cancel(_ context.Context, _ vmcopy.VM, _ *vmcopy.Copy) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels++
	return nil
}

func (f *fakeVMCopier) RemoveStale(_ context.Context, _ vmcopy.VM, _ []string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeStale++
	return nil
}

var vmimGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstanceMigration"}

func newVMCopyDirectClient() client.Client {
	s := newReplTestScheme()
	s.AddKnownTypeWithName(vmimGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(vmimGVK.GroupVersion().WithKind("VirtualMachineInstanceMigrationList"), &unstructured.UnstructuredList{})
	return fakeclient.NewClientBuilder().WithScheme(s).WithStatusSubresource(&batchv1.Job{}).Build()
}

// newLauncherPod returns a running virt-launcher pod for VMI vmi that mounts
// the given claims, keyed by pod volume name.
func newLauncherPod(name, uid, vmi string, claims map[string]string) *corev1.Pod {
	p := newVMPod(name, "default", "virt-launcher", "", corev1.PodRunning)
	p.UID = types.UID(uid)
	p.OwnerReferences = []metav1.OwnerReference{{APIVersion: "kubevirt.io/v1", Kind: "VirtualMachineInstance", Name: vmi, UID: "vmi-uid"}}
	p.Spec.Volumes = nil
	for vol, claim := range claims {
		p.Spec.Volumes = append(p.Spec.Volumes, corev1.Volume{Name: vol, VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
		}})
	}
	return p
}

type vmCopyEnv struct {
	k      *replFakeK8sClient
	dc     client.Client
	cop    *fakeVMCopier
	rc     *ReplicationController
	clock  time.Time
	ctx    context.Context
	policy string
}

// newVMCopyEnv makes a policy on a pool with two SubVolumes, both disks of
// VM "web": rootdisk (claim root-pvc) and datadisk (claim data-pvc).
func newVMCopyEnv(t *testing.T) *vmCopyEnv {
	t.Helper()
	e := &vmCopyEnv{k: newReplFakeK8sClient(), dc: newVMCopyDirectClient(), cop: newFakeVMCopier(),
		ctx: context.Background(), policy: "prod-to-dr", clock: time.Date(2026, 10, 20, 12, 0, 30, 0, time.UTC)}
	sv1 := newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 20)
	sv1.Spec.PVCName = "root-pvc"
	sv2 := newTestSubVolume(vmGuardSV2, "prod-pool", "share-1", 10)
	sv2.Spec.PVCName = "data-pvc"
	e.k.addSubVolume(sv1)
	e.k.addSubVolume(sv2)
	p := newTestReplicationPolicy(e.policy, "prod-pool", "1h")
	p.Spec.MaxParallelSyncs = 4
	e.k.addPolicy(p)
	if err := e.dc.Create(e.ctx, newLauncherPod("virt-launcher-web-abcde", "pod-uid-1", "web",
		map[string]string{"rootdisk": "root-pvc", "datadisk": "data-pvc"})); err != nil {
		t.Fatal(err)
	}
	e.rc = newReplController(e.k, e.dc, newFakeNFSOperations())
	e.rc.SetDriverImage("registry.example/driver:test")
	e.rc.SetVMCopier(e.cop)
	e.rc.SetNowFunc(func() time.Time { return e.clock })
	return e
}

// tick runs one controller cycle a little later than the last one.
func (e *vmCopyEnv) tick() {
	e.clock = e.clock.Add(30 * time.Second)
	e.rc.processOnce(e.ctx)
}

func (e *vmCopyEnv) status(t *testing.T, sv string) v1alpha1.SubVolumeReplicationStatus {
	t.Helper()
	for _, s := range e.k.getPolicy(e.policy).Status.SubVolumeStatuses {
		if s.SubVolumeName == sv {
			return s
		}
	}
	t.Fatalf("no status for %s", sv)
	return v1alpha1.SubVolumeReplicationStatus{}
}

func (e *vmCopyEnv) job(t *testing.T, sv string) *batchv1.Job {
	t.Helper()
	job := &batchv1.Job{}
	if err := e.dc.Get(e.ctx, types.NamespacedName{Namespace: replJobNamespace, Name: replJobName(e.policy, sv)}, job); err != nil {
		return nil
	}
	return job
}

func (e *vmCopyEnv) succeedJobs(t *testing.T) {
	t.Helper()
	for _, sv := range []string{vmGuardSV1, vmGuardSV2} {
		job := e.job(t, sv)
		if job == nil {
			t.Fatalf("no apply Job for %s", sv)
		}
		job.Status.Succeeded = 1
		if err := e.dc.Status().Update(e.ctx, job); err != nil {
			t.Fatal(err)
		}
	}
}

func TestVMCopy_StartsOneCopyForAllDisksOfAVM(t *testing.T) {
	e := newVMCopyEnv(t)
	e.tick()

	if len(e.cop.starts) != 1 {
		t.Fatalf("expected one Start for the VM, got %d", len(e.cop.starts))
	}
	st := e.cop.starts[0]
	if st.vm != (vmcopy.VM{Namespace: "default", Pod: "virt-launcher-web-abcde", Name: "web"}) {
		t.Errorf("Start VM = %+v", st.vm)
	}
	if strings.Join(st.volumes, ",") != "rootdisk,datadisk" {
		t.Errorf("Start volumes = %v, want rootdisk,datadisk in SubVolume name order", st.volumes)
	}
	if st.speed != 20<<20 {
		t.Errorf("Start speed = %d, want the 20 MB/s default", st.speed)
	}
	if e.cop.removeStale != 1 {
		t.Errorf("expected leftovers cleared before the copy, got %d RemoveStale calls", e.cop.removeStale)
	}
	for i, sv := range []string{vmGuardSV1, vmGuardSV2} {
		s := e.status(t, sv)
		if s.VMCopy == nil || s.VMCopy.Phase != "Copying" || s.VMCopy.ID != st.id || s.VMCopy.DiskIndex != int32(i) ||
			s.VMCopy.PodUID != "pod-uid-1" || s.VMCopy.VMIName != "web" {
			t.Errorf("%s VMCopy = %+v", sv, s.VMCopy)
		}
		if e.job(t, sv) != nil {
			t.Errorf("expected no Job for %s while QEMU copies", sv)
		}
	}
}

func TestVMCopy_SpeedLimitSetting(t *testing.T) {
	for _, tc := range []struct {
		limit *int32
		want  int64
	}{{int32Ptr(50), 50 << 20}, {int32Ptr(0), 0}} {
		e := newVMCopyEnv(t)
		p := e.k.getPolicy(e.policy)
		p.Spec.VMCopySpeedLimitMBps = tc.limit
		e.k.addPolicy(p)
		e.tick()
		if got := e.cop.starts[0].speed; got != tc.want {
			t.Errorf("limit %d: speed = %d, want %d", *tc.limit, got, tc.want)
		}
	}
}

func TestVMCopy_FullCycle(t *testing.T) {
	e := newVMCopyEnv(t)
	e.tick() // start

	// Not due again for an hour, but the copy still advances every tick.
	e.cop.state = vmcopy.Running
	e.tick()
	if e.job(t, vmGuardSV1) != nil {
		t.Fatal("expected no Job while QEMU still copies")
	}

	e.cop.state = vmcopy.Done
	e.tick()
	if len(e.cop.finishes) != 1 || e.cop.finishes[0] {
		t.Fatalf("expected one Finish that keeps the staging files, got %v", e.cop.finishes)
	}
	for _, sv := range []string{vmGuardSV1, vmGuardSV2} {
		if s := e.status(t, sv); s.VMCopy == nil || s.VMCopy.Phase != "Sending" {
			t.Fatalf("%s VMCopy = %+v, want Sending", sv, s.VMCopy)
		}
	}
	job := e.job(t, vmGuardSV1)
	if job == nil {
		t.Fatal("expected an apply Job")
	}
	c := job.Spec.Template.Spec.Containers[0]
	args := strings.Join(append(c.Command, c.Args...), " ")
	for _, want := range []string{"--mode=apply-vm-disk",
		"--staging-path=/source/pvcs/" + vmGuardSV1 + "/" + vmcopy.StagingPrefix + e.cop.starts[0].id + ".qcow2",
		"--disk-path=/dest/pvcs/pvcs/" + vmGuardSV1 + "/disk.img"} {
		if !strings.Contains(args, want) {
			t.Errorf("apply Job command %q lacks %q", args, want)
		}
	}
	if c.Image != "registry.example/driver:test" {
		t.Errorf("apply Job image = %q", c.Image)
	}
	sc := job.Spec.Template.Spec.SecurityContext
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != 107 || sc.RunAsGroup == nil || *sc.RunAsGroup != 107 {
		t.Errorf("expected the apply Job to run as the QEMU user 107:107, got %+v", sc)
	}
	for _, m := range c.VolumeMounts {
		if m.Name == "source" && m.ReadOnly {
			t.Error("expected the source mounted read-write, so the Job can delete the staging file")
		}
	}

	e.succeedJobs(t)
	e.tick()
	for _, sv := range []string{vmGuardSV1, vmGuardSV2} {
		s := e.status(t, sv)
		if s.VMCopy != nil {
			t.Errorf("%s: expected the copy state cleared, got %+v", sv, s.VMCopy)
		}
		if s.LastSyncTime == nil || !s.LastSyncTime.Time.Equal(e.cop.moment) {
			t.Errorf("%s: LastSyncTime = %v, want the copy moment %v", sv, s.LastSyncTime, e.cop.moment)
		}
		if s.CopyMoment == nil || !s.CopyMoment.Time.Equal(e.cop.moment) || s.Consistency != "FileSystemConsistent" || s.LastError != "" {
			t.Errorf("%s: status = %+v", sv, s)
		}
		if e.job(t, sv) != nil {
			t.Errorf("%s: expected the apply Job cleaned up", sv)
		}
	}
}

func TestVMCopy_CrashConsistentWhenFreezeFails(t *testing.T) {
	e := newVMCopyEnv(t)
	e.cop.freezeErr = "guest agent is not connected"
	e.tick()
	e.cop.state = vmcopy.Done
	e.tick()
	e.succeedJobs(t)
	e.tick()
	s := e.status(t, vmGuardSV1)
	if s.Consistency != "CrashConsistent" || !strings.Contains(s.ConsistencyReason, "guest agent is not connected") {
		t.Errorf("status = %+v", s)
	}
}

func TestVMCopy_CancelsWhenVMMoves(t *testing.T) {
	e := newVMCopyEnv(t)
	e.tick()
	e.cop.state = vmcopy.Running

	mig := &unstructured.Unstructured{}
	mig.SetGroupVersionKind(vmimGVK)
	mig.SetNamespace("default")
	mig.SetName("web-migration")
	if err := unstructured.SetNestedField(mig.Object, "web", "spec", "vmiName"); err != nil {
		t.Fatal(err)
	}
	if err := unstructured.SetNestedField(mig.Object, "Scheduling", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := e.dc.Create(e.ctx, mig); err != nil {
		t.Fatal(err)
	}
	e.tick()

	if e.cop.cancels != 1 {
		t.Fatalf("expected the copy cancelled, got %d Cancel calls", e.cop.cancels)
	}
	p := e.k.getPolicy(e.policy)
	if p.Status.ConsecutiveFailures != 0 {
		t.Errorf("a cancel for a VM move must not count as a failure, got %d", p.Status.ConsecutiveFailures)
	}
	for _, sv := range []string{vmGuardSV1, vmGuardSV2} {
		s := e.status(t, sv)
		if s.VMCopy != nil || !strings.Contains(s.LastError, "VM is moving") {
			t.Errorf("%s: status = %+v", sv, s)
		}
	}

	// While the move runs, no new copy starts.
	e.clock = e.clock.Add(2 * time.Hour)
	e.rc.processOnce(e.ctx)
	if len(e.cop.starts) != 1 {
		t.Errorf("expected no new copy while the VM moves, got %d starts", len(e.cop.starts))
	}

	// A finished migration does not block a copy.
	if err := unstructured.SetNestedField(mig.Object, "Succeeded", "status", "phase"); err != nil {
		t.Fatal(err)
	}
	if err := e.dc.Update(e.ctx, mig); err != nil {
		t.Fatal(err)
	}
	e.clock = e.clock.Add(2 * time.Hour)
	e.rc.processOnce(e.ctx)
	if len(e.cop.starts) != 2 {
		t.Errorf("expected a new copy after the move, got %d starts", len(e.cop.starts))
	}
}

func TestVMCopy_LostWhenPodChanges(t *testing.T) {
	e := newVMCopyEnv(t)
	e.tick()

	old := &corev1.Pod{}
	if err := e.dc.Get(e.ctx, types.NamespacedName{Namespace: "default", Name: "virt-launcher-web-abcde"}, old); err != nil {
		t.Fatal(err)
	}
	if err := e.dc.Delete(e.ctx, old); err != nil {
		t.Fatal(err)
	}
	if err := e.dc.Create(e.ctx, newLauncherPod("virt-launcher-web-fghij", "pod-uid-2", "web",
		map[string]string{"rootdisk": "root-pvc", "datadisk": "data-pvc"})); err != nil {
		t.Fatal(err)
	}
	e.tick()

	s := e.status(t, vmGuardSV1)
	if s.VMCopy != nil || !strings.Contains(s.LastError, "restarted or moved") {
		t.Errorf("status = %+v", s)
	}
	if len(e.cop.starts) != 1 {
		t.Errorf("expected no new copy until the policy is due, got %d starts", len(e.cop.starts))
	}
}

func TestVMCopy_FailedCopyRemovesStaging(t *testing.T) {
	e := newVMCopyEnv(t)
	e.tick()
	e.cop.state, e.cop.stateMsg = vmcopy.Failed, "rootdisk: No space left on device"
	e.tick()

	if len(e.cop.finishes) != 1 || !e.cop.finishes[0] {
		t.Fatalf("expected one Finish that removes the staging files, got %v", e.cop.finishes)
	}
	s := e.status(t, vmGuardSV1)
	if s.VMCopy != nil || !strings.Contains(s.LastError, "No space left on device") {
		t.Errorf("status = %+v", s)
	}
	if e.job(t, vmGuardSV1) != nil {
		t.Error("expected no apply Job for a failed copy")
	}
}

func TestVMCopy_StartError(t *testing.T) {
	e := newVMCopyEnv(t)
	e.cop.startErr = errors.New("volume \"rootdisk\" is not a disk of VM default/web")
	e.tick()
	s := e.status(t, vmGuardSV1)
	if s.VMCopy != nil || !strings.Contains(s.LastError, "is not a disk of VM") {
		t.Errorf("status = %+v", s)
	}
}

func TestVMCopy_ApplyJobFails(t *testing.T) {
	e := newVMCopyEnv(t)
	e.tick()
	e.cop.state = vmcopy.Done
	e.tick()
	for _, sv := range []string{vmGuardSV1, vmGuardSV2} {
		job := e.job(t, sv)
		job.Status.Failed = 4
		if err := e.dc.Status().Update(e.ctx, job); err != nil {
			t.Fatal(err)
		}
	}
	e.tick()
	s := e.status(t, vmGuardSV1)
	if s.VMCopy != nil || s.LastSyncTime != nil || !strings.Contains(s.LastError, "apply") {
		t.Errorf("status = %+v", s)
	}
}

func TestVMCopy_ReceiverModeStillSkipped(t *testing.T) {
	e := newVMCopyEnv(t)
	p := e.k.getPolicy(e.policy)
	p.Spec.DestinationEndpoint = "https://receiver.example"
	e.k.addPolicy(p)
	e.tick()
	if len(e.cop.starts) != 0 {
		t.Fatal("expected no VM copy in receiver mode yet")
	}
	if s := e.status(t, vmGuardSV1); !strings.Contains(s.LastError, skippedRunningVMPrefix) {
		t.Errorf("status = %+v", s)
	}
}

func TestReplicationJob_ExcludesStagingFiles(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	k.addSubVolume(newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10))
	k.addPolicy(newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms"))
	rc := newReplController(k, dc, newFakeNFSOperations())
	rc.processOnce(context.Background())

	job := &batchv1.Job{}
	if err := dc.Get(context.Background(), types.NamespacedName{Namespace: replJobNamespace, Name: replJobName("prod-to-dr", vmGuardSV1)}, job); err != nil {
		t.Fatal(err)
	}
	script := strings.Join(job.Spec.Template.Spec.Containers[0].Command, " ")
	if !strings.Contains(script, "--exclude=/"+vmcopy.StagingPrefix+"*") {
		t.Errorf("expected rsync to skip staging files of an earlier VM copy, script:\n%s", script)
	}
}

func int32Ptr(v int32) *int32 { return &v }
