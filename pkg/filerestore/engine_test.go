package filerestore

import (
	"context"
	"errors"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

func sampleTree() *fakeTree {
	t := newFakeTree()
	t.dir("var", 0o755, 0, 0)
	t.dir("var/www", 0o750, 33, 33)
	t.dir("var/www/html", 0o755, 33, 33)
	t.file("var/www/html/index.php", "<?php echo 1;", 0o644, 33, 33)
	t.file("var/www/html/secret.key", "s3cret", 0o600, 1000, 1000)
	t.dir("var/www/html/locked", 0o500, 2000, 2000)
	t.file("var/www/html/locked/inner", "inside", 0o600, 2000, 2000)
	t.link("var/www/html/etc", "../../../etc")
	return t
}

func TestRestoreKeepsTreeAndParents(t *testing.T) {
	d := newRecDest()
	res, err := Restore(context.Background(), sampleTree(), d, paths("/var/www/html/index.php"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if got := d.files["var/www/html/index.php"].data.String(); got != "<?php echo 1;" {
		t.Fatalf("content %q", got)
	}
	if res.Files != 1 || res.Bytes != 13 {
		t.Fatalf("result %+v", res)
	}
	// The folders above the file are made from the backup's own folders.
	if m := d.dirs["var/www"].meta; m.Mode != 0o750 || m.UID == nil || *m.UID != 33 {
		t.Fatalf("parent meta %+v", m)
	}
	if _, ok := d.dirs["var/www/html/locked"]; ok {
		t.Fatal("a folder that was not chosen was restored")
	}
}

// A folder with mode 0500 must still get its children, so folders are finished
// (mode and owner set) only after everything in them is written.
func TestFoldersAreFinishedAfterTheirChildren(t *testing.T) {
	d := newRecDest()
	if _, err := Restore(context.Background(), sampleTree(), d, paths("/var/www/html"), Options{}); err != nil {
		t.Fatal(err)
	}
	if m := d.dirs["var/www/html/locked"].meta; m.Mode != 0o500 {
		t.Fatalf("locked folder mode %o", m.Mode)
	}
	after := d.opIndex("finishdir /var/www/html/locked")
	for _, op := range []string{"create /var/www/html/locked/inner", "finishfile /var/www/html/locked/inner"} {
		if i := d.opIndex(op); i < 0 || i > after {
			t.Fatalf("%q must come before its folder is finished: %v", op, d.ops)
		}
	}
	if d.opIndex("finishdir /var/www/html") < d.opIndex("finishdir /var/www/html/locked") || d.opIndex("finishdir /var") < d.opIndex("finishdir /var/www") {
		t.Fatalf("a parent was finished before its child: %v", d.ops)
	}
	if d.opIndex("finishdir /") < d.opIndex("finishdir /var") {
		t.Fatalf("the root was finished before its content: %v", d.ops)
	}
}

func TestConfiguredOwnerIsGivenToEverythingIncludingMode0600(t *testing.T) {
	d := newRecDest()
	opt := Options{Owner: Owner{UID: i64(107), GID: i64(107)}}
	if _, err := Restore(context.Background(), sampleTree(), d, paths("/var/www/html"), opt); err != nil {
		t.Fatal(err)
	}
	f := d.files["var/www/html/secret.key"]
	if f.meta.Mode != 0o600 || f.meta.UID == nil || *f.meta.UID != 107 || *f.meta.GID != 107 {
		t.Fatalf("0600 file meta %+v", f.meta)
	}
	if m := d.dirs["var/www/html/locked"].meta; *m.UID != 107 {
		t.Fatalf("folder owner %+v", m)
	}
	if m := d.linkMeta["var/www/html/etc"]; m.UID == nil || *m.UID != 107 {
		t.Fatalf("link owner %+v", m)
	}
	if m := d.dirs[""].meta; m.UID == nil || *m.UID != 107 {
		t.Fatalf("root owner %+v", m)
	}
}

func TestSourceOwnerIsKeptWhenNoOwnerIsConfigured(t *testing.T) {
	d := newRecDest()
	if _, err := Restore(context.Background(), sampleTree(), d, paths("/var/www/html/secret.key"), Options{}); err != nil {
		t.Fatal(err)
	}
	m := d.files["var/www/html/secret.key"].meta
	if m.UID == nil || *m.UID != 1000 || m.GID == nil || *m.GID != 1000 {
		t.Fatalf("owner %+v", m)
	}
}

func TestSetuidAndSetgidAreRemoved(t *testing.T) {
	tr := newFakeTree()
	tr.file("su", "x", 0o6755, 0, 0)
	tr.dir("sg", 0o2775, 0, 0)
	tr.file("sticky", "x", 0o1644, 0, 0)
	d := newRecDest()
	if _, err := Restore(context.Background(), tr, d, paths("/su", "/sg", "/sticky"), Options{}); err != nil {
		t.Fatal(err)
	}
	if m := d.files["su"].meta.Mode; m != 0o755 {
		t.Fatalf("setuid file mode %o", m)
	}
	if m := d.dirs["sg"].meta.Mode; m != 0o775 {
		t.Fatalf("setgid folder mode %o", m)
	}
	if m := d.files["sticky"].meta.Mode; m != 0o1644 {
		t.Fatalf("the sticky bit must stay: %o", m)
	}
}

func TestSymbolicLinkIsCopiedAsALinkAndNeverFollowed(t *testing.T) {
	tr := sampleTree()
	d := newRecDest()
	res, err := Restore(context.Background(), tr, d, paths("/var/www/html/etc"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if string(d.links["var/www/html/etc"]) != "../../../etc" || res.Links != 1 || res.Files != 0 {
		t.Fatalf("links %v result %+v", d.links, res)
	}
}

func TestSpecialAndUnreadableEntriesInsideAFolderAreCountedAndSkipped(t *testing.T) {
	tr := newFakeTree()
	tr.dir("d", 0o755, 0, 0)
	tr.file("d/ok", "ok", 0o644, 0, 0)
	tr.add("d/fifo", &fnode{e: browse.Entry{Type: browse.TypeOther, Mode: 0o644}})
	tr.add("d/secret", &fnode{e: browse.Entry{Type: browse.TypeFile, Mode: 0o600, Size: 3}, data: []byte("abc"), unreadable: true})
	tr.add("d/closed", &fnode{e: browse.Entry{Type: browse.TypeDir, Mode: 0o700}, unreadable: true})
	d := newRecDest()
	res, err := Restore(context.Background(), tr, d, paths("/d"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 1 || res.Skipped != 1 || res.Unreadable != 2 {
		t.Fatalf("result %+v", res)
	}
	if _, ok := d.files["d/secret"]; ok {
		t.Fatal("an unreadable file was created")
	}
}

func TestAChosenPathThatDoesNotExistFailsAndRemovesWhatWasWritten(t *testing.T) {
	d := newRecDest()
	_, err := Restore(context.Background(), sampleTree(), d, paths("/var/www/html/index.php", "/no/such/file"), Options{})
	var be *browse.Error
	if !errors.As(err, &be) || be.Code != browse.CodeNotFound {
		t.Fatalf("err %v", err)
	}
	if !d.aborted {
		t.Fatal("what was written must be removed")
	}
}

func TestAChosenPathThatIsNotReadableFails(t *testing.T) {
	tr := newFakeTree()
	tr.add("secret", &fnode{e: browse.Entry{Type: browse.TypeFile, Mode: 0o600, Size: 1}, data: []byte("x"), unreadable: true})
	d := newRecDest()
	_, err := Restore(context.Background(), tr, d, paths("/secret"), Options{})
	var be *browse.Error
	if !errors.As(err, &be) || be.Code != browse.CodeNotReadable || !d.aborted {
		t.Fatalf("err %v aborted %v", err, d.aborted)
	}
}

// If the owner cannot be set the restore fails. Files that nobody can read are
// worse than no restore.
func TestFailedOwnerChangeFailsTheRestoreAndAborts(t *testing.T) {
	d := newRecDest()
	d.failOwner = true
	opt := Options{Owner: Owner{UID: i64(107), GID: i64(107)}}
	_, err := Restore(context.Background(), sampleTree(), d, paths("/var/www/html/secret.key"), opt)
	if err == nil || !d.aborted {
		t.Fatalf("err %v aborted %v", err, d.aborted)
	}
}

func TestNotEnoughSpaceIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	cases := []struct {
		name string
		opt  Options
		free int64
	}{
		{"limit", Options{MaxBytes: 10}, 1 << 40},
		{"free space", Options{}, 10},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d := newRecDest()
			d.free = c.free
			_, err := Restore(context.Background(), sampleTree(), d, paths("/var/www/html"), c.opt)
			var ns *NoSpaceError
			if !errors.As(err, &ns) {
				t.Fatalf("err %v", err)
			}
			if len(d.ops) > 1 || (len(d.ops) == 1 && d.ops[0] != "abort") {
				t.Fatalf("something was written: %v", d.ops)
			}
		})
	}
}

func TestOverlappingPathsAreCopiedOnce(t *testing.T) {
	d := newRecDest()
	res, err := Restore(context.Background(), sampleTree(), d, paths("/var/www/html/index.php", "/var/www/html", "/var/www/html/index.php"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 {
		t.Fatalf("files %d: %v", res.Files, d.ops)
	}
}

func TestBadPathsAreRefusedBeforeAnythingIsWritten(t *testing.T) {
	for _, bad := range [][][]byte{{[]byte("..")}, {[]byte("a"), []byte(".."), []byte("b")}, {[]byte("a/b")}, {[]byte{}}, {[]byte("a\x00b")}} {
		d := newRecDest()
		if _, err := Restore(context.Background(), sampleTree(), d, [][][]byte{bad}, Options{}); err == nil {
			t.Fatalf("path %q was accepted", bad)
		}
		for _, op := range d.ops {
			if op != "abort" {
				t.Fatalf("something was written for %q: %v", bad, d.ops)
			}
		}
	}
}

func TestNoPathIsAnError(t *testing.T) {
	if _, err := Restore(context.Background(), sampleTree(), newRecDest(), nil, Options{}); !errors.Is(err, ErrNothingToRestore) {
		t.Fatalf("err %v", err)
	}
}

func TestACancelledContextStopsAndAborts(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	d := newRecDest()
	if _, err := Restore(ctx, sampleTree(), d, paths("/var"), Options{}); !errors.Is(err, context.Canceled) || !d.aborted {
		t.Fatalf("err %v aborted %v", err, d.aborted)
	}
}

func TestRootGetsTheRootModeAndOwner(t *testing.T) {
	d := newRecDest()
	opt := Options{RootMode: 0o777, Owner: Owner{UID: i64(107), GID: i64(107)}}
	if _, err := Restore(context.Background(), sampleTree(), d, paths("/var/www/html/index.php"), opt); err != nil {
		t.Fatal(err)
	}
	if m := d.dirs[""].meta; m.Mode != 0o777 || *m.UID != 107 {
		t.Fatalf("root meta %+v", m)
	}
}

func TestMeasureCountsBytesWithoutWriting(t *testing.T) {
	m, err := Measure(context.Background(), sampleTree(), paths("/var/www/html"))
	if err != nil {
		t.Fatal(err)
	}
	if m.Bytes != int64(len("<?php echo 1;")+len("s3cret")+len("inside")) || m.Files != 3 {
		t.Fatalf("measure %+v", m)
	}
}

func TestTheWholeVolumeCanBeRestored(t *testing.T) {
	d := newRecDest()
	res, err := Restore(context.Background(), sampleTree(), d, paths("/"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 3 || d.opIndex("finishdir /") < d.opIndex("finishdir /var") {
		t.Fatalf("result %+v ops %v", res, d.ops)
	}
}
