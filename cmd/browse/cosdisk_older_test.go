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

// olderRepo is a fake repository whose export holds a backup folder snapshot
// (disk.img and the file list at the export's restore point) and, unless
// noMoments is set, a moments snapshot with one folder for each restore point,
// each with a full disk.img. Both are served like the Kopia WebDAV server: files
// with ranges, and PROPFIND for the root of the moments snapshot.
type olderRepo struct {
	backupDir  string
	momentsDir string
	moment     time.Time
	noMoments  bool
	// propfindStatus, if set, is what the moments snapshot answers to PROPFIND.
	propfindStatus int
	// momentsPropfind is the same for the moments snapshot only.
	momentsPropfind int
	stopped         *int
	mounted         []string
}

func (f *olderRepo) Snapshots(context.Context) ([]cosbrowse.Snapshot, error) {
	snaps := []cosbrowse.Snapshot{{
		ID: "k1234", SourcePath: "/share0/backup/pol/pvcs/" + testVolume, Volume: testVolume,
		Moment: f.moment, Root: "kroot",
	}}
	if !f.noMoments {
		snaps = append(snaps, cosbrowse.Snapshot{
			ID: "kmoments", SourcePath: "/share0/backup/pol/.moments/pvcs/" + testVolume, Volume: testVolume,
			Moment: f.moment, Root: "kmroot",
		})
	}
	return snaps, nil
}

func (f *olderRepo) StartWebDAV(_ context.Context, id string, _ func() (int, bool), _ time.Duration) (*cosbrowse.WebDAV, error) {
	f.mounted = append(f.mounted, id)
	dir := f.backupDir
	if id == "kmoments" {
		dir = f.momentsDir
	}
	files := http.FileServer(http.Dir(dir))
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PROPFIND" {
			files.ServeHTTP(w, r)
			return
		}
		if st := f.propfindStatus; st != 0 || (id == "kmoments" && f.momentsPropfind != 0) {
			if st == 0 {
				st = f.momentsPropfind
			}
			w.WriteHeader(st)
			return
		}
		// Like Kopia's WebDAV server, a path that is not there answers 405.
		listed := dir
		if r.URL.Path != "/" {
			listed = filepath.Join(dir, filepath.FromSlash(r.URL.Path))
		}
		entries, err := os.ReadDir(listed)
		if err != nil {
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><D:multistatus xmlns:D="DAV:"><D:response><D:href>/</D:href></D:response>`)
		for _, e := range entries {
			fmt.Fprintf(&b, `<D:response><D:href>%s%s</D:href></D:response>`, r.URL.Path, e.Name())
		}
		b.WriteString(`</D:multistatus>`)
		w.WriteHeader(http.StatusMultiStatus)
		_, _ = io.WriteString(w, b.String())
	}))
	return cosbrowse.NewWebDAV(ts.URL+"/disk.img", func() { ts.Close(); *f.stopped++ }), nil
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

// olderBackup makes the folders of a backup of four restore points, one hour
// apart from start: the backup folder holds the newest disk, and the moments
// folder holds a full disk.img in a folder for each restore point (including the
// newest, as the snapshot pool does). Each disk is different and a different
// size.
func olderBackup(t *testing.T, start time.Time) (backup, moments string, points []time.Time, disks [][]byte) {
	t.Helper()
	backup, moments = t.TempDir(), t.TempDir()
	for i := 0; i < 4; i++ {
		at := start.Add(time.Duration(i) * time.Hour)
		points = append(points, at)
		d := bytes.Repeat([]byte{byte(i + 1)}, (3+i%2)<<16)
		disks = append(disks, d)
		dir := filepath.Join(moments, at.UTC().Format(cosdisk.RestorePointFolderLayout))
		if err := os.MkdirAll(dir, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "disk.img"), d, 0o660); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(backup, "disk.img"), disks[3], 0o660); err != nil {
		t.Fatal(err)
	}
	return
}

func newOlderRepo(t *testing.T, start time.Time) (*olderRepo, []time.Time, [][]byte) {
	t.Helper()
	backup, moments, points, disks := olderBackup(t, start)
	f := &olderRepo{backupDir: backup, momentsDir: moments, moment: points[3], stopped: new(int)}
	withFakeDiskRepoAny(t, f)
	return f, points, disks
}

var firstPoint = time.Date(2026, 10, 22, 7, 15, 0, 0, time.UTC)

func readDisk(t *testing.T, client *http.Client, base, token string) *cosdisk.RemoteSource {
	t.Helper()
	src, err := cosdisk.NewRemoteSource(context.Background(), client, base+"/disk", token)
	if err != nil {
		t.Fatal(err)
	}
	return src
}

func TestCOSDiskModeServesAnOlderRestorePointFromItsFolder(t *testing.T) {
	for i := 0; i < 3; i++ {
		f, points, disks := newOlderRepo(t, firstPoint)
		client, base, token, exit, cancel, _, errb := serveAt(t, points[i].Add(10*time.Minute))
		if base == "" {
			t.Fatalf("restore point %d: exited with %d: %s", i, <-exit, errb.String())
		}
		src := readDisk(t, client, base, token)
		if !src.Moment().Equal(points[i]) || src.Size() != int64(len(disks[i])) {
			t.Fatalf("restore point %d: moment %v size %d; want %v %d", i, src.Moment(), src.Size(), points[i], len(disks[i]))
		}
		got := make([]byte, len(disks[i]))
		if n, rerr := src.ReadRange(context.Background(), got, 0); rerr != nil || n != len(got) || !bytes.Equal(got, disks[i]) {
			t.Fatalf("restore point %d: ReadRange = %d, %v", i, n, rerr)
		}
		if len(f.mounted) != 1 || f.mounted[0] != "kmoments" {
			t.Errorf("restore point %d: mounted %v; want only the moments snapshot", i, f.mounted)
		}
		cancel()
		<-exit
		if *f.stopped != 1 {
			t.Errorf("restore point %d: %d Kopia mounts stopped, want 1", i, *f.stopped)
		}
	}
}

func TestCOSDiskModeServesTheNewestRestorePointFromTheBackupSnapshot(t *testing.T) {
	f, points, disks := newOlderRepo(t, firstPoint)
	client, base, token, exit, cancel, _, errb := serveAt(t, time.Time{})
	if base == "" {
		t.Fatalf("exited with %d: %s", <-exit, errb.String())
	}
	defer func() { cancel(); <-exit }()
	src := readDisk(t, client, base, token)
	if !src.Moment().Equal(points[3]) || src.Size() != int64(len(disks[3])) {
		t.Errorf("moment %v size %d", src.Moment(), src.Size())
	}
	if len(f.mounted) != 1 || f.mounted[0] != "k1234" {
		t.Errorf("mounted %v; the moments snapshot is not needed to read the newest", f.mounted)
	}
}

func TestCOSDiskModeRefusesARestorePointOlderThanEveryFolder(t *testing.T) {
	f, points, _ := newOlderRepo(t, firstPoint)
	_, base, _, exit, _, term, errb := serveAt(t, points[0].Add(-time.Hour))
	if base != "" {
		t.Fatal("a restore point older than every folder must not be served")
	}
	if code := <-exit; code != 1 {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(term)
	if !strings.Contains(string(b), "restore point") || strings.Contains(strings.ToLower(string(b)), "moment") {
		t.Errorf("termination log = %q (%s)", b, errb.String())
	}
	if *f.stopped != 2 {
		t.Errorf("%d Kopia mounts stopped, want 2 (folders, then the backup snapshot for undo files)", *f.stopped)
	}
}

func TestCOSDiskModeRefusesAnOlderRestorePointWhenTheExportHasNoMomentsSnapshot(t *testing.T) {
	f, points, _ := newOlderRepo(t, firstPoint)
	f.noMoments = true
	_, base, _, exit, _, term, _ := serveAt(t, points[1])
	if base != "" {
		t.Fatal("without a moments snapshot only the newest restore point exists")
	}
	if code := <-exit; code != 1 {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(term)
	if !strings.Contains(string(b), "no restore point in COS") {
		t.Errorf("termination log = %q", b)
	}
	if len(f.mounted) != 1 || f.mounted[0] != "k1234" {
		t.Errorf("mounted %v; only the backup snapshot is tried, for undo files", f.mounted)
	}
}

func TestCOSDiskModeRefusesAnOlderRestorePointWhenKopiaCannotListTheSnapshot(t *testing.T) {
	f, points, _ := newOlderRepo(t, firstPoint)
	f.propfindStatus = http.StatusMethodNotAllowed
	_, base, _, exit, _, term, _ := serveAt(t, points[1])
	if base != "" {
		t.Fatal("an unlistable snapshot has no older restore point")
	}
	<-exit
	b, _ := os.ReadFile(term)
	if !strings.Contains(string(b), "no restore point in COS") {
		t.Errorf("termination log = %q", b)
	}
}

func TestCOSDiskModeIgnoresTheFileListOfAnotherRestorePoint(t *testing.T) {
	for _, c := range []struct {
		older bool
		want  int
	}{{false, http.StatusOK}, {true, http.StatusNotFound}} {
		f, points, _ := newOlderRepo(t, firstPoint)
		if err := os.WriteFile(filepath.Join(f.backupDir, ".filelist.json.gz"), []byte("list"), 0o660); err != nil {
			t.Fatal(err)
		}
		at := time.Time{}
		if c.older {
			at = points[1].Add(time.Minute)
		}
		client, base, token, exit, cancel, _, _ := serveAt(t, at)
		req, _ := http.NewRequest(http.MethodGet, base+cosdisk.FileListPath, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != c.want {
			t.Errorf("older=%v: file list status %d, want %d", c.older, resp.StatusCode, c.want)
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

func listPoints(t *testing.T, client *http.Client, base, token string) []time.Time {
	t.Helper()
	got, err := cosdisk.FetchRestorePoints(context.Background(), client, base+cosdisk.RestorePointsPath, token)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// A session at the newest restore point mounts the moments snapshot only when
// the restore points are asked for, and only once.
func TestCOSDiskModeListsTheRestorePointsOfTheExport(t *testing.T) {
	f, points, _ := newOlderRepo(t, firstPoint)
	client, base, token, exit, cancel, _, errb := serveAt(t, time.Time{})
	if base == "" {
		t.Fatalf("exited with %d: %s", <-exit, errb.String())
	}
	defer func() { cancel(); <-exit }()
	if len(f.mounted) != 1 {
		t.Fatalf("mounted %v before the listing; want only the backup snapshot", f.mounted)
	}
	for round := 0; round < 2; round++ {
		got := listPoints(t, client, base, token)
		if len(got) != len(points) {
			t.Fatalf("restore points = %v, want %v", got, points)
		}
		for i := range points {
			if !got[i].Equal(points[i]) {
				t.Errorf("restore point %d = %v, want %v", i, got[i], points[i])
			}
		}
	}
	if len(f.mounted) != 2 || f.mounted[1] != "kmoments" {
		t.Errorf("mounted %v; want the moments snapshot once", f.mounted)
	}
}

func TestCOSDiskModeListsTheRestorePointsOfAnOlderSession(t *testing.T) {
	f, points, _ := newOlderRepo(t, firstPoint)
	client, base, token, exit, cancel, _, errb := serveAt(t, points[1])
	if base == "" {
		t.Fatalf("exited with %d: %s", <-exit, errb.String())
	}
	defer func() { cancel(); <-exit }()
	if got := listPoints(t, client, base, token); len(got) != len(points) {
		t.Errorf("restore points = %v, want %v", got, points)
	}
	if len(f.mounted) != 2 {
		t.Errorf("mounted %v; the moments snapshot is reused and the backup snapshot is mounted once for its undo files", f.mounted)
	}
}

func TestCOSDiskModeListsOnlyTheOwnRestorePointWithoutAMomentsSnapshot(t *testing.T) {
	for _, mode := range []string{"none", "405"} {
		f, points, _ := newOlderRepo(t, firstPoint)
		switch mode {
		case "none":
			f.noMoments = true
		default:
			f.propfindStatus = http.StatusMethodNotAllowed
		}
		client, base, token, exit, cancel, _, errb := serveAt(t, time.Time{})
		if base == "" {
			t.Fatalf("%s: exited with %d: %s", mode, <-exit, errb.String())
		}
		got := listPoints(t, client, base, token)
		if len(got) != 1 || !got[0].Equal(points[3]) {
			t.Errorf("%s: restore points = %v, want only %v", mode, got, points[3])
		}
		cancel()
		<-exit
	}
}

// A folder is named in whole seconds and the export's own restore point keeps its
// nanoseconds: the two are one restore point in the listing.
func TestCOSDiskModeListsARestorePointOnceWhenTheFolderAndTheExportAgree(t *testing.T) {
	f2, points, _ := newOlderRepo(t, firstPoint)
	f2.moment = points[3].Add(500 * time.Millisecond)
	client, base, token, exit, cancel, _, errb := serveAt(t, time.Time{})
	if base == "" {
		t.Fatalf("exited with %d: %s", <-exit, errb.String())
	}
	defer func() { cancel(); <-exit }()
	got := listPoints(t, client, base, token)
	if len(got) != 4 || !got[3].Equal(f2.moment) {
		t.Errorf("restore points = %v; want 4 with the export's own last", got)
	}
}

// Restore points keep nanoseconds, but a person names one in whole seconds, as
// the listing prints it. Asking for that second must give that restore point.
func TestCOSDiskModeServesARestorePointNamedInWholeSeconds(t *testing.T) {
	_, points, _ := newOlderRepo(t, firstPoint.Add(500*time.Millisecond))
	// The listing prints whole seconds; the folder is named by them too.
	named := points[1].Truncate(time.Second)
	client, base, token, exit, cancel, _, errb := serveAt(t, named)
	if base == "" {
		t.Fatalf("exited with %d: %s", <-exit, errb.String())
	}
	defer func() { cancel(); <-exit }()
	if got := readDisk(t, client, base, token).Moment(); !got.Equal(named) {
		t.Errorf("served restore point %v, want %v", got, named)
	}
}

type wholeCopy struct{ data []byte }

func (c wholeCopy) Size() int64 { return int64(len(c.data)) }
func (c wholeCopy) Walk(fn func(qcow2.Extent) error) error {
	return fn(qcow2.Extent{Length: int64(len(c.data)), Kind: qcow2.Data})
}
func (c wholeCopy) DataReader(e qcow2.Extent) io.Reader {
	return bytes.NewReader(c.data[e.Offset : e.Offset+e.Length])
}

// undoBackup makes a backup folder of a running VM's disk: disk.img at the
// newest restore point and an undo file in .repl-undo for each earlier one. It
// returns the restore points and what the disk held at each (as a whole-disk
// restore gives it).
func undoBackup(t *testing.T, start time.Time) (dir string, points []time.Time, disks [][]byte) {
	t.Helper()
	dir = t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "disk.img"), bytes.Repeat([]byte{1}, 3<<16), 0o660); err != nil {
		t.Fatal(err)
	}
	points = []time.Time{start}
	for i := 2; i <= 4; i++ {
		from := points[len(points)-1]
		if _, err := replication.ApplyCopy(wholeCopy{bytes.Repeat([]byte{byte(i)}, (3+i%2)<<16)}, filepath.Join(dir, "disk.img"),
			replication.ApplyOptions{From: from, To: from.Add(time.Hour), Full: true}); err != nil {
			t.Fatal(err)
		}
		points = append(points, from.Add(time.Hour))
	}
	for i, m := range points {
		out := filepath.Join(t.TempDir(), "out.img")
		if _, err := replication.RestoreDisk(filepath.Join(dir, "disk.img"), m.Add(10*time.Minute), out); err != nil {
			t.Fatal(err)
		}
		b, err := os.ReadFile(out)
		if err != nil {
			t.Fatal(err)
		}
		disks = append(disks, b)
		_ = i
	}
	return dir, points, disks
}

func serveDisk(t *testing.T, f *olderRepo, at time.Time, want time.Time, wantDisk []byte) {
	t.Helper()
	client, base, token, exit, cancel, _, errb := serveAt(t, at)
	if base == "" {
		t.Fatalf("at %v: exited with %d: %s", at, <-exit, errb.String())
	}
	defer func() { cancel(); <-exit }()
	src := readDisk(t, client, base, token)
	if !src.Moment().Equal(want) || src.Size() != int64(len(wantDisk)) {
		t.Fatalf("at %v: moment %v size %d; want %v %d", at, src.Moment(), src.Size(), want, len(wantDisk))
	}
	got := make([]byte, len(wantDisk))
	if n, err := src.ReadRange(context.Background(), got, 0); err != nil || n != len(got) || !bytes.Equal(got, wantDisk) {
		t.Fatalf("at %v: ReadRange = %d, %v", at, n, err)
	}
}

// A running VM: the backup snapshot holds undo files and the export has no
// restore point folders.
func TestCOSDiskModeServesAnOlderRestorePointFromUndoFiles(t *testing.T) {
	dir, points, disks := undoBackup(t, firstPoint)
	for i := 0; i < 3; i++ {
		f := &olderRepo{backupDir: dir, noMoments: true, moment: points[3], stopped: new(int)}
		withFakeDiskRepoAny(t, f)
		serveDisk(t, f, points[i].Add(10*time.Minute), points[i], disks[i])
		if *f.stopped != 1 {
			t.Errorf("restore point %d: %d Kopia mounts stopped, want 1", i, *f.stopped)
		}
	}
}

// Both forms in one export: a time that a folder reaches uses the folder; an
// earlier one falls back to the undo files.
func TestCOSDiskModeUsesFoldersFirstThenUndoFiles(t *testing.T) {
	dir, points, disks := undoBackup(t, firstPoint)
	moments := t.TempDir()
	folderDisk := bytes.Repeat([]byte{9}, 2<<16) // marks a read from the folder
	for _, m := range points[2:] {
		fd := filepath.Join(moments, m.Format(cosdisk.RestorePointFolderLayout))
		if err := os.MkdirAll(fd, 0o750); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(fd, "disk.img"), folderDisk, 0o660); err != nil {
			t.Fatal(err)
		}
	}
	f := &olderRepo{backupDir: dir, momentsDir: moments, moment: points[3], stopped: new(int)}
	withFakeDiskRepoAny(t, f)
	serveDisk(t, f, points[2].Add(10*time.Minute), points[2], folderDisk)
	serveDisk(t, f, points[1].Add(10*time.Minute), points[1], disks[1])
	serveDisk(t, f, points[0].Add(10*time.Minute), points[0], disks[0])
}

func TestCOSDiskModeRefusesAnOlderRestorePointWhenNeitherFormReachesBack(t *testing.T) {
	dir, points, _ := undoBackup(t, firstPoint)
	moments := t.TempDir() // a moments snapshot with no folder at all
	f := &olderRepo{backupDir: dir, momentsDir: moments, moment: points[3], stopped: new(int)}
	withFakeDiskRepoAny(t, f)
	_, base, _, exit, _, term, _ := serveAt(t, points[0].Add(-time.Hour))
	if base != "" {
		t.Fatal("a restore point before every folder and every undo file must not be served")
	}
	<-exit
	b, _ := os.ReadFile(term)
	if !strings.Contains(string(b), "no restore point in COS") {
		t.Errorf("termination log = %q", b)
	}
	if *f.stopped != 2 {
		t.Errorf("%d Kopia mounts stopped, want 2 (folders and backup)", *f.stopped)
	}
}

// The list of a session is the union of the folders, the undo files and the
// export's own restore point, without duplicates.
func TestCOSDiskModeListsFoldersAndUndoFilesTogether(t *testing.T) {
	dir, points, _ := undoBackup(t, firstPoint)
	moments := t.TempDir()
	extra := points[1].Add(20 * time.Minute) // a folder that no undo file knows
	for _, m := range []time.Time{points[2], extra} {
		fd := filepath.Join(moments, m.Format(cosdisk.RestorePointFolderLayout))
		if err := os.MkdirAll(fd, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	f := &olderRepo{backupDir: dir, momentsDir: moments, moment: points[3], stopped: new(int)}
	withFakeDiskRepoAny(t, f)
	client, base, token, exit, cancel, _, errb := serveAt(t, time.Time{})
	if base == "" {
		t.Fatalf("exited with %d: %s", <-exit, errb.String())
	}
	defer func() { cancel(); <-exit }()
	got := listPoints(t, client, base, token)
	want := []time.Time{points[0], points[1], extra, points[2], points[3]}
	if len(got) != len(want) {
		t.Fatalf("restore points = %v, want %v", got, want)
	}
	for i := range want {
		if !got[i].Equal(want[i]) {
			t.Errorf("restore point %d = %v, want %v", i, got[i], want[i])
		}
	}
}

// A moments snapshot that cannot be listed does not hide the undo files.
func TestCOSDiskModeListsUndoFilesWhenTheMomentsSnapshotCannotBeListed(t *testing.T) {
	dir, points, _ := undoBackup(t, firstPoint)
	f := &olderRepo{backupDir: dir, momentsDir: t.TempDir(), momentsPropfind: http.StatusMethodNotAllowed, moment: points[3], stopped: new(int)}
	withFakeDiskRepoAny(t, f)
	client, base, token, exit, cancel, _, errb := serveAt(t, time.Time{})
	if base == "" {
		t.Fatalf("exited with %d: %s", <-exit, errb.String())
	}
	defer func() { cancel(); <-exit }()
	if got := listPoints(t, client, base, token); len(got) != len(points) {
		t.Errorf("restore points = %v, want %v", got, points)
	}
}
