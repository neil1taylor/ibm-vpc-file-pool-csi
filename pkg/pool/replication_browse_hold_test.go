package pool

import (
	"context"
	"strings"
	"testing"
	"time"
)

// A browse session on a VM disk holds the disk's next backup copy. The
// replication controller makes no new copy of a held volume.

func TestReplicationController_BrowseHoldGetsNoCopy(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	held := newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10)
	held.Annotations = map[string]string{browseHoldAnnotation: "uid-1/" + time.Now().UTC().Format(time.RFC3339)}
	k.addSubVolume(held)
	k.addSubVolume(newTestSubVolume(vmGuardSV2, "prod-pool", "share-1", 10))
	k.addPolicy(newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms"))

	rc := newReplController(k, dc, newFakeNFSOperations())
	rc.processOnce(context.Background())

	if replJobExists(t, dc, "prod-to-dr", vmGuardSV1) {
		t.Errorf("a copy of the volume with an open browse session was started")
	}
	if !replJobExists(t, dc, "prod-to-dr", vmGuardSV2) {
		t.Errorf("the other volume was not copied")
	}
	lastErr, _ := svReplStatus(t, k, "prod-to-dr", vmGuardSV1)
	if !strings.HasPrefix(lastErr, "held: waiting for a browse session") {
		t.Errorf("status of the held volume = %q", lastErr)
	}
	if p := k.getPolicy("prod-to-dr"); p.Status.ConsecutiveFailures != 0 {
		t.Errorf("a hold counted as %d failed cycles", p.Status.ConsecutiveFailures)
	}
}

func TestReplicationController_StaleBrowseHoldIsIgnored(t *testing.T) {
	k := newReplFakeK8sClient()
	dc := newReplFakeDirectClient()
	old := newTestSubVolume(vmGuardSV1, "prod-pool", "share-1", 10)
	// A session lives 2 hours at most; a hold that is much older is left over.
	old.Annotations = map[string]string{browseHoldAnnotation: "uid-1/" + time.Now().Add(-5*time.Hour).UTC().Format(time.RFC3339)}
	k.addSubVolume(old)
	k.addPolicy(newTestReplicationPolicy("prod-to-dr", "prod-pool", "1ms"))

	rc := newReplController(k, dc, newFakeNFSOperations())
	rc.processOnce(context.Background())

	if !replJobExists(t, dc, "prod-to-dr", vmGuardSV1) {
		t.Errorf("a hold older than its limit stopped the backup of the volume")
	}
}

func TestReplicationController_BrowseHoldOfOneDiskHoldsEveryDiskOfItsRunningVM(t *testing.T) {
	e := newVMCopyEnv(t)
	e.k.mu.Lock()
	e.k.subVolumes[vmGuardSV2].Annotations = map[string]string{browseHoldAnnotation: "uid-1/" + e.clock.UTC().Format(time.RFC3339)}
	e.k.mu.Unlock()

	e.tick()

	if len(e.cop.starts) != 0 {
		t.Fatalf("a VM copy started with %d disks while a browse session holds a disk of the VM", len(e.cop.starts[0].volumes))
	}
	for _, sv := range []string{vmGuardSV1, vmGuardSV2} {
		if got := e.status(t, sv).LastError; !strings.HasPrefix(got, "held: waiting for a browse session") {
			t.Errorf("status of %s = %q, want it to say a browse session holds it", sv, got)
		}
	}
}

func TestParseBrowseHolds(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	cases := []struct {
		name  string
		value string
		now   time.Time
		want  bool
	}{
		{"none", "", at, false},
		{"one fresh", "u1/" + at.Format(time.RFC3339), at.Add(time.Hour), true},
		{"one stale", "u1/" + at.Format(time.RFC3339), at.Add(browseHoldLimit + time.Minute), false},
		{"one stale and one fresh", "u1/" + at.Add(-10*time.Hour).Format(time.RFC3339) + ",u2/" + at.Format(time.RFC3339), at, true},
		{"junk", "u1,u2/notatime,/", at, false},
		{"from the future is not trusted forever", "u1/" + at.Add(48*time.Hour).Format(time.RFC3339), at, false},
	}
	for _, tc := range cases {
		if got := browseHeld(tc.value, tc.now); got != tc.want {
			t.Errorf("%s: browseHeld = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestBrowseHoldValue_AddAndRemoveBySession(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)
	v := addBrowseHold("", "u1", at)
	v = addBrowseHold(v, "u2", at.Add(time.Minute))
	v = addBrowseHold(v, "u1", at.Add(time.Hour)) // the same session again changes nothing
	holds := parseBrowseHolds(v)
	if len(holds) != 2 || holds[0].session != "u1" || !holds[0].since.Equal(at) || holds[1].session != "u2" {
		t.Fatalf("holds = %+v", holds)
	}
	v = removeBrowseHold(v, "u1")
	if holds = parseBrowseHolds(v); len(holds) != 1 || holds[0].session != "u2" {
		t.Fatalf("holds = %+v", holds)
	}
	if v = removeBrowseHold(v, "u2"); v != "" {
		t.Fatalf("value = %q, want empty", v)
	}
}
