package cosdisk

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
)

// wholeCopy is a copy that holds a whole disk as one extent.
type wholeCopy struct{ data []byte }

func (c wholeCopy) Size() int64 { return int64(len(c.data)) }
func (c wholeCopy) Walk(fn func(qcow2.Extent) error) error {
	return fn(qcow2.Extent{Offset: 0, Length: int64(len(c.data)), Kind: qcow2.Data})
}
func (c wholeCopy) DataReader(e qcow2.Extent) io.Reader {
	return bytes.NewReader(c.data[e.Offset : e.Offset+e.Length])
}

var tStart = time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC)

// backupHistory makes a backup folder with a disk and several copies applied.
func backupHistory(t *testing.T, seed int64, copies int) (dir string, moments []time.Time) {
	t.Helper()
	rng := rand.New(rand.NewSource(seed))
	dir = t.TempDir()
	disk := filepath.Join(dir, "disk.img")
	size := (6 + rng.Intn(4)) << 16
	cur := make([]byte, size+rng.Intn(1<<16))
	rng.Read(cur)
	if err := os.WriteFile(disk, cur, 0o660); err != nil {
		t.Fatal(err)
	}
	moments = []time.Time{tStart}
	for i := 0; i < copies; i++ {
		next := append([]byte(nil), cur...)
		for j := 0; j < 1+rng.Intn(5); j++ {
			off := rng.Intn(len(next) - 1)
			n := 1 + rng.Intn(min(len(next)-off, 3<<16))
			rng.Read(next[off : off+n])
		}
		switch rng.Intn(4) {
		case 0:
			extra := make([]byte, 1+rng.Intn(1<<17))
			rng.Read(extra)
			next = append(next, extra...)
		case 1:
			next = next[:len(next)-1-rng.Intn(1<<16)]
		}
		from := moments[len(moments)-1]
		to := from.Add(time.Hour)
		if _, err := replication.ApplyCopy(wholeCopy{next}, disk, replication.ApplyOptions{From: from, To: to, Full: true}); err != nil {
			t.Fatalf("ApplyCopy: %v", err)
		}
		cur = next
		moments = append(moments, to)
	}
	return dir, moments
}

// davServer serves a backup folder like the Kopia WebDAV server: PROPFIND lists
// a folder, HEAD gives a size and GET with Range gives 206. It counts the bytes
// that GETs sent, by path.
type davServer struct {
	*httptest.Server
	dir     string
	extra   []string // names to list that are not in the folder
	served  map[string]*atomic.Int64
	propGet atomic.Int64
}

func newDAV(t *testing.T, dir string, extra ...string) *davServer {
	t.Helper()
	d := &davServer{dir: dir, extra: extra, served: map[string]*atomic.Int64{}}
	counter := func(p string) *atomic.Int64 {
		if d.served[p] == nil {
			d.served[p] = new(atomic.Int64)
		}
		return d.served[p]
	}
	for _, p := range []string{"/disk.img"} {
		counter(p)
	}
	entries, _ := os.ReadDir(filepath.Join(dir, ".repl-undo"))
	for _, e := range entries {
		counter("/.repl-undo/" + e.Name())
	}
	d.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "PROPFIND" {
			d.propGet.Add(1)
			d.propfind(w, r)
			return
		}
		c := d.served[r.URL.Path]
		if c == nil || strings.Contains(r.URL.Path, "..") {
			http.NotFound(w, r)
			return
		}
		f, err := os.Open(filepath.Join(dir, filepath.FromSlash(r.URL.Path)))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		defer func() { _ = f.Close() }()
		http.ServeContent(&countingWriter{ResponseWriter: w, n: c}, r, "", time.Time{}, f)
	}))
	t.Cleanup(d.Close)
	return d
}

type countingWriter struct {
	http.ResponseWriter
	n *atomic.Int64
}

func (c *countingWriter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return c.ResponseWriter.Write(p)
}

func (d *davServer) propfind(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/.repl-undo/" {
		http.NotFound(w, r)
		return
	}
	entries, err := os.ReadDir(filepath.Join(d.dir, ".repl-undo"))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	names := []string{}
	for _, e := range entries {
		names = append(names, e.Name())
	}
	names = append(names, d.extra...)
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><D:multistatus xmlns:D="DAV:">`)
	b.WriteString(`<D:response><D:href>/.repl-undo/</D:href><D:propstat><D:prop><D:resourcetype><D:collection xmlns:D="DAV:"/></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`)
	for _, n := range names {
		fmt.Fprintf(&b, `<D:response><D:href>/.repl-undo/%s</D:href><D:propstat><D:prop><D:resourcetype></D:resourcetype></D:prop><D:status>HTTP/1.1 200 OK</D:status></D:propstat></D:response>`, strings.ReplaceAll(n, "&", "&amp;"))
	}
	b.WriteString(`</D:multistatus>`)
	w.Header().Set("Content-Type", `application/xml; charset="utf-8"`)
	w.WriteHeader(http.StatusMultiStatus)
	_, _ = io.WriteString(w, b.String())
}

func openCOSView(t *testing.T, d *davServer, latest, at time.Time) (*replication.DiskView, func(), error) {
	t.Helper()
	v, err := OpenUndoView(context.Background(), d.Client(), d.URL+"/disk.img", latest, at, Options{})
	if err != nil {
		return nil, func() {}, err
	}
	return v.View, func() { _ = v.Close() }, nil
}

func TestUndoView_EqualsRestoreDiskForEveryRestorePoint(t *testing.T) {
	for seed := int64(1); seed <= 8; seed++ {
		dir, moments := backupHistory(t, seed, 3+int(seed%3))
		latest := moments[len(moments)-1]
		half := 30 * time.Minute
		ats := []time.Time{{}}
		for _, m := range moments {
			ats = append(ats, m, m.Add(half), m.Add(-half))
		}
		d := newDAV(t, dir)
		for _, at := range ats {
			out := filepath.Join(t.TempDir(), "out.img")
			want, werr := replication.RestoreDisk(filepath.Join(dir, "disk.img"), at, out)
			view, closeView, verr := openCOSView(t, d, latest, at)
			if (werr == nil) != (verr == nil) {
				t.Fatalf("seed %d at %v: RestoreDisk error %v, COS view error %v", seed, at, werr, verr)
			}
			if werr != nil {
				if !errors.Is(verr, replication.ErrNoRestorePoint) {
					t.Fatalf("seed %d at %v: %v is not ErrNoRestorePoint", seed, at, verr)
				}
				continue
			}
			expect, _ := os.ReadFile(out)
			if view.Size() != int64(len(expect)) || !view.Moment().Equal(want.Moment) {
				t.Fatalf("seed %d at %v: size %d moment %v; want %d %v", seed, at, view.Size(), view.Moment(), len(expect), want.Moment)
			}
			got := make([]byte, view.Size())
			if n, err := view.ReadAt(got, 0); int64(n) != view.Size() || (err != nil && !errors.Is(err, io.EOF)) {
				t.Fatalf("ReadAt: %d, %v", n, err)
			}
			if !bytes.Equal(got, expect) {
				t.Fatalf("seed %d at %v: the COS view differs from the restored disk", seed, at)
			}
			closeView()
		}
	}
}

func TestUndoView_FetchesOnlyWhatARestorePointNeeds(t *testing.T) {
	dir, moments := backupHistory(t, 11, 5)
	d := newDAV(t, dir)
	latest := moments[len(moments)-1]
	view, closeView, err := openCOSView(t, d, latest, moments[len(moments)-2]) // the newest restore point
	if err != nil {
		t.Fatal(err)
	}
	defer closeView()
	// Older undo files: only a header and a trailer each.
	names := []string{}
	for n := range d.served {
		if strings.HasPrefix(n, "/.repl-undo/") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	for _, n := range names[:len(names)-1] {
		if got := d.served[n].Load(); got > 40+16 {
			t.Errorf("%s: %d bytes fetched; only its header and trailer are needed", n, got)
		}
	}
	buf := make([]byte, 4096)
	if _, err = view.ReadAt(buf, 100); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(filepath.Join(dir, "disk.img"))
	if got := d.served["/disk.img"].Load(); got > 4<<20 || got > fi.Size() {
		t.Errorf("a 4 KiB read fetched %d bytes of the disk", got)
	}
}

func TestUndoView_OlderThanEveryUndoFileIsRefusedInWords(t *testing.T) {
	dir, moments := backupHistory(t, 4, 2)
	d := newDAV(t, dir)
	_, _, err := openCOSView(t, d, moments[len(moments)-1], moments[0].Add(-time.Hour))
	if !errors.Is(err, replication.ErrNoRestorePoint) {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(strings.ToLower(err.Error()), "moment") {
		t.Errorf("the words must say restore point: %v", err)
	}
}

func TestUndoView_HostileNamesAreNeverFetched(t *testing.T) {
	dir, moments := backupHistory(t, 6, 2)
	d := newDAV(t, dir, "../disk.img", "a%2Fb.undo", "x.undo", "0000000001.undo.part", "evil.txt", "..%2F..%2Fetc%2Fpasswd.undo")
	view, closeView, err := openCOSView(t, d, moments[len(moments)-1], moments[0])
	if err != nil {
		t.Fatal(err)
	}
	defer closeView()
	if view.Size() <= 0 {
		t.Error("no view")
	}
	if got := d.served["/disk.img"].Load(); got != 0 {
		t.Errorf("opening a view fetched %d bytes of the disk", got)
	}
}

func TestUndoView_NoUndoFolderMeansOnlyTheLatest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), bytes.Repeat([]byte{7}, 1<<17), 0o660); err != nil {
		t.Fatal(err)
	}
	d := newDAV(t, dir)
	_, _, err := openCOSView(t, d, tStart, tStart.Add(-time.Hour))
	if !errors.Is(err, replication.ErrNoRestorePoint) {
		t.Fatalf("err = %v", err)
	}
	view, closeView, err := openCOSView(t, d, tStart, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeView()
	if !view.Latest() || view.Size() != 1<<17 {
		t.Errorf("latest %v size %d", view.Latest(), view.Size())
	}
}

func TestUndoView_OnlyAThisHostUndoServerIsUsed(t *testing.T) {
	_, err := OpenUndoView(context.Background(), http.DefaultClient, "http://example.com/disk.img", tStart, tStart, Options{})
	if err == nil {
		t.Fatal("a server that is not 127.0.0.1 must be refused")
	}
}

// Many undo layers must not multiply the cache: each layer holds at most one
// small segment, and a view opens only a limited number of layers.
func TestUndoView_CacheStaysWithinTheBudgetAcrossManyLayers(t *testing.T) {
	dir, moments := backupHistory(t, 21, 40)
	d := newDAV(t, dir)
	v, err := OpenUndoView(context.Background(), d.Client(), d.URL+"/disk.img", moments[len(moments)-1], moments[0], Options{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = v.Close() }()
	got := make([]byte, v.View.Size())
	if _, err = v.View.ReadAt(got, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	if layers, cached := v.undoLayers(), v.UndoCachedBytes(); cached > int64(layers)*undoReadSegment {
		t.Errorf("%d layers cache %d bytes; the budget is %d each", layers, cached, undoReadSegment)
	}
	if v.UndoCachedBytes() > int64(replication.MaxViewLayers())*undoReadSegment {
		t.Errorf("the undo cache is over the total budget")
	}
}

func TestRestorePointsOf_ListsWhatTheUndoFilesReachFromTheirHeadersOnly(t *testing.T) {
	dir, moments := backupHistory(t, 3, 4)
	d := newDAV(t, dir, "notes.txt", "../x.undo", "7.undo.bak")
	got, err := RestorePointsOf(context.Background(), d.Client(), d.URL+"/disk.img")
	if err != nil {
		t.Fatal(err)
	}
	want := moments[:len(moments)-1] // the last moment is the disk itself
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("restore point %d = %v, want %v", i, got[i], want[i])
		}
	}
	// Only a few bytes at each end of each undo file are read.
	for p, n := range d.served {
		if strings.HasPrefix(p, "/.repl-undo/") && n.Load() > 1024 {
			t.Errorf("%s: %d bytes were fetched to list the restore points", p, n.Load())
		}
	}
}

func TestRestorePointsOf_NoUndoFolderMeansNoOlderRestorePoints(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), []byte("x"), 0o660); err != nil {
		t.Fatal(err)
	}
	d := newDAV(t, dir)
	got, err := RestorePointsOf(context.Background(), d.Client(), d.URL+"/disk.img")
	if err != nil || len(got) != 0 {
		t.Fatalf("got %v, %v", got, err)
	}
}

func TestRestorePointsOf_OnlyAThisHostUndoServerIsUsed(t *testing.T) {
	if _, err := RestorePointsOf(context.Background(), http.DefaultClient, "http://example.com/disk.img"); err == nil {
		t.Fatal("a disk URL on another host must be refused")
	}
}
