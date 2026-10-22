//go:build linux

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

func TestRunServesAndEndsWhenIdle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("k", 32)
	crt, key, certPEM := writeCert(t)
	client, err := browse.PinnedClient(certPEM)
	if err != nil {
		t.Fatal(err)
	}
	termLog := filepath.Join(t.TempDir(), "termination-log")
	addrc := make(chan net.Addr, 1)
	exit := make(chan int, 1)
	var out, errb bytes.Buffer
	go func() {
		exit <- run(context.Background(), []string{
			"--mode", "browse-server", "--root", root, "--token-file", writeToken(t, token),
			"--listen", "127.0.0.1:0", "--idle", "400ms", "--max-life", "1m", "--termination-log", termLog,
			"--tls-cert-file", crt, "--tls-key-file", key,
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
	req, _ := http.NewRequest(http.MethodPost, "https://"+addr.String()+"/v1/list", strings.NewReader(`{"path":[]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	var page struct {
		Entries []struct{ Name []byte }
	}
	_ = json.NewDecoder(resp.Body).Decode(&page)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 || len(page.Entries) != 1 || string(page.Entries[0].Name) != "a.txt" {
		t.Fatalf("list: %d %+v", resp.StatusCode, page)
	}
	select {
	case code := <-exit:
		if code != 0 {
			t.Errorf("exit %d: %s", code, errb.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not end when idle")
	}
	b, _ := os.ReadFile(termLog)
	if string(b) != "idle" {
		t.Errorf("termination log = %q, want idle", b)
	}
	if !strings.Contains(out.String(), "openat2") {
		t.Errorf("the start line should say how names are opened: %q", out.String())
	}
	if strings.Contains(out.String()+errb.String(), token) {
		t.Error("the token was printed")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	root := t.TempDir()
	crt, key, _ := writeCert(t)
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan net.Addr, 1)
	exit := make(chan int, 1)
	go func() {
		exit <- run(ctx, []string{
			"--mode", "browse-server", "--root", root, "--token-file", writeToken(t, strings.Repeat("k", 32)),
			"--listen", "127.0.0.1:0", "--termination-log", filepath.Join(t.TempDir(), "log"),
			"--tls-cert-file", crt, "--tls-key-file", key,
		}, &bytes.Buffer{}, &bytes.Buffer{}, func(a net.Addr) { addrc <- a })
	}()
	<-addrc
	cancel()
	select {
	case code := <-exit:
		if code != 0 {
			t.Errorf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not stop")
	}
}

// serveMoments runs the helper over a latest folder and a moments folder and
// returns what /v1/info and /v1/list say.
func serveMoments(t *testing.T, extra ...string) (info browse.Info, listed []string, code int) {
	t.Helper()
	base := t.TempDir()
	latest, moments := filepath.Join(base, "latest"), filepath.Join(base, "moments")
	for name, content := range map[string]string{
		"latest/new.txt":                     "",
		"moments/20261022T090000Z/one.txt":   "",
		"moments/20261022T100000Z/two.txt":   "",
		"moments/20261022T110000Z/three.txt": "",
		"moments/not-a-moment/x.txt":         "",
	} {
		p := filepath.Join(base, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	token := strings.Repeat("k", 32)
	crt, key, certPEM := writeCert(t)
	client, _ := browse.PinnedClient(certPEM)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addrc := make(chan net.Addr, 1)
	exit := make(chan int, 1)
	var errb bytes.Buffer
	args := append([]string{"--mode", "browse-server", "--root", latest, "--moments", moments, "--token-file", writeToken(t, token),
		"--listen", "127.0.0.1:0", "--termination-log", filepath.Join(t.TempDir(), "log"), "--tls-cert-file", crt, "--tls-key-file", key}, extra...)
	go func() { exit <- run(ctx, args, &bytes.Buffer{}, &errb, func(a net.Addr) { addrc <- a }) }()
	var addr net.Addr
	select {
	case addr = <-addrc:
	case c := <-exit:
		return browse.Info{}, nil, c
	case <-time.After(5 * time.Second):
		t.Fatal("did not start")
	}
	call := func(path, body string, v any) {
		req, _ := http.NewRequest(http.MethodPost, "https://"+addr.String()+path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		_ = json.NewDecoder(resp.Body).Decode(v)
	}
	call("/v1/info", `{}`, &info)
	var page struct{ Entries []struct{ Name []byte } }
	call("/v1/list", `{"path":[]}`, &page)
	for _, e := range page.Entries {
		listed = append(listed, string(e.Name))
	}
	return info, listed, 0
}

func TestRunReadsTheLatestOrAMoment(t *testing.T) {
	info, listed, _ := serveMoments(t)
	if len(listed) != 1 || listed[0] != "new.txt" || info.Moment != "20261022T110000Z" || !info.Latest {
		t.Errorf("latest: info %+v, listed %v", info, listed)
	}
	info, listed, _ = serveMoments(t, "--at", "2026-10-22T10:30:00Z")
	if len(listed) != 1 || listed[0] != "two.txt" || info.Moment != "20261022T100000Z" || info.Latest {
		t.Errorf("moment: info %+v, listed %v", info, listed)
	}
	info, listed, _ = serveMoments(t, "--at", "2026-10-23T00:00:00Z")
	if len(listed) != 1 || listed[0] != "three.txt" || !info.Latest || info.Moment != "20261022T110000Z" {
		t.Errorf("after the newest the newest moment folder is read: info %+v, listed %v", info, listed)
	}
	if _, _, code := serveMoments(t, "--at", "2026-10-22T08:00:00Z"); code != 1 {
		t.Errorf("a time before the first moment must fail, exit %d", code)
	}
}
