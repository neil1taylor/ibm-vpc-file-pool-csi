package main

import (
	"bytes"
	"context"
	"encoding/json"
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
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosdisk"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/filelist"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/guestfs"
)

// otherTools lists a different name than the file list does, so a listing shows
// which one answered.
type otherTools struct{ fakeTools }

func (otherTools) Output(ctx context.Context, args []string) ([]byte, error) {
	if args[0] == "guestls" {
		return []byte("d,0755,4096,0,0,0,1700000000,0,/\n-,0644,5,0,0,0,1700000000,0,/from-disk.txt\n"), nil
	}
	return fakeTools{}.Output(ctx, args)
}

type memDisk []byte

func (m memDisk) Size() int64 { return int64(len(m)) }
func (m memDisk) ReadRange(_ context.Context, p []byte, off int64) (int, error) {
	return copy(p, m[off:]), nil
}

func listBytes(t *testing.T, moment time.Time) []byte {
	t.Helper()
	l := filelist.NewList(moment)
	l.AddFilesystem(filelist.Filesystem{ID: "sda1", VFS: "xfs", Readable: true})
	if err := l.AddFolder("sda1", nil, []browse.Entry{{Name: []byte("from-list.txt"), Type: browse.TypeFile, Size: 5, Mode: 0o644, Readable: true}}); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := l.Encode(&buf); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// diskServer starts a real disk server handler over TLS and returns its URL,
// the certificate file for the client and the token file.
func diskServer(t *testing.T, list []byte, momentHeader string) (url, certFile, tokenFile string) {
	t.Helper()
	var extra []cosdisk.HandlerOption
	if list != nil {
		extra = append(extra, cosdisk.WithFileList(func(context.Context) (io.ReadCloser, int64, error) {
			return io.NopCloser(bytes.NewReader(list)), int64(len(list)), nil
		}))
	}
	return diskServerWith(t, momentHeader, extra...)
}

// diskServerWith is diskServer with more handler options.
func diskServerWith(t *testing.T, momentHeader string, extra ...cosdisk.HandlerOption) (url, certFile, tokenFile string) {
	t.Helper()
	certPEM, keyPEM, err := browse.NewSessionCert(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _ := browse.ServerTLSConfig(certPEM, keyPEM)
	token := strings.Repeat("k", 32)
	d := cosdisk.New(memDisk(make([]byte, 1<<20)), cosdisk.Options{})
	t.Cleanup(func() { _ = d.Close() })
	opts := []cosdisk.HandlerOption{cosdisk.WithOnAuthed(func(w http.ResponseWriter, _ *http.Request) func() {
		w.Header().Set(cosdisk.MomentHeader, momentHeader)
		return func() {}
	})}
	opts = append(opts, extra...)
	h, _ := cosdisk.NewHandler(d, token, opts...)
	ts := httptest.NewUnstartedServer(h)
	ts.TLS = cfg
	ts.StartTLS()
	t.Cleanup(ts.Close)
	certFile = filepath.Join(t.TempDir(), "disk.crt")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return ts.URL + "/disk", certFile, writeToken(t, token)
}

func runRemoteGuest(t *testing.T, diskURL, diskCert, diskToken string) (post func(path, body string) []byte, stop func() int) {
	t.Helper()
	crt, key, certPEM := writeCert(t)
	client, _ := browse.PinnedClient(certPEM)
	token := strings.Repeat("g", 32)
	addrc := make(chan net.Addr, 1)
	exit := make(chan int, 1)
	var out, errb bytes.Buffer
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		exit <- run(ctx, []string{"--mode", "browse-server", "--guest", "--guest-disk-url", diskURL,
			"--guest-disk-cert-file", diskCert, "--guest-disk-token-file", diskToken, "--guest-socket", guestSocket(t),
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
	post = func(path, body string) []byte {
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
	stop = func() int {
		cancel()
		select {
		case c := <-exit:
			return c
		case <-time.After(10 * time.Second):
			t.Fatal("did not stop")
			return -1
		}
	}
	return post, stop
}

func names(t *testing.T, b []byte) []string {
	t.Helper()
	var page struct{ Entries []struct{ Name []byte } }
	if err := json.Unmarshal(b, &page); err != nil {
		t.Fatalf("%v: %s", err, b)
	}
	var out []string
	for _, e := range page.Entries {
		out = append(out, string(e.Name))
	}
	return out
}

func TestRemoteGuestShowsFoldersFromTheFileListWhenItMatches(t *testing.T) {
	old := newToolRunner
	newToolRunner = func(string, bool) guestfs.Runner { return otherTools{} }
	defer func() { newToolRunner = old }()
	moment := time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC)
	url, cert, tok := diskServer(t, listBytes(t, moment.Add(-time.Minute)), moment.Format(time.RFC3339))
	post, stop := runRemoteGuest(t, url, cert, tok)
	if got := names(t, post("/v1/list", `{"path":["c2RhMQ=="]}`)); len(got) != 1 || got[0] != "from-list.txt" {
		t.Errorf("sda1 lists %v; want the file list", got)
	}
	var info browse.Info
	if err := json.Unmarshal(post("/v1/info", `{}`), &info); err != nil || !info.Guest || info.Moment != "20261022T091500Z" {
		t.Errorf("info = %+v, %v", info, err)
	}
	if !strings.Contains(strings.Join(info.Notices, " "), "saved") {
		t.Errorf("notices = %v; the person should be told the folders come from a saved list", info.Notices)
	}
	if code := stop(); code != 0 {
		t.Errorf("exit %d", code)
	}
}

func TestRemoteGuestReadsTheDiskWhenTheListIsForAnotherMoment(t *testing.T) {
	old := newToolRunner
	newToolRunner = func(string, bool) guestfs.Runner { return otherTools{} }
	defer func() { newToolRunner = old }()
	moment := time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC)
	url, cert, tok := diskServer(t, listBytes(t, moment.Add(-48*time.Hour)), moment.Format(time.RFC3339))
	post, stop := runRemoteGuest(t, url, cert, tok)
	defer stop()
	if got := names(t, post("/v1/list", `{"path":["c2RhMQ=="]}`)); len(got) != 1 || got[0] != "from-disk.txt" {
		t.Errorf("sda1 lists %v; want the disk, slowly", got)
	}
}

func TestRemoteGuestReadsTheDiskWhenThereIsNoList(t *testing.T) {
	old := newToolRunner
	newToolRunner = func(string, bool) guestfs.Runner { return otherTools{} }
	defer func() { newToolRunner = old }()
	url, cert, tok := diskServer(t, nil, "2026-10-22T09:15:00Z")
	post, stop := runRemoteGuest(t, url, cert, tok)
	defer stop()
	if got := names(t, post("/v1/list", `{"path":["c2RhMQ=="]}`)); len(got) != 1 || got[0] != "from-disk.txt" {
		t.Errorf("sda1 lists %v", got)
	}
	var info browse.Info
	_ = json.Unmarshal(post("/v1/info", `{}`), &info)
	if !strings.Contains(strings.Join(info.Notices, " "), "slow") {
		t.Errorf("notices = %v; the person should be told the listing is slow", info.Notices)
	}
}

func TestRemoteGuestFailsInWordsWhenTheDiskServerRefuses(t *testing.T) {
	old := newToolRunner
	newToolRunner = func(string, bool) guestfs.Runner { return otherTools{} }
	defer func() { newToolRunner = old }()
	url, cert, _ := diskServer(t, nil, "2026-10-22T09:15:00Z")
	crt, key, _ := writeCert(t)
	term := filepath.Join(t.TempDir(), "term")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{"--mode", "browse-server", "--guest", "--guest-disk-url", url,
		"--guest-disk-cert-file", cert, "--guest-disk-token-file", writeToken(t, strings.Repeat("w", 32)),
		"--guest-socket", guestSocket(t), "--token-file", writeToken(t, strings.Repeat("g", 32)),
		"--termination-log", term, "--tls-cert-file", crt, "--tls-key-file", key}, &out, &errb, nil)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	if b, _ := os.ReadFile(term); !strings.Contains(string(b), "disk server") {
		t.Errorf("termination log = %q", b)
	}
}

func TestRemoteGuestRefusesBadArguments(t *testing.T) {
	crt, key, _ := writeCert(t)
	tok := writeToken(t, strings.Repeat("g", 32))
	base := []string{"--mode", "browse-server", "--guest", "--token-file", tok, "--tls-cert-file", crt, "--tls-key-file", key}
	for name, c := range map[string][]string{
		"plain http":    append([]string{"--guest-disk-url", "http://10.0.0.1/disk", "--guest-disk-cert-file", crt, "--guest-disk-token-file", tok}, base...),
		"with root":     append([]string{"--guest-disk-url", "https://10.0.0.1/disk", "--root", "/x", "--guest-disk-cert-file", crt, "--guest-disk-token-file", tok}, base...),
		"no cert":       append([]string{"--guest-disk-url", "https://10.0.0.1/disk", "--guest-disk-token-file", tok}, base...),
		"no disk token": append([]string{"--guest-disk-url", "https://10.0.0.1/disk", "--guest-disk-cert-file", crt}, base...),
	} {
		var out, errb bytes.Buffer
		if code := run(context.Background(), c, &out, &errb, nil); code != 2 {
			t.Errorf("%s: exit %d (%s)", name, code, errb.String())
		}
	}
}

func TestRemoteGuestListsTheRestorePointsTheDiskServerHolds(t *testing.T) {
	old := newToolRunner
	newToolRunner = func(string, bool) guestfs.Runner { return otherTools{} }
	defer func() { newToolRunner = old }()
	points := []time.Time{
		time.Date(2026, 10, 20, 9, 0, 0, 7, time.UTC),
		time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC),
		time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC),
	}
	url, cert, tok := diskServerWith(t, points[2].Format(time.RFC3339),
		cosdisk.WithRestorePoints(func(context.Context) ([]time.Time, error) { return points, nil }))
	post, stop := runRemoteGuest(t, url, cert, tok)
	defer stop()
	var got struct {
		Moments []string `json:"moments"`
	}
	if err := json.Unmarshal(post("/v1/moments", `{}`), &got); err != nil {
		t.Fatal(err)
	}
	want := []string{"20261020T090000Z", "20261021T090000Z", "20261022T091500Z"}
	if strings.Join(got.Moments, ",") != strings.Join(want, ",") {
		t.Errorf("restore points = %v, want %v", got.Moments, want)
	}
}

func TestRemoteGuestListsOnlyTheServedRestorePointFromAServerWithoutAList(t *testing.T) {
	old := newToolRunner
	newToolRunner = func(string, bool) guestfs.Runner { return otherTools{} }
	defer func() { newToolRunner = old }()
	url, cert, tok := diskServer(t, nil, "2026-10-22T09:15:00Z")
	post, stop := runRemoteGuest(t, url, cert, tok)
	defer stop()
	var got struct {
		Moments []string `json:"moments"`
	}
	if err := json.Unmarshal(post("/v1/moments", `{}`), &got); err != nil || len(got.Moments) != 1 || got.Moments[0] != "20261022T091500Z" {
		t.Errorf("restore points = %v, %v", got.Moments, err)
	}
}
