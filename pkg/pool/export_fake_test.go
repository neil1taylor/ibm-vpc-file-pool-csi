package pool

import (
	"context"
	"fmt"
	"time"
)

// fakeExport is what the fake share data does for the COS export and
// maintenance Jobs. Every field is guarded by fakeShareData.mu.
type fakeExport struct {
	calls []exportCall
	// running makes export Jobs report "not finished yet".
	running bool
	// failFor makes the export Job with that key fail as a Job that used up its
	// retries (the Job is gone, the next call starts a new one).
	failFor map[string]string
	// startErr makes every export call fail before any Job (for example an
	// API error).
	startErr error
	// bytes is what a finished export reports as sent; momentOverride replaces
	// the moment it reports (the zero time means the Job lost its message).
	bytes          int64
	momentOverride *time.Time
	lostMessage    bool

	// list is what the file list Jobs do.
	list fakeList

	maintenances    []maintainCall
	maintainRunning bool
	maintainFail    string
}

// fakeList is what the fake share data does for the file list Jobs.
type fakeList struct {
	calls []listCall
	// running makes the list Job report "not finished yet".
	running bool
	// err makes every call fail; notEnabled makes it say the guest image is not set.
	err        error
	notEnabled bool
	// entries is what a finished list reports; momentOverride replaces the
	// moment it reports (the default is the moment the backup is from).
	entries        int
	momentOverride *time.Time
	// order records "list" and "export" in the order they were called.
	order []string
}

type listCall struct {
	key         string
	backupPath  string
	consistency string
}

func (f *fakeShareData) BuildFileList(_ context.Context, key string, _ ShareMount, backupPath, consistency string) (bool, FileListOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.export.list.calls = append(f.export.list.calls, listCall{key: key, backupPath: backupPath, consistency: consistency})
	f.export.list.order = append(f.export.list.order, "list")
	switch {
	case f.export.list.notEnabled:
		return false, FileListOutcome{}, errNoBrowseGuest
	case f.export.list.err != nil:
		return false, FileListOutcome{}, f.export.list.err
	case f.export.list.running:
		return false, FileListOutcome{}, nil
	}
	out := FileListOutcome{Entries: f.export.list.entries, Bytes: 1000}
	if f.export.list.momentOverride != nil {
		out.Moment = *f.export.list.momentOverride
	}
	return true, out, nil
}

func (f *fakeShareData) listCalls() []listCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]listCall(nil), f.export.list.calls...)
}

type exportCall struct {
	key         string
	repo        ExportRepo
	backupPath  string
	momentsPath string
	moment      time.Time
	srcIP       string
}

type maintainCall struct {
	key  string
	repo ExportRepo
}

func (f *fakeShareData) ExportToCOS(_ context.Context, key string, share ShareMount, repo ExportRepo, backupPath, momentsPath string, moment time.Time) (bool, ExportOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.export.list.order = append(f.export.list.order, "export")
	f.export.calls = append(f.export.calls, exportCall{key: key, repo: repo, backupPath: backupPath, momentsPath: momentsPath, moment: moment, srcIP: share.MountTargetIP})
	if f.export.startErr != nil {
		return false, ExportOutcome{}, f.export.startErr
	}
	if msg, bad := f.export.failFor[key]; bad {
		return false, ExportOutcome{}, fmt.Errorf("%w: %s", ErrShareJobFailed, msg)
	}
	if f.export.running {
		return false, ExportOutcome{}, nil
	}
	out := ExportOutcome{Moment: moment, Bytes: f.export.bytes}
	if f.export.momentOverride != nil {
		out.Moment = *f.export.momentOverride
	}
	if f.export.lostMessage {
		out = ExportOutcome{}
	}
	return true, out, nil
}

func (f *fakeShareData) MaintainCOSRepo(_ context.Context, key string, repo ExportRepo) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.export.maintenances = append(f.export.maintenances, maintainCall{key: key, repo: repo})
	if f.export.maintainFail != "" {
		return false, fmt.Errorf("%w: %s", ErrShareJobFailed, f.export.maintainFail)
	}
	return !f.export.maintainRunning, nil
}

func (f *fakeShareData) exportCalls() []exportCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]exportCall(nil), f.export.calls...)
}

func (f *fakeShareData) maintainCalls() []maintainCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]maintainCall(nil), f.export.maintenances...)
}
