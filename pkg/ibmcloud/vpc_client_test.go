package ibmcloud

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// --- Helper tests (pure, table-driven) ---

func TestRegionFromZone(t *testing.T) {
	tests := []struct {
		zone string
		want string
	}{
		{"us-south-1", "us-south"},
		{"us-south-2", "us-south"},
		{"eu-de-2", "eu-de"},
		{"jp-tok-3", "jp-tok"},
		{"au-syd-1", "au-syd"},
		{"", ""},
		{"single", "single"},
	}

	for _, tt := range tests {
		t.Run(tt.zone, func(t *testing.T) {
			got := regionFromZone(tt.zone)
			if got != tt.want {
				t.Errorf("regionFromZone(%q) = %q, want %q", tt.zone, got, tt.want)
			}
		})
	}
}

func TestMapHTTPError(t *testing.T) {
	sdkErr := fmt.Errorf("sdk error")

	tests := []struct {
		name       string
		statusCode int
		wantErr    error
	}{
		{"not found", 404, ErrShareNotFound},
		{"rate limit", 429, ErrAPIRateLimit},
		{"unauthorized", 401, ErrAuthentication},
		{"forbidden", 403, ErrAuthentication},
		{"server error 500", 500, nil}, // wrapped, not sentinel
		{"server error 503", 503, nil},
		{"client error 400", 400, nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := mapHTTPError(tt.statusCode, sdkErr)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
				t.Errorf("mapHTTPError(%d) should wrap %v, got: %v", tt.statusCode, tt.wantErr, err)
			}
		})
	}
}

func TestParseStartFromURL(t *testing.T) {
	tests := []struct {
		name string
		href string
		want *string
	}{
		{"valid URL with start", "https://example.com/v1/shares?start=abc123&limit=100", strPtr("abc123")},
		{"no start param", "https://example.com/v1/shares?limit=100", nil},
		{"empty URL", "", nil},
		{"invalid URL", "://bad", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseStartFromURL(tt.href)
			if tt.want == nil {
				if got != nil {
					t.Errorf("parseStartFromURL(%q) = %q, want nil", tt.href, *got)
				}
			} else {
				if got == nil {
					t.Errorf("parseStartFromURL(%q) = nil, want %q", tt.href, *tt.want)
				} else if *got != *tt.want {
					t.Errorf("parseStartFromURL(%q) = %q, want %q", tt.href, *got, *tt.want)
				}
			}
		})
	}
}

// --- fakeSecretProvider for auth tests ---

type fakeSecretProvider struct {
	token    string
	tokenErr error

	riaasEndpoint        string
	riaasEndpointErr     error
	privateEndpoint      string
	privateEndpointErr   error
}

func (f *fakeSecretProvider) GetDefaultIAMToken(_ bool, _ ...string) (string, uint64, error) {
	if f.tokenErr != nil {
		return "", 0, f.tokenErr
	}
	return f.token, 3600, nil
}

func (f *fakeSecretProvider) GetRIAASEndpoint(_ bool) (string, error) {
	return f.riaasEndpoint, f.riaasEndpointErr
}

func (f *fakeSecretProvider) GetPrivateRIAASEndpoint(_ bool) (string, error) {
	return f.privateEndpoint, f.privateEndpointErr
}

// --- Auth flow tests ---

func TestNewClientWithProvider_Success(t *testing.T) {
	sp := &fakeSecretProvider{
		token:           "test-token",
		riaasEndpoint:   "https://us-south.iaas.cloud.ibm.com/v1",
	}

	client, err := NewClientWithProvider(sp, "us-south")
	if err != nil {
		t.Fatalf("NewClientWithProvider() error = %v", err)
	}
	if client == nil {
		t.Fatal("NewClientWithProvider() returned nil client")
	}
	if client.region != "us-south" {
		t.Errorf("client.region = %q, want %q", client.region, "us-south")
	}
}

func TestNewClientWithProvider_TokenError(t *testing.T) {
	sp := &fakeSecretProvider{
		tokenErr: fmt.Errorf("no credentials"),
	}

	_, err := NewClientWithProvider(sp, "us-south")
	if err == nil {
		t.Fatal("NewClientWithProvider() should have returned error")
	}
	if !contains(err.Error(), "IAM token") {
		t.Errorf("error should mention IAM token, got: %v", err)
	}
}

func TestNewClientWithProvider_EndpointSelection(t *testing.T) {
	tests := []struct {
		name            string
		privateEndpoint string
		privateErr      error
		publicEndpoint  string
		publicErr       error
		region          string
	}{
		{
			name:            "uses private endpoint",
			privateEndpoint: "https://private.iaas.cloud.ibm.com/v1",
		},
		{
			name:           "falls back to public",
			privateErr:     fmt.Errorf("not configured"),
			publicEndpoint: "https://us-south.iaas.cloud.ibm.com/v1",
		},
		{
			name:       "falls back to region-derived",
			privateErr: fmt.Errorf("not configured"),
			publicErr:  fmt.Errorf("not configured"),
			region:     "eu-de",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := &fakeSecretProvider{
				token:              "test-token",
				privateEndpoint:    tt.privateEndpoint,
				privateEndpointErr: tt.privateErr,
				riaasEndpoint:      tt.publicEndpoint,
				riaasEndpointErr:   tt.publicErr,
			}

			region := tt.region
			if region == "" {
				region = "us-south"
			}

			client, err := NewClientWithProvider(sp, region)
			if err != nil {
				t.Fatalf("NewClientWithProvider() error = %v", err)
			}
			if client == nil {
				t.Fatal("client is nil")
			}
		})
	}
}

// --- waitForShareStable tests ---

func TestWaitForShareStable_ImmediatelyStable(t *testing.T) {
	c := &Client{
		pollInterval: 1 * time.Millisecond,
		getShareFunc: func(_ context.Context, _ string) (*ShareInfo, error) {
			return &ShareInfo{
				ID:             "share-1",
				LifecycleState: "stable",
				SizeGB:         100,
			}, nil
		},
	}

	info, err := c.waitForShareStable(context.Background(), "share-1", 5*time.Second)
	if err != nil {
		t.Fatalf("waitForShareStable() error = %v", err)
	}
	if info.LifecycleState != "stable" {
		t.Errorf("got state %q, want stable", info.LifecycleState)
	}
}

func TestWaitForShareStable_PendingThenStable(t *testing.T) {
	callCount := 0
	c := &Client{
		pollInterval: 1 * time.Millisecond,
		getShareFunc: func(_ context.Context, _ string) (*ShareInfo, error) {
			callCount++
			if callCount < 3 {
				return &ShareInfo{ID: "share-1", LifecycleState: "pending"}, nil
			}
			return &ShareInfo{ID: "share-1", LifecycleState: "stable", SizeGB: 100}, nil
		},
	}

	info, err := c.waitForShareStable(context.Background(), "share-1", 5*time.Second)
	if err != nil {
		t.Fatalf("waitForShareStable() error = %v", err)
	}
	if info.LifecycleState != "stable" {
		t.Errorf("got state %q, want stable", info.LifecycleState)
	}
	if callCount < 3 {
		t.Errorf("expected at least 3 calls, got %d", callCount)
	}
}

func TestWaitForShareStable_Failed(t *testing.T) {
	c := &Client{
		pollInterval: 1 * time.Millisecond,
		getShareFunc: func(_ context.Context, _ string) (*ShareInfo, error) {
			return &ShareInfo{ID: "share-1", LifecycleState: "failed"}, nil
		},
	}

	_, err := c.waitForShareStable(context.Background(), "share-1", 5*time.Second)
	if err == nil {
		t.Fatal("expected error for failed share")
	}
	if !errors.Is(err, ErrShareCreationFailed) {
		t.Errorf("expected ErrShareCreationFailed, got: %v", err)
	}
}

func TestWaitForShareStable_Timeout(t *testing.T) {
	c := &Client{
		pollInterval: 1 * time.Millisecond,
		getShareFunc: func(_ context.Context, _ string) (*ShareInfo, error) {
			return &ShareInfo{ID: "share-1", LifecycleState: "pending"}, nil
		},
	}

	_, err := c.waitForShareStable(context.Background(), "share-1", 10*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, ErrShareNotStable) {
		t.Errorf("expected ErrShareNotStable, got: %v", err)
	}
}

func TestWaitForShareStable_ContextCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	callCount := 0
	c := &Client{
		pollInterval: 50 * time.Millisecond,
		getShareFunc: func(_ context.Context, _ string) (*ShareInfo, error) {
			callCount++
			if callCount == 2 {
				cancel()
			}
			return &ShareInfo{ID: "share-1", LifecycleState: "pending"}, nil
		},
	}

	_, err := c.waitForShareStable(ctx, "share-1", 5*time.Second)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("expected context.Canceled, got: %v", err)
	}
}

func TestWaitForShareStable_GetError(t *testing.T) {
	getErr := fmt.Errorf("network error")
	c := &Client{
		pollInterval: 1 * time.Millisecond,
		getShareFunc: func(_ context.Context, _ string) (*ShareInfo, error) {
			return nil, getErr
		},
	}

	_, err := c.waitForShareStable(context.Background(), "share-1", 5*time.Second)
	if err == nil {
		t.Fatal("expected error")
	}
	if !errors.Is(err, getErr) {
		t.Errorf("expected wrapped network error, got: %v", err)
	}
}

// --- test helpers ---

func strPtr(s string) *string {
	return &s
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
