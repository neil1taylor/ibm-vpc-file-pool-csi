package replication

import (
	"context"
	"encoding/pem"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const checkToken = "check-token"

func checkContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// reasonOf returns the Reason of a CheckError, or "" when err is nil or another kind of error.
func reasonOf(err error) string {
	var ce *CheckError
	if errors.As(err, &ce) {
		return ce.Reason
	}
	return ""
}

func TestReceiver_AuthCheck(t *testing.T) {
	r := NewReceiver(t.TempDir(), checkToken, ":0")

	tests := []struct {
		name   string
		method string
		auth   string
		want   int
	}{
		{"valid token", http.MethodGet, "Bearer " + checkToken, http.StatusOK},
		{"no token", http.MethodGet, "", http.StatusUnauthorized},
		{"wrong token", http.MethodGet, "Bearer nope", http.StatusUnauthorized},
		{"wrong method", http.MethodPost, "Bearer " + checkToken, http.StatusMethodNotAllowed},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(tt.method, "/api/v1/auth-check", nil)
			if tt.auth != "" {
				req.Header.Set("Authorization", tt.auth)
			}
			w := httptest.NewRecorder()
			r.httpServer.Handler.ServeHTTP(w, req)
			if w.Code != tt.want {
				t.Fatalf("got %d, want %d: %s", w.Code, tt.want, w.Body.String())
			}
			if tt.want == http.StatusOK && !strings.Contains(w.Body.String(), `"status":"ok"`) {
				t.Errorf("body has no ok status: %s", w.Body.String())
			}
		})
	}
}

func TestReceiver_AuthCheck_StorageNotReachable(t *testing.T) {
	r := NewReceiver("/nonexistent/path/that/does/not/exist", checkToken, ":0")
	req := httptest.NewRequest(http.MethodGet, "/api/v1/auth-check", nil)
	req.Header.Set("Authorization", "Bearer "+checkToken)
	w := httptest.NewRecorder()
	r.httpServer.Handler.ServeHTTP(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("got %d, want 503", w.Code)
	}
}

func TestSyncClient_Check_OK(t *testing.T) {
	srv := httptest.NewServer(NewReceiver(t.TempDir(), checkToken, ":0").httpServer.Handler)
	defer srv.Close()
	if err := NewSyncClient(srv.URL, checkToken).Check(checkContext(t)); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestSyncClient_Check_TokenRefused(t *testing.T) {
	srv := httptest.NewServer(NewReceiver(t.TempDir(), checkToken, ":0").httpServer.Handler)
	defer srv.Close()
	err := NewSyncClient(srv.URL, "wrong").Check(checkContext(t))
	if got := reasonOf(err); got != ReasonTokenRefused {
		t.Fatalf("reason = %q (err %v), want %q", got, err, ReasonTokenRefused)
	}
}

func TestSyncClient_Check_CannotConnect(t *testing.T) {
	srv := httptest.NewServer(http.NotFoundHandler())
	url := srv.URL
	srv.Close()
	err := NewSyncClient(url, checkToken).Check(checkContext(t))
	if got := reasonOf(err); got != ReasonCannotConnect {
		t.Fatalf("reason = %q (err %v), want %q", got, err, ReasonCannotConnect)
	}
}

func TestSyncClient_Check_CertificateNotTrusted(t *testing.T) {
	srv := httptest.NewTLSServer(NewReceiver(t.TempDir(), checkToken, ":0").httpServer.Handler)
	defer srv.Close()
	err := NewSyncClient(srv.URL, checkToken).Check(checkContext(t))
	if got := reasonOf(err); got != ReasonCertNotTrusted {
		t.Fatalf("reason = %q (err %v), want %q", got, err, ReasonCertNotTrusted)
	}
}

func TestSyncClient_Check_TrustedWithCA(t *testing.T) {
	srv := httptest.NewTLSServer(NewReceiver(t.TempDir(), checkToken, ":0").httpServer.Handler)
	defer srv.Close()
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	c, err := NewSyncClientWithCA(srv.URL, checkToken, caPEM)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Check(checkContext(t)); err != nil {
		t.Fatalf("expected ok with the CA, got %v", err)
	}
}

func TestSyncClient_Check_StorageNotReachable(t *testing.T) {
	srv := httptest.NewServer(NewReceiver("/nonexistent/path/that/does/not/exist", checkToken, ":0").httpServer.Handler)
	defer srv.Close()
	err := NewSyncClient(srv.URL, checkToken).Check(checkContext(t))
	if got := reasonOf(err); got != ReasonStorageNotReachable {
		t.Fatalf("reason = %q (err %v), want %q", got, err, ReasonStorageNotReachable)
	}
}

func TestSyncClient_Check_ReceiverTooOld(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	err := NewSyncClient(srv.URL, checkToken).Check(checkContext(t))
	if got := reasonOf(err); got != ReasonReceiverTooOld {
		t.Fatalf("reason = %q (err %v), want %q", got, err, ReasonReceiverTooOld)
	}
}

func TestCheckReceiver_ReadsTokenAndCAFiles(t *testing.T) {
	srv := httptest.NewTLSServer(NewReceiver(t.TempDir(), checkToken, ":0").httpServer.Handler)
	defer srv.Close()
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	caFile := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(tokenFile, []byte(checkToken+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	if err := os.WriteFile(caFile, caPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := CheckReceiver(checkContext(t), srv.URL, tokenFile, caFile); err != nil {
		t.Fatalf("expected ok, got %v", err)
	}
}

func TestCheckReceiver_MissingTokenFile(t *testing.T) {
	err := CheckReceiver(checkContext(t), "https://example.invalid", filepath.Join(t.TempDir(), "absent"), "")
	if got := reasonOf(err); got != ReasonBadInput {
		t.Fatalf("reason = %q (err %v), want %q", got, err, ReasonBadInput)
	}
}

func TestCheckReceiver_BadCAFile(t *testing.T) {
	dir := t.TempDir()
	tokenFile := filepath.Join(dir, "token")
	caFile := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(tokenFile, []byte(checkToken), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := CheckReceiver(checkContext(t), "https://example.invalid", tokenFile, caFile)
	if got := reasonOf(err); got != ReasonBadInput {
		t.Fatalf("reason = %q (err %v), want %q", got, err, ReasonBadInput)
	}
}
