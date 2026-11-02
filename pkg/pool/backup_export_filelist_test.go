package pool

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// vmEnv is a COS environment where volume a is the disk of a VM.
func vmEnv(t *testing.T) (*cosEnv, time.Time) {
	e := newCOSEnv(t, nil)
	e.addVM("apps", "web", "a")
	moment := e.now.Add(-time.Hour).Truncate(time.Second)
	e.backedUp(map[string]time.Time{e.a.Name: moment})
	return e, moment
}

func TestBackupExportFileList_IsBuiltBeforeTheExportOfAVMDisk(t *testing.T) {
	e, moment := vmEnv(t)
	e.sd.export.list.entries = 28758
	e.sd.export.list.momentOverride = &moment
	e.reconcile()
	calls := e.sd.listCalls()
	if len(calls) != 1 || calls[0].key != "exportlist-nightly-"+e.a.Name || calls[0].backupPath != "/backup/nightly/pvcs/pvc-apps-a" {
		t.Fatalf("list calls = %+v", calls)
	}
	if got := strings.Join(e.sd.export.list.order, ","); got != "list,export" {
		t.Errorf("order = %s; the list must come first", got)
	}
	v := e.volume(e.a.Name)
	if v.ExportedTime == nil || v.FileListEntries != 28758 || v.FileListError != "" {
		t.Errorf("status = %v %d %q", v.ExportedTime, v.FileListEntries, v.FileListError)
	}
}

func TestBackupExportFileList_NotBuiltForAVolumeThatIsNotAVMDisk(t *testing.T) {
	e := newCOSEnv(t, nil)
	e.backedUp(map[string]time.Time{e.a.Name: e.now.Add(-time.Hour)})
	e.reconcile()
	if n := len(e.sd.listCalls()); n != 0 {
		t.Errorf("%d list Jobs for a plain volume", n)
	}
	if v := e.volume(e.a.Name); v.ExportedTime == nil || v.FileListError != "" {
		t.Errorf("status = %+v", v)
	}
}

func TestBackupExportFileList_AFailedListNeverFailsTheExport(t *testing.T) {
	e, _ := vmEnv(t)
	e.sd.export.list.err = errors.New("boom")
	e.reconcile()
	v := e.volume(e.a.Name)
	if v.ExportedTime == nil || v.ExportError != "" {
		t.Fatalf("the export must be done: %v %q", v.ExportedTime, v.ExportError)
	}
	if v.FileListError == "" || v.FileListEntries != 0 {
		t.Errorf("list status = %d %q; want an error in words", v.FileListEntries, v.FileListError)
	}
	if !strings.Contains(v.FileListError, "slower") {
		t.Errorf("the message should say what the person gets instead: %q", v.FileListError)
	}
}

func TestBackupExportFileList_NotSetUpIsNotAnError(t *testing.T) {
	e, _ := vmEnv(t)
	e.sd.export.list.notEnabled = true
	e.reconcile()
	v := e.volume(e.a.Name)
	if v.ExportedTime == nil || v.FileListError != "" {
		t.Errorf("status = %v %q; no guest image means no list and no complaint", v.ExportedTime, v.FileListError)
	}
}

func TestBackupExportFileList_TheExportWaitsForARunningList(t *testing.T) {
	e, moment := vmEnv(t)
	e.sd.export.list.running = true
	e.reconcile()
	if n := len(e.sd.exportCalls()); n != 0 {
		t.Fatalf("export started while the list runs: %d calls", n)
	}
	e.sd.mu.Lock()
	e.sd.export.list.running = false
	e.sd.export.list.momentOverride = &moment
	e.sd.mu.Unlock()
	e.reconcile()
	if v := e.volume(e.a.Name); v.ExportedTime == nil {
		t.Error("the export should run once the list is done")
	}
}

func TestBackupExportFileList_AListForAnotherMomentIsNotUsed(t *testing.T) {
	e, moment := vmEnv(t)
	other := moment.Add(-24 * time.Hour)
	e.sd.export.list.momentOverride = &other
	e.sd.export.list.entries = 10
	e.reconcile()
	v := e.volume(e.a.Name)
	if v.ExportedTime == nil || v.FileListEntries != 0 || v.FileListError == "" {
		t.Errorf("status = %v %d %q", v.ExportedTime, v.FileListEntries, v.FileListError)
	}
}
