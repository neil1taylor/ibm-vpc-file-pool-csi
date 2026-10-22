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

	maintenances    []maintainCall
	maintainRunning bool
	maintainFail    string
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
