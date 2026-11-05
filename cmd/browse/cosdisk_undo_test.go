package main

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosdisk"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/qcow2"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
)

type wholeCopy struct{ data []byte }

func (c wholeCopy) Size() int64 { return int64(len(c.data)) }
func (c wholeCopy) Walk(fn func(qcow2.Extent) error) error {
	return fn(qcow2.Extent{Length: int64(len(c.data)), Kind: qcow2.Data})
}
func (c wholeCopy) DataReader(e qcow2.Extent) io.Reader {
	return bytes.NewReader(c.data[e.Offset : e.Offset+e.Length])
}

// undoRepo is a fake repository whose export holds a backup folder with undo
// files, served like the Kopia WebDAV server: files with ranges, and PROPFIND
// for the undo folder.
type undoRepo struct {
	dir     string
	moment  time.Time
	stopped *bool
}

func (f *undoRepo) Snapshots(context.Context) ([]cosbrowse.Snapshot, error) {
	return []cosbrowse.Snapshot{{
		ID: "k1234", SourcePath: "/share0/backup/pol/pvcs/" + testVolume, Volume: testVolume,
		Moment: f.moment, Root: "kroot",
	}}, nil
}

func (f *undoRepo) StartWebDAV(_ context.Context, _ string, _ func() (int, bool), _ time.Duration) (*cosbrowse.WebDAV, error) {
	files := http.FileServer(http.Dir(f.dir))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			files.ServeHTTP(w, r)
			return
		}
		entries, err := os.ReadDir(filepath.Join(f.dir, ".repl-undo"))
		if err != nil {
			http.NotFound(w, r)
			return
		}
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/.repl-undo/</D:href></D:response>`)
		for _, e := range entries {
			fmt.Fprintf(&b, `<D:response><D:href>/.repl-undo/%s</D:href></D:response>`, e.Name())
		}
		b.WriteString(`</D:multistatus>`)
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, b.String())
	}))
	return cosbrowse.NewWebDAV(ts.URL+"/disk.img", func() { ts.Close(); *f.stopped = true }), nil
}

// serveAt starts the disk server on the fake repository, asking for at.
func serveAt(t *testing.T, at time.Time) (client *http.Client, base, token string, exit chan int, cancel func(), term string, errb *bytes.Buffer) {
	t.Helper()
	token = strings.Repeat("d", 32)
	crt, key, certPEM := writeCert(t)
	client, err := browse.PinnedClient(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	addrc := make(chan net.Addr, 1)
	exit = make(chan int, 1)
	errb = &bytes.Buffer{}
	var out bytes.Buffer
	ctx, c := context.WithCancel(context.Background())
	term = filepath.Join(t.TempDir(), "term")
	args := []string{"--mode", "cos-disk-server", "--cos-source", cosSourceJSON,
		"--token-file", writeToken(t, token), "--listen", "127.0.0.1:0", "--idle", "1m", "--max-life", "1m",
		"--termination-log", term, "--tls-cert-file", crt, "--tls-key-file", key}
	if !at.IsZero() {
		args = append(args, "--at", at.UTC().Format(time.RFC3339))
	}
	go func() { exit <- run(ctx, args, &out, errb, func(a net.Addr) { addrc <- a }) }()
	select {
	case a := <-addrc:
		return client, "https://" + a.String(), token, exit, c, term, errb
	case code := <-exit:
		c()
		exit <- code // let the caller read it
		return client, "", token, exit, func() {}, term, errb
	case <-time.After(10 * time.Second):
		t.Fatal("did not start")
	}
	return
}

func undoBackup(t *testing.T) (dir string, moments []time.Time) {
	t.Helper()
	dir = t.TempDir()
	cur := bytes.Repeat([]byte{1}, 3<<16)
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), cur, 0o660); err != nil {
		t.Fatal(err)
	}
	moments = []time.Time{time.Date(2026, 10, 22, 7, 15, 0, 0, time.UTC)}
	for i := 2; i <= 4; i++ {
		next := bytes.Repeat([]byte{byte(i)}, (3+i%2)<<16)
		from := moments[len(moments)-1]
		if _, err := replication.ApplyCopy(wholeCopy{next}, filepath.Join(dir, "disk.img"),
			replication.ApplyOptions{From: from, To: from.Add(time.Hour), Full: true}); err != nil {
			t.Fatal(err)
		}
		moments = append(moments, from.Add(time.Hour))
	}
	return dir, moments
}

func TestCOSDiskModeServesAnOlderRestorePoint(t *testing.T) {
	dir, moments := undoBackup(t)
	for i, m := range moments[:len(moments)-1] {
		stopped := false
		withFakeDiskRepoAny(t, &undoRepo{dir: dir, moment: moments[len(moments)-1], stopped: &stopped})
		out := filepath.Join(t.TempDir(), "out.img")
		want, err := replication.RestoreDisk(filepath.Join(dir, "disk.img"), m.Add(10*time.Minute), out)
		if err != nil {
			t.Fatal(err)
		}
		expect, _ := os.ReadFile(out)
		client, base, token, exit, cancel, _, errb := serveAt(t, m.Add(10*time.Minute))
		if base == "" {
			t.Fatalf("restore point %d: exited with %d: %s", i, <-exit, errb.String())
		}
		src, err := cosdisk.NewRemoteSource(context.Background(), client, base+"/disk", token)
		if err != nil {
			t.Fatal(err)
		}
		if !src.Moment().Equal(want.Moment) || src.Size() != int64(len(expect)) {
			t.Fatalf("restore point %d: moment %v size %d; want %v %d", i, src.Moment(), src.Size(), want.Moment, len(expect))
		}
		got := make([]byte, len(expect))
		if n, rerr := src.ReadRange(context.Background(), got, 0); rerr != nil || n != len(expect) || !bytes.Equal(got, expect) {
			t.Fatalf("restore point %d: ReadRange = %d, %v", i, n, rerr)
		}
		cancel()
		<-exit
	}
}

func TestCOSDiskModeRefusesARestorePointOlderThanTheUndoFiles(t *testing.T) {
	dir, moments := undoBackup(t)
	stopped := false
	withFakeDiskRepoAny(t, &undoRepo{dir: dir, moment: moments[len(moments)-1], stopped: &stopped})
	_, base, _, exit, _, term, errb := serveAt(t, moments[0].Add(-time.Hour))
	if base != "" {
		t.Fatal("a restore point older than every undo file must not be served")
	}
	if code := <-exit; code != 1 {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(term)
	if !strings.Contains(string(b), "restore point") || strings.Contains(strings.ToLower(string(b)), "moment") {
		t.Errorf("termination log = %q (%s)", b, errb.String())
	}
	if !stopped {
		t.Error("the Kopia mount was not stopped")
	}
}

func TestCOSDiskModeIgnoresTheFileListOfAnotherRestorePoint(t *testing.T) {
	dir, moments := undoBackup(t)
	if err := os.WriteFile(filepath.Join(dir, ".filelist.json.gz"), []byte("list"), 0o660); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		at   time.Time
		want int
	}{{time.Time{}, http.StatusOK}, {moments[1].Add(time.Minute), http.StatusNotFound}} {
		stopped := false
		withFakeDiskRepoAny(t, &undoRepo{dir: dir, moment: moments[len(moments)-1], stopped: &stopped})
		client, base, token, exit, cancel, _, _ := serveAt(t, c.at)
		req, _ := http.NewRequest(http.MethodGet, base+cosdisk.FileListPath, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("at %v: file list status %d, want %d", c.at, resp.StatusCode, c.want)
		}
		cancel()
		<-exit
	}
}

func withFakeDiskRepoAny(t *testing.T, f diskRepo) {
	t.Helper()
	old := openDiskRepo
	openDiskRepo = func(context.Context, cosbrowse.Source, string, string, io.Writer) (diskRepo, error) { return f, nil }
	t.Cleanup(func() { openDiskRepo = old })
}
