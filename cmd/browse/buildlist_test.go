package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filelist"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/guestfs"
)

// listTools answers the file system search and streams one whole-tree listing.
type listTools struct{}

func (listTools) Output(_ context.Context, args []string) ([]byte, error) {
	if args[0] == "virt-filesystems" {
		return []byte("Name,Type,VFS,Label,MBR,Size,Parent\n/dev/sda1,filesystem,xfs,root,,1000,\n"), nil
	}
	return nil, &guestfs.ExitError{}
}

func (listTools) Stream(_ context.Context, args []string) (io.ReadCloser, error) {
	if args[0] == "virt-ls" {
		return io.NopCloser(strings.NewReader("d,0755,4096,/\nd,0755,4096,/etc\nf,0644,3,/etc/hosts\n-,0644,5,/hello.txt\n")), nil
	}
	return nil, &guestfs.ExitError{}
}

func buildListArgs(t *testing.T, root, out, term string) []string {
	return []string{"--mode", "build-list", "--root", root, "--guest-socket", guestSocket(t), "--list-out", out, "--termination-log", term}
}

func TestBuildListModeWritesTheListAndReportsIt(t *testing.T) {
	old := newToolRunner
	newToolRunner = func(string, bool) guestfs.Runner { return listTools{} }
	defer func() { newToolRunner = old }()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "disk.img"), make([]byte, 1<<20), 0o660); err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	out, term := filepath.Join(outDir, "filelist.json.gz"), filepath.Join(t.TempDir(), "term")
	var so, se bytes.Buffer
	if code := run(context.Background(), buildListArgs(t, root, out, term), &so, &se, nil); code != 0 {
		t.Fatalf("exit %d: %s", code, se.String())
	}
	f, err := os.Open(out)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	l, err := filelist.Read(f, filelist.Limits{})
	if err != nil {
		t.Fatal(err)
	}
	page, err := filelist.NewTree(l, nil).List([][]byte{[]byte("sda1")}, nil, 100)
	if err != nil || len(page.Entries) != 2 {
		t.Fatalf("list = %+v, %v", page, err)
	}
	var msg struct {
		Entries int `json:"entries"`
	}
	b, _ := os.ReadFile(term)
	if err := json.Unmarshal(b, &msg); err != nil || msg.Entries != 3 {
		t.Errorf("termination message %q: %+v, %v", b, msg, err)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 1 {
		t.Errorf("the output folder holds %d files; only the list may be there", len(entries))
	}
	_ = browse.Info{}
}

func TestBuildListModeFailsInWordsAndLeavesNoFile(t *testing.T) {
	old := newToolRunner
	newToolRunner = func(string, bool) guestfs.Runner { return failingTools{} }
	defer func() { newToolRunner = old }()
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "disk.img"), make([]byte, 1<<20), 0o660)
	outDir := t.TempDir()
	out, term := filepath.Join(outDir, "filelist.json.gz"), filepath.Join(t.TempDir(), "term")
	var so, se bytes.Buffer
	if code := run(context.Background(), buildListArgs(t, root, out, term), &so, &se, nil); code != 1 {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(term)
	if !strings.Contains(string(b), "file systems") {
		t.Errorf("termination log = %q", b)
	}
	if entries, _ := os.ReadDir(outDir); len(entries) != 0 {
		t.Errorf("left %d files after a failure", len(entries))
	}
}

func TestBuildListModeNeedsAnOutputPath(t *testing.T) {
	var so, se bytes.Buffer
	if code := run(context.Background(), []string{"--mode", "build-list", "--root", t.TempDir()}, &so, &se, nil); code != 2 {
		t.Fatalf("exit %d", code)
	}
	if code := run(context.Background(), []string{"--mode", "build-list", "--root", t.TempDir(), "--list-out", "relative"}, &so, &se, nil); code != 2 {
		t.Fatalf("relative path: exit %d", code)
	}
}
