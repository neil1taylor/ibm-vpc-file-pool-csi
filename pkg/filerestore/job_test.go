//go:build unix

package filerestore

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

func TestParsePaths(t *testing.T) {
	good := "/var/www/html/index.php\n\n  \n/etc/my\\x0aname\n/\n"
	got, err := ParsePaths(strings.NewReader(good))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 3 || string(got[1][1]) != "my\nname" || len(got[2]) != 0 {
		t.Fatalf("paths %q", got)
	}
	for _, bad := range []string{"/a/../b\n", "/a/\\xZZ\n", "/a\\\n", ""} {
		if _, err := ParsePaths(strings.NewReader(bad)); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
}

func fakeDeps(tr browse.Tree, moment string) jobDeps {
	return jobDeps{
		choose: func(latest, moments string, at time.Time) (string, browse.Info, error) {
			return latest, browse.Info{Moment: moment, Latest: moment == ""}, nil
		},
		open: func(string) (browse.Tree, func() error, error) { return tr, func() error { return nil }, nil },
	}
}

func writePaths(t *testing.T, lines ...string) string {
	t.Helper()
	f := filepath.Join(t.TempDir(), "paths")
	mustWrite(t, f, strings.Join(lines, "\n")+"\n")
	return f
}

func ownerSpec(s JobSpec) JobSpec {
	u, g := int64(os.Getuid()), int64(os.Getgid())
	s.UID, s.GID = &u, &g
	return s
}

func TestRunJobMeasure(t *testing.T) {
	out, err := runJob(context.Background(), JobSpec{Mode: ModeMeasure, Latest: "/src"}, writePaths(t, "/var/www/html"), fakeDeps(sampleTree(), ""))
	if err != nil {
		t.Fatal(err)
	}
	if out.Bytes != 25 || out.Files != 3 {
		t.Fatalf("outcome %+v", out)
	}
}

func TestRunJobIntoAnExistingClaimMakesANamedFolderAndNeverReplacesOne(t *testing.T) {
	share, claim := claimDir(t)
	spec := ownerSpec(JobSpec{Mode: ModeFolder, Latest: "/src", Dest: share, Volume: volName, Staging: StagingName("uid-1"), Policy: "nightly", Permissions: "0755"})
	deps := fakeDeps(sampleTree(), "20261022T091500Z")
	out, err := runJob(context.Background(), spec, writePaths(t, "/var/www/html/index.php"), deps)
	if err != nil {
		t.Fatal(err)
	}
	if out.Folder != "restored-nightly-20261022T091500Z" || out.Moment != "2026-10-22T09:15:00Z" || out.Latest {
		t.Fatalf("outcome %+v", out)
	}
	if b, rerr := os.ReadFile(filepath.Join(claim, out.Folder, "var/www/html/index.php")); rerr != nil || string(b) != "<?php echo 1;" {
		t.Fatalf("file %q %v", b, rerr)
	}
	spec.Staging = StagingName("uid-2")
	out2, err := runJob(context.Background(), spec, writePaths(t, "/var/www/html/index.php"), deps)
	if err != nil || out2.Folder != "restored-nightly-20261022T091500Z-2" {
		t.Fatalf("second outcome %+v %v", out2, err)
	}
	entries, _ := os.ReadDir(claim)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".restoring-") {
			t.Fatalf("staging folder left: %s", e.Name())
		}
	}
}

func TestRunJobIntoANewClaimWritesTheVolumeFolder(t *testing.T) {
	share := t.TempDir()
	spec := ownerSpec(JobSpec{Mode: ModeNewClaim, Latest: "/src", Dest: share, Volume: "pvcs/pvc-0a1b2c3d-0a1b-0a1b-0a1b-0a1b2c3d4e5f", Policy: "p", Permissions: "0777"})
	out, err := runJob(context.Background(), spec, writePaths(t, "/var/www/html/secret.key"), fakeDeps(sampleTree(), ""))
	if err != nil {
		t.Fatal(err)
	}
	vol := filepath.Join(share, "pvcs", "pvc-0a1b2c3d-0a1b-0a1b-0a1b-0a1b2c3d4e5f")
	if b, err := os.ReadFile(filepath.Join(vol, "var/www/html/secret.key")); err != nil || string(b) != "s3cret" {
		t.Fatalf("file %q %v", b, err)
	}
	st, _ := os.Stat(vol)
	if st.Mode().Perm() != 0o777 || out.Folder != "" || !out.Latest {
		t.Fatalf("volume mode %v outcome %+v", st.Mode(), out)
	}
}

func TestRunJobFailureCarriesAMessageWithoutPathsAndLeavesNothing(t *testing.T) {
	share, claim := claimDir(t)
	spec := ownerSpec(JobSpec{Mode: ModeFolder, Latest: "/src", Dest: share, Volume: volName, Staging: StagingName("u"), Policy: "p"})
	out, err := runJob(context.Background(), spec, writePaths(t, "/no/such/secret-file-name"), fakeDeps(sampleTree(), ""))
	if err == nil || out.Error == "" {
		t.Fatalf("err %v outcome %+v", err, out)
	}
	if strings.Contains(out.Error, "secret-file-name") {
		t.Fatalf("the message names a path: %q", out.Error)
	}
	if entries, _ := os.ReadDir(claim); len(entries) != 0 {
		t.Fatalf("left in the claim: %v", entries)
	}
}

func TestRunJobRefusesAVMClaimAndSaysSo(t *testing.T) {
	share, claim := claimDir(t)
	mustWrite(t, filepath.Join(claim, "disk.img"), "x")
	spec := ownerSpec(JobSpec{Mode: ModeFolder, Latest: "/src", Dest: share, Volume: volName, Staging: StagingName("u"), Policy: "p"})
	out, err := runJob(context.Background(), spec, writePaths(t, "/var"), fakeDeps(sampleTree(), ""))
	if err == nil || !strings.Contains(out.Error, "VM disk") {
		t.Fatalf("err %v outcome %+v", err, out)
	}
}

func TestRunJobRefusesWhatDoesNotFit(t *testing.T) {
	share, claim := claimDir(t)
	spec := ownerSpec(JobSpec{Mode: ModeFolder, Latest: "/src", Dest: share, Volume: volName, Staging: StagingName("u"), Policy: "p", CapacityBytes: 5})
	out, err := runJob(context.Background(), spec, writePaths(t, "/var"), fakeDeps(sampleTree(), ""))
	if err == nil || !strings.Contains(out.Error, "not enough space") {
		t.Fatalf("err %v outcome %+v", err, out)
	}
	if entries, _ := os.ReadDir(claim); len(entries) != 0 {
		t.Fatalf("left in the claim: %v", entries)
	}
}

func TestBadJobSpecsAreRefused(t *testing.T) {
	cases := map[string]JobSpec{
		"no mode":         {Latest: "/src"},
		"unknown mode":    {Mode: "wipe", Latest: "/src"},
		"no source":       {Mode: ModeMeasure},
		"bad permissions": {Mode: ModeFolder, Latest: "/src", Dest: "/d", Staging: ".restoring-x", Policy: "p", Volume: "pvcs/pvc-0a1b2c3d-0a1b-0a1b-0a1b-0a1b2c3d4e5f", Permissions: "9999"},
		"no staging":      {Mode: ModeFolder, Latest: "/src", Dest: "/d", Volume: "pvcs/pvc-0a1b2c3d-0a1b-0a1b-0a1b-0a1b2c3d4e5f"},
		"no volume":       {Mode: ModeNewClaim, Latest: "/src", Dest: "/d"},
		"bad volume":      {Mode: ModeNewClaim, Latest: "/src", Dest: "/d", Volume: "pvcs/../.."},
		"bad moment":      {Mode: ModeMeasure, Latest: "/src", At: "yesterday"},
	}
	for name, spec := range cases {
		if _, err := runJob(context.Background(), spec, writePaths(t, "/a"), fakeDeps(sampleTree(), "")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestOutcomeFitsTheTerminationMessage(t *testing.T) {
	msg := EncodeOutcome(Outcome{Error: strings.Repeat("x", 10000)})
	if len(msg) > 4096 {
		t.Fatalf("message is %d bytes", len(msg))
	}
	var o Outcome
	if err := json.Unmarshal([]byte(msg), &o); err != nil || o.Error == "" {
		t.Fatalf("not valid JSON: %v", err)
	}
}
