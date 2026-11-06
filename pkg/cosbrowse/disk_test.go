package cosbrowse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestChooseBackup_NewestExportByDefault(t *testing.T) {
	f := newFakeRepo()
	addExport(f, "pol", volA, at(20, 9), at(20, 9))
	addExport(f, "pol", volA, at(21, 9), at(20, 9), at(21, 9))
	addExport(f, "pol", volB, at(22, 9))
	snaps, _ := f.Snapshots(context.Background())
	got, err := ChooseBackup(snaps, Params{Policy: "pol", Volume: volA})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Moment.Equal(at(21, 9)) || !got.Newest {
		t.Errorf("pick = %+v", got)
	}
	if got.Snapshot.SourcePath != backupPath("pol", volA) || !ValidOID(got.Snapshot.ID) {
		t.Errorf("snapshot = %+v", got.Snapshot)
	}
}

// Phase 6d: a time before the export's restore point is no longer refused here.
// The export that covers the time (the oldest one at or after it) is chosen, and
// the folders of its moments snapshot or its undo files decide whether the restore point exists.
func TestChooseBackup_AnOlderRestorePointPicksTheCoveringExport(t *testing.T) {
	f := newFakeRepo()
	addExport(f, "pol", volA, at(20, 9))
	addExport(f, "pol", volA, at(21, 9), at(20, 9), at(21, 9))
	snaps, _ := f.Snapshots(context.Background())
	got, err := ChooseBackup(snaps, Params{Policy: "pol", Volume: volA, At: at(20, 12)})
	if err != nil || !got.Moment.Equal(at(21, 9)) || !got.Newest {
		t.Fatalf("got %+v, %v; want the export of 21 Oct", got, err)
	}
	// Before every export: the oldest export covers it, and its folders say.
	got, err = ChooseBackup(snaps, Params{Policy: "pol", Volume: volA, At: at(19, 0)})
	if err != nil || !got.Moment.Equal(at(20, 9)) || got.Newest {
		t.Errorf("got %+v, %v; want the export of 20 Oct", got, err)
	}
	// A time after the newest export reads the newest.
	if got, err := ChooseBackup(snaps, Params{Policy: "pol", Volume: volA, At: at(25, 0)}); err != nil || !got.Moment.Equal(at(21, 9)) {
		t.Errorf("got %+v, %v", got, err)
	}
}

func TestChooseBackup_CarriesTheMomentsSnapshotOfTheExport(t *testing.T) {
	f := newFakeRepo()
	addExport(f, "pol", volA, at(20, 9))
	addExport(f, "pol", volA, at(21, 9), at(20, 9), at(21, 9))
	snaps, _ := f.Snapshots(context.Background())
	got, err := ChooseBackup(snaps, Params{Policy: "pol", Volume: volA})
	if err != nil || got.Moments == nil {
		t.Fatalf("got %+v, %v; want the moments snapshot of the newest export", got, err)
	}
	if got.Moments.SourcePath != momentsPath("pol", volA) || !ValidOID(got.Moments.ID) {
		t.Errorf("moments snapshot = %+v", got.Moments)
	}
	// An export without a moments snapshot has none, and that is not an error.
	got, err = ChooseBackup(snaps, Params{Policy: "pol", Volume: volA, At: at(19, 0)})
	if err != nil || got.Moments != nil {
		t.Errorf("got %+v, %v; want no moments snapshot", got, err)
	}
}

func TestChooseBackup_NoBackupAndOtherVolumes(t *testing.T) {
	f := newFakeRepo()
	addExport(f, "pol", volB, at(22, 9))
	snaps, _ := f.Snapshots(context.Background())
	if _, err := ChooseBackup(snaps, Params{Policy: "pol", Volume: volA}); err == nil {
		t.Fatal("a volume with no backup must be an error")
	}
}

func TestParseListenPorts_FindsTheLoopbackListeners(t *testing.T) {
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:1FA4 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 11111 1 0000000000000000 100 0 0 10 0
   1: 0100007F:C350 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 22222 1 0000000000000000 100 0 0 10 0
   2: 0100007F:C351 0100007F:C350 01 00000000:00000000 00:00000000 00000000     0        0 33333 1 0000000000000000 100 0 0 10 0
   3: 0100007F:C352 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 44444 1 0000000000000000 100 0 0 10 0
`
	got := parseListenPorts(tcp)
	if len(got) != 2 || got[0] != 0xC350 || got[1] != 0xC352 {
		t.Fatalf("ports = %v; want [50000 50002]", got)
	}
	if got := parseListenPorts("  sl  local_address\n"); len(got) != 0 {
		t.Errorf("no listener should give no port, got %v", got)
	}
}

// A second mount in the same pod must get its own port, not the first mount's.
func TestNewListenPort_SkipsPortsThatWereOpenBefore(t *testing.T) {
	known := map[int]bool{50000: true}
	if p, ok := newListenPort([]int{50000}, known); ok {
		t.Errorf("got %d; the only listener was there before", p)
	}
	if p, ok := newListenPort([]int{50000, 50002}, known); !ok || p != 50002 {
		t.Errorf("got %d, %v; want 50002", p, ok)
	}
	if p, ok := newListenPort([]int{50002}, nil); !ok || p != 50002 {
		t.Errorf("got %d, %v; want 50002 with nothing known", p, ok)
	}
}

func TestStartWebDAV_RunsKopiaMountOnTheSnapshotWithItsOwnEnvironment(t *testing.T) {
	dir := t.TempDir()
	rec := filepath.Join(dir, "args")
	script := filepath.Join(dir, "kopia")
	body := "#!/bin/sh\necho \"$@\" > " + rec + "\nenv > " + rec + ".env\nexec sleep 30\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil { //nolint:gosec // a test script
		t.Fatal(err)
	}
	k := NewKopia(KopiaConfig{Binary: script, StateDir: filepath.Join(dir, "state"), AccessKey: "AK", SecretKey: "SK", Password: "PW"})
	w, err := k.StartWebDAV(context.Background(), "k1234abcd", func() (int, bool) { return 40123, true }, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Stop()
	if w.URL != "http://127.0.0.1:40123/disk.img" {
		t.Errorf("url = %q", w.URL)
	}
	var args []byte
	for i := 0; i < 50 && len(args) == 0; i++ {
		args, _ = os.ReadFile(rec)
		time.Sleep(20 * time.Millisecond)
	}
	for _, want := range []string{"mount k1234abcd * --webdav --no-browse"} {
		if !strings.Contains(string(args), want) {
			t.Errorf("args = %q; want %q", args, want)
		}
	}
	env, _ := os.ReadFile(rec + ".env")
	if strings.Contains(string(env), "HOME=/Users") {
		t.Error("the helper's own environment must not reach Kopia")
	}
}

func TestStartWebDAV_RefusesABadSnapshotIDAndGivesUpWithoutAPort(t *testing.T) {
	k := NewKopia(KopiaConfig{Binary: "/bin/true", StateDir: t.TempDir()})
	for _, id := range []string{"", "-x", "a b", "a;b", "../x"} {
		if _, err := k.StartWebDAV(context.Background(), id, func() (int, bool) { return 1, true }, time.Second); err == nil {
			t.Errorf("snapshot id %q was accepted", id)
		}
	}
	dir := t.TempDir()
	script := filepath.Join(dir, "kopia")
	_ = os.WriteFile(script, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755) //nolint:gosec // a test script
	k = NewKopia(KopiaConfig{Binary: script, StateDir: dir})
	if _, err := k.StartWebDAV(context.Background(), "k1", func() (int, bool) { return 0, false }, 300*time.Millisecond); err == nil {
		t.Error("want an error when no port shows up")
	}
}
