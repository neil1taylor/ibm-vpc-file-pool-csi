package pool

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
)

// TestReplicationController_HeldVolumeGetsNoCopyWhileItsExportRuns verifies
// that a volume held by a COS export gets no new local backup, that the other
// volumes are copied, and that the hold is not a failure.
func TestReplicationController_HeldVolumeGetsNoCopyWhileItsExportRuns(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	held := newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10)
	held.Annotations = map[string]string{exportHoldAnnotation: "nightly/2026-10-27T16:00:00Z"}
	k.addSubVolume(held)
	k.addSubVolume(newTestSubVolume(vmGuardSV2, "prod-pool", "share-1", 10))
	k.addPolicy(newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms"))

	rc := newReplController(k, dc, newFakeNFSOperations())
	rc.processOnce(context.Background())

	if replJobExists(t, dc, "prod-to-dr", vmGuardSV1) {
		t.Errorf("a copy of the held volume was started")
	}
	if !replJobExists(t, dc, "prod-to-dr", vmGuardSV2) {
		t.Errorf("the other volume was not copied")
	}
	lastErr, _ := svReplStatus(t, k, "prod-to-dr", vmGuardSV1)
	if !strings.HasPrefix(lastErr, "held: export to COS running") {
		t.Errorf("status of the held volume = %q, want it to say the export holds it", lastErr)
	}
	if p := k.getPolicy("prod-to-dr"); p.Status.ConsecutiveFailures != 0 {
		t.Errorf("a hold counted as %d failed cycles", p.Status.ConsecutiveFailures)
	}
}

// TestReplicationController_HeldDiskHoldsEveryDiskOfItsRunningVM verifies that
// when a COS export holds one disk of a running VM, no disk of that VM is
// copied. A copy of the other disk alone would be from another moment.
func TestReplicationController_HeldDiskHoldsEveryDiskOfItsRunningVM(t *testing.T) {
	e := newVMCopyEnv(t)
	e.k.mu.Lock()
	e.k.subVolumes[vmGuardSV2].Annotations = map[string]string{exportHoldAnnotation: "nightly/2026-10-27T16:00:00Z"}
	e.k.mu.Unlock()

	e.tick()

	if len(e.cop.starts) != 0 {
		t.Fatalf("a VM copy started with %d disks while an export holds a disk of the VM", len(e.cop.starts[0].volumes))
	}
	for _, sv := range []string{vmGuardSV1, vmGuardSV2} {
		if got := e.status(t, sv).LastError; !strings.HasPrefix(got, "held: export to COS running") {
			t.Errorf("status of %s = %q, want it to say the export holds it", sv, got)
		}
	}
}

// TestReplicationController_HoldDoesNotDropACopyThatRuns verifies that a VM
// copy in flight keeps its tracking when a hold appears on one of its disks.
// A copy that loses its tracking stays in the VM and blocks every later copy.
func TestReplicationController_HoldDoesNotDropACopyThatRuns(t *testing.T) {
	e := newVMCopyEnv(t)
	e.cop.state = vmcopy.Running
	e.tick()
	if len(e.cop.starts) != 1 {
		t.Fatalf("expected one VM copy to start, got %d", len(e.cop.starts))
	}
	e.k.mu.Lock()
	e.k.subVolumes[vmGuardSV2].Annotations = map[string]string{exportHoldAnnotation: "nightly/2026-10-27T16:00:00Z"}
	e.k.mu.Unlock()

	e.clock = e.clock.Add(2 * time.Hour) // the next cycle is due
	e.tick()

	for _, sv := range []string{vmGuardSV1, vmGuardSV2} {
		if e.status(t, sv).VMCopy == nil {
			t.Errorf("%s lost the tracking of its running VM copy", sv)
		}
	}
}
