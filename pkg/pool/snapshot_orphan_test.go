package pool

import (
	"context"
	"errors"
	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"testing"
	"time"

	"fmt"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmsnap"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	fakeclient "sigs.k8s.io/controller-runtime/pkg/client/fake"
)

const (
	orphanUID   = "0a1b2c3d-1111-2222-3333-444455556666"
	orphanName  = "snapshot-" + orphanUID
	orphanOther = "snapshot-9f9f9f9f-1111-2222-3333-444455556666"
)

var vscTestGVK = schema.GroupVersionKind{Group: "snapshot.storage.k8s.io", Version: "v1", Kind: "VolumeSnapshotContent"}

// orphanEnv is an instant-snapshot environment whose snapshot of the test disk
// failed because the disk is owned by another user, as VolumeSnapshot
// snapshot-<uid> would have made it.
type orphanEnv struct {
	*instantEnv
	clock time.Time
}

func newOrphanEnv(t *testing.T, vscListable bool) *orphanEnv {
	t.Helper()
	e := newInstantEnv(t)
	s := newReplTestScheme()
	s.AddKnownTypeWithName(vscTestGVK, &unstructured.Unstructured{})
	s.AddKnownTypeWithName(vscTestGVK.GroupVersion().WithKind("VolumeSnapshotContentList"), &unstructured.UnstructuredList{})
	for _, gvk := range []schema.GroupVersionKind{vmimGVK, kubevirtGVK, vmGVK, vmiGVK} {
		s.AddKnownTypeWithName(gvk, &unstructured.Unstructured{})
		s.AddKnownTypeWithName(gvk.GroupVersion().WithKind(gvk.Kind+"List"), &unstructured.UnstructuredList{})
	}
	b := fakeclient.NewClientBuilder().WithScheme(s)
	if !vscListable {
		b = b.WithInterceptorFuncs(interceptor.Funcs{List: func(ctx context.Context, c client.WithWatch, l client.ObjectList, opts ...client.ListOption) error {
			if l.GetObjectKind().GroupVersionKind().Kind == "VolumeSnapshotContentList" {
				return errors.New("no matches for kind VolumeSnapshotContent")
			}
			return c.List(ctx, l, opts...)
		}})
	}
	e.dc = b.Build()
	e.mgr.SetVMSnapshots(newFakeVMCopier(), e.dc)
	e.mgr.SetInstantSnapshots(e.snap, e.jobs, e.dc)
	e.setGate(true)
	o := &orphanEnv{instantEnv: e, clock: time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)}
	e.mgr.vmNow = func() time.Time { return o.clock }
	return o
}

func (o *orphanEnv) failSnapshot(name string) {
	o.t.Helper()
	o.startVM(false)
	o.snap.takeErr = fmt.Errorf("%w: disk.img is owned by UID 65534", vmsnap.ErrDiskNotOwned)
	if _, err := o.create(name); err == nil {
		o.t.Fatal("the snapshot should have failed")
	}
	// The record's own clock is the real one; make it old by the manager's clock.
	s := o.k.getSnapshot(name)
	t := metav1.NewTime(o.clock)
	s.Status.CreationTime = &t
	if err := o.k.UpdateSnapshotStatus(o.ctx, s); err != nil {
		o.t.Fatal(err)
	}
}

func (o *orphanEnv) addVSC(uid string) {
	o.t.Helper()
	c := &unstructured.Unstructured{}
	c.SetGroupVersionKind(vscTestGVK)
	c.SetName("snapcontent-" + uid)
	_ = unstructured.SetNestedField(c.Object, "vpc-file-pool.csi.ibm.io", "spec", "driver")
	_ = unstructured.SetNestedField(c.Object, uid, "spec", "volumeSnapshotRef", "uid")
	if err := o.dc.Create(o.ctx, c); err != nil {
		o.t.Fatal(err)
	}
}

func (o *orphanEnv) later(d time.Duration) { o.clock = o.clock.Add(d) }

func (o *orphanEnv) allocated() int64 {
	o.t.Helper()
	p, err := o.k.GetFileSharePool(context.Background(), "test-pool")
	if err != nil {
		o.t.Fatal(err)
	}
	return p.Status.TotalAllocatedGB
}

func TestRemoveOrphanSnapshots_FailedRecordWithNoVolumeSnapshotIsRemoved(t *testing.T) {
	o := newOrphanEnv(t, true)
	before := o.allocated()
	o.failSnapshot(orphanName)
	if o.allocated() == before {
		t.Fatal("test set-up: the failed record should still hold its charge")
	}
	o.later(10 * time.Minute)
	o.mgr.RemoveOrphanSnapshots(o.ctx)
	if o.k.getSnapshot(orphanName) != nil {
		t.Error("a Failed record whose VolumeSnapshotContent is gone must be removed")
	}
	if got := o.allocated(); got != before {
		t.Errorf("allocated = %d GB, want %d: the record's charge must be given back", got, before)
	}
}

func TestRemoveOrphanSnapshots_KeepsWhatAVolumeSnapshotStillOwns(t *testing.T) {
	o := newOrphanEnv(t, true)
	o.failSnapshot(orphanName)
	o.addVSC(orphanUID)
	o.later(10 * time.Minute)
	o.mgr.RemoveOrphanSnapshots(o.ctx)
	if o.k.getSnapshot(orphanName) == nil {
		t.Error("the VolumeSnapshotContent still exists; the sidecar may still ask about the snapshot")
	}
}

func TestRemoveOrphanSnapshots_WaitsForTheGracePeriod(t *testing.T) {
	o := newOrphanEnv(t, true)
	o.failSnapshot(orphanName)
	o.later(30 * time.Second)
	o.mgr.RemoveOrphanSnapshots(o.ctx)
	if o.k.getSnapshot(orphanName) == nil {
		t.Error("a record younger than the grace period must stay: its VolumeSnapshotContent may not be visible yet")
	}
}

func TestRemoveOrphanSnapshots_KeepsReadySnapshots(t *testing.T) {
	o := newOrphanEnv(t, true)
	o.startVM(false)
	o.mustCreate(orphanName)
	s := o.k.getSnapshot(orphanName)
	t0 := metav1.NewTime(o.clock)
	s.Status.CreationTime = &t0
	_ = o.k.UpdateSnapshotStatus(o.ctx, s)
	o.later(time.Hour)
	o.mgr.RemoveOrphanSnapshots(o.ctx)
	if o.k.getSnapshot(orphanName) == nil {
		t.Error("a ready snapshot holds data; only its VolumeSnapshot may delete it")
	}
}

func TestRemoveOrphanSnapshots_KeepsRecordsThatAreNotNamedByTheSidecar(t *testing.T) {
	o := newOrphanEnv(t, true)
	// A group snapshot names its members <group>-pvc-<uuid>.
	name := "group-one-pvc-" + orphanUID
	o.failSnapshot(name)
	o.later(time.Hour)
	o.mgr.RemoveOrphanSnapshots(o.ctx)
	if o.k.getSnapshot(name) == nil {
		t.Error("a record that no VolumeSnapshot names must be left to its owner")
	}
}

func TestRemoveOrphanSnapshots_KeepsEverythingWhenTheContentsCannotBeListed(t *testing.T) {
	o := newOrphanEnv(t, false) // listing the VolumeSnapshotContents fails
	o.failSnapshot(orphanName)
	o.later(time.Hour)
	o.mgr.RemoveOrphanSnapshots(o.ctx)
	if o.k.getSnapshot(orphanName) == nil {
		t.Error("when the contents cannot be listed, nothing may be removed")
	}
}

func TestRemoveOrphanSnapshots_FailedCopyRecordsGoToo(t *testing.T) {
	for _, method := range []string{snapshotMethodFile, snapshotMethodVM} {
		t.Run(method, func(t *testing.T) {
			o := newOrphanEnv(t, true)
			when := metav1.NewTime(o.clock)
			snap := &v1alpha1.Snapshot{
				ObjectMeta: metav1.ObjectMeta{Name: orphanName, Annotations: map[string]string{snapshotMethodAnnotation: method}},
				Spec: v1alpha1.SnapshotSpec{SourceSubVolume: testPVName, PoolName: "test-pool", ShareID: "share-1",
					SnapshotPath: "/pvcs/.snapshots/snap-" + orphanName, SourceSubPath: "/pvcs/" + testPVName, SizeGB: 1},
				Status: v1alpha1.SnapshotStatus{Phase: "Failed", Message: "the copy failed", CreationTime: &when},
			}
			if err := o.k.CreateSnapshot(o.ctx, snap); err != nil {
				t.Fatal(err)
			}
			o.later(10 * time.Minute)
			o.mgr.RemoveOrphanSnapshots(o.ctx)
			if o.k.getSnapshot(orphanName) != nil {
				t.Errorf("a failed %s snapshot record whose VolumeSnapshot is gone must be removed", method)
			}
		})
	}
}
