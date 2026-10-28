//go:build unix

package filerestore

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

// own is the owner of the test process, which it may always give.
func own() Options {
	uid, gid := int64(os.Getuid()), int64(os.Getgid())
	return Options{Owner: Owner{UID: &uid, GID: &gid}}
}

const volName = "pvcs/pvc-0a1b2c3d-0a1b-0a1b-0a1b-0a1b2c3d4e5f"

// claimDir makes a share with one claim folder in it.
func claimDir(t *testing.T) (share, claim string) {
	t.Helper()
	share = t.TempDir()
	claim = filepath.Join(share, filepath.FromSlash(volName))
	if err := os.MkdirAll(claim, 0o755); err != nil {
		t.Fatal(err)
	}
	return share, claim
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDirDestWritesATreeWithModesTimesAndLinks(t *testing.T) {
	share, claim := claimDir(t)
	d, err := NewFolderDest(share, p(volName), ".restoring-x", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	uid, gid := int64(os.Getuid()), int64(os.Getgid())
	res, err := Restore(context.Background(), sampleTree(), d, paths("/var/www/html"), Options{RootMode: 0o755, Owner: Owner{UID: &uid, GID: &gid}})
	if err != nil {
		t.Fatal(err)
	}
	name, err := d.Commit("restored-p-m")
	if err != nil || name != "restored-p-m" {
		t.Fatalf("commit %q %v", name, err)
	}
	base := filepath.Join(claim, name)
	// The 0500 folder got its child, and ended with its own mode.
	if b, err := os.ReadFile(filepath.Join(base, "var/www/html/locked/inner")); err != nil || string(b) != "inside" {
		t.Fatalf("inner: %q %v", b, err)
	}
	st, _ := os.Stat(filepath.Join(base, "var/www/html/locked"))
	if st.Mode().Perm() != 0o500 {
		t.Fatalf("locked mode %v", st.Mode())
	}
	if !st.ModTime().Equal(testTime) {
		t.Fatalf("mtime %v", st.ModTime())
	}
	key, _ := os.Stat(filepath.Join(base, "var/www/html/secret.key"))
	if key.Mode().Perm() != 0o600 {
		t.Fatalf("0600 file mode %v", key.Mode())
	}
	if l, err := os.Readlink(filepath.Join(base, "var/www/html/etc")); err != nil || l != "../../../etc" {
		t.Fatalf("link %q %v", l, err)
	}
	if res.Files != 3 || res.Links != 1 {
		t.Fatalf("result %+v", res)
	}
	// The staging folder is gone.
	if _, err := os.Lstat(filepath.Join(claim, ".restoring-x")); !os.IsNotExist(err) {
		t.Fatalf("staging folder still there: %v", err)
	}
	// Restore of the folder with mode 0500 can be removed again by the test.
	_ = os.Chmod(filepath.Join(base, "var/www/html/locked"), 0o700)
}

func TestACommitNeverReplacesANameThatExists(t *testing.T) {
	share, claim := claimDir(t)
	outside := t.TempDir()
	// A person made a link with the final name. A restore must not go through it.
	if err := os.Symlink(outside, filepath.Join(claim, "restored-p-m")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(claim, "restored-p-m-2"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := NewFolderDest(share, p(volName), ".restoring-x", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if _, rerr := Restore(context.Background(), sampleTree(), d, paths("/var/www/html/index.php"), own()); rerr != nil {
		t.Fatal(rerr)
	}
	name, err := d.Commit("restored-p-m")
	if err != nil || name != "restored-p-m-3" {
		t.Fatalf("commit %q %v", name, err)
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("something was written through the link: %v", entries)
	}
	if l, err := os.Readlink(filepath.Join(claim, "restored-p-m")); err != nil || l != outside {
		t.Fatalf("the person's link was changed: %q %v", l, err)
	}
}

func TestALinkPlantedInTheDestinationIsNeverFollowed(t *testing.T) {
	share, claim := claimDir(t)
	outside := t.TempDir()
	d, err := NewFolderDest(share, p(volName), ".restoring-x", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	staging := filepath.Join(claim, ".restoring-x")
	// A folder name is a link to a folder outside.
	if err := os.Symlink(outside, filepath.Join(staging, "evil")); err != nil {
		t.Fatal(err)
	}
	if err := d.Mkdir(p("evil/sub")); err == nil {
		t.Fatal("Mkdir went through a link")
	}
	if _, err := d.Create(p("evil/file")); err == nil {
		t.Fatal("Create went through a link")
	}
	// A file name is a link to a file outside.
	target := filepath.Join(outside, "victim")
	mustWrite(t, target, "keep")
	if err := os.Symlink(target, filepath.Join(staging, "f")); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Create(p("f")); err == nil {
		t.Fatal("Create replaced or followed a link")
	}
	if err := d.Symlink(p("f"), []byte("x"), Meta{}); err == nil {
		t.Fatal("Symlink replaced a name that exists")
	}
	if b, _ := os.ReadFile(target); string(b) != "keep" {
		t.Fatalf("the outside file changed: %q", b)
	}
	if err := d.FinishDir(p("evil"), Meta{Mode: 0o777}); err == nil {
		t.Fatal("FinishDir went through a link")
	}
	if st, _ := os.Stat(outside); st.Mode().Perm() == 0o777 {
		t.Fatal("the outside folder was changed")
	}
}

func TestAClaimWithAVMDiskIsRefused(t *testing.T) {
	share, claim := claimDir(t)
	mustWrite(t, filepath.Join(claim, "disk.img"), "x")
	if _, err := NewFolderDest(share, p(volName), ".restoring-x", 0); !errors.Is(err, ErrVMDiskClaim) {
		t.Fatalf("err %v", err)
	}
}

func TestAbortRemovesTheStagingFolderAndLeavesTheClaimAlone(t *testing.T) {
	share, claim := claimDir(t)
	mustWrite(t, filepath.Join(claim, "mine.txt"), "mine")
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "keep"), "keep")
	d, err := NewFolderDest(share, p(volName), ".restoring-x", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if _, err := Restore(context.Background(), sampleTree(), d, paths("/var/www/html"), own()); err != nil {
		t.Fatal(err)
	}
	// A link inside the staging folder to a folder outside must be removed, not followed.
	if err := os.Symlink(outside, filepath.Join(claim, ".restoring-x", "link")); err != nil {
		t.Fatal(err)
	}
	if err := d.Abort(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(filepath.Join(claim, ".restoring-x")); !os.IsNotExist(err) {
		t.Fatalf("staging folder still there: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(claim, "mine.txt")); string(b) != "mine" {
		t.Fatal("the claim's own file changed")
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "keep")); string(b) != "keep" {
		t.Fatal("abort followed a link")
	}
}

func TestAStagingFolderOfAnEarlierTryIsReplaced(t *testing.T) {
	share, claim := claimDir(t)
	if err := os.MkdirAll(filepath.Join(claim, ".restoring-x", "old"), 0o755); err != nil {
		t.Fatal(err)
	}
	d, err := NewFolderDest(share, p(volName), ".restoring-x", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if _, err := os.Lstat(filepath.Join(claim, ".restoring-x", "old")); !os.IsNotExist(err) {
		t.Fatalf("the earlier try's content is still there: %v", err)
	}
}

func TestVolumeDestMakesTheFolderAndEmptiesIt(t *testing.T) {
	share := t.TempDir()
	vol := filepath.Join(share, filepath.FromSlash(volName))
	if err := os.MkdirAll(vol, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	mustWrite(t, filepath.Join(outside, "keep"), "keep")
	mustWrite(t, filepath.Join(vol, "left-over"), "x")
	if err := os.Symlink(outside, filepath.Join(vol, "l")); err != nil {
		t.Fatal(err)
	}
	d, err := NewVolumeDest(share, p(volName), 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if entries, _ := os.ReadDir(vol); len(entries) != 0 {
		t.Fatalf("the new volume was not emptied: %v", entries)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "keep")); string(b) != "keep" {
		t.Fatal("emptying followed a link")
	}
	// A volume folder that does not exist yet is made.
	d2, err := NewVolumeDest(share, p("pvcs/pvc-b"), 0)
	if err != nil {
		t.Fatal(err)
	}
	_ = d2.Close()
	if st, err := os.Stat(filepath.Join(share, "pvcs", "pvc-b")); err != nil || !st.IsDir() {
		t.Fatalf("not made: %v", err)
	}
}

func TestVolumeDestRefusesALinkOnTheWayAndAnEmptyVolume(t *testing.T) {
	share := t.TempDir()
	outside := t.TempDir()
	if err := os.Symlink(outside, filepath.Join(share, "pvcs")); err != nil {
		t.Fatal(err)
	}
	if _, err := NewVolumeDest(share, p(volName), 0); err == nil {
		t.Fatal("a link on the way was followed")
	}
	if _, err := NewVolumeDest(share, nil, 0); err == nil {
		t.Fatal("an empty volume path would empty the whole share")
	}
}

func TestFreeCountsTheClaimsCapacityLessWhatItHolds(t *testing.T) {
	share, claim := claimDir(t)
	mustWrite(t, filepath.Join(claim, "a"), "0123456789")
	d, err := NewFolderDest(share, p(volName), ".restoring-x", 100)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	free, err := d.Free()
	if err != nil || free != 90 {
		t.Fatalf("free %d %v", free, err)
	}
}

func TestSourceSideLinksInATreeAreNotFollowedByTheEngine(t *testing.T) {
	// A link in the backup to a folder: the engine copies the link and lists nothing below it.
	tr := newFakeTree()
	tr.dir("d", 0o755, 0, 0)
	tr.link("d/escape", "/etc")
	share, claim := claimDir(t)
	d, err := NewFolderDest(share, p(volName), ".restoring-x", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	if _, err := Restore(context.Background(), tr, d, paths("/d"), own()); err != nil {
		t.Fatal(err)
	}
	if _, err := d.Commit("r"); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(filepath.Join(claim, "r", "d", "escape")); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("not a link: %v %v", st, err)
	}
}

func TestACancelledRestoreLeavesNoStagingFolder(t *testing.T) {
	share, claim := claimDir(t)
	d, err := NewFolderDest(share, p(volName), ".restoring-x", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	time.Sleep(time.Millisecond)
	_, err = Restore(ctx, sampleTree(), d, paths("/var"), own())
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Lstat(filepath.Join(claim, ".restoring-x")); !os.IsNotExist(err) {
		t.Fatalf("staging folder still there: %v", err)
	}
}

var _ browse.Tree = (*fakeTree)(nil)

func TestAClaimWithoutAFolderIsNotReady(t *testing.T) {
	share := t.TempDir()
	if _, err := NewFolderDest(share, p(volName), ".restoring-x", 0); !errors.Is(err, ErrClaimNotReady) {
		t.Fatalf("err %v", err)
	}
	if _, err := os.Lstat(filepath.Join(share, "pvcs")); !os.IsNotExist(err) {
		t.Fatal("a folder was made for a claim that has none")
	}
}

func TestAClaimFolderThatIsALinkIsRefused(t *testing.T) {
	share := t.TempDir()
	outside := t.TempDir()
	if err := os.MkdirAll(filepath.Join(share, "pvcs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(share, filepath.FromSlash(volName))); err != nil {
		t.Fatal(err)
	}
	if _, err := NewFolderDest(share, p(volName), ".restoring-x", 0); err == nil {
		t.Fatal("a link as the claim folder was followed")
	}
	if entries, _ := os.ReadDir(outside); len(entries) != 0 {
		t.Fatalf("something was written outside: %v", entries)
	}
}
