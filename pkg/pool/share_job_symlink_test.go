package pool

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A backup can hold symlinks (a tenant made them). The scratch copy must not
// keep any, or the restore Job would follow them to other files on the share.
func TestCOSRestoreScript_RemovesSymlinksFromTheScratchCopy(t *testing.T) {
	requireKopia(t)
	l := newLocalExport(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("other tenant"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(l.root, testBackupPath, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(l.root, testMomentsPath, "20261020T090000Z", "undo-link")); err != nil {
		t.Fatal(err)
	}
	if _, out, err := runScript(t, exportScript(l.params), l.termLog, l.env); err != nil {
		t.Fatalf("export: %v\n%s", err, out)
	}
	req := COSRestoreRequest{SourceVolume: testExportVolume, TargetVolume: testTargetVolume, Scratch: COSScratchName("t")}
	p := cosRestoreParams{exportParams: l.params, req: req}
	p.stateDir = t.TempDir()
	if _, out, err := runScript(t, cosRestoreScript(p), l.termLog, l.env); err != nil {
		t.Fatalf("restore: %v\n%s", err, out)
	}
	_ = filepath.Walk(filepath.Join(l.root, "backup", req.Scratch), func(path string, info os.FileInfo, err error) error {
		if err == nil && info.Mode()&os.ModeSymlink != 0 {
			t.Errorf("the scratch copy holds a symlink: %s", path)
		}
		return nil
	})
	if _, err := os.Stat(filepath.Join(l.root, "backup", req.Scratch, "pvcs", testTargetVolume, "disk.img")); err != nil {
		t.Errorf("the real files must stay: %v", err)
	}
}

func TestRestoreBackup_ScriptRefusesSymlinkedSources(t *testing.T) {
	r, c := newShareJobTestRunner()
	r.SetDriverImage("registry.example/driver:test")
	if _, _, err := r.RestoreBackup(context.Background(), "rs-1", testShareA, testBackupPath, testMomentsPath, testShareB, "/pvcs/"+testTargetVolume, time.Time{}, nil); err != nil {
		t.Fatal(err)
	}
	script := jobScript(getShareJob(t, c, "rs-1"))
	for _, want := range []string{`-L "$src"`, `-L "$src/disk.img"`, `-L "$pick"`} {
		if !strings.Contains(script, want) {
			t.Errorf("script does not refuse a symlink with %s:\n%s", want, script)
		}
	}
}
