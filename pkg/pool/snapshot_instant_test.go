package pool

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmsnap"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var (
	kubevirtGVK = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "KubeVirt"}
	vmiGVK      = schema.GroupVersionKind{Group: "kubevirt.io", Version: "v1", Kind: "VirtualMachineInstance"}
)

// fakeInstant is an in-memory chain per volume of VM "web", like pkg/vmsnap.
type fakeInstant struct {
	mu      sync.Mutex
	chains  map[string][]string
	takes   []fakeTake
	merges  [][2]string
	takeErr error
	moment  time.Time
}

type fakeTake struct {
	vm      vmcopy.VM
	volume  string
	newFile string
	freeze  bool
}

func newFakeInstant() *fakeInstant {
	return &fakeInstant{chains: map[string][]string{}, moment: time.Date(2026, 10, 21, 14, 4, 0, 500, time.UTC)}
}

func (f *fakeInstant) chain(vol string) []string {
	if c, ok := f.chains[vol]; ok {
		return c
	}
	return []string{vmsnap.BaseFile}
}

func files(names []string) []vmsnap.File {
	var out []vmsnap.File
	for _, n := range names {
		out = append(out, vmsnap.File{Name: n, Format: vmsnap.FormatOf(n)})
	}
	return out
}

func (f *fakeInstant) Take(_ context.Context, vm vmcopy.VM, volume, newFile string, freeze bool) (*vmsnap.Result, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.takes = append(f.takes, fakeTake{vm, volume, newFile, freeze})
	if f.takeErr != nil {
		return nil, f.takeErr
	}
	c := f.chain(volume)
	if c[0] == newFile {
		return &vmsnap.Result{Frozen: c[1], FileSystemConsistent: true, Chain: files(c)}, nil
	}
	f.chains[volume] = append([]string{newFile}, c...)
	return &vmsnap.Result{Moment: f.moment, Frozen: c[0], FileSystemConsistent: true, Chain: files(f.chains[volume])}, nil
}

func (f *fakeInstant) Merge(_ context.Context, _ vmcopy.VM, volume, file, child string) ([]vmsnap.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.merges = append(f.merges, [2]string{file, child})
	c := f.chain(volume)
	var out []string
	for _, n := range c {
		if n != child {
			out = append(out, n)
		}
	}
	f.chains[volume] = out
	return files(out), nil
}

func (f *fakeInstant) Chain(_ context.Context, _ vmcopy.VM, volume string) ([]vmsnap.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return files(f.chain(volume)), nil
}

// fakeChainJobs records chain Job scripts. A Job is done when done is set.
type fakeChainJobs struct {
	mu      sync.Mutex
	done    bool
	scripts map[string]string
	inits   map[string]string
	folders map[string][]ShareFolder
}

func newFakeChainJobs() *fakeChainJobs {
	return &fakeChainJobs{scripts: map[string]string{}, inits: map[string]string{}, folders: map[string][]ShareFolder{}}
}

func (f *fakeChainJobs) RunChainScript(_ context.Context, key string, folders []ShareFolder, script, initScript string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts[key], f.inits[key], f.folders[key] = script, initScript, folders
	return f.done, nil
}

type instantEnv struct {
	t    *testing.T
	ctx  context.Context
	k    *fakeK8sClient
	mgr  *Manager
	sd   *fakeShareData
	dc   client.Client
	snap *fakeInstant
	jobs *fakeChainJobs
}

func newInstantDirectClient() client.Client {
	s := newReplTestScheme()
	for _, gvk := range []schema.GroupVersionKind{vmimGVK, kubevirtGVK, vmGVK, vmiGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	return fakeclient.NewClientBuilder().WithScheme(s).WithStatusSubresource(&batchv1.Job{}).Build()
}

func newInstantEnv(t *testing.T) *instantEnv {
	t.Helper()
	k, _, mgr := newShareDataTestManager(t)
	e := &instantEnv{t: t, ctx: context.Background(), k: k, mgr: mgr, sd: shareDataOf(mgr),
		dc: newInstantDirectClient(), snap: newFakeInstant(), jobs: newFakeChainJobs()}
	mgr.SetVMSnapshots(newFakeVMCopier(), e.dc)
	mgr.SetInstantSnapshots(e.snap, e.jobs, e.dc)
	e.setGate(true)
	return e
}

func (e *instantEnv) setGate(on bool) {
	e.t.Helper()
	kv := &unstructured.Unstructured{}
	kv.SetGroupVersionKind(kubevirtGVK)
	kv.SetNamespace("openshift-cnv")
	kv.SetName("kubevirt-kubevirt-hyperconverged")
	gates := []any{"Snapshot", "HotplugVolumes"}
	if on {
		gates = append(gates, "Sidecar")
	}
	if err := unstructured.SetNestedSlice(kv.Object, gates, "spec", "configuration", "developerConfiguration", "featureGates"); err != nil {
		e.t.Fatal(err)
	}
	_ = e.dc.Delete(e.ctx, kv)
	if err := e.dc.Create(e.ctx, kv); err != nil {
		e.t.Fatal(err)
	}
}

// startVM runs VM "web" with the test claim my-pvc as volume rootdisk.
func (e *instantEnv) startVM(frozen bool) {
	e.t.Helper()
	if err := e.dc.Create(e.ctx, newLauncherPod("virt-launcher-web-abcde", "pod-uid-1", "web", map[string]string{"rootdisk": "my-pvc"})); err != nil {
		e.t.Fatal(err)
	}
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(vmiGVK)
	vmi.SetNamespace("default")
	vmi.SetName("web")
	if frozen {
		_ = unstructured.SetNestedField(vmi.Object, "frozen", "status", "fsFreezeStatus")
	}
	if err := e.dc.Create(e.ctx, vmi); err != nil {
		e.t.Fatal(err)
	}
}

func (e *instantEnv) stopVM() {
	e.t.Helper()
	if err := e.dc.Delete(e.ctx, newLauncherPod("virt-launcher-web-abcde", "pod-uid-1", "web", nil)); err != nil {
		e.t.Fatal(err)
	}
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(vmiGVK)
	vmi.SetNamespace("default")
	vmi.SetName("web")
	_ = e.dc.Delete(e.ctx, vmi)
}

// defineVM makes VirtualMachine "web" whose volume rootdisk is the test claim.
func (e *instantEnv) defineVM() {
	e.t.Helper()
	vm := &unstructured.Unstructured{}
	vm.SetGroupVersionKind(vmGVK)
	vm.SetNamespace("default")
	vm.SetName("web")
	vols := []any{map[string]any{"name": "rootdisk", "persistentVolumeClaim": map[string]any{"claimName": "my-pvc"}}}
	if err := unstructured.SetNestedSlice(vm.Object, vols, "spec", "template", "spec", "volumes"); err != nil {
		e.t.Fatal(err)
	}
	if err := e.dc.Create(e.ctx, vm); err != nil {
		e.t.Fatal(err)
	}
}

func (e *instantEnv) create(name string) (*SnapshotResult, error) {
	return e.mgr.CreateSnapshot(e.ctx, name, "test-pool/share-1/"+testPVName, nil)
}

func (e *instantEnv) mustCreate(name string) *SnapshotResult {
	e.t.Helper()
	res, err := e.create(name)
	if err != nil {
		e.t.Fatalf("CreateSnapshot %s: %v", name, err)
	}
	return res
}

func (e *instantEnv) sv() *v1alpha1.SubVolume {
	e.t.Helper()
	sv := e.k.getSubVolume(testPVName)
	if sv == nil {
		e.t.Fatal("no SubVolume")
	}
	return sv
}

func (e *instantEnv) diskTops() map[string]string {
	e.t.Helper()
	vmi := &unstructured.Unstructured{}
	vmi.SetGroupVersionKind(vmiGVK)
	if err := e.dc.Get(e.ctx, types.NamespacedName{Namespace: "default", Name: "web"}, vmi); err != nil {
		e.t.Fatal(err)
	}
	tops := map[string]string{}
	if v := vmi.GetAnnotations()[DiskTopsAnnotation]; v != "" {
		if err := json.Unmarshal([]byte(v), &tops); err != nil {
			e.t.Fatal(err)
		}
	}
	return tops
}

func TestInstantSnapshot_RunningVMIsReadyAtOnce(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)

	res := e.mustCreate("snap-one")
	if !res.ReadyToUse {
		t.Fatal("an instant snapshot must be ready when CreateSnapshot returns")
	}
	if !res.CreationTime.Equal(e.snap.moment) {
		t.Errorf("CreationTime = %v, want the moment of the snapshot %v", res.CreationTime, e.snap.moment)
	}
	if len(e.snap.takes) != 1 || !e.snap.takes[0].freeze || e.snap.takes[0].volume != "rootdisk" ||
		e.snap.takes[0].vm != (vmcopy.VM{Namespace: "default", Pod: "virt-launcher-web-abcde", Name: "web"}) {
		t.Fatalf("takes = %+v", e.snap.takes)
	}
	newFile := e.snap.takes[0].newFile
	if newFile != instantFileName("snap-one") || !vmsnap.FileNameRe.MatchString(newFile) {
		t.Errorf("new file %q", newFile)
	}
	s := e.k.getSnapshot("snap-one")
	if s.Annotations[snapshotMethodAnnotation] != snapshotMethodInstant || s.Status.ChainFile != vmsnap.BaseFile ||
		s.Status.Phase != "Ready" || s.Status.Consistency != consistencyFileSystem || s.Status.CopyMoment == nil {
		t.Errorf("Snapshot = %+v %+v", s.Annotations, s.Status)
	}
	if got := fmt.Sprint(e.sv().Status.DiskChain); got != fmt.Sprint([]string{newFile, vmsnap.BaseFile}) {
		t.Errorf("DiskChain = %s", got)
	}
	if e.diskTops()["rootdisk"] != newFile {
		t.Errorf("the VMI must name the new top file for the next start, annotations %v", e.diskTops())
	}
	cm := &corev1.ConfigMap{}
	if err := e.dc.Get(e.ctx, types.NamespacedName{Namespace: "default", Name: VMHookConfigMap}, cm); err != nil {
		t.Fatalf("the hook ConfigMap must exist in the VM's namespace: %v", err)
	}
	if !strings.Contains(cm.Data[VMHookScriptKey], DiskTopsAnnotation) {
		t.Error("the hook script must read the disk-tops annotation")
	}
	if len(e.sd.copies) != 0 || len(e.sd.applies) != 0 {
		t.Error("an instant snapshot copies nothing")
	}
	// Space: a tenth of the disk, at least 1 GB.
	share := e.k.getPool("test-pool").Status.Shares[0]
	if share.AllocatedGB != 101 {
		t.Errorf("AllocatedGB = %d, want 100 + 1", share.AllocatedGB)
	}
}

func TestInstantSnapshot_InsideOpenShiftsFreezeDoesNotFreezeAgain(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(true)
	e.mustCreate("snap-one")
	if e.snap.takes[0].freeze {
		t.Error("the guest is frozen by OpenShift; the snapshot must not freeze again")
	}
}

func TestInstantSnapshot_RepeatIsSafe(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	first := e.mustCreate("snap-one")
	again := e.mustCreate("snap-one")
	if len(e.snap.takes) != 1 {
		t.Errorf("a repeat of a ready snapshot must not take again, takes %d", len(e.snap.takes))
	}
	if !again.ReadyToUse || !again.CreationTime.Equal(first.CreationTime) {
		t.Errorf("repeat = %+v", again)
	}
}

func TestInstantSnapshot_FailedTakeIsRetried(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.snap.takeErr = errors.New("virsh: command terminated with exit code 1")
	if _, err := e.create("snap-one"); err == nil {
		t.Fatal("expected an error")
	}
	if s := e.k.getSnapshot("snap-one"); s != nil && s.Status.ReadyToUse {
		t.Fatal("a failed snapshot must not be ready")
	}
	e.snap.takeErr = nil
	if res := e.mustCreate("snap-one"); !res.ReadyToUse {
		t.Error("the next call must take the snapshot")
	}
}

func TestInstantSnapshot_GateOffFallsBackToTheCopy(t *testing.T) {
	e := newInstantEnv(t)
	e.setGate(false)
	e.startVM(false)
	res := e.mustCreate("snap-one")
	if res.ReadyToUse || len(e.snap.takes) != 0 {
		t.Fatal("without the Sidecar gate the snapshot is a QEMU copy")
	}
	s := e.k.getSnapshot("snap-one")
	if s.Annotations[snapshotMethodAnnotation] != snapshotMethodVM || !strings.Contains(s.Status.MethodReason, "Sidecar") {
		t.Errorf("method %q, reason %q", s.Annotations[snapshotMethodAnnotation], s.Status.MethodReason)
	}
}

func TestInstantSnapshot_GateOffRefusesADiskWithAChain(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.mustCreate("snap-one")
	e.setGate(false)
	_, err := e.create("snap-two")
	if !errors.Is(err, ErrSnapshotUnsupported) {
		t.Fatalf("err = %v, want ErrSnapshotUnsupported: a copy of a chained disk would not be a snapshot", err)
	}
}

func TestInstantSnapshot_PlainVolumeIsAFileCopy(t *testing.T) {
	e := newInstantEnv(t)
	res := e.mustCreate("snap-one")
	if len(e.snap.takes) != 0 || len(e.sd.copies) != 1 || !res.ReadyToUse {
		t.Errorf("a volume no VM uses is copied as files, takes %d copies %d", len(e.snap.takes), len(e.sd.copies))
	}
}

func TestInstantSnapshot_DeleteNewestMergesTheLiveFile(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.mustCreate("snap-one")
	top := instantFileName("snap-one")
	if err := e.mgr.DeleteSnapshot(e.ctx, "snap-one"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if fmt.Sprint(e.snap.merges) != fmt.Sprint([][2]string{{vmsnap.BaseFile, top}}) {
		t.Errorf("merges = %v", e.snap.merges)
	}
	if e.k.getSnapshot("snap-one") != nil {
		t.Error("the Snapshot must be gone")
	}
	if c := e.sv().Status.DiskChain; len(c) != 0 {
		t.Errorf("a disk with no snapshots has no chain, got %v", c)
	}
	if top, ok := e.diskTops()["rootdisk"]; ok {
		t.Errorf("the VMI must not name a top file for a plain disk, got %q", top)
	}
	if e.k.getPool("test-pool").Status.Shares[0].AllocatedGB != 100 {
		t.Error("the snapshot's space must be given back")
	}
}

func TestInstantSnapshot_DeleteOlderHandsItsFileToTheNewer(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.mustCreate("snap-one")
	e.mustCreate("snap-two")
	one, two := instantFileName("snap-one"), instantFileName("snap-two")
	if got := e.k.getSnapshot("snap-two").Status.ChainFile; got != one {
		t.Fatalf("snap-two is held in %s, want %s", got, one)
	}
	if err := e.mgr.DeleteSnapshot(e.ctx, "snap-one"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if fmt.Sprint(e.snap.merges) != fmt.Sprint([][2]string{{vmsnap.BaseFile, one}}) {
		t.Errorf("merges = %v", e.snap.merges)
	}
	if got := e.k.getSnapshot("snap-two").Status.ChainFile; got != vmsnap.BaseFile {
		t.Errorf("snap-two must now be held in disk.img, got %s", got)
	}
	if got := fmt.Sprint(e.sv().Status.DiskChain); got != fmt.Sprint([]string{two, vmsnap.BaseFile}) {
		t.Errorf("DiskChain = %s", got)
	}
}

func TestInstantSnapshot_DeleteWaitsForAMigration(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.mustCreate("snap-one")
	mig := &unstructured.Unstructured{}
	mig.SetGroupVersionKind(vmimGVK)
	mig.SetNamespace("default")
	mig.SetName("web-migration")
	_ = unstructured.SetNestedField(mig.Object, "web", "spec", "vmiName")
	_ = unstructured.SetNestedField(mig.Object, "Running", "status", "phase")
	if err := e.dc.Create(e.ctx, mig); err != nil {
		t.Fatal(err)
	}
	if err := e.mgr.DeleteSnapshot(e.ctx, "snap-one"); !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("err = %v, want ErrOperationInProgress while the VM moves", err)
	}
	if len(e.snap.merges) != 0 {
		t.Error("no merge while the VM moves")
	}
}

func TestInstantSnapshot_StoppedVMUsesAJob(t *testing.T) {
	e := newInstantEnv(t)
	e.defineVM()
	res := e.mustCreate("snap-one")
	if res.ReadyToUse {
		t.Fatal("the Job has not run yet")
	}
	newFile := instantFileName("snap-one")
	key := "chain-snap-one"
	if !strings.Contains(e.jobs.scripts[key], "-b disk.img") || !strings.Contains(e.jobs.scripts[key], newFile) {
		t.Fatalf("Job script:\n%s", e.jobs.scripts[key])
	}
	if want := []ShareFolder{{Share: shareOne, Path: "/pvcs/" + testPVName}}; fmt.Sprint(e.jobs.folders[key]) != fmt.Sprint(want) {
		t.Errorf("folders = %v, want only the volume's folder", e.jobs.folders[key])
	}
	if e.sv().Status.ChainOperation != key {
		t.Errorf("ChainOperation = %q: the VM must not start while the Job runs", e.sv().Status.ChainOperation)
	}
	e.jobs.done = true
	res = e.mustCreate("snap-one")
	if !res.ReadyToUse {
		t.Fatal("ready once the Job is done")
	}
	sv := e.sv()
	if sv.Status.ChainOperation != "" || fmt.Sprint(sv.Status.DiskChain) != fmt.Sprint([]string{newFile, vmsnap.BaseFile}) {
		t.Errorf("SubVolume status = %+v", sv.Status)
	}
	if s := e.k.getSnapshot("snap-one"); s.Status.ChainFile != vmsnap.BaseFile || s.Status.Consistency != consistencyFileSystem {
		t.Errorf("a snapshot of a stopped VM is held in disk.img and consistent, got %+v", s.Status)
	}
}

func TestInstantSnapshot_DeleteOnAStoppedVMUsesAJob(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.mustCreate("snap-one")
	e.stopVM()
	e.defineVM()
	if err := e.mgr.DeleteSnapshot(e.ctx, "snap-one"); !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("err = %v, want ErrOperationInProgress while the Job runs", err)
	}
	script := e.jobs.scripts["chain-rm-snap-one"]
	if !strings.Contains(script, "commit") || !strings.Contains(script, instantFileName("snap-one")) {
		t.Fatalf("merge Job script:\n%s", script)
	}
	e.jobs.done = true
	if err := e.mgr.DeleteSnapshot(e.ctx, "snap-one"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if sv := e.sv(); len(sv.Status.DiskChain) != 0 || sv.Status.ChainOperation != "" {
		t.Errorf("SubVolume status = %+v", sv.Status)
	}
	if e.k.getSnapshot("snap-one") != nil {
		t.Error("the Snapshot must be gone")
	}
}

func TestInstantSnapshot_RestoreConvertsTheSnapshotsFile(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.mustCreate("snap-one")
	e.jobs.done = true
	_, err := e.mgr.RestoreSnapshot(e.ctx, "snap-one", AllocationRequest{
		PVName: testRestorePVName, PVCName: "restored", PVCNamespace: "default", PoolName: "test-pool", RequestedGB: 10,
	})
	if err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	key := "restore-" + testRestorePVName
	script := e.jobs.scripts[key]
	if !strings.Contains(script, `convert -f raw -O raw "/vol0/disk.img" "/vol1/disk.img.part"`) {
		t.Errorf("restore script:\n%s", script)
	}
	// The Job sees only the two volume folders, never the whole share.
	want := []ShareFolder{{Share: shareOne, Path: "/pvcs/" + testPVName}, {Share: shareOne, Path: "/pvcs/" + testRestorePVName}}
	if fmt.Sprint(e.jobs.folders[key]) != fmt.Sprint(want) {
		t.Errorf("folders = %v, want %v", e.jobs.folders[key], want)
	}
	if len(e.sd.copies) != 0 {
		t.Error("an instant snapshot is restored by converting its file, not by copying the folder")
	}
	if e.k.getSubVolume(testRestorePVName).Status.Phase != "Bound" {
		t.Error("the restored volume is bound once the Job is done")
	}
}

func TestInstantSnapshot_RestoreRefusesASmallerVolume(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.mustCreate("snap-one")
	_, err := e.mgr.RestoreSnapshot(e.ctx, "snap-one", AllocationRequest{
		PVName: testRestorePVName, PVCName: "restored", PVCNamespace: "default", PoolName: "test-pool", RequestedGB: 5,
	})
	if err == nil {
		t.Fatal("a 5 GB volume cannot hold a 10 GB disk")
	}
}

func TestInstantSnapshot_DeleteWaitsForARestore(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.mustCreate("snap-one")
	if _, err := e.mgr.RestoreSnapshot(e.ctx, "snap-one", AllocationRequest{
		PVName: testRestorePVName, PVCName: "restored", PVCNamespace: "default", PoolName: "test-pool", RequestedGB: 10,
	}); !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("restore: %v", err)
	}
	if err := e.mgr.DeleteSnapshot(e.ctx, "snap-one"); !errors.Is(err, ErrOperationInProgress) {
		t.Fatalf("err = %v: a merge must not change files a restore reads", err)
	}
	if len(e.snap.merges) != 0 {
		t.Error("no merge while a restore reads the chain")
	}
}

func TestInstantSnapshot_DeletedVolumeKeepsItsFolderForItsSnapshots(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.mustCreate("snap-one")
	e.stopVM()
	if err := e.mgr.Deallocate(e.ctx, testPVName); err != nil {
		t.Fatalf("Deallocate: %v", err)
	}
	if e.sd.removeCount() != 0 {
		t.Fatal("the folder holds the snapshot's files and must stay")
	}
	if e.k.getSubVolume(testPVName) != nil {
		t.Error("the SubVolume goes with the claim")
	}
	// Restore still works from the kept files.
	e.jobs.done = true
	if _, err := e.mgr.RestoreSnapshot(e.ctx, "snap-one", AllocationRequest{
		PVName: testRestorePVName, PVCName: "restored", PVCNamespace: "default", PoolName: "test-pool", RequestedGB: 10,
	}); err != nil {
		t.Fatalf("RestoreSnapshot: %v", err)
	}
	// The last snapshot takes the folder with it.
	if err := e.mgr.DeleteSnapshot(e.ctx, "snap-one"); err != nil {
		t.Fatalf("DeleteSnapshot: %v", err)
	}
	if got := e.sd.lastRemove(t).dstPath; got != "/pvcs/"+testPVName {
		t.Errorf("removed %s, want the kept volume folder", got)
	}
}

func TestInstantSnapshot_CloneRefusesAChainedDisk(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.mustCreate("snap-one")
	_, err := e.mgr.CloneVolume(e.ctx, "test-pool/share-1/"+testPVName, AllocationRequest{
		PVName: testRestorePVName, PVCName: "clone", PVCNamespace: "default", PoolName: "test-pool", RequestedGB: 10,
	})
	if !errors.Is(err, ErrSnapshotUnsupported) {
		t.Fatalf("err = %v: a folder copy of a chained disk is not a clone", err)
	}
}

func TestInstantSnapshot_DeleteKeepsTheFolderWhenTheVolumeCannotBeRead(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.mustCreate("snap-one")
	e.k.GetSubVolumeErr = errors.New("etcdserver: request timed out")
	if err := e.mgr.DeleteSnapshot(e.ctx, "snap-one"); err == nil {
		t.Fatal("expected an error to retry: the volume may still exist")
	}
	e.k.GetSubVolumeErr = nil
	if e.sd.removeCount() != 0 {
		t.Fatal("a failed read must never remove the folder of a disk that may be in use")
	}
	if e.k.getSnapshot("snap-one") == nil {
		t.Error("the Snapshot must stay until the delete can finish")
	}
}
