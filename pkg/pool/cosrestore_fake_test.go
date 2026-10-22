package pool

import (
	"context"
	"fmt"
	"time"
)

// fakeCOSRestore is what the fake share data does for the Jobs of a restore
// from COS. Every field is guarded by fakeShareData.mu.
type fakeCOSRestore struct {
	fetches []fetchCall
	// fetchRunning makes the fetch Job report "not finished yet"; fetchFail
	// makes it fail as a Job that used up its retries.
	fetchRunning bool
	fetchFail    string
	// fetchMoment is the moment a finished fetch reports.
	fetchMoment time.Time

	cleanups       []cleanupCall
	cleanupRunning bool
	cleanupFail    string
}

type fetchCall struct {
	key  string
	dst  ShareMount
	repo ExportRepo
	req  COSRestoreRequest
}

type cleanupCall struct {
	key     string
	share   ShareMount
	scratch string
}

func (f *fakeShareData) FetchFromCOS(_ context.Context, key string, dst ShareMount, repo ExportRepo, req COSRestoreRequest) (bool, COSRestoreOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cos.fetches = append(f.cos.fetches, fetchCall{key: key, dst: dst, repo: repo, req: req})
	if f.cos.fetchFail != "" {
		return false, COSRestoreOutcome{}, fmt.Errorf("%w: %s", ErrShareJobFailed, f.cos.fetchFail)
	}
	if f.cos.fetchRunning {
		return false, COSRestoreOutcome{}, nil
	}
	return true, COSRestoreOutcome{Moment: f.cos.fetchMoment}, nil
}

func (f *fakeShareData) RemoveCOSScratch(_ context.Context, key string, share ShareMount, scratch string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cos.cleanups = append(f.cos.cleanups, cleanupCall{key: key, share: share, scratch: scratch})
	if f.cos.cleanupFail != "" {
		return false, fmt.Errorf("%w: %s", ErrShareJobFailed, f.cos.cleanupFail)
	}
	return !f.cos.cleanupRunning, nil
}

func (f *fakeShareData) cosFetches() []fetchCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]fetchCall(nil), f.cos.fetches...)
}

func (f *fakeShareData) cosCleanups() []cleanupCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]cleanupCall(nil), f.cos.cleanups...)
}
