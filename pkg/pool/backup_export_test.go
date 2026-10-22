package pool

import (
	"errors"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// cosEnv is a backup environment with two volumes whose policy exports to COS.
type cosEnv struct {
	*backupEnv
	sd   *fakeShareData
	now  time.Time
	a, b *v1alpha1.SubVolume
}

func testKeyProtect() v1alpha1.KeyProtectRef {
	return v1alpha1.KeyProtectRef{
		InstanceID: "11111111-2222-3333-4444-555555555555", Region: "eu-de",
		RootKeyID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", APIKeySecret: "kp-apikey",
	}
}

func testBackupExport() *v1alpha1.BackupExport {
	return &v1alpha1.BackupExport{
		Bucket: "pool-backups", Endpoint: "s3.eu-de.cloud-object-storage.appdomain.cloud", Prefix: "lab",
		CredentialsSecret: "cos-credentials", BackupKeySecret: "cos-backup-key",
		KeyProtect: testKeyProtect(),
		KeepFor:    "30d", Lock: v1alpha1.ExportLockCompliance,
	}
}

// newCOSEnv makes the policy "nightly" with an export block and one pass, so
// the ReplicationPolicy exists. No volume has a backup yet.
func newCOSEnv(t *testing.T, mutate func(*v1alpha1.BackupPolicy)) *cosEnv {
	t.Helper()
	e := &cosEnv{backupEnv: newBackupEnv(t), now: time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)}
	e.sd = &fakeShareData{nfs: newFakeNFSOperations(), base: "/x", export: fakeExport{bytes: 4096}}
	e.bc.SetShareData(e.sd)
	e.bc.SetNowFunc(func() time.Time { return e.now })
	e.a = e.addClaim("apps", "a", "data-a", yes)
	e.b = e.addClaim("apps", "b", "data-a", yes)
	e.policy("nightly", func(p *v1alpha1.BackupPolicy) {
		p.Spec.Export = testBackupExport()
		if mutate != nil {
			mutate(p)
		}
	})
	e.reconcile()
	return e
}

// backedUp sets the moment of the newest local backup of each volume, as the
// replication controller does. A volume that is not in the map has none.
func (e *cosEnv) backedUp(moments map[string]time.Time) {
	e.t.Helper()
	rp := e.repls()[0]
	rp.Status.SubVolumeStatuses = nil
	for name, at := range moments {
		m := metav1.NewTime(at)
		rp.Status.SubVolumeStatuses = append(rp.Status.SubVolumeStatuses, v1alpha1.SubVolumeReplicationStatus{SubVolumeName: name, LastSyncTime: &m})
	}
	if err := e.c.Status().Update(e.ctx, &rp); err != nil {
		e.t.Fatal(err)
	}
}

func (e *cosEnv) volume(name string) v1alpha1.BackupVolumeStatus {
	e.t.Helper()
	for _, v := range e.bp("nightly").Status.Volumes {
		if v.SubVolumeName == name {
			return v
		}
	}
	e.t.Fatalf("no status entry for %s", name)
	return v1alpha1.BackupVolumeStatus{}
}

func (e *cosEnv) advance(d time.Duration) { e.now = e.now.Add(d) }

func exportKey(sv *v1alpha1.SubVolume) string { return "export-nightly-" + sv.Name }

func TestBackupExport_NoExportBlockStartsNoJobs(t *testing.T) {
	e := newCOSEnv(t, func(p *v1alpha1.BackupPolicy) { p.Spec.Export = nil })
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.reconcile()
	if n := len(e.sd.exportCalls()) + len(e.sd.maintainCalls()); n != 0 {
		t.Errorf("a policy without spec.export started %d export Jobs", n)
	}
	if v := e.volume(e.a.Name); v.ExportedTime != nil || v.ExportError != "" {
		t.Errorf("export status = %v %q, want none", v.ExportedTime, v.ExportError)
	}
}

func TestBackupExport_OnlyVolumesWithABackupMomentAreExported(t *testing.T) {
	e := newCOSEnv(t, nil)
	moment := e.now.Add(-time.Hour)
	e.backedUp(map[string]time.Time{e.a.Name: moment}) // volume b has no backup yet
	e.reconcile()
	calls := e.sd.exportCalls()
	if len(calls) != 1 || calls[0].key != exportKey(e.a) {
		t.Fatalf("export calls = %+v, want one for volume a", calls)
	}
	if v := e.volume(e.b.Name); v.ExportedTime != nil || v.ExportError != "" {
		t.Errorf("volume b without a moment must have no export status, got %v %q", v.ExportedTime, v.ExportError)
	}
}

func TestBackupExport_StartsAJobForAFinishedBackupAndRecordsTheResult(t *testing.T) {
	e := newCOSEnv(t, nil)
	moment := e.now.Add(-time.Hour).Truncate(time.Second)
	e.backedUp(map[string]time.Time{e.a.Name: moment})
	e.reconcile()

	calls := e.sd.exportCalls()
	if len(calls) != 1 {
		t.Fatalf("export calls = %d, want 1", len(calls))
	}
	c := calls[0]
	if c.backupPath != "/backup/nightly/pvcs/pvc-apps-a" || c.momentsPath != "/backup/nightly/.moments/pvcs/pvc-apps-a" {
		t.Errorf("paths = %q, %q", c.backupPath, c.momentsPath)
	}
	if !c.moment.Equal(moment) {
		t.Errorf("moment = %s, want %s", c.moment, moment)
	}
	if c.srcIP != "10.240.9.9" {
		t.Errorf("the Job must mount the snapshot pool's share, got %q", c.srcIP)
	}
	want := ExportRepo{
		Endpoint: "s3.eu-de.cloud-object-storage.appdomain.cloud", Bucket: "pool-backups", Prefix: "lab/nightly/",
		CredentialsSecret: "cos-credentials", BackupKeySecret: "cos-backup-key", KeyProtect: testKeyProtect(),
		LockMode: ExportLockCompliance, KeepFor: 30 * 24 * time.Hour, MaintenanceInterval: 24 * time.Hour,
	}
	if c.repo != want {
		t.Errorf("repo = %+v, want %+v", c.repo, want)
	}
	v := e.volume(e.a.Name)
	if v.ExportedTime == nil || !v.ExportedTime.Time.Equal(moment) || v.ExportedBytes != 4096 || v.ExportError != "" {
		t.Errorf("status = %v %d %q, want the moment, 4096 bytes and no error", v.ExportedTime, v.ExportedBytes, v.ExportError)
	}
}

func TestBackupExport_PrefixIsNormalisedAndHoldsThePolicyArea(t *testing.T) {
	for in, want := range map[string]string{"": "nightly/", "lab": "lab/nightly/", "/lab/": "lab/nightly/", "a/b/": "a/b/nightly/"} {
		e := newCOSEnv(t, func(p *v1alpha1.BackupPolicy) { p.Spec.Export.Prefix = in })
		e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
		e.reconcile()
		calls := e.sd.exportCalls()
		if len(calls) != 1 || calls[0].repo.Prefix != want {
			t.Errorf("prefix %q gave %+v, want %q", in, calls, want)
		}
	}
}

func TestBackupExport_TheControllerNeverReadsASecret(t *testing.T) {
	// The environment holds no Secret at all, and the export still works: the
	// controller passes only the names.
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.reconcile()
	if v := e.volume(e.a.Name); v.ExportedTime == nil {
		t.Errorf("the export did not run: %+v", v)
	}
	for _, c := range e.sd.exportCalls() {
		if c.repo.CredentialsSecret != "cos-credentials" || c.repo.BackupKeySecret != "cos-backup-key" {
			t.Errorf("repo = %+v, want references to Secrets by name", c.repo)
		}
	}
}

func TestBackupExport_ExportedMomentsAreNotExportedAgainAndANewMomentIs(t *testing.T) {
	e := newCOSEnv(t, nil)
	m1 := e.now.Add(-2 * time.Hour)
	e.backedUp(map[string]time.Time{e.a.Name: m1})
	e.reconcile()
	e.reconcile()
	e.reconcile()
	if n := len(e.sd.exportCalls()); n != 1 {
		t.Fatalf("export calls = %d after three passes, want 1", n)
	}
	if v := e.volume(e.a.Name); v.ExportedTime == nil || !v.ExportedTime.Time.Equal(m1) {
		t.Fatalf("the exported moment was not kept across passes: %v", v.ExportedTime)
	}

	m2 := e.now.Add(-time.Hour)
	e.backedUp(map[string]time.Time{e.a.Name: m2})
	e.reconcile()
	calls := e.sd.exportCalls()
	if len(calls) != 2 || !calls[1].moment.Equal(m2) {
		t.Fatalf("export calls = %+v, want a second one for the new moment", calls)
	}
	if v := e.volume(e.a.Name); !v.ExportedTime.Time.Equal(m2) {
		t.Errorf("exportedTime = %v, want %v", v.ExportedTime, m2)
	}
}

func TestBackupExport_OneJobAtATimeForAPolicy(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour), e.b.Name: e.now.Add(-time.Hour)})
	e.sd.export.running = true
	e.reconcile()
	e.reconcile()
	for _, c := range e.sd.exportCalls() {
		if c.key != exportKey(e.a) {
			t.Fatalf("a second Job (%s) was started while the first runs", c.key)
		}
	}
	if len(e.sd.exportCalls()) == 0 {
		t.Fatal("the first Job was never started")
	}

	e.sd.export.running = false
	e.reconcile()
	if v := e.volume(e.a.Name); v.ExportedTime == nil {
		t.Fatalf("volume a is not exported once its Job finished")
	}
	// The pass that sees a Job finish may start the next one.
	e.reconcile()
	if v := e.volume(e.b.Name); v.ExportedTime == nil {
		t.Errorf("volume b was not exported after volume a")
	}
}

func TestBackupExport_AFailedExportSetsTheErrorAndNeverStopsTheBackups(t *testing.T) {
	e := newCOSEnv(t, nil)
	moment := e.now.Add(-time.Hour)
	e.backedUp(map[string]time.Time{e.a.Name: moment})
	e.sd.export.failFor = map[string]string{exportKey(e.a): "bucket unreachable"}
	e.reconcile()

	v := e.volume(e.a.Name)
	if v.ExportedTime != nil || !strings.Contains(v.ExportError, "bucket unreachable") {
		t.Errorf("status = %v %q, want no export and the error", v.ExportedTime, v.ExportError)
	}
	st := e.bp("nightly").Status
	if st.Phase != "Active" || st.Message != "" {
		t.Errorf("an export problem must not change the phase or message: %q %q", st.Phase, st.Message)
	}
	if v.LastBackupTime == nil || !v.LastBackupTime.Time.Equal(moment) {
		t.Errorf("the local backup status was lost: %v", v.LastBackupTime)
	}
	if e.repls()[0].Annotations[pausedAnnotation] == "true" {
		t.Errorf("an export problem must never pause the backups to the snapshot pool")
	}

	// A new local moment is still recorded while the export keeps failing.
	later := e.now.Add(-time.Minute)
	e.backedUp(map[string]time.Time{e.a.Name: later})
	e.reconcile()
	if v := e.volume(e.a.Name); v.LastBackupTime == nil || !v.LastBackupTime.Time.Equal(later) {
		t.Errorf("local backup status did not follow: %v", v.LastBackupTime)
	}
}

func TestBackupExport_AFailedExportIsRetriedLaterAndTheErrorClears(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.sd.export.failFor = map[string]string{exportKey(e.a): "bucket unreachable"}
	e.reconcile()
	first := len(e.sd.exportCalls())

	e.reconcile() // the next pass, a moment later
	if n := len(e.sd.exportCalls()); n != first {
		t.Errorf("a failed export was retried at once (%d calls, was %d); wait before trying again", n, first)
	}

	e.sd.export.failFor = nil
	e.advance(exportRetryDelay + time.Second)
	e.reconcile()
	v := e.volume(e.a.Name)
	if v.ExportedTime == nil || v.ExportError != "" {
		t.Errorf("after the retry: %v %q, want exported and no error", v.ExportedTime, v.ExportError)
	}
}

func TestBackupExport_AVolumeThatFailsDoesNotBlockTheOthers(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour), e.b.Name: e.now.Add(-time.Hour)})
	e.sd.export.failFor = map[string]string{exportKey(e.a): "disk read error"}
	e.reconcile()
	e.reconcile()
	if v := e.volume(e.b.Name); v.ExportedTime == nil {
		t.Errorf("volume b was not exported because volume a fails")
	}
	if v := e.volume(e.a.Name); v.ExportError == "" {
		t.Errorf("volume a shows no error")
	}
}

func TestBackupExport_AJobThatCannotBeStartedSetsTheError(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.sd.export.startErr = errors.New("jobs are forbidden")
	e.reconcile()
	if v := e.volume(e.a.Name); !strings.Contains(v.ExportError, "jobs are forbidden") {
		t.Errorf("exportError = %q", v.ExportError)
	}
	if got := e.bp("nightly").Status.Phase; got != "Active" {
		t.Errorf("phase = %q, want Active", got)
	}
}

func TestBackupExport_AJobThatLostItsMessageIsNotCountedAsDone(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.sd.export.lostMessage = true
	e.reconcile()
	v := e.volume(e.a.Name)
	if v.ExportedTime != nil {
		t.Errorf("a result without a moment was recorded as done: %v", v.ExportedTime)
	}
	if v.ExportError == "" {
		t.Errorf("a Job with no result must set an error")
	}
}

func TestBackupExport_TheExportedMomentIsWhatTheJobReports(t *testing.T) {
	e := newCOSEnv(t, nil)
	older := e.now.Add(-3 * time.Hour)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.sd.export.momentOverride = &older // a newer backup arrived while the Job ran
	e.reconcile()
	if v := e.volume(e.a.Name); v.ExportedTime == nil || !v.ExportedTime.Time.Equal(older) {
		t.Fatalf("exportedTime = %v, want the moment the Job reported", v.ExportedTime)
	}
	e.sd.export.momentOverride = nil
	e.reconcile()
	if n := len(e.sd.exportCalls()); n != 2 {
		t.Errorf("export calls = %d, want a second export for the newer moment", n)
	}
}

func TestBackupExport_AScheduleLimitsHowOftenAVolumeIsSent(t *testing.T) {
	e := newCOSEnv(t, func(p *v1alpha1.BackupPolicy) { p.Spec.Export.Schedule = "6h" })
	m1 := e.now.Add(-time.Hour)
	e.backedUp(map[string]time.Time{e.a.Name: m1})
	e.reconcile()
	if n := len(e.sd.exportCalls()); n != 1 {
		t.Fatalf("the first export must go at once, calls = %d", n)
	}
	e.backedUp(map[string]time.Time{e.a.Name: m1.Add(time.Hour)})
	e.advance(time.Hour)
	e.reconcile()
	if n := len(e.sd.exportCalls()); n != 1 {
		t.Errorf("calls = %d; a new moment inside the schedule must wait", n)
	}
	e.advance(5 * time.Hour)
	e.reconcile()
	if n := len(e.sd.exportCalls()); n != 2 {
		t.Errorf("calls = %d; the new moment must go once the schedule has passed", n)
	}
}

func TestBackupExport_AnInvalidBlockStartsNoJobAndSaysWhy(t *testing.T) {
	// The webhook refuses this block; the controller checks again, because the
	// webhook can be off.
	e := newCOSEnv(t, func(p *v1alpha1.BackupPolicy) { p.Spec.Export.KeepFor = "2h" })
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.reconcile()
	if n := len(e.sd.exportCalls()); n != 0 {
		t.Errorf("export calls = %d, want none", n)
	}
	if v := e.volume(e.a.Name); !strings.Contains(v.ExportError, "25h") {
		t.Errorf("exportError = %q, want the rule", v.ExportError)
	}
	if got := e.bp("nightly").Status.Phase; got != "Active" {
		t.Errorf("phase = %q, want Active", got)
	}
}

func TestBackupExport_RemovingTheBlockClearsTheExportStatus(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.reconcile()
	bp := e.bp("nightly")
	bp.Spec.Export = nil
	if err := e.c.Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if v := e.volume(e.a.Name); v.ExportedTime != nil || v.ExportedBytes != 0 || v.ExportError != "" {
		t.Errorf("export status stayed after the block was removed: %+v", v)
	}
}

func TestBackupExport_TheShortestLockUsesAnHourlyMaintenanceInterval(t *testing.T) {
	e := newCOSEnv(t, func(p *v1alpha1.BackupPolicy) { p.Spec.Export.KeepFor = "25h" })
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.reconcile()
	calls := e.sd.exportCalls()
	if len(calls) != 1 || calls[0].repo.KeepFor != 25*time.Hour || calls[0].repo.MaintenanceInterval != time.Hour {
		t.Fatalf("calls = %+v, want keep 25h and an hourly maintenance interval", calls)
	}
}

// Maintenance.

func TestBackupExport_NoMaintenanceBeforeTheFirstExport(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{})
	e.reconcile()
	if n := len(e.sd.maintainCalls()); n != 0 {
		t.Errorf("maintenance ran %d time(s) for a repository that does not exist yet", n)
	}
}

func TestBackupExport_MaintenanceRunsAfterAnExportAndThenOnceEveryInterval(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.reconcile()
	calls := e.sd.maintainCalls()
	if len(calls) == 0 {
		t.Fatal("maintenance never ran after an export")
	}
	c := calls[0]
	if c.key != "exportmaint-nightly" {
		t.Errorf("key = %q", c.key)
	}
	if want := e.now.Add(-30 * 24 * time.Hour); !c.repo.ExpireBefore.Equal(want) {
		t.Errorf("ExpireBefore = %s, want %s (now minus keepFor)", c.repo.ExpireBefore, want)
	}
	if c.repo.MaintenanceInterval != 24*time.Hour || c.repo.Prefix != "lab/nightly/" || c.repo.Bucket != "pool-backups" {
		t.Errorf("repo = %+v", c.repo)
	}

	n := len(e.sd.maintainCalls())
	e.advance(23 * time.Hour)
	e.reconcile()
	if got := len(e.sd.maintainCalls()); got != n {
		t.Errorf("maintenance ran again after 23h (%d calls, was %d)", got, n)
	}
	e.advance(2 * time.Hour)
	e.reconcile()
	if got := len(e.sd.maintainCalls()); got != n+1 {
		t.Errorf("maintenance calls = %d, want %d after the interval", got, n+1)
	}
}

func TestBackupExport_MaintenanceKeepsPollingARunningJob(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.sd.export.maintainRunning = true
	e.reconcile()
	e.reconcile()
	if n := len(e.sd.maintainCalls()); n < 2 {
		t.Fatalf("maintenance calls = %d; the controller must keep asking about a running Job", n)
	}
	e.sd.export.maintainRunning = false
	e.reconcile()
	n := len(e.sd.maintainCalls())
	e.reconcile()
	if got := len(e.sd.maintainCalls()); got != n {
		t.Errorf("maintenance ran again at once after it finished")
	}
}

func TestBackupExport_AFailedMaintenanceNeverStopsBackupsOrExports(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.sd.export.maintainFail = "kopia crashed"
	e.reconcile()
	first := len(e.sd.maintainCalls())
	e.reconcile()
	if got := len(e.sd.maintainCalls()); got != first {
		t.Errorf("a failed maintenance was retried at once")
	}
	e.advance(exportRetryDelay + time.Second)
	e.sd.export.maintainFail = ""
	e.reconcile()
	if got := len(e.sd.maintainCalls()); got != first+1 {
		t.Errorf("maintenance was not retried after the delay")
	}
	if got := e.bp("nightly").Status.Phase; got != "Active" {
		t.Errorf("phase = %q", got)
	}
}

// Metrics.

func TestBackupExport_ReportsTheAgeOfTheOldestUnexportedMomentAndTheNewestExport(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.sd.export.running = true // nothing finishes: both volumes stay unexported
	mA, mB := e.now.Add(-5*time.Hour), e.now.Add(-2*time.Hour)
	e.backedUp(map[string]time.Time{e.a.Name: mA, e.b.Name: mB})
	e.reconcile()
	if got := gaugeValue(metrics.BackupExportOldestUnexportedAgeSeconds.WithLabelValues("nightly")); got != 5*3600 {
		t.Errorf("oldest unexported age = %v, want %v", got, 5*3600)
	}

	e.sd.export.running = false
	e.reconcile()
	e.reconcile() // the second volume
	if got := gaugeValue(metrics.BackupExportOldestUnexportedAgeSeconds.WithLabelValues("nightly")); got != 0 {
		t.Errorf("oldest unexported age = %v after everything went out, want 0", got)
	}
	// The newest export is volume b's moment, 2 hours old.
	if got := gaugeValue(metrics.BackupExportNewestAgeSeconds.WithLabelValues("nightly")); got != 2*3600 {
		t.Errorf("newest export age = %v, want %v", got, 2*3600)
	}
}

func TestBackupExport_NewestExportAgeCountsFromThePolicyWhenNothingWasExported(t *testing.T) {
	e := newCOSEnv(t, func(p *v1alpha1.BackupPolicy) {
		p.CreationTimestamp = metav1.NewTime(time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC))
	})
	e.sd.export.startErr = errors.New("no bucket")
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.reconcile()
	if got := gaugeValue(metrics.BackupExportNewestAgeSeconds.WithLabelValues("nightly")); got != 3*3600 {
		t.Errorf("newest export age = %v, want %v (since the policy was made)", got, 3*3600)
	}
}

func TestBackupExport_NoExportMetricsWithoutABlockOrAfterThePolicyIsGone(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.reconcile()
	if !hasPolicySeries(metrics.BackupExportNewestAgeSeconds, "nightly") {
		t.Fatal("no export series for a policy with an export block")
	}
	bp := e.bp("nightly")
	bp.Spec.Export = nil
	if err := e.c.Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if hasPolicySeries(metrics.BackupExportNewestAgeSeconds, "nightly") || hasPolicySeries(metrics.BackupExportOldestUnexportedAgeSeconds, "nightly") {
		t.Errorf("export series left after the block was removed; an alert on them would be false")
	}

	bp = e.bp("nightly")
	bp.Spec.Export = testBackupExport()
	if err := e.c.Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if err := e.c.Delete(e.ctx, e.bp("nightly")); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if hasPolicySeries(metrics.BackupExportNewestAgeSeconds, "nightly") || hasPolicySeries(metrics.BackupExportOldestUnexportedAgeSeconds, "nightly") {
		t.Errorf("export series left after the policy was deleted")
	}
}

// hasPolicySeries says whether a metric has a series for the policy.
func hasPolicySeries(c prometheus.Collector, policy string) bool {
	ch := make(chan prometheus.Metric, 1024)
	c.Collect(ch)
	close(ch)
	for m := range ch {
		var d dto.Metric
		if err := m.Write(&d); err != nil {
			continue
		}
		for _, l := range d.GetLabel() {
			if l.GetName() == "policy" && l.GetValue() == policy {
				return true
			}
		}
	}
	return false
}
