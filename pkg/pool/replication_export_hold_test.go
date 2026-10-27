package pool

import (
	"context"
	"strings"
	"testing"
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
