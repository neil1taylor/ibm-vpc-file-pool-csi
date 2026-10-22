package pool

import "sync"

// volumeLocks holds one lock per volume. Snapshot calls hold the lock of the
// volume they change for their whole run, so two changes to one disk never
// overlap, while calls for other volumes go on. The Manager's own lock is
// only held for the short pool bookkeeping. A caller that needs both takes
// the volume lock first.
type volumeLocks struct {
	mu    sync.Mutex
	locks map[string]*volumeLock
}

type volumeLock struct {
	mu   sync.Mutex
	refs int
}

// lock locks the volume's lock and returns the function that unlocks it.
func (v *volumeLocks) lock(volume string) (unlock func()) {
	v.mu.Lock()
	if v.locks == nil {
		v.locks = map[string]*volumeLock{}
	}
	l := v.locks[volume]
	if l == nil {
		l = &volumeLock{}
		v.locks[volume] = l
	}
	l.refs++
	v.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		v.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(v.locks, volume)
		}
		v.mu.Unlock()
	}
}
