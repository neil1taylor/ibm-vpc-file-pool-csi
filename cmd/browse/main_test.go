package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeToken(t *testing.T, tok string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(p, []byte(tok+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestRunRefusesBadArguments(t *testing.T) {
	root := t.TempDir()
	tok := writeToken(t, strings.Repeat("t", 32))
	short := writeToken(t, "short")
	tests := []struct {
		name string
		args []string
		want int
		msg  string
	}{
		{"no mode", []string{"--root", root, "--token-file", tok}, 2, "--mode"},
		{"unknown mode", []string{"--mode", "shell", "--root", root, "--token-file", tok}, 2, "--mode"},
		{"no root", []string{"--mode", "browse-server", "--token-file", tok}, 2, "--root"},
		{"no token file", []string{"--mode", "browse-server", "--root", root}, 2, "--token-file"},
		{"token file missing", []string{"--mode", "browse-server", "--root", root, "--token-file", "/nonexistent/token"}, 1, "token"},
		{"token too short", []string{"--mode", "browse-server", "--root", root, "--token-file", short}, 1, "token"},
		{"unknown flag", []string{"--mode", "browse-server", "--nope"}, 2, "nope"},
		{"root is a file", []string{"--mode", "browse-server", "--root", tok, "--token-file", tok}, 1, "root"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var out, errb bytes.Buffer
			got := run(context.Background(), tt.args, &out, &errb, nil)
			if got != tt.want {
				t.Errorf("exit %d, want %d (stderr: %s)", got, tt.want, errb.String())
			}
			if !strings.Contains(errb.String(), tt.msg) {
				t.Errorf("stderr %q does not mention %q", errb.String(), tt.msg)
			}
			if strings.Contains(errb.String()+out.String(), strings.Repeat("t", 32)) {
				t.Error("the token was printed")
			}
		})
	}
}
