package hooks

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

func TestHTTPHook_Execute_Success(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if r.Header.Get("X-Snapshot-Id") != "snap-123" {
			t.Errorf("expected header X-Snapshot-Id=snap-123, got %s", r.Header.Get("X-Snapshot-Id"))
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"quiesced"}`))
	}))
	defer server.Close()

	hook := NewHTTPHook(server.Client())

	h := v1alpha1.Hook{
		Name:           "notify-app",
		Type:           v1alpha1.HookTypeHTTP,
		TimeoutSeconds: 10,
		HTTP: &v1alpha1.HTTPHookSpec{
			URL:     server.URL,
			Method:  "POST",
			Headers: map[string]string{"X-Snapshot-Id": "snap-123"},
		},
	}

	result, err := hook.Execute(context.Background(), h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success, got failure: %s", result.Message)
	}
}

func TestHTTPHook_Execute_ServerError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("internal error"))
	}))
	defer server.Close()

	hook := NewHTTPHook(server.Client())

	h := v1alpha1.Hook{
		Name: "notify-app",
		Type: v1alpha1.HookTypeHTTP,
		HTTP: &v1alpha1.HTTPHookSpec{
			URL:    server.URL,
			Method: "POST",
		},
	}

	result, err := hook.Execute(context.Background(), h)
	if err == nil {
		t.Fatal("expected error for 500 response")
	}
	if result.Success {
		t.Fatal("expected failure result")
	}
}

func TestHTTPHook_Execute_DefaultMethod(t *testing.T) {
	var receivedMethod string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedMethod = r.Method
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	hook := NewHTTPHook(server.Client())

	h := v1alpha1.Hook{
		Name: "test",
		Type: v1alpha1.HookTypeHTTP,
		HTTP: &v1alpha1.HTTPHookSpec{
			URL:    server.URL,
			Method: "", // should default to POST
		},
	}

	_, err := hook.Execute(context.Background(), h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if receivedMethod != "POST" {
		t.Fatalf("expected POST, got %s", receivedMethod)
	}
}

func TestHTTPHook_Execute_GetMethod(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("expected GET, got %s", r.Method)
		}
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("healthy"))
	}))
	defer server.Close()

	hook := NewHTTPHook(server.Client())

	h := v1alpha1.Hook{
		Name: "health-check",
		Type: v1alpha1.HookTypeHTTP,
		HTTP: &v1alpha1.HTTPHookSpec{
			URL:    server.URL,
			Method: "GET",
		},
	}

	result, err := hook.Execute(context.Background(), h)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Success {
		t.Fatalf("expected success: %s", result.Message)
	}
}

func TestHTTPHook_Execute_ConnectionError(t *testing.T) {
	hook := NewHTTPHook(&http.Client{})

	h := v1alpha1.Hook{
		Name: "unreachable",
		Type: v1alpha1.HookTypeHTTP,
		HTTP: &v1alpha1.HTTPHookSpec{
			URL:    "http://127.0.0.1:1", // port 1 should be unreachable
			Method: "POST",
		},
	}

	result, err := hook.Execute(context.Background(), h)
	if err == nil {
		t.Fatal("expected error for connection failure")
	}
	if result.Success {
		t.Fatal("expected failure result")
	}
}
