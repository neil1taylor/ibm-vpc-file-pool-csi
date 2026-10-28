package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/cosbrowse"
)

const testVolume = "pvc-11111111-2222-3333-4444-555555555555"

const cosSourceJSON = `{"endpoint":"s3.eu-de.cloud-object-storage.appdomain.cloud","bucket":"my-bucket","prefix":"p/pol/","policy":"pol",` +
	`"volume":"` + testVolume + `","keyProtect":{"region":"eu-de","instanceID":"i1","rootKeyID":"r1"}}`

// memRepo is a repository with one export: a latest copy with one file.
type memRepo map[string][]byte

func (m memRepo) Snapshots(context.Context) ([]cosbrowse.Snapshot, error) {
	return []cosbrowse.Snapshot{{
		ID: "s1", SourcePath: "/share0/backup/pol/pvcs/" + testVolume, Volume: testVolume,
		Moment: time.Date(2026, 10, 22, 9, 15, 0, 0, time.UTC), Root: "kroot", RootMode: 0o755,
	}}, nil
}

func (m memRepo) Object(_ context.Context, oid string) (io.ReadCloser, error) {
	b, ok := m[oid]
	if !ok {
		return nil, errors.New("not found")
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

func withFakeCOS(t *testing.T, fn func(cosbrowse.Source, time.Time, string, string) (*cosbrowse.Opened, error)) {
	t.Helper()
	old := openCOS
	openCOS = func(_ context.Context, src cosbrowse.Source, at time.Time, kopia, state string, _ io.Writer) (*cosbrowse.Opened, error) {
		return fn(src, at, kopia, state)
	}
	t.Cleanup(func() { openCOS = old })
}

func TestCOSModeServesTheBackupOverTLS(t *testing.T) {
	repo := memRepo{"kroot": []byte(`{"stream":"kopia:directory","entries":[{"name":"hello.txt","type":"f","mode":"0644","size":5,"mtime":"2026-10-22T09:00:00Z","obj":"kfile"}]}`),
		"kfile": []byte("hello")}
	var gotSource cosbrowse.Source
	var gotKopia, gotState string
	withFakeCOS(t, func(src cosbrowse.Source, at time.Time, kopia, state string) (*cosbrowse.Opened, error) {
		gotSource, gotKopia, gotState = src, kopia, state
		return cosbrowse.OpenRepo(context.Background(), repo, cosbrowse.Params{Policy: src.Policy, Volume: src.Volume, At: at})
	})
	token := strings.Repeat("c", 32)
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
			"--mode", "browse-server", "--cos-source", cosSourceJSON, "--kopia", "/opt/kopia", "--kopia-state-dir", "/scratch/k",
			"--token-file", writeToken(t, token), "--listen", "127.0.0.1:0", "--idle", "1m", "--max-life", "1m",
			"--termination-log", filepath.Join(t.TempDir(), "term"), "--tls-cert-file", crt, "--tls-key-file", key,
		}, &out, &errb, func(a net.Addr) { addrc <- a })
	}()
	var addr net.Addr
	select {
	case addr = <-addrc:
	case code := <-exit:
		t.Fatalf("exited with %d: %s", code, errb.String())
	case <-time.After(5 * time.Second):
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
		Entries []struct{ Name []byte }
	}
	if err := json.Unmarshal(post("/v1/list", `{"path":[]}`), &page); err != nil || len(page.Entries) != 1 || string(page.Entries[0].Name) != "hello.txt" {
		t.Fatalf("list = %+v, %v", page, err)
	}
	if got := post("/v1/file", `{"path":["aGVsbG8udHh0"]}`); string(got) != "hello" {
		t.Errorf("file = %q", got)
	}
	var info struct {
		Moment string
		Latest bool
	}
	if err := json.Unmarshal(post("/v1/info", `{}`), &info); err != nil || info.Moment != "20261022T091500Z" || !info.Latest {
		t.Errorf("info = %+v, %v", info, err)
	}
	if gotSource.Volume != testVolume || gotSource.Policy != "pol" || gotKopia != "/opt/kopia" || gotState != "/scratch/k" {
		t.Errorf("source = %+v, kopia = %s, state = %s", gotSource, gotKopia, gotState)
	}
	if !strings.Contains(out.String(), "object IDs from the COS repository") {
		t.Errorf("stdout = %q", out.String())
	}
	if strings.Contains(out.String()+errb.String(), token) {
		t.Error("the token was printed")
	}
	cancel()
	select {
	case <-exit:
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
}

func TestCOSModePutsAPlainReasonInTheTerminationLogWhenItCannotStart(t *testing.T) {
	withFakeCOS(t, func(cosbrowse.Source, time.Time, string, string) (*cosbrowse.Opened, error) {
		return nil, errors.New("the backup key could not be unwrapped: Key Protect answered 403")
	})
	crt, key, _ := writeCert(t)
	termLog := filepath.Join(t.TempDir(), "term")
	var out, errb bytes.Buffer
	code := run(context.Background(), []string{
		"--mode", "browse-server", "--cos-source", cosSourceJSON, "--token-file", writeToken(t, strings.Repeat("c", 32)),
		"--termination-log", termLog, "--tls-cert-file", crt, "--tls-key-file", key,
	}, &out, &errb, nil)
	if code != 1 {
		t.Fatalf("exit %d", code)
	}
	b, err := os.ReadFile(termLog)
	if err != nil || !strings.Contains(string(b), "Key Protect answered 403") {
		t.Errorf("termination log = %q, %v", b, err)
	}
}

func TestCOSModeRefusesBadArguments(t *testing.T) {
	crt, key, _ := writeCert(t)
	tok := writeToken(t, strings.Repeat("c", 32))
	base := []string{"--mode", "browse-server", "--token-file", tok, "--tls-cert-file", crt, "--tls-key-file", key,
		"--termination-log", filepath.Join(t.TempDir(), "term")}
	for name, c := range map[string]struct {
		args []string
		msg  string
	}{
		"with a root":        {[]string{"--cos-source", cosSourceJSON, "--root", "/x"}, "cannot be used with"},
		"with moments":       {[]string{"--cos-source", cosSourceJSON, "--moments", "/x"}, "cannot be used with"},
		"not json":           {[]string{"--cos-source", "nope"}, "--cos-source is not valid"},
		"an unknown field":   {[]string{"--cos-source", `{"endpoint":"s3.x","extra":1}`}, "--cos-source is not valid"},
		"a bad volume":       {[]string{"--cos-source", strings.Replace(cosSourceJSON, testVolume, "../x", 1)}, "volume"},
		"a hostile endpoint": {[]string{"--cos-source", strings.Replace(cosSourceJSON, "s3.eu-de.cloud-object-storage.appdomain.cloud", "evil.example;ls", 1)}, "endpoint"},
	} {
		t.Run(name, func(t *testing.T) {
			// The real opener validates the source before it does anything.
			var out, errb bytes.Buffer
			code := run(context.Background(), append(append([]string{}, base...), c.args...), &out, &errb, nil)
			if code == 0 || !strings.Contains(errb.String(), c.msg) {
				t.Errorf("exit %d, stderr %q does not mention %q", code, errb.String(), c.msg)
			}
		})
	}
}
