package pool

import (
	"context"
	"sync"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
)

// A snapshot call can wait a long time on the VM pod or the Kubernetes API.
// Other volumes must not wait behind it, but a second change to the same
// disk must (issue #99).

// holdNextCall makes the next Take or Merge wait until release is called, and
// returns once that call has started.
func (e *instantEnv) holdNextCall(start func()) (release func()) {
	e.t.Helper()
	e.snap.mu.Lock()
	e.snap.gate, e.snap.entered = make(chan struct{}), make(chan string, 4)
	gate, entered := e.snap.gate, e.snap.entered
	e.snap.mu.Unlock()
	go start()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		e.t.Fatal("the held call never started")
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			e.snap.mu.Lock()
			e.snap.gate = nil
			e.snap.mu.Unlock()
			close(gate)
		})
	}
}

func (e *instantEnv) allocateOther(done chan<- error) {
	_, err := e.mgr.Allocate(context.Background(), AllocationRequest{
		PVName: testRestorePVName, PVCName: "other", PVCNamespace: "default", PoolName: "test-pool", RequestedGB: 10,
	})
	done <- err
}

func waitDone(t *testing.T, done <-chan error, what string) {
	t.Helper()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("%s: %v", what, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatalf("%s waited behind a snapshot of another volume", what)
	}
}

func TestSnapshotLock_ASlowSnapshotDoesNotHoldUpOtherVolumes(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	created := make(chan error, 1)
	release := e.holdNextCall(func() { _, err := e.create("snap-one"); created <- err })
	defer release()

	done := make(chan error, 1)
	go e.allocateOther(done)
	waitDone(t, done, "a new volume")

	release()
	waitDone(t, created, "the snapshot")
}

func TestSnapshotLock_ASlowDeleteDoesNotHoldUpOtherVolumes(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	e.mustCreate("snap-one")
	deleted := make(chan error, 1)
	release := e.holdNextCall(func() { deleted <- e.mgr.DeleteSnapshot(context.Background(), "snap-one") })
	defer release()

	done := make(chan error, 1)
	go e.allocateOther(done)
	waitDone(t, done, "a new volume")

	release()
	waitDone(t, deleted, "the delete")
}

func TestSnapshotLock_TheSameDiskWaits(t *testing.T) {
	e := newInstantEnv(t)
	e.startVM(false)
	first := make(chan error, 1)
	release := e.holdNextCall(func() { _, err := e.create("snap-one"); first <- err })
	defer release()

	second := make(chan error, 1)
	go func() { _, err := e.create("snap-two"); second <- err }()
	select {
	case call := <-e.snap.entered:
		t.Fatalf("%s ran while the first snapshot of the same disk was still being taken", call)
	case err := <-second:
		t.Fatalf("the second snapshot returned (%v) while the first of the same disk was still being taken", err)
	case <-time.After(300 * time.Millisecond):
	}

	release()
	waitDone(t, first, "the first snapshot")
	waitDone(t, second, "the second snapshot")
	if len(e.snap.takes) != 2 {
		t.Errorf("takes = %d, want 2", len(e.snap.takes))
	}
}

// OpenShift snapshots every disk of a VM at once, so two calls can record a
// disk's top file on the same VMI at the same time. Neither may lose the
// other's entry: the hook opens the wrong file for a disk without one.
func TestSetDiskTop_KeepsAConcurrentEntry(t *testing.T) {
	base := newInstantDirectClient()
	injected := false
	c := interceptor.NewClient(base.(client.WithWatch), interceptor.Funcs{
		Get: func(ctx context.Context, cl client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
			if err := cl.Get(ctx, key, obj, opts...); err != nil {
				return err
			}
			// Right after this call reads the VMI, another call records
			// the data disk's top file.
			if u, ok := obj.(*unstructured.Unstructured); ok && u.GetKind() == "VirtualMachineInstance" && !injected {
				injected = true
				other := u.DeepCopy()
				other.SetAnnotations(map[string]string{DiskTopsAnnotation: `{"datadisk":"snap-bbbb.qcow2"}`})
				if err := cl.Update(ctx, other); err != nil {
					t.Fatalf("inject the other entry: %v", err)
				}
			}
			return nil
		},
	})
	e := newInstantEnv(t)
	e.dc = c
	e.mgr.SetInstantSnapshots(e.snap, e.jobs, c)
	e.createVMI(false)

	e.mgr.setDiskTop(context.Background(), "default", "web", "rootdisk", "snap-aaaa.qcow2")

	if tops := e.diskTops(); tops["rootdisk"] != "snap-aaaa.qcow2" || tops["datadisk"] != "snap-bbbb.qcow2" {
		t.Errorf("disk tops = %v, want both disks' entries", tops)
	}
}
