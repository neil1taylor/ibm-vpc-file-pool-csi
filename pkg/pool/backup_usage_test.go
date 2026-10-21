package pool

import (
	"errors"
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics"
)

// usageEnv is a backup environment whose snapshot share reports its usage.
type usageEnv struct {
	*backupEnv
	sd  *fakeShareData
	now time.Time
}

func newUsageEnv(t *testing.T, percent int) *usageEnv {
	t.Helper()
	e := &usageEnv{backupEnv: newBackupEnv(t), now: time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)}
	e.sd = &fakeShareData{nfs: newFakeNFSOperations(), base: "/x", usagePercent: percent}
	e.bc.SetShareData(e.sd)
	e.bc.SetNowFunc(func() time.Time { return e.now })
	e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", nil)
	return e
}

func (e *usageEnv) paused() bool { return e.repls()[0].Annotations[pausedAnnotation] == "true" }

func TestBackupController_StopsBackupsWhenTheSnapshotPoolIsAbove85Percent(t *testing.T) {
	e := newUsageEnv(t, 91)
	e.reconcile()
	if !e.paused() {
		t.Errorf("the owned replication policy must be paused when the share is 91%% full")
	}
	st := e.bp("nightly").Status
	if st.Phase != "Error" {
		t.Errorf("phase = %q, want Error", st.Phase)
	}
	for _, want := range []string{"91%", "85%", "not deleted"} {
		if !strings.Contains(st.Message, want) {
			t.Errorf("message %q lacks %q", st.Message, want)
		}
	}
	if len(st.Volumes) != 1 {
		t.Errorf("the status must still list the volumes a restore needs, got %d", len(st.Volumes))
	}
}

func TestBackupController_ExactlyAt85PercentKeepsBackingUp(t *testing.T) {
	e := newUsageEnv(t, 85)
	e.reconcile()
	if e.paused() {
		t.Errorf("85%% is the limit, not over it; backups must go on")
	}
	if got := e.bp("nightly").Status.Phase; got != "Active" {
		t.Errorf("phase = %q, want Active", got)
	}
}

func TestBackupController_ResumesWhenUsageFalls(t *testing.T) {
	e := newUsageEnv(t, 91)
	e.reconcile()
	e.sd.usagePercent = 60
	e.now = e.now.Add(snapshotUsageInterval + time.Second)
	e.reconcile()
	if e.paused() {
		t.Errorf("the owned replication policy must resume when usage falls to 60%%")
	}
	st := e.bp("nightly").Status
	if st.Phase != "Active" || st.Message != "" {
		t.Errorf("status = %q %q, want Active with no message", st.Phase, st.Message)
	}
}

func TestBackupController_ReadsUsageOnlyNowAndThen(t *testing.T) {
	e := newUsageEnv(t, 40)
	e.reconcile()
	e.reconcile()
	if e.sd.usageCalls != 1 {
		t.Errorf("usage calls = %d after two quick passes, want 1", e.sd.usageCalls)
	}
	e.now = e.now.Add(snapshotUsageInterval + time.Second)
	e.reconcile()
	if e.sd.usageCalls != 2 {
		t.Errorf("usage calls = %d after the interval, want 2", e.sd.usageCalls)
	}
}

func TestBackupController_UsageJobStillRunningChangesNothing(t *testing.T) {
	e := newUsageEnv(t, 91)
	e.reconcile() // full
	e.sd.inProgress = true
	e.now = e.now.Add(snapshotUsageInterval + time.Second)
	e.reconcile()
	if !e.paused() {
		t.Errorf("an unfinished usage Job must not resume backups")
	}
	if e.sd.usageCalls < 2 {
		t.Errorf("the controller must keep asking about the running Job")
	}
}

func TestBackupController_UnreadableUsageChangesNothing(t *testing.T) {
	e := newUsageEnv(t, 40)
	e.sd.usageErr = errors.New("df failed")
	e.reconcile()
	if e.paused() {
		t.Errorf("a failed usage check must not stop backups")
	}
	if got := e.bp("nightly").Status.Phase; got != "Active" {
		t.Errorf("phase = %q, want Active", got)
	}
}

func TestBackupController_FullPoolNeverDeletesBackups(t *testing.T) {
	e := newUsageEnv(t, 99)
	e.reconcile()
	if n := e.sd.removeCount(); n != 0 {
		t.Errorf("RemoveDir called %d times; a full pool must never delete backups", n)
	}
	rps := e.repls()
	if len(rps) != 1 || rps[0].Spec.RestorePointKeepHours == nil || *rps[0].Spec.RestorePointKeepHours != 168 {
		t.Errorf("the keep time must stay as set: %+v", rps)
	}
}

func TestBackupController_SuspendedPolicyStaysPausedWhenUsageIsLow(t *testing.T) {
	e := newUsageEnv(t, 10)
	bp := e.bp("nightly")
	bp.Spec.Suspend = true
	if err := e.c.Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if !e.paused() {
		t.Errorf("suspend must still pause the policy")
	}
	if got := e.bp("nightly").Status.Phase; got != "Suspended" {
		t.Errorf("phase = %q, want Suspended", got)
	}
}

func TestBackupController_FullPoolIsReportedEvenWhenSuspended(t *testing.T) {
	e := newUsageEnv(t, 95)
	bp := e.bp("nightly")
	bp.Spec.Suspend = true
	if err := e.c.Update(e.ctx, bp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if got := e.bp("nightly").Status.Phase; got != "Error" {
		t.Errorf("phase = %q, want Error: a full pool is the more urgent news", got)
	}
}

func TestBackupController_UsageCheckNeedsAShareRunner(t *testing.T) {
	e := newBackupEnv(t)
	e.addClaim("apps", "db", "data-a", yes)
	e.policy("nightly", nil)
	e.reconcile()
	if got := e.bp("nightly").Status.Phase; got != "Active" {
		t.Errorf("without a share runner the check is skipped; phase = %q", got)
	}
}

func TestBackupController_ReportsTheUsageOfTheSnapshotPool(t *testing.T) {
	e := newUsageEnv(t, 91)
	e.reconcile()
	if got := gaugeValue(metrics.BackupSnapshotPoolUsedPercent.WithLabelValues("backups")); got != 91 {
		t.Errorf("used percent metric = %v, want 91", got)
	}
}

func TestBackupController_OneUsageJobPerSnapshotPool(t *testing.T) {
	e := newUsageEnv(t, 10)
	e.addClaim("apps", "web", "data-a", map[string]string{"tier": "web"})
	e.policy("weekly", func(bp *v1alpha1.BackupPolicy) {
		bp.Spec.Selector.MatchLabels = map[string]string{"tier": "web"}
	})
	e.reconcile()
	if e.sd.usageCalls != 1 {
		t.Errorf("two policies on one snapshot pool made %d usage reads, want 1", e.sd.usageCalls)
	}
}
