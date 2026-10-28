package main

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

func TestFSIDNormalisation(t *testing.T) {
	for in, want := range map[string]string{
		"sda1": "sda1", "/dev/sda1": "sda1", "/dev/rhel/root": "rhel-root", "/dev/mapper/vg-lv": "vg-lv", "rhel-root": "rhel-root",
	} {
		got, err := fsName(in)
		if err != nil || got != want {
			t.Errorf("fsName(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "..", "/dev/..", "a b\n", "/dev/", "x/../y"} {
		if _, err := fsName(bad); err == nil {
			t.Errorf("fsName(%q) was accepted", bad)
		}
	}
}

func TestLs_FSPrefixesThePath(t *testing.T) {
	s, gw := newStub(t)
	s.guest = true
	s.pages[""] = browse.ListPage{}
	var out bytes.Buffer
	if err := runLs(context.Background(), gw, &out, args("--fs", "/dev/rhel/root", "/etc")); err != nil {
		t.Fatal(err)
	}
	if got := s.listed; len(got) != 1 || got[0] != "rhel-root/etc" {
		t.Errorf("listed %q", got)
	}
	s.listed = nil
	if err := runLs(context.Background(), gw, &out, args("--fs", "sda1")); err != nil {
		t.Fatal(err)
	}
	if got := s.listed; len(got) != 1 || got[0] != "sda1" {
		t.Errorf("listed %q", got)
	}
}

func TestLs_WithoutFSListsTheFileSystemsOfAVMDisk(t *testing.T) {
	s, gw := newStub(t)
	s.guest = true
	s.pages[""] = browse.ListPage{Entries: []browse.Entry{
		{Name: []byte("sda1"), Type: browse.TypeDir, Mode: 0o555, Size: 1 << 30, Readable: true, Note: "xfs boot"},
		{Name: []byte("sdb1"), Type: browse.TypeDir, Mode: 0o555, Readable: false, Note: "This volume is encrypted with BitLocker.\nline two"},
	}}
	var out bytes.Buffer
	if err := runLs(context.Background(), gw, &out, args()); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	for _, want := range []string{"sda1/", "xfs boot", "sdb1/", "not readable", "BitLocker"} {
		if !strings.Contains(got, want) {
			t.Errorf("output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "line two") && strings.Contains(got, "BitLocker.\nline") {
		t.Error("a raw newline from guest text reached the terminal")
	}
}

func TestLs_NoticesGoToStderr(t *testing.T) {
	s, gw := newStub(t)
	s.guest = true
	s.pages[""] = browse.ListPage{}
	s.info = browse.Info{Guest: true, Notices: []string{"This copy was taken while the VM was running. The newest changes may be missing."}}
	var errb bytes.Buffer
	old := errOut
	errOut = &errb
	defer func() { errOut = old }()
	var out bytes.Buffer
	if err := runLs(context.Background(), gw, &out, args()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(errb.String(), "VM was running") || strings.Contains(out.String(), "VM was running") {
		t.Errorf("stderr %q, stdout %q", errb.String(), out.String())
	}
}

func TestLs_APlainVolumeAsksNothingAboutNotices(t *testing.T) {
	s, gw := newStub(t)
	s.pages[""] = browse.ListPage{}
	var out bytes.Buffer
	if err := runLs(context.Background(), gw, &out, args()); err != nil {
		t.Fatal(err)
	}
	for _, c := range s.calls {
		if strings.HasSuffix(c, "/info") {
			t.Error("an info call for a plain volume")
		}
	}
}

func TestGet_FSPrefixesThePath(t *testing.T) {
	s, gw := newStub(t)
	s.guest = true
	s.stat = browse.Entry{Name: []byte("hosts"), Type: browse.TypeFile, Size: 5, Readable: true}
	s.file = []byte("hello")
	var out bytes.Buffer
	dest := t.TempDir() + "/hosts"
	if err := runGet(context.Background(), gw, &out, args("--fs", "sda1", "/etc/hosts", "--out", dest)); err != nil {
		t.Fatal(err)
	}
	if got := s.statted; len(got) != 1 || got[0] != "sda1/etc/hosts" {
		t.Errorf("statted %q", got)
	}
}

func TestRestoreFiles_FSPrefixesEveryPath(t *testing.T) {
	s, gw := newRestoreStub(t)
	if _, err := runRF(t, gw, []string{"--policy", "nightly", "--pvc", "apps/web-root", "--fs", "/dev/sda1",
		"--path", "/etc/hosts", "--path", `/home/a\x0ab`, "--to", "apps/files"}); err != nil {
		t.Fatal(err)
	}
	r := s.requests[0]
	if len(r.Paths) != 2 || r.Paths[0] != "/sda1/etc/hosts" || r.Paths[1] != `/sda1/home/a\x0ab` {
		t.Errorf("paths %q", r.Paths)
	}
}

func TestRestoreFiles_WithoutFSPathsAreSentAsGiven(t *testing.T) {
	s, gw := newRestoreStub(t)
	if _, err := runRF(t, gw, []string{"--policy", "nightly", "--pvc", "apps/web-root",
		"--path", "/sda1/etc/hosts", "--to", "apps/files"}); err != nil {
		t.Fatal(err)
	}
	if r := s.requests[0]; len(r.Paths) != 1 || r.Paths[0] != "/sda1/etc/hosts" {
		t.Errorf("paths %q", r.Paths)
	}
}

func TestRestoreFiles_BadFSIsRefusedBeforeAnyCall(t *testing.T) {
	s, gw := newRestoreStub(t)
	if _, err := runRF(t, gw, []string{"--policy", "nightly", "--pvc", "apps/web-root", "--fs", "..", "--path", "/a", "--to", "apps/files"}); err == nil {
		t.Fatal("accepted")
	}
	if len(s.requests) != 0 {
		t.Error("a request was sent")
	}
}

func TestGuestSessionsGetMoreTimeToStart(t *testing.T) {
	if got := sessionWait(false, defaultSessionTimeout); got != defaultSessionTimeout {
		t.Errorf("plain: %v", got)
	}
	if got := sessionWait(true, defaultSessionTimeout); got != guestSessionTimeout {
		t.Errorf("guest: %v", got)
	}
	if got := sessionWait(true, time.Minute); got != time.Minute {
		t.Errorf("a time the user chose is kept: %v", got)
	}
}
