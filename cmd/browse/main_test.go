package main

import (
	"time"

	"bytes"
	"context"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
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

// writeCert writes a session certificate and key and returns the two paths and
// the certificate PEM.
func writeCert(t *testing.T) (certFile, keyFile string, certPEM []byte) {
	t.Helper()
	certPEM, keyPEM, err := browse.NewSessionCert(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key")
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return certFile, keyFile, certPEM
}

func TestRunRefusesBadArguments(t *testing.T) {
	root := t.TempDir()
	tok := writeToken(t, strings.Repeat("t", 32))
	short := writeToken(t, "short")
	crt, key, _ := writeCert(t)
	tlsArgs := []string{"--tls-cert-file", crt, "--tls-key-file", key}
	tests := []struct {
		name string
		args []string
		want int
		msg  string
	}{
		{"no mode", append([]string{"--root", root, "--token-file", tok}, tlsArgs...), 2, "--mode"},
		{"unknown mode", append([]string{"--mode", "shell", "--root", root, "--token-file", tok}, tlsArgs...), 2, "--mode"},
		{"no root", append([]string{"--mode", "browse-server", "--token-file", tok}, tlsArgs...), 2, "--root"},
		{"no token file", append([]string{"--mode", "browse-server", "--root", root}, tlsArgs...), 2, "--token-file"},
		{"token file missing", append([]string{"--mode", "browse-server", "--root", root, "--token-file", "/nonexistent/token"}, tlsArgs...), 1, "token"},
		{"token too short", append([]string{"--mode", "browse-server", "--root", root, "--token-file", short}, tlsArgs...), 1, "token"},
		{"no tls files", []string{"--mode", "browse-server", "--root", root, "--token-file", tok}, 2, "--tls"},
		{"tls files missing", []string{"--mode", "browse-server", "--root", root, "--token-file", tok, "--tls-cert-file", "/nonexistent/c", "--tls-key-file", "/nonexistent/k"}, 1, "certificate"},
		{"at without moments", append([]string{"--mode", "browse-server", "--root", root, "--token-file", tok, "--at", "2026-10-22T10:00:00Z"}, tlsArgs...), 2, "--moments"},
		{"bad at", append([]string{"--mode", "browse-server", "--root", root, "--token-file", tok, "--moments", root, "--at", "yesterday"}, tlsArgs...), 2, "--at"},
		{"unknown flag", []string{"--mode", "browse-server", "--nope"}, 2, "nope"},
		{"root is a file", append([]string{"--mode", "browse-server", "--root", tok, "--token-file", tok}, tlsArgs...), 1, "root"},
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
