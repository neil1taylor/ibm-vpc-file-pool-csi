package pool

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Retention: Kopia expires snapshots by count, not by age. The export sets a
// policy that keeps every snapshot, and the maintenance Job deletes the ones
// that are older than the keep time.

// snapshotMoments lists the moment tags of the snapshots in the local repository.
func snapshotMoments(t *testing.T, l *localExport) []string {
	t.Helper()
	list := exec.Command("kopia", "snapshot", "list", "--all", "--json")
	list.Env = append(os.Environ(), l.env...)
	list.Env = append(list.Env, "KOPIA_CONFIG_PATH="+filepath.Join(l.params.stateDir, "repository.config"),
		"KOPIA_CACHE_DIRECTORY="+filepath.Join(l.params.stateDir, "cache"), "KOPIA_LOG_DIR="+filepath.Join(l.params.stateDir, "logs"))
	out, err := list.CombinedOutput()
	if err != nil {
		t.Fatalf("kopia snapshot list: %v\n%s", err, out)
	}
	const tag = `"tag:moment":"`
	var moments []string
	for _, line := range strings.Split(string(out), "\n") {
		if i := strings.Index(line, tag); i >= 0 {
			rest := line[i+len(tag):]
			moments = append(moments, rest[:strings.Index(rest, `"`)])
		}
	}
	return moments
}

func TestExportScript_SetsAPolicyThatKeepsEverySnapshotBeforeSnapshotting(t *testing.T) {
	l := newLocalExport(t)
	script := exportScript(l.params)
	policy := strings.Index(script, "kopia policy set --global")
	snap := strings.Index(script, "kopia snapshot create")
	if policy < 0 || snap < 0 || policy > snap {
		t.Fatalf("the export must set the policy before it makes a snapshot:\n%s", script)
	}
	for _, want := range []string{"--keep-latest=", "--keep-hourly=0", "--keep-daily=0", "--keep-weekly=0", "--keep-monthly=0", "--keep-annual=0"} {
		if !strings.Contains(script, want) {
			t.Errorf("policy lacks %s:\n%s", want, script)
		}
	}
}

func TestMaintenanceScript_NoCutoffDeletesNothing(t *testing.T) {
	l := newLocalExport(t)
	if script := maintenanceScript(l.params); strings.Contains(script, "snapshot delete") {
		t.Errorf("without a cutoff the maintenance must not delete snapshots:\n%s", script)
	}
}

func TestMaintainCOSRepo_PassesTheCutoffToTheScript(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	repo := testExportRepo()
	repo.ExpireBefore = time.Date(2026, 9, 21, 9, 30, 15, 0, time.UTC)
	if _, err := r.MaintainCOSRepo(context.Background(), "mnt-1", repo); err != nil {
		t.Fatal(err)
	}
	script := jobScript(getShareJob(t, c, "mnt-1"))
	for _, want := range []string{"cutoff='2026-09-21T09:30:15Z'", "kopia snapshot delete"} {
		if !strings.Contains(script, want) {
			t.Errorf("script lacks %s:\n%s", want, script)
		}
	}
}

func TestMaintenanceScript_LocalRepository_DeletesOnlySnapshotsOlderThanTheCutoff(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("first export failed: %v\n%s", err, out)
	}
	time.Sleep(1100 * time.Millisecond)
	cutoff := time.Now().UTC()
	time.Sleep(1100 * time.Millisecond)
	l.params.moment = testExportMoment.Add(6 * time.Hour)
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("second export failed: %v\n%s", err, out)
	}
	if got := snapshotMoments(t, l); len(got) != 4 {
		t.Fatalf("snapshots before maintenance = %v, want 4 (2 exports of 2 folders)", got)
	}

	l.params.repo.ExpireBefore = cutoff
	_, out, err := runScript(t, maintenanceScript(l.params), l.termLog, l.env)
	if err != nil {
		t.Fatalf("maintenance failed: %v\n%s", err, out)
	}
	got := snapshotMoments(t, l)
	if len(got) != 2 {
		t.Fatalf("snapshots after maintenance = %v, want the 2 of the newer export", got)
	}
	for _, m := range got {
		if m != "2026-10-21T15:30:00Z" {
			t.Errorf("snapshot with moment %s survived; only the newer export (15:30) may", m)
		}
	}
	if !strings.Contains(out, "Finished full maintenance") {
		t.Errorf("maintenance did not finish:\n%s", out)
	}
}

func TestMaintenanceScript_LocalRepository_ACutoffInThePastKeepsEverything(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("export failed: %v\n%s", err, out)
	}
	l.params.repo.ExpireBefore = time.Now().UTC().Add(-48 * time.Hour)
	if _, out, err := runScript(t, maintenanceScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("maintenance failed: %v\n%s", err, out)
	}
	if got := snapshotMoments(t, l); len(got) != 2 {
		t.Errorf("snapshots = %v, want both kept", got)
	}
}

func TestExportScript_LocalRepository_ManySnapshotsAreNotExpiredByCount(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	for i := 0; i < 12; i++ { // Kopia's default policy keeps only the latest 10 of a source here
		l.params.moment = testExportMoment.Add(time.Duration(i) * time.Minute)
		if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
			t.Fatalf("export %d failed: %v\n%s", i, err, out)
		}
	}
	if got := snapshotMoments(t, l); len(got) != 24 {
		t.Errorf("snapshots = %d, want 24: nothing may expire by count", len(got))
	}
}
