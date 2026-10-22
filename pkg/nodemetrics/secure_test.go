package nodemetrics

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"k8s.io/client-go/rest"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// fakeAPI answers TokenReview and SubjectAccessReview. The token "good" is a
// user who may read /metrics, "denied" is a user who may not, others are unknown.
func fakeAPI(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/tokenreviews"):
			auth := strings.Contains(string(body), `"good"`) || strings.Contains(string(body), `"denied"`)
			user := "good"
			if strings.Contains(string(body), `"denied"`) {
				user = "denied"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "authentication.k8s.io/v1", "kind": "TokenReview",
				"status": map[string]any{"authenticated": auth, "user": map[string]any{"username": user}},
			})
		case strings.HasSuffix(r.URL.Path, "/subjectaccessreviews"):
			allowed := strings.Contains(string(body), `"good"`)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"apiVersion": "authorization.k8s.io/v1", "kind": "SubjectAccessReview",
				"status": map[string]any{"allowed": allowed},
			})
		default:
			http.NotFound(w, r)
		}
	}))
}

func TestSecureServer_RefusesUnauthenticatedAndUnauthorised(t *testing.T) {
	api := fakeAPI(t)
	defer api.Close()

	c := NewCollector(baseConfig())
	c.Refresh(context.Background())
	ctrlmetrics.Registry.MustRegister(c)

	cfg := &rest.Config{Host: api.URL}
	hc, err := rest.HTTPClientFor(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	_ = l.Close()

	srv, err := NewSecureServer(addr, cfg, hc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = srv.Start(ctx) }()

	client := &http.Client{Timeout: 5 * time.Second, Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}} //nolint:gosec // self-signed test cert
	get := func(token string) (int, string) {
		var resp *http.Response
		var err error
		for i := 0; i < 50; i++ { // wait for the listener
			req, _ := http.NewRequest(http.MethodGet, "https://"+addr+"/metrics", nil)
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			if resp, err = client.Do(req); err == nil {
				break
			}
			time.Sleep(100 * time.Millisecond)
		}
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		defer resp.Body.Close() //nolint:errcheck
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}

	if code, _ := get(""); code != http.StatusUnauthorized {
		t.Errorf("no token: want 401, got %d", code)
	}
	if code, body := get("unknown"); code < 400 || strings.Contains(body, "vpc_file_pool_share") {
		t.Errorf("bad token: want a refusal, got %d", code)
	}
	if code, _ := get("denied"); code != http.StatusForbidden {
		t.Errorf("token without rights: want 403, got %d", code)
	}
	code, body := get("good")
	if code != http.StatusOK || !strings.Contains(body, "vpc_file_pool_share_used_bytes") {
		t.Errorf("good token: want 200 with metrics, got %d", code)
	}
}
