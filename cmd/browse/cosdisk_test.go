package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosdisk"
)

// fakeDiskRepo has one export; its "WebDAV" is a plain server that holds the disk.
type fakeDiskRepo struct {
	disk    []byte
	started []string
	stopped *bool
	listErr error
}

func (f *fakeDiskRepo) Snapshots(context.Context) ([]cosbrowse.Snapshot, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return []cosbrowse.Snapshot{{
		ID: "k1234", SourcePath: "/share0/backup/pol/pvcs/" + testVolume, Volume: testVolume,
		Moment: time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC), Root: "kroot",
	}}, nil
}

func (f *fakeDiskRepo) StartWebDAV(_ context.Context, id string, _ func() (int, bool), _ time.Duration) (*cosbrowse.WebDAV, error) {
	f.started = append(f.started, id)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.ServeContent(w, r, "disk.img", time.Time{}, bytes.NewReader(f.disk))
	}))
	return cosbrowse.NewWebDAV(ts.URL+"/disk.img", func() { ts.Close(); *f.stopped = true }), nil
}

func withFakeDiskRepo(t *testing.T, f *fakeDiskRepo) {
	t.Helper()
	old := openDiskRepo
	openDiskRepo = func(context.Context, cosbrowse.Source, string, string, io.Writer) (diskRepo, error) {
		return f, nil
	}
	t.Cleanup(func() { openDiskRepo = old })
}

func TestCOSDiskModeServesTheDiskOverTLSWithTheToken(t *testing.T) {
	data := make([]byte, 20_000)
	for i := range data {
		data[i] = byte(i * 7)
	}
	stopped := false
	f := &fakeDiskRepo{disk: data, stopped: &stopped}
	withFakeDiskRepo(t, f)
	token := strings.Repeat("d", 32)
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
	term := filepath.Join(t.TempDir(), "term")
	go func() {
		exit <- run(ctx, []string{"--mode", "cos-disk-server", "--cos-source", cosSourceJSON,
			"--token-file", writeToken(t, token), "--listen", "127.0.0.1:0", "--idle", "1m", "--max-life", "1m",
			"--termination-log", term, "--tls-cert-file", crt, "--tls-key-file", key}, &out, &errb, func(a net.Addr) { addrc <- a })
	}()
	var addr net.Addr
	select {
	case addr = <-addrc:
	case code := <-exit:
		t.Fatalf("exited with %d: %s", code, errb.String())
	case <-time.After(10 * time.Second):
		t.Fatal("did not start")
	}
	base := "https://" + addr.String()
	src, err := cosdisk.NewRemoteSource(context.Background(), client, base+"/disk", token)
	if err != nil {
		t.Fatal(err)
	}
	if src.Size() != int64(len(data)) || !src.Moment().Equal(time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC)) {
		t.Fatalf("size=%d moment=%s", src.Size(), src.Moment())
	}
	got := make([]byte, 3000)
	if n, err := src.ReadRange(context.Background(), got, 5000); err != nil || n != 3000 || !bytes.Equal(got, data[5000:8000]) {
		t.Fatalf("ReadRange = %d, %v", n, err)
	}
	if len(f.started) != 1 || f.started[0] != "k1234" {
		t.Errorf("mounted %v", f.started)
	}
	resp, err := client.Get(base + "/healthz")
	if err != nil || resp.StatusCode != http.StatusOK {
		t.Errorf("healthz = %v, %v", resp, err)
	}
	cancel()
	select {
	case code := <-exit:
		if code != 0 {
			t.Fatalf("exit %d: %s", code, errb.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("did not stop")
	}
	if !stopped {
		t.Error("the Kopia mount was not stopped")
	}
}

func TestCOSDiskModeEndsWhenIdle(t *testing.T) {
	stopped := false
	withFakeDiskRepo(t, &fakeDiskRepo{disk: make([]byte, 10), stopped: &stopped})
	crt, key, _ := writeCert(t)
	term := filepath.Join(t.TempDir(), "term")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"--mode", "cos-disk-server", "--cos-source", cosSourceJSON,
		"--token-file", writeToken(t, strings.Repeat("d", 32)), "--listen", "127.0.0.1:0", "--idle", "300ms", "--max-life", "1m",
		"--termination-log", term, "--tls-cert-file", crt, "--tls-key-file", key}, &out, &errb, nil)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	b, _ := os.ReadFile(term)
	if !strings.Contains(string(b), "idle") {
		t.Errorf("termination log = %q", b)
	}
}

func TestCOSDiskModeFailsInWords(t *testing.T) {
	stopped := false
	withFakeDiskRepo(t, &fakeDiskRepo{stopped: &stopped, listErr: errors.New("the bucket could not be reached")})
	crt, key, _ := writeCert(t)
	term := filepath.Join(t.TempDir(), "term")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"--mode", "cos-disk-server", "--cos-source", cosSourceJSON,
		"--token-file", writeToken(t, strings.Repeat("d", 32)), "--termination-log", term,
		"--tls-cert-file", crt, "--tls-key-file", key}, &out, &errb, nil)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(term); !strings.Contains(string(b), "bucket") {
		t.Errorf("termination log = %q", b)
	}
}

func TestCOSDiskModeRefusesBadArguments(t *testing.T) {
	crt, key, _ := writeCert(t)
	tok := writeToken(t, strings.Repeat("d", 32))
	base := []string{"--mode", "cos-disk-server", "--token-file", tok, "--tls-cert-file", crt, "--tls-key-file", key}
	for name, c := range map[string][]string{
		"no source": base,
		"with root": append([]string{"--cos-source", cosSourceJSON, "--root", "/x"}, base...),
		"no token":  {"--mode", "cos-disk-server", "--cos-source", cosSourceJSON, "--tls-cert-file", crt, "--tls-key-file", key},
		"no cert":   {"--mode", "cos-disk-server", "--cos-source", cosSourceJSON, "--token-file", tok},
		"bad cache": append([]string{"--cos-source", cosSourceJSON, "--cache-mb", "0"}, base...),
		"bad ahead": append([]string{"--cos-source", cosSourceJSON, "--read-ahead", "100"}, base...),
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), c, &out, &errb, nil); code != 2 {
			t.Errorf("%s: exit %d (%s)", name, code, errb.String())
		}
	}
}
