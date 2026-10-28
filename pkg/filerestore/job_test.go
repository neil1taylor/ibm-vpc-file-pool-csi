//go:build unix

package filerestore

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
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

func cosSource() *cosbrowse.Source {
	return &cosbrowse.Source{
		Endpoint: "s3.eu-de.cloud-object-storage.appdomain.cloud", Bucket: "my-bucket", Prefix: "p/pol/", Policy: "pol",
		Volume:     "pvc-0a1b2c3d-0a1b-0a1b-0a1b-0a1b2c3d4e5f",
		KeyProtect: &cosbrowse.KeyProtect{Region: "eu-de", InstanceID: "i1", RootKeyID: "r1"},
	}
}

// A job that reads a backup in COS gets its tree from COS, with the time the
// controller chose, and never looks at the folders of a snapshot pool.
func TestRunJobReadsABackupInCOS(t *testing.T) {
	var gotSource cosbrowse.Source
	var gotAt time.Time
	closed := false
	deps := jobDeps{
		choose: func(string, string, time.Time) (string, browse.Info, error) {
			t.Error("a COS job looked for a folder on a share")
			return "", browse.Info{}, nil
		},
		open: func(string) (browse.Tree, func() error, error) {
			t.Error("a COS job opened a folder on a share")
			return nil, nil, nil
		},
		openCOS: func(_ context.Context, src cosbrowse.Source, at time.Time) (browse.Tree, browse.Info, func() error, error) {
			gotSource, gotAt = src, at
			return sampleTree(), browse.Info{Moment: "20261022T091500Z"}, func() error { closed = true; return nil }, nil
		},
	}
	spec := JobSpec{Mode: ModeMeasure, COS: cosSource(), At: "2026-10-22T10:00:00Z"}
	out, err := runJob(context.Background(), spec, writePaths(t, "/var/www/html"), deps)
	if err != nil {
		t.Fatal(err)
	}
	if out.Bytes != 25 || out.Files != 3 || out.Moment != "2026-10-22T09:15:00Z" {
		t.Fatalf("outcome %+v", out)
	}
	if gotSource.Volume != cosSource().Volume || !gotAt.Equal(time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)) || !closed {
		t.Errorf("source %+v, at %v, closed %v", gotSource, gotAt, closed)
	}
}

func TestRunJobFromCOSSaysWhyItCannotOpenTheBackup(t *testing.T) {
	deps := jobDeps{openCOS: func(context.Context, cosbrowse.Source, time.Time) (browse.Tree, browse.Info, func() error, error) {
		return nil, browse.Info{}, nil, errors.New("COS refused the key: check the COS key Secret")
	}}
	out, err := runJob(context.Background(), JobSpec{Mode: ModeMeasure, COS: cosSource()}, writePaths(t, "/a"), deps)
	if err == nil || !strings.Contains(out.Error, "COS refused the key") {
		t.Fatalf("err %v outcome %+v", err, out)
	}
}

func TestBadCOSJobSpecsAreRefused(t *testing.T) {
	bad := cosSource()
	bad.Volume = "../x"
	cases := map[string]JobSpec{
		"both sources":   {Mode: ModeMeasure, Latest: "/src", COS: cosSource()},
		"with moments":   {Mode: ModeMeasure, Moments: "/moments", COS: cosSource()},
		"a bad source":   {Mode: ModeMeasure, COS: bad},
		"a bad moment":   {Mode: ModeMeasure, COS: cosSource(), At: "yesterday"},
		"no mode at all": {COS: cosSource()},
	}
	deps := jobDeps{openCOS: func(context.Context, cosbrowse.Source, time.Time) (browse.Tree, browse.Info, func() error, error) {
		t.Error("a bad spec reached the repository")
		return nil, browse.Info{}, nil, errors.New("no")
	}}
	for name, spec := range cases {
		if _, err := runJob(context.Background(), spec, writePaths(t, "/a"), deps); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// A job that reads the file systems inside a VM disk gets its tree from the
// guest reader, with the disk in the backup folder and the time the controller
// chose.
func TestRunJobReadsAVMDisk(t *testing.T) {
	var gotDisk string
	var gotAt time.Time
	var gotGuest GuestSource
	closed := false
	deps := jobDeps{
		choose: func(string, string, time.Time) (string, browse.Info, error) {
			t.Error("a guest job looked for a moments folder")
			return "", browse.Info{}, nil
		},
		open: func(string) (browse.Tree, func() error, error) {
			t.Error("a guest job read the backup folder as files")
			return nil, nil, nil
		},
		openGuest: func(_ context.Context, disk string, g GuestSource, at time.Time) (browse.Tree, browse.Info, func() error, error) {
			gotDisk, gotGuest, gotAt = disk, g, at
			return sampleTree(), browse.Info{Moment: "20261022T091500Z", Guest: true}, func() error { closed = true; return nil }, nil
		},
	}
	spec := JobSpec{Mode: ModeMeasure, Latest: "/src", At: "2026-10-22T10:00:00Z",
		Guest: &GuestSource{Socket: "/tmp/disk.sock", Appliance: "/opt/guestfs-appliance", Consistency: "CrashConsistent"}}
	out, err := runJob(context.Background(), spec, writePaths(t, "/var/www/html"), deps)
	if err != nil {
		t.Fatal(err)
	}
	if out.Bytes != 25 || out.Files != 3 || out.Moment != "2026-10-22T09:15:00Z" {
		t.Fatalf("outcome %+v", out)
	}
	if gotDisk != "/src/disk.img" || gotGuest.Consistency != "CrashConsistent" || gotGuest.Socket != "/tmp/disk.sock" ||
		!gotAt.Equal(time.Date(2026, 10, 22, 10, 0, 0, 0, time.UTC)) || !closed {
		t.Errorf("disk %q, guest %+v, at %v, closed %v", gotDisk, gotGuest, gotAt, closed)
	}
}

func TestRunJobFromAVMDiskSaysWhyItCannotOpenTheDisk(t *testing.T) {
	deps := jobDeps{openGuest: func(context.Context, string, GuestSource, time.Time) (browse.Tree, browse.Info, func() error, error) {
		return nil, browse.Info{}, nil, errors.New("the reader could not list the file systems of the disk")
	}}
	out, err := runJob(context.Background(), JobSpec{Mode: ModeMeasure, Latest: "/src", Guest: &GuestSource{Socket: "/tmp/d.sock"}}, writePaths(t, "/a"), deps)
	if err == nil || !strings.Contains(out.Error, "file systems") {
		t.Fatalf("out %+v, err %v", out, err)
	}
}

func TestBadGuestJobSpecsAreRefused(t *testing.T) {
	deps := jobDeps{openGuest: func(context.Context, string, GuestSource, time.Time) (browse.Tree, browse.Info, func() error, error) {
		t.Error("a bad spec reached the disk")
		return nil, browse.Info{}, nil, errors.New("no")
	}}
	g := &GuestSource{Socket: "/tmp/d.sock"}
	for name, spec := range map[string]JobSpec{
		"with COS":             {Mode: ModeMeasure, Latest: "", COS: cosSource(), Guest: g},
		"with moments":         {Mode: ModeMeasure, Latest: "/src", Moments: "/moments", Guest: g},
		"no folder":            {Mode: ModeMeasure, Guest: g},
		"no socket":            {Mode: ModeMeasure, Latest: "/src", Guest: &GuestSource{}},
		"relative socket":      {Mode: ModeMeasure, Latest: "/src", Guest: &GuestSource{Socket: "d.sock"}},
		"bad consistency":      {Mode: ModeMeasure, Latest: "/src", Guest: &GuestSource{Socket: "/tmp/d.sock", Consistency: "Maybe"}},
		"appliance not a path": {Mode: ModeMeasure, Latest: "/src", Guest: &GuestSource{Socket: "/tmp/d.sock", Appliance: "rel"}},
	} {
		if _, err := runJob(context.Background(), spec, writePaths(t, "/a"), deps); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
