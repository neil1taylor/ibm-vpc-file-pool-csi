package cosbrowse

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

const (
	volA = "pvc-aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"
	volB = "pvc-bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"
)

func backupPath(policy, vol string) string { return "/share0/backup/" + policy + "/pvcs/" + vol }
func momentsPath(policy, vol string) string {
	return "/share0/backup/" + policy + "/.moments/pvcs/" + vol
}

func at(day, hour int) time.Time { return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC) }

// addExport adds the two snapshots of one export. The latest copy holds a file
// "who" that says which export it is; each moment folder holds a file "who" that
// says which moment it is. moments may be empty: then there is no moments
// snapshot.
func addExport(f *fakeRepo, policy, vol string, moment time.Time, moments ...time.Time) {
	latest := f.dir(f.fileEnt("who", "latest@"+moment.Format(browse.MomentLayout)))
	f.snapshot(backupPath(policy, vol), vol, moment, latest)
	if len(moments) == 0 {
		return
	}
	var ents []ent
	for _, m := range moments {
		name := m.Format(browse.MomentLayout)
		ents = append(ents, dirEnt(name, f.dir(f.fileEnt("who", "moment@"+name))))
	}
	f.snapshot(momentsPath(policy, vol), vol, moment, f.dir(ents...))
}

func whoIs(t *testing.T, o *Opened) string {
	t.Helper()
	file, err := o.Tree.Open(p("who"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	b := make([]byte, file.Size())
	if _, err := file.ReadAt(b, 0); err != nil && !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	return string(b)
}

func open(t *testing.T, f *fakeRepo, vol string, when time.Time) (*Opened, error) {
	t.Helper()
	return OpenRepo(context.Background(), f, Params{Policy: "pol", Volume: vol, At: when})
}

func TestOpen_LatestReadsTheNewestExportsLatestCopy(t *testing.T) {
	f := newFakeRepo()
	addExport(f, "pol", volA, at(20, 9), at(20, 9))
	addExport(f, "pol", volA, at(21, 9), at(20, 9), at(21, 9))
	o, err := open(t, f, volA, time.Time{})
	mustT(t, err)
	if got := whoIs(t, o); got != "latest@20261021T090000Z" {
		t.Errorf("who = %s", got)
	}
	if o.Info.Moment != "20261021T090000Z" || !o.Info.Latest {
		t.Errorf("info = %+v", o.Info)
	}
}

// The same cases as browse.ChooseRoot has for a folder on disk: the newest
// moment at or before the time, the latest copy for the newest, an error before
// the first.
func TestOpen_MomentsFollowTheRulesOfAFolderOnDisk(t *testing.T) {
	f := newFakeRepo()
	// Export 1 holds moments 20 and 21; export 2 holds 21, 22, 23 (the keep time
	// removed 20 locally); export 3 holds 22, 23, 24.
	addExport(f, "pol", volA, at(21, 12), at(20, 9), at(21, 9))
	addExport(f, "pol", volA, at(23, 12), at(21, 9), at(22, 9), at(23, 9))
	addExport(f, "pol", volA, at(24, 12), at(22, 9), at(23, 9), at(24, 9))
	for _, c := range []struct {
		name       string
		when       time.Time
		wantWho    string
		wantMoment string
		wantLatest bool
		wantErr    string
	}{
		{"exact moment", at(22, 9), "moment@20261022T090000Z", "20261022T090000Z", false, ""},
		{"between moments takes the older", at(22, 15), "moment@20261022T090000Z", "20261022T090000Z", false, ""},
		{"the newest moment", at(24, 9), "moment@20261024T090000Z", "20261024T090000Z", true, ""},
		{"after the newest", at(26, 0), "moment@20261024T090000Z", "20261024T090000Z", true, ""},
		{"only in the oldest export", at(20, 10), "moment@20261020T090000Z", "20261020T090000Z", false, ""},
		{"before the first moment", at(19, 0), "", "", false, "no backup at or before"},
	} {
		o, err := open(t, f, volA, c.when)
		if c.wantErr != "" {
			if err == nil || !strings.Contains(err.Error(), c.wantErr) {
				t.Errorf("%s: err = %v", c.name, err)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if got := whoIs(t, o); got != c.wantWho {
			t.Errorf("%s: who = %s, want %s", c.name, got, c.wantWho)
		}
		if o.Info.Moment != c.wantMoment || o.Info.Latest != c.wantLatest {
			t.Errorf("%s: info = %+v", c.name, o.Info)
		}
	}
}

func TestOpen_MomentsListIsTheUnionOfAllExports(t *testing.T) {
	f := newFakeRepo()
	addExport(f, "pol", volA, at(21, 12), at(20, 9), at(21, 9))
	addExport(f, "pol", volA, at(23, 12), at(21, 9), at(22, 9), at(23, 9))
	o, err := open(t, f, volA, time.Time{})
	mustT(t, err)
	got, err := o.Moments()
	mustT(t, err)
	if strings.Join(got, ",") != "20261020T090000Z,20261021T090000Z,20261022T090000Z,20261023T090000Z" {
		t.Errorf("moments = %v", got)
	}
}

func TestOpen_AnExportWithoutMomentsReadsOnlyTheLatestCopy(t *testing.T) {
	f := newFakeRepo()
	addExport(f, "pol", volA, at(20, 9))
	o, err := open(t, f, volA, at(19, 0))
	mustT(t, err)
	if got := whoIs(t, o); got != "latest@20261020T090000Z" || !o.Info.Latest {
		t.Errorf("who = %s, info = %+v", got, o.Info)
	}
}

func TestOpen_NoBackupOfTheVolume(t *testing.T) {
	f := newFakeRepo()
	addExport(f, "pol", volB, at(20, 9))
	_, err := open(t, f, volA, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "no backup of volume "+volA) {
		t.Errorf("err = %v", err)
	}
}

// The credentials read the whole area of the policy, which holds other
// volumes. Only the snapshots that carry the volume in their tag AND have its
// exact source path are used.
func TestOpen_OtherVolumesAndOtherPoliciesAreNeverRead(t *testing.T) {
	f := newFakeRepo()
	secret := f.dir(f.fileEnt("who", "SECRET of another volume"))
	// Tagged with our volume, but the path is another volume's.
	f.snapshot(backupPath("pol", volB), volA, at(22, 9), secret)
	// Our path, but tagged with another volume.
	f.snapshot(backupPath("pol", volA), volB, at(22, 9), secret)
	// Our volume in another policy's area.
	f.snapshot(backupPath("other", volA), volA, at(22, 9), secret)
	// Crafted source paths that try to be ours.
	for _, path := range []string{
		backupPath("pol", volA) + "/..",
		backupPath("pol", volA) + "/",
		backupPath("pol", volA) + "/sub",
		"/share0/backup/pol/pvcs/../pvcs/" + volA,
		"/share0/backup/pol/pvcs/" + volA + "x",
		"/x" + backupPath("pol", volA),
		"/share0/backup/pol/.moments/../pvcs/" + volA,
		strings.Replace(backupPath("pol", volA), "pol", "po?", 1),
	} {
		f.snapshot(path, volA, at(22, 9), secret)
	}
	// A snapshot with no moment tag was not made by the export.
	f.snapshot(backupPath("pol", volA), volA, time.Time{}, secret)
	// Our real backup, older than all of the above.
	addExport(f, "pol", volA, at(20, 9))

	o, err := open(t, f, volA, time.Time{})
	mustT(t, err)
	if got := whoIs(t, o); got != "latest@20261020T090000Z" {
		t.Errorf("who = %s", got)
	}
	if f.callCount(secret) != 0 {
		t.Error("a folder of another volume or policy was read")
	}
	// The other volume has its own backup, and is asked for by its own name.
	addExport(f, "pol", volB, at(21, 9))
	ob, err := open(t, f, volB, time.Time{})
	mustT(t, err)
	if got := whoIs(t, ob); got != "latest@20261021T090000Z" {
		t.Errorf("volume B who = %s", got)
	}
}

func TestOpen_AHostileMomentFolderNameCannotLeaveTheVolume(t *testing.T) {
	f := newFakeRepo()
	other := f.dir(f.fileEnt("who", "SECRET of another volume"))
	latest := f.dir(f.fileEnt("who", "latest"))
	f.snapshot(backupPath("pol", volA), volA, at(22, 9), latest)
	moments := f.dir(
		dirEnt("..", other),
		dirEnt("../"+volB, other),
		dirEnt("20261022T090000Z/../..", other),
		dirEnt("20261022T090000Z", f.dir(f.fileEnt("who", "moment@20261022T090000Z"))),
	)
	f.snapshot(momentsPath("pol", volA), volA, at(22, 9), moments)
	o, err := open(t, f, volA, at(22, 10))
	mustT(t, err)
	if got := whoIs(t, o); got != "moment@20261022T090000Z" {
		t.Errorf("who = %s", got)
	}
	names, err := o.Moments()
	mustT(t, err)
	if strings.Join(names, ",") != "20261022T090000Z" {
		t.Errorf("moments = %v", names)
	}
	if f.callCount(other) != 0 {
		t.Error("a folder behind a hostile moment name was read")
	}
}

func TestOpen_RepositoryFailuresAreWordedForPeople(t *testing.T) {
	f := newFakeRepo()
	f.snapErr = &ReadError{Msg: "COS refused the key: check the COS key Secret"}
	_, err := open(t, f, volA, time.Time{})
	if err == nil || !strings.Contains(err.Error(), "COS refused the key") {
		t.Errorf("err = %v", err)
	}
}
