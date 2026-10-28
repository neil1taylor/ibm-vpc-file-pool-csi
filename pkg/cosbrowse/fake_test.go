package cosbrowse

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// fakeRepo is an in-memory Kopia repository. It makes the same JSON that
// `kopia show` prints for a folder.
type fakeRepo struct {
	mu    sync.Mutex
	objs  map[string][]byte
	snaps []Snapshot
	n     int

	// calls records every object that was asked for.
	calls   []string
	snapErr error
	objErr  map[string]error

	// running and maxRunning count Object streams that are open at once.
	running    atomic.Int32
	maxRunning atomic.Int32
	// hold, if set, makes Object wait for it before it returns a stream.
	hold chan struct{}
}

func newFakeRepo() *fakeRepo {
	return &fakeRepo{objs: map[string][]byte{}, objErr: map[string]error{}}
}

func (f *fakeRepo) put(b []byte) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	oid := fmt.Sprintf("k%08d", f.n)
	f.objs[oid] = b
	return oid
}

func (f *fakeRepo) file(content string) string { return f.put([]byte(content)) }

// ent is one entry of a folder.
type ent struct {
	Name  string    `json:"name"`
	Type  string    `json:"type"`
	Mode  string    `json:"mode,omitempty"`
	Size  int64     `json:"size,omitempty"`
	MTime time.Time `json:"mtime"`
	UID   uint32    `json:"uid,omitempty"`
	GID   uint32    `json:"gid,omitempty"`
	Obj   string    `json:"obj"`
}

var epoch = time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC)

func (f *fakeRepo) fileEnt(name, content string) ent {
	return ent{Name: name, Type: "f", Mode: "0644", Size: int64(len(content)), MTime: epoch, Obj: f.file(content)}
}

func (f *fakeRepo) linkEnt(name, target string) ent {
	return ent{Name: name, Type: "s", Mode: "0777", Size: int64(len(target)), MTime: epoch, Obj: f.file(target)}
}

func dirEnt(name, oid string) ent {
	return ent{Name: name, Type: "d", Mode: "0755", MTime: epoch, Obj: oid}
}

func (f *fakeRepo) dir(ents ...ent) string {
	b, err := json.Marshal(map[string]any{"stream": "kopia:directory", "entries": ents})
	if err != nil {
		panic(err)
	}
	return f.put(b)
}

func (f *fakeRepo) snapshot(path, volume string, moment time.Time, root string) Snapshot {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.n++
	s := Snapshot{
		ID: fmt.Sprintf("snap%04d", f.n), SourcePath: path, Volume: volume, Moment: moment,
		Start: moment.Add(time.Minute), Root: root, RootMode: 0o755, RootMTime: epoch,
	}
	f.snaps = append(f.snaps, s)
	return s
}

func (f *fakeRepo) Snapshots(_ context.Context) ([]Snapshot, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.snapErr != nil {
		return nil, f.snapErr
	}
	return append([]Snapshot(nil), f.snaps...), nil
}

type countingReader struct {
	io.Reader
	f      *fakeRepo
	closed bool
}

func (c *countingReader) Close() error {
	if !c.closed {
		c.closed = true
		c.f.running.Add(-1)
	}
	return nil
}

func (f *fakeRepo) Object(ctx context.Context, oid string) (io.ReadCloser, error) {
	if !ValidOID(oid) {
		return nil, fmt.Errorf("the fake was asked for an object ID that fails the check: %q", oid)
	}
	f.mu.Lock()
	f.calls = append(f.calls, oid)
	err := f.objErr[oid]
	b, ok := f.objs[oid]
	hold := f.hold
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errors.New("object not found")
	}
	cur := f.running.Add(1)
	for {
		m := f.maxRunning.Load()
		if cur <= m || f.maxRunning.CompareAndSwap(m, cur) {
			break
		}
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			f.running.Add(-1)
			return nil, ctx.Err()
		}
	}
	return &countingReader{Reader: bytes.NewReader(b), f: f}, nil
}

func (f *fakeRepo) callCount(oid string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if c == oid {
			n++
		}
	}
	return n
}

func (f *fakeRepo) totalCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// p turns "a/b" into a path.
func p(s string) [][]byte {
	if s == "" {
		return nil
	}
	var out [][]byte
	start := 0
	for i := 0; i <= len(s); i++ {
		if i == len(s) || s[i] == '/' {
			out = append(out, []byte(s[start:i]))
			start = i + 1
		}
	}
	return out
}

func mustT(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
