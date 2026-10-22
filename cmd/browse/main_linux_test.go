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
)

func TestRunServesAndEndsWhenIdle(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	token := strings.Repeat("k", 32)
	termLog := filepath.Join(t.TempDir(), "termination-log")
	addrc := make(chan net.Addr, 1)
	exit := make(chan int, 1)
	var out, errb bytes.Buffer
	go func() {
		exit <- run(context.Background(), []string{
			"--mode", "browse-server", "--root", root, "--token-file", writeToken(t, token),
			"--listen", "127.0.0.1:0", "--idle", "400ms", "--max-life", "1m", "--termination-log", termLog,
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
	req, _ := http.NewRequest(http.MethodPost, "http://"+addr.String()+"/v1/list", strings.NewReader(`{"path":[]}`))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
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
	if strings.Contains(out.String()+errb.String(), token) {
		t.Error("the token was printed")
	}
}

func TestRunStopsOnCancel(t *testing.T) {
	root := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	addrc := make(chan net.Addr, 1)
	exit := make(chan int, 1)
	go func() {
		exit <- run(ctx, []string{
			"--mode", "browse-server", "--root", root, "--token-file", writeToken(t, strings.Repeat("k", 32)),
			"--listen", "127.0.0.1:0", "--termination-log", filepath.Join(t.TempDir(), "log"),
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
