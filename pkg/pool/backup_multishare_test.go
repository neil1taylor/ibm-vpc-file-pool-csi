package pool

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	"k8s.io/apimachinery/pkg/types"
)

// addShare adds a ready share to a pool.
func (e *backupEnv) addShare(pool, id, ip, export string) {
	e.t.Helper()
	p := &v1alpha1.FileSharePool{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: pool}, p); err != nil {
		e.t.Fatal(err)
	}
	p.Status.Shares = append(p.Status.Shares, v1alpha1.PoolShareStatus{ShareID: id, MountTargetIP: ip, ExportPath: export, State: "stable", TotalGB: 100})
	if err := e.c.Update(e.ctx, p); err != nil {
		e.t.Fatal(err)
	}
}

// shareOf returns the share a status entry says holds the backups of a claim.
func shareOf(t *testing.T, st v1alpha1.BackupPolicyStatus, claim string) string {
	t.Helper()
	for _, v := range st.Volumes {
		if v.PVCName == claim {
			return v.ShareID
		}
	}
	t.Fatalf("no status entry for %s in %+v", claim, st.Volumes)
	return ""
}

func TestBackupMultiShare_NewVolumesSpreadOverTheShares(t *testing.T) {
	e := newBackupEnv(t)
	e.addShare("backups", "second", "10.240.9.10", "/export_b")
	e.addClaim("apps", "a", "data-a", yes)
	e.addClaim("apps", "b", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()

	st := e.bp("nightly").Status
	if st.Phase != "Active" {
		t.Fatalf("status = %+v, want Active: a pool with two shares is allowed", st)
	}
	if shareOf(t, st, "a") == shareOf(t, st, "b") {
		t.Errorf("both volumes went to share %q, want one on each share", shareOf(t, st, "a"))
	}
	rps := e.repls()
	if len(rps) != 2 {
		t.Fatalf("replication policies = %d, want 2 (one for each share)", len(rps))
	}
	dest := map[string]string{}
	for _, rp := range rps {
		dest[rp.Spec.DestinationNFSServer] = rp.Spec.SubVolumeSelector.MatchLabels[backupShareLabel]
	}
	if dest["10.240.9.9"] != "backups-share" || dest["10.240.9.10"] != "second" {
		t.Errorf("each replication policy must copy only the volumes of its own share, got %v", dest)
	}
	if got := e.sv("pvc-apps-a").Labels[backupShareLabel]; got != shareOf(t, st, "a") {
		t.Errorf("SubVolume share label = %q, want %q", got, shareOf(t, st, "a"))
	}
}

func TestBackupMultiShare_AVolumeStaysOnItsShare(t *testing.T) {
	e := newBackupEnv(t)
	e.addClaim("apps", "a", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()
	if got := shareOf(t, e.bp("nightly").Status, "a"); got != "backups-share" {
		t.Fatalf("share = %q", got)
	}
	// A share is added and is empty; the volume must not move, its backups are on the first share.
	e.addShare("backups", "second", "10.240.9.10", "/export_b")
	e.addClaim("apps", "b", "data-a", yes)
	e.reconcile()
	st := e.bp("nightly").Status
	if got := shareOf(t, st, "a"); got != "backups-share" {
		t.Errorf("volume a moved to %q", got)
	}
	if got := shareOf(t, st, "b"); got != "second" {
		t.Errorf("new volume b went to %q, want the emptier share", got)
	}
}

func TestBackupMultiShare_AnEntryFromBeforeBelongsToTheFirstShare(t *testing.T) {
	e := newBackupEnv(t)
	e.addShare("backups", "second", "10.240.9.10", "/export_b")
	e.addClaim("apps", "a", "data-a", yes)
	bp := e.policy("nightly", nil)
	// The status of a policy made when a snapshot pool had exactly one share has no share ID.
	bp.Status.Volumes = []v1alpha1.BackupVolumeStatus{{PVCNamespace: "apps", PVCName: "a", SubVolumeName: "pvc-apps-a", SubPath: "/pvcs/pvc-apps-a"}}
	if err := e.c.Status().Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := shareOf(t, e.bp("nightly").Status, "a"); got != "backups-share" {
		t.Errorf("an old entry went to %q, want the first share", got)
	}
}

func TestBackupMultiShare_ANotReadyShareGetsNoNewVolumes(t *testing.T) {
	e := newBackupEnv(t)
	e.addShare("backups", "second", "10.240.9.10", "/export_b")
	p := &v1alpha1.FileSharePool{}
	_ = e.c.Get(e.ctx, types.NamespacedName{Name: "backups"}, p)
	p.Status.Shares[0].State = "creating"
	_ = e.c.Update(e.ctx, p)
	e.addClaim("apps", "a", "data-a", yes)
	e.addClaim("apps", "b", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()
	st := e.bp("nightly").Status
	if shareOf(t, st, "a") != "second" || shareOf(t, st, "b") != "second" {
		t.Errorf("volumes must use the ready share only: %+v", st.Volumes)
	}
}

func TestBackupMultiShare_NoReadyShareIsAnError(t *testing.T) {
	e := newBackupEnv(t)
	p := &v1alpha1.FileSharePool{}
	_ = e.c.Get(e.ctx, types.NamespacedName{Name: "backups"}, p)
	p.Status.Shares[0].State = "creating"
	_ = e.c.Update(e.ctx, p)
	e.addClaim("apps", "a", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()
	st := e.bp("nightly").Status
	if st.Phase != "Error" || !strings.Contains(st.Message, "share") {
		t.Errorf("status = %+v, want Error naming the share", st)
	}
	if len(e.repls()) != 0 {
		t.Errorf("no replication policy may run without a ready share")
	}
}

// newTwoShareUsageEnv has one volume on each of two shares, then reads the usage.
func newTwoShareUsageEnv(t *testing.T, firstPercent, secondPercent int) *usageEnv {
	t.Helper()
	e := &usageEnv{backupEnv: newBackupEnv(t)}
	e.now = time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	e.sd = &fakeShareData{nfs: newFakeNFSOperations(), base: "/x",
		usageByIP: map[string]int{"10.240.9.9": 10, "10.240.9.10": 10}}
	e.bc.SetShareData(e.sd)
	e.bc.SetNowFunc(func() time.Time { return e.now })
	e.addShare("backups", "second", "10.240.9.10", "/export_b")
	e.addClaim("apps", "a", "data-a", yes)
	e.addClaim("apps", "b", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile() // a on the first share, b on the second
	e.sd.usageByIP["10.240.9.9"], e.sd.usageByIP["10.240.9.10"] = firstPercent, secondPercent
	e.now = e.now.Add(snapshotUsageInterval + 1)
	return e
}

func (e *usageEnv) pausedFor(ip string) bool {
	e.t.Helper()
	for _, rp := range e.repls() {
		if rp.Spec.DestinationNFSServer == ip {
			return rp.Annotations[pausedAnnotation] == "true"
		}
	}
	e.t.Fatalf("no replication policy for %s", ip)
	return false
}

func TestBackupMultiShare_OnlyTheFullShareIsPaused(t *testing.T) {
	e := newTwoShareUsageEnv(t, 91, 40)
	e.reconcile()
	if !e.pausedFor("10.240.9.9") {
		t.Errorf("the copy to the full share must be paused")
	}
	if e.pausedFor("10.240.9.10") {
		t.Errorf("the copy to the share with room must go on")
	}
	st := e.bp("nightly").Status
	if st.Phase != "Error" || !strings.Contains(st.Message, "91%") || !strings.Contains(st.Message, "backups-share") {
		t.Errorf("status = %+v, want Error naming the full share and its usage", st)
	}
}

func TestBackupMultiShare_NewVolumesAvoidAFullShare(t *testing.T) {
	e := newTwoShareUsageEnv(t, 40, 91)
	e.addClaim("apps", "c", "data-a", yes)
	e.addClaim("apps", "d", "data-a", yes)
	e.reconcile()
	st := e.bp("nightly").Status
	if got := shareOf(t, st, "c"); got != "backups-share" {
		t.Errorf("c went to %q, want the share with room", got)
	}
	if got := shareOf(t, st, "d"); got != "backups-share" {
		t.Errorf("d went to %q, want the share with room", got)
	}
	if got := shareOf(t, st, "b"); got != "second" {
		t.Errorf("b moved off its share to %q", got)
	}
}

func TestBackupMultiShare_WhenEveryShareIsFullNewVolumesWait(t *testing.T) {
	e := newTwoShareUsageEnv(t, 91, 92)
	e.addClaim("apps", "c", "data-a", yes)
	e.reconcile()
	st := e.bp("nightly").Status
	var c *v1alpha1.BackupVolumeStatus
	for i := range st.Volumes {
		if st.Volumes[i].PVCName == "c" {
			c = &st.Volumes[i]
		}
	}
	if c == nil || c.ShareID != "" || !strings.Contains(c.LastError, "room") {
		t.Errorf("a new volume with no share that has room must wait with a reason, got %+v", c)
	}
	if len(e.repls()) != 2 {
		t.Errorf("replication policies = %d; the volume that waits needs none", len(e.repls()))
	}
}

func TestBackupMultiShare_OneUsageJobForEachShare(t *testing.T) {
	e := newTwoShareUsageEnv(t, 10, 10)
	before := e.sd.usageCalls
	e.reconcile()
	if got := e.sd.usageCalls - before; got != 2 {
		t.Errorf("usage reads = %d, want 2 (one for each share)", got)
	}
}

func TestBackupMultiShare_ReportsTheUsageOfEachShare(t *testing.T) {
	e := newTwoShareUsageEnv(t, 33, 77)
	e.reconcile()
	if got := gaugeValue(metrics.BackupSnapshotPoolUsedPercent.WithLabelValues("backups", "backups-share")); got != 33 {
		t.Errorf("first share metric = %v, want 33", got)
	}
	if got := gaugeValue(metrics.BackupSnapshotPoolUsedPercent.WithLabelValues("backups", "second")); got != 77 {
		t.Errorf("second share metric = %v, want 77", got)
	}
}

func TestBackupMultiShare_RestoreReadsTheShareOfTheVolume(t *testing.T) {
	e := newRestoreEnv(t)
	e.addShare("backups", "second", "10.240.9.10", "/export_b")
	// The volume's backups are on the second share.
	bp := e.bp("nightly")
	bp.Status.Volumes[0].ShareID = "second"
	if err := e.c.Status().Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.request("r1", nil)
	e.reconcile()
	e.bindTarget()
	e.reconcile()
	if len(e.sd.restores) == 0 {
		t.Fatalf("the restore never started; status %+v", e.br("r1").Status)
	}
	if got := e.sd.restores[0].srcIP; got != "10.240.9.10" {
		t.Errorf("restore read from %s, want the second share 10.240.9.10", got)
	}
}

func TestBackupMultiShare_TheFirstShareKeepsTheNameOfBefore(t *testing.T) {
	// A policy made when the pool had one share must find its replication policy, with its change lists, after an upgrade.
	before := backupReplicationName("nightly", "data-a", "s1", "s1")
	if again := backupReplicationName("nightly", "data-a", "s1", "s1"); again != before {
		t.Errorf("the name is not stable: %q then %q", before, again)
	}
	if other := backupReplicationName("nightly", "data-a", "s2", "s1"); other == before {
		t.Errorf("the second share must get its own replication policy, got the same name %q", other)
	}
}

func TestBackupReplicationName_FitsALabelValueForALongPolicyName(t *testing.T) {
	// The name is a label value on the temporary PV of a copy: 63 characters at most.
	long := strings.Repeat("p", 55) // a policy name may have up to 63 characters
	for _, tc := range []struct{ policy, pool string }{
		{"rt-1021-2037-backup-policy", "rt-1021-2037-backup-src"}, // the name that failed stage 18 (66 characters)
		{long, "data-a"},
		{long, strings.Repeat("q", 63)},
	} {
		name := backupReplicationName(tc.policy, tc.pool, "s1", "s1")
		if len(name) > 63 {
			t.Errorf("name %q for %q and %q has %d characters, want at most 63", name, tc.policy, tc.pool, len(name))
		}
		if strings.HasSuffix(strings.TrimSuffix(name, name[len(name)-6:]), "--") {
			t.Errorf("name %q has a double dash before the hash", name)
		}
	}
	// Two long names that start alike stay apart, and so do two shares.
	a := backupReplicationName(long, "data-a", "s1", "s1")
	b := backupReplicationName(long, "data-b", "s1", "s1")
	c := backupReplicationName(long, "data-a", "s2", "s1")
	if a == b || a == c || b == c {
		t.Errorf("long names collide: %q %q %q", a, b, c)
	}
}

func TestBackupReplicationName_ShortNamesStayAsTheyWere(t *testing.T) {
	// A policy made before this fix keeps its replication policy, and with it the change lists of its VM copies.
	sum := sha256.Sum256([]byte("nightly\x00data-a"))
	want := "backup-nightly-data-a-" + hex.EncodeToString(sum[:3])
	if got := backupReplicationName("nightly", "data-a", "s1", "s1"); got != want {
		t.Errorf("name = %q, want the old form %q", got, want)
	}
}
