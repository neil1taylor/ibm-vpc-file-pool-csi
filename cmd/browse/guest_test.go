package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/guestfs"
)

// fakeTools answers the two calls a session makes at the start and one listing.
type fakeTools struct{}

func (fakeTools) Output(_ context.Context, args []string) ([]byte, error) {
	switch args[0] {
	case "virt-filesystems":
		return []byte("Name,Type,VFS,Label,MBR,Size,Parent\n/dev/sda1,filesystem,xfs,root,,1000,\n/dev/sda2,filesystem,crypto_LUKS,,,2000,\n"), nil
	case "virt-ls":
		return []byte("f,0644,5,hello.txt\n"), nil
	}
	return nil, &guestfs.ExitError{}
}

func (fakeTools) Stream(context.Context, []string) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("hello")), nil
}

func withFakeTools(t *testing.T) {
	t.Helper()
	old := newToolRunner
	newToolRunner = func(string, bool) guestfs.Runner { return fakeTools{} }
	t.Cleanup(func() { newToolRunner = old })
}

func guestSocket(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/tmp", "gh")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(d) })
	return filepath.Join(d, "disk.sock")
}

func TestGuestModeServesTheFileSystemsOfTheDisk(t *testing.T) {
	withFakeTools(t)
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "disk.img"), make([]byte, 1<<20), 0o660); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("g", 32)
	crt, key, certPEM := writeCert(t)
	client, err := browse.PinnedClient(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	addrc := make(chan net.Addr, 1)
	exit := make(chan int, 1)
	var out, errb bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		exit <- run(ctx, []string{
			"--mode", "browse-server", "--guest", "--root", root, "--guest-socket", guestSocket(t),
			"--guest-consistency", "CrashConsistent",
			"--token-file", writeToken(t, token), "--listen", "127.0.0.1:0", "--idle", "1m", "--max-life", "1m",
			"--termination-log", filepath.Join(t.TempDir(), "term"), "--tls-cert-file", crt, "--tls-key-file", key,
		}, &out, &errb, func(a net.Addr) { addrc <- a })
	}()
	var addr net.Addr
	select {
	case addr = <-addrc:
	case code := <-exit:
		t.Fatalf("exited with %d: %s", code, errb.String())
	case <-time.After(10 * time.Second):
		t.Fatal("did not start")
	}
	post := func(path, body string) []byte {
		req, _ := http.NewRequest(http.MethodPost, "https://"+addr.String()+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		b, _ := io.ReadAll(resp.Body)
		return b
	}
	var page struct {
		Entries []struct {
			Name     []byte
			Readable bool
			Note     string
		}
	}
	if err := json.Unmarshal(post("/v1/list", `{"path":[]}`), &page); err != nil || len(page.Entries) != 2 {
		t.Fatalf("list = %+v, %v", page, err)
	}
	if string(page.Entries[0].Name) != "sda1" || !page.Entries[0].Readable || page.Entries[1].Readable || !strings.Contains(page.Entries[1].Note, "LUKS") {
		t.Errorf("entries = %+v", page.Entries)
	}
	var info browse.Info
	if err := json.Unmarshal(post("/v1/info", `{}`), &info); err != nil || !info.Guest || info.Consistency != "CrashConsistent" || len(info.Notices) == 0 {
		t.Errorf("info = %+v, %v", info, err)
	}
	if !strings.Contains(strings.Join(info.Notices, " "), "VM was running") {
		t.Errorf("notices = %v", info.Notices)
	}
	cancel()
	select {
	case <-exit:
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop")
	}
}

func TestGuestModeFailsInWordsInTheTerminationLog(t *testing.T) {
	old := newToolRunner
	newToolRunner = func(string, bool) guestfs.Runner { return failingTools{} }
	defer func() { newToolRunner = old }()
	root := t.TempDir()
	_ = os.WriteFile(filepath.Join(root, "disk.img"), make([]byte, 1<<20), 0o660)
	crt, key, _ := writeCert(t)
	termLog := filepath.Join(t.TempDir(), "term")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{
		"--mode", "browse-server", "--guest", "--root", root, "--guest-socket", guestSocket(t),
		"--token-file", writeToken(t, strings.Repeat("g", 32)), "--termination-log", termLog,
		"--tls-cert-file", crt, "--tls-key-file", key,
	}, &out, &errb, nil)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	b, _ := os.ReadFile(termLog)
	if !strings.Contains(string(b), "file systems") {
		t.Errorf("termination log = %q", b)
	}
}

type failingTools struct{}

func (failingTools) Output(context.Context, []string) ([]byte, error) {
	return nil, &guestfs.ExitError{Stderr: "kvm: not available"}
}
func (failingTools) Stream(context.Context, []string) (io.ReadCloser, error) {
	return nil, &guestfs.ExitError{}
}

func TestGuestModeRefusesBadArguments(t *testing.T) {
	crt, key, _ := writeCert(t)
	tok := writeToken(t, strings.Repeat("g", 32))
	root := t.TempDir()
	base := []string{"--mode", "browse-server", "--token-file", tok, "--tls-cert-file", crt, "--tls-key-file", key}
	for name, c := range map[string][]string{
		"no root":         append([]string{"--guest"}, base...),
		"with moments":    append([]string{"--guest", "--root", root, "--moments", root}, base...),
		"with cos":        append([]string{"--guest", "--cos-source", "{}"}, base...),
		"bad consistency": append([]string{"--guest", "--root", root, "--guest-consistency", "Maybe"}, base...),
		"bad socket":      append([]string{"--guest", "--root", root, "--guest-socket", "relative"}, base...),
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), c, &out, &errb, nil); code != 2 {
			t.Errorf("%s: exit %d (%s)", name, code, errb.String())
		}
	}
}
