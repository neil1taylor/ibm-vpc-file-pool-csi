package pool

import (
	"strings"
	"testing"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

func (e *cosEnv) holdOf(sv *v1alpha1.SubVolume) string {
	e.t.Helper()
	got := &v1alpha1.SubVolume{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: sv.Name}, got); err != nil {
		e.t.Fatal(err)
	}
	return got.Annotations[exportHoldAnnotation]
}

func (e *cosEnv) setHold(sv *v1alpha1.SubVolume, value string) {
	e.t.Helper()
	got := &v1alpha1.SubVolume{}
	if err := e.c.Get(e.ctx, types.NamespacedName{Name: sv.Name}, got); err != nil {
		e.t.Fatal(err)
	}
	if got.Annotations == nil {
		got.Annotations = map[string]string{}
	}
	got.Annotations[exportHoldAnnotation] = value
	if err := e.c.Update(e.ctx, got); err != nil {
		e.t.Fatal(err)
	}
}

func TestBackupExport_ARunningExportHoldsOnlyItsVolumeAndTheHoldEndsWithTheJob(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour), e.b.Name: e.now.Add(-time.Hour)})
	e.sd.export.running = true
	e.reconcile()

	if h := e.holdOf(e.a); !strings.HasPrefix(h, "nightly/") {
		t.Errorf("hold on the volume being exported = %q, want one that names the policy", h)
	}
	if h := e.holdOf(e.b); h != "" {
		t.Errorf("hold on a volume that is not being exported = %q, want none", h)
	}

	e.sd.export.running = false
	e.reconcile()
	if h := e.holdOf(e.a); h != "" {
		t.Errorf("hold after the export finished = %q, want none", h)
	}
}

func TestBackupExport_AFailedExportReleasesTheHold(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.sd.export.failFor = map[string]string{exportKey(e.a): "bucket unreachable"}
	e.reconcile()
	if h := e.holdOf(e.a); h != "" {
		t.Errorf("hold after a failed export = %q, want none", h)
	}
}

func TestBackupExport_AHoldWithNoJobBehindItIsRemoved(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.setHold(e.a, "nightly/"+e.now.Add(-time.Minute).Format(time.RFC3339))
	e.setHold(e.b, "other/"+e.now.Add(-time.Minute).Format(time.RFC3339))
	e.reconcile()
	if h := e.holdOf(e.a); h != "" {
		t.Errorf("stale hold of this policy = %q, want it removed", h)
	}
	if h := e.holdOf(e.b); !strings.HasPrefix(h, "other/") {
		t.Errorf("hold of another policy = %q, want it left alone", h)
	}
}

func TestBackupExport_AHoldPastTheLimitStopsTheJobAndSaysWhy(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.sd.export.running = true
	e.reconcile()
	if e.holdOf(e.a) == "" {
		t.Fatal("no hold while the export runs")
	}

	e.advance(exportHoldLimit + time.Minute)
	e.reconcile()
	if h := e.holdOf(e.a); h != "" {
		t.Errorf("hold past the limit = %q, want it removed", h)
	}
	stopped := false
	for _, k := range e.sd.stops {
		stopped = stopped || k == exportKey(e.a)
	}
	if !stopped {
		t.Errorf("the export Job was not stopped; stops = %v", e.sd.stops)
	}
	if v := e.volume(e.a.Name); !strings.Contains(v.ExportError, "6 hours") {
		t.Errorf("export error = %q, want it to say the limit of 6 hours passed", v.ExportError)
	}
}

func TestBackupExport_AVolumeWhoseCopyIsRunningIsNotExportedYet(t *testing.T) {
	e := newCOSEnv(t, nil)
	rp := e.repls()[0]
	m := metav1.NewTime(e.now.Add(-time.Hour))
	rp.Status.SubVolumeStatuses = []v1alpha1.SubVolumeReplicationStatus{{SubVolumeName: e.a.Name, LastSyncTime: &m, LastError: "sync in progress"}}
	if err := e.c.Status().Update(e.ctx, &rp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if n := len(e.sd.exportCalls()); n != 0 {
		t.Errorf("export started while a local copy of the volume runs (%d calls)", n)
	}
	if h := e.holdOf(e.a); h != "" {
		t.Errorf("hold = %q, want none while no export runs", h)
	}

	rp = e.repls()[0]
	rp.Status.SubVolumeStatuses[0].LastError = ""
	if err := e.c.Status().Update(e.ctx, &rp); err != nil {
		t.Fatal(err)
	}
	e.reconcile()
	if n := len(e.sd.exportCalls()); n == 0 {
		t.Errorf("the export did not start once the local copy ended")
	}
}

func TestBackupExport_AVolumeHeldByAnotherPolicyWaits(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.setHold(e.a, "other/"+e.now.Add(-time.Minute).Format(time.RFC3339))
	e.reconcile()
	if n := len(e.sd.exportCalls()); n != 0 {
		t.Errorf("export started on a volume held by another policy (%d calls)", n)
	}
	if h := e.holdOf(e.a); !strings.HasPrefix(h, "other/") {
		t.Errorf("hold of another policy = %q, want it left alone", h)
	}
}
