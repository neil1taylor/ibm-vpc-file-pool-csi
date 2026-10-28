package guestfs

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestExecRunner_Output(t *testing.T) {
	r := &ExecRunner{Env: []string{"PATH=/usr/bin:/bin"}}
	out, err := r.Output(context.Background(), []string{"echo", "-n", "hi there"})
	if err != nil || string(out) != "hi there" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestExecRunner_NoShell(t *testing.T) {
	r := &ExecRunner{Env: []string{"PATH=/usr/bin:/bin"}}
	out, err := r.Output(context.Background(), []string{"echo", "$(id);", "`id`", "a b"})
	if err != nil || strings.TrimSpace(string(out)) != "$(id); `id` a b" {
		t.Fatalf("%q %v", out, err)
	}
}

func TestExecRunner_FailureKeepsStderr(t *testing.T) {
	r := &ExecRunner{Env: []string{"PATH=/usr/bin:/bin"}}
	_, err := r.Output(context.Background(), []string{"sh", "-c", "echo oops >&2; exit 3"})
	var ee *ExitError
	if !errors.As(err, &ee) || !strings.Contains(ee.Stderr, "oops") {
		t.Fatalf("%v", err)
	}
	if strings.Contains(err.Error(), "oops") {
		t.Error("stderr must not be in the error text")
	}
}

func TestExecRunner_OutputLimit(t *testing.T) {
	r := &ExecRunner{Env: []string{"PATH=/usr/bin:/bin"}, MaxOutput: 10}
	if _, err := r.Output(context.Background(), []string{"sh", "-c", "yes | head -c 1000"}); err == nil {
		t.Fatal("output over the limit must fail")
	}
}

func TestExecRunner_Timeout(t *testing.T) {
	r := &ExecRunner{Env: []string{"PATH=/usr/bin:/bin"}, Timeout: 200 * time.Millisecond}
	start := time.Now()
	if _, err := r.Output(context.Background(), []string{"sleep", "30"}); err == nil {
		t.Fatal("must time out")
	}
	if time.Since(start) > 10*time.Second {
		t.Error("took too long")
	}
}

func TestExecRunner_Stream(t *testing.T) {
	r := &ExecRunner{Env: []string{"PATH=/usr/bin:/bin"}}
	rc, err := r.Stream(context.Background(), []string{"printf", "abc"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := io.ReadAll(rc)
	_ = rc.Close()
	if err != nil || string(b) != "abc" {
		t.Fatalf("%q %v", b, err)
	}
}

func TestExecRunner_StreamThatFailsEndsWithAnError(t *testing.T) {
	r := &ExecRunner{Env: []string{"PATH=/usr/bin:/bin"}}
	rc, err := r.Stream(context.Background(), []string{"sh", "-c", "printf abc; exit 2"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rc.Close() }()
	if _, err = io.ReadAll(rc); err == nil {
		t.Fatal("a cut stream must not look whole")
	}
}

func TestExecRunner_CloseStopsTheTool(t *testing.T) {
	r := &ExecRunner{Env: []string{"PATH=/usr/bin:/bin"}}
	rc, err := r.Stream(context.Background(), []string{"sleep", "30"})
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() { _ = rc.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Close did not stop the tool")
	}
}

func TestEnvironment(t *testing.T) {
	has := func(env []string, want string) bool {
		for _, e := range env {
			if e == want {
				return true
			}
		}
		return false
	}
	with := Environment("/opt/appliance", true)
	if !has(with, "LIBGUESTFS_BACKEND=direct") || !has(with, "LIBGUESTFS_PATH=/opt/appliance") || !has(with, "LIBGUESTFS_TMPDIR=/tmp") {
		t.Errorf("env = %v", with)
	}
	for _, e := range with {
		if strings.HasPrefix(e, "LIBGUESTFS_BACKEND_SETTINGS") {
			t.Errorf("with KVM the appliance is not emulated: %v", with)
		}
	}
	if !has(Environment("", false), "LIBGUESTFS_BACKEND_SETTINGS=force_tcg") {
		t.Error("without KVM the appliance must be emulated")
	}
}
