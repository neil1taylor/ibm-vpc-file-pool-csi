package ibmcloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/IBM/go-sdk-core/v5/core"
	"github.com/IBM/vpc-go-sdk/vpcv1"
	"github.com/go-openapi/strfmt"
	"golang.org/x/time/rate"
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

func TestParseMountPathServer(t *testing.T) {
	tests := []struct {
		name      string
		mountPath string
		want      string
	}{
		{"IP address format", "10.240.0.5:/share_path", "10.240.0.5"},
		{"FQDN format", "fsf-dal1234.adn.networklayer.com:/share_path", "fsf-dal1234.adn.networklayer.com"},
		{"empty string", "", ""},
		{"no colon-slash", "invalid", ""},
		{"colon but no slash", "server:", ""},
		{"just colon-slash", ":/path", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseMountPathServer(tt.mountPath)
			if got != tt.want {
				t.Errorf("parseMountPathServer(%q) = %q, want %q", tt.mountPath, got, tt.want)
			}
		})
	}
}

// --- fakeSecretProvider for auth tests ---

type fakeSecretProvider struct {
	token    string
	tokenErr error

	riaasEndpoint      string
	riaasEndpointErr   error
	privateEndpoint    string
	privateEndpointErr error
	resourceGroupID    string
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

func (f *fakeSecretProvider) GetResourceGroupID() string {
	return f.resourceGroupID
}

// --- ParseRegionFromEndpoint tests ---

func TestParseRegionFromEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		want     string
	}{
		{"private endpoint", "https://eu-de.private.iaas.cloud.ibm.com", "eu-de"},
		{"public endpoint", "https://us-south.iaas.cloud.ibm.com/v1", "us-south"},
		{"jp-tok", "https://jp-tok.iaas.cloud.ibm.com", "jp-tok"},
		{"au-syd private", "https://au-syd.private.iaas.cloud.ibm.com/v1", "au-syd"},
		{"br-sao", "https://br-sao.iaas.cloud.ibm.com/v1", "br-sao"},
		{"empty string", "", ""},
		{"no host", "/v1", ""},
		{"no hyphen in region", "https://localhost/v1", ""},
		{"bare hostname", "https://iaas.cloud.ibm.com", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ParseRegionFromEndpoint(tt.endpoint)
			if got != tt.want {
				t.Errorf("ParseRegionFromEndpoint(%q) = %q, want %q", tt.endpoint, got, tt.want)
			}
		})
	}
}

// --- ResourceGroupID tests ---

func TestNewClientWithProvider_ResourceGroupID(t *testing.T) {
	sp := &fakeSecretProvider{
		token:           "test-token",
		riaasEndpoint:   "https://us-south.iaas.cloud.ibm.com/v1",
		resourceGroupID: "rg-abc123",
	}

	client, err := NewClientWithProvider(sp, "us-south")
	if err != nil {
		t.Fatalf("NewClientWithProvider() error = %v", err)
	}
	if client.ResourceGroupID() != "rg-abc123" {
		t.Errorf("ResourceGroupID() = %q, want %q", client.ResourceGroupID(), "rg-abc123")
	}
}

func TestNewClientWithProvider_AutoDiscoverRegion(t *testing.T) {
	sp := &fakeSecretProvider{
		token:           "test-token",
		privateEndpoint: "https://eu-de.private.iaas.cloud.ibm.com",
	}

	client, err := NewClientWithProvider(sp, "")
	if err != nil {
		t.Fatalf("NewClientWithProvider() error = %v", err)
	}
	if client.region != "eu-de" {
		t.Errorf("client.region = %q, want %q", client.region, "eu-de")
	}
}

// --- Auth flow tests ---

func TestNewClientWithProvider_Success(t *testing.T) {
	sp := &fakeSecretProvider{
		token:         "test-token",
		riaasEndpoint: "https://us-south.iaas.cloud.ibm.com/v1",
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
	if !strings.Contains(err.Error(), "IAM token") {
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

func TestNewClientWithProvider_EndpointNormalization(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
	}{
		{"no trailing slash", "https://us-south.iaas.cloud.ibm.com"},
		{"trailing slash", "https://us-south.iaas.cloud.ibm.com/"},
		{"already has v1", "https://us-south.iaas.cloud.ibm.com/v1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sp := &fakeSecretProvider{
				token:         "test-token",
				riaasEndpoint: tt.endpoint,
			}

			client, err := NewClientWithProvider(sp, "us-south")
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

func TestWaitForShareStable_UpdatingThenStable(t *testing.T) {
	callCount := 0
	c := &Client{
		pollInterval: 1 * time.Millisecond,
		getShareFunc: func(_ context.Context, _ string) (*ShareInfo, error) {
			callCount++
			if callCount < 2 {
				return &ShareInfo{ID: "share-1", LifecycleState: "updating"}, nil
			}
			return &ShareInfo{ID: "share-1", LifecycleState: "stable", SizeGB: 200}, nil
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

// =============================================================================
// HTTP-level mocking of VPC API for testing Client CRUD methods
// =============================================================================

// newTestVPCService creates a vpcv1.VpcV1 service pointed at the given test
// server URL. Uses a BearerTokenAuthenticator so that refreshToken() can
// update the token without type-assertion failures.
func newTestVPCService(t *testing.T, serverURL string) *vpcv1.VpcV1 {
	t.Helper()
	svc, err := vpcv1.NewVpcV1(&vpcv1.VpcV1Options{
		Authenticator: &core.BearerTokenAuthenticator{
			BearerToken: "test-token",
		},
		URL: serverURL,
	})
	if err != nil {
		t.Fatalf("failed to create test VPC service: %v", err)
	}
	return svc
}

// newTestClient creates a Client with the given VPC service, a fake secret
// provider, and a fast rate limiter for test speed.
func newTestClient(t *testing.T, vpcService *vpcv1.VpcV1) *Client {
	t.Helper()
	return &Client{
		vpcService: vpcService,
		secretProvider: &fakeSecretProvider{
			token:         "test-token",
			riaasEndpoint: "https://us-south.iaas.cloud.ibm.com/v1",
		},
		region:          "us-south",
		resourceGroupID: "rg-test-123",
		rateLimiter:     rate.NewLimiter(rate.Inf, 1), // no rate limit in tests
		pollInterval:    1 * time.Millisecond,
	}
}

// shareJSON produces the JSON body for a VPC Share response.
// The IBM VPC SDK unmarshals JSON into vpcv1.Share.
func shareJSON(id, name, state, profile, zone string, sizeGB, iops int64) map[string]interface{} {
	return map[string]interface{}{
		"id":              id,
		"name":            name,
		"lifecycle_state": state,
		"size":            sizeGB,
		"iops":            iops,
		"profile": map[string]interface{}{
			"name": profile,
		},
		"zone": map[string]interface{}{
			"name": zone,
		},
		"created_at":    "2025-01-15T10:00:00.000Z",
		"mount_targets": []interface{}{},
	}
}

// shareJSONWithMountTargets produces a Share JSON response with mount target references.
func shareJSONWithMountTargets(id, name, state, profile, zone string, sizeGB, iops int64, mountTargets []map[string]interface{}) map[string]interface{} {
	s := shareJSON(id, name, state, profile, zone, sizeGB, iops)
	s["mount_targets"] = mountTargets
	return s
}

// mountTargetRef produces a mount target reference for inclusion in share responses.
func mountTargetRef(id, name string) map[string]interface{} {
	return map[string]interface{}{
		"id":   id,
		"name": name,
	}
}

// mountTargetJSON produces the JSON body for a VPC mount target response.
func mountTargetJSON(id, name, mountPath string, primaryIP string) map[string]interface{} {
	mt := map[string]interface{}{
		"id":   id,
		"name": name,
	}
	if mountPath != "" {
		mt["mount_path"] = mountPath
	}
	if primaryIP != "" {
		mt["primary_ip"] = map[string]interface{}{
			"address": primaryIP,
		}
	}
	return mt
}

func writeJSON(w http.ResponseWriter, statusCode int, body interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	if body != nil {
		json.NewEncoder(w).Encode(body) //nolint:errcheck
	}
}

// --- shareToInfo tests ---

func TestShareToInfo(t *testing.T) {
	share := &vpcv1.Share{
		ID:             core.StringPtr("share-123"),
		Name:           core.StringPtr("test-share"),
		LifecycleState: core.StringPtr("stable"),
		Size:           core.Int64Ptr(500),
		Iops:           core.Int64Ptr(3000),
		Profile: &vpcv1.ShareProfileReference{
			Name: core.StringPtr("dp2"),
		},
		Zone: &vpcv1.ZoneReference{
			Name: core.StringPtr("us-south-1"),
		},
	}

	info := shareToInfo(share)
	if info.ID != "share-123" {
		t.Errorf("ID = %q, want %q", info.ID, "share-123")
	}
	if info.Name != "test-share" {
		t.Errorf("Name = %q, want %q", info.Name, "test-share")
	}
	if info.LifecycleState != "stable" {
		t.Errorf("LifecycleState = %q, want %q", info.LifecycleState, "stable")
	}
	if info.SizeGB != 500 {
		t.Errorf("SizeGB = %d, want %d", info.SizeGB, 500)
	}
	if info.IOPS != 3000 {
		t.Errorf("IOPS = %d, want %d", info.IOPS, 3000)
	}
	if info.Profile != "dp2" {
		t.Errorf("Profile = %q, want %q", info.Profile, "dp2")
	}
	if info.Zone != "us-south-1" {
		t.Errorf("Zone = %q, want %q", info.Zone, "us-south-1")
	}

	// Test with nil profile and zone.
	shareNoProfile := &vpcv1.Share{
		ID:             core.StringPtr("share-456"),
		Name:           core.StringPtr("no-profile"),
		LifecycleState: core.StringPtr("pending"),
		Size:           core.Int64Ptr(100),
		Iops:           core.Int64Ptr(1000),
	}
	info2 := shareToInfo(shareNoProfile)
	if info2.Profile != "" {
		t.Errorf("Profile should be empty for nil profile, got %q", info2.Profile)
	}
	if info2.Zone != "" {
		t.Errorf("Zone should be empty for nil zone, got %q", info2.Zone)
	}

	// Test with CreatedAt set.
	now := strfmt.DateTime(time.Now())
	shareWithTime := &vpcv1.Share{
		ID:             core.StringPtr("share-789"),
		Name:           core.StringPtr("with-time"),
		LifecycleState: core.StringPtr("stable"),
		Size:           core.Int64Ptr(200),
		Iops:           core.Int64Ptr(2000),
		CreatedAt:      &now,
	}
	info3 := shareToInfo(shareWithTime)
	if info3.CreatedAt.IsZero() {
		t.Error("CreatedAt should not be zero when set in share")
	}
}

// --- refreshToken tests ---

func TestRefreshToken_Success(t *testing.T) {
	sp := &fakeSecretProvider{
		token:         "initial-token",
		riaasEndpoint: "https://us-south.iaas.cloud.ibm.com/v1",
	}

	client, err := NewClientWithProvider(sp, "us-south")
	if err != nil {
		t.Fatalf("NewClientWithProvider() error = %v", err)
	}

	// Change the token for the next refresh.
	sp.token = "refreshed-token"

	err = client.refreshToken(context.Background(), "test-refresh")
	if err != nil {
		t.Fatalf("refreshToken() error = %v", err)
	}

	auth, ok := client.vpcService.Service.Options.Authenticator.(*core.BearerTokenAuthenticator)
	if !ok {
		t.Fatal("expected BearerTokenAuthenticator")
	}
	if auth.BearerToken != "refreshed-token" {
		t.Errorf("token = %q, want %q", auth.BearerToken, "refreshed-token")
	}
}

func TestRefreshToken_Error(t *testing.T) {
	sp := &fakeSecretProvider{
		token:         "initial-token",
		riaasEndpoint: "https://us-south.iaas.cloud.ibm.com/v1",
	}

	client, err := NewClientWithProvider(sp, "us-south")
	if err != nil {
		t.Fatalf("NewClientWithProvider() error = %v", err)
	}

	// Make the next token refresh fail.
	sp.tokenErr = fmt.Errorf("token expired")

	err = client.refreshToken(context.Background(), "test-refresh")
	if err == nil {
		t.Fatal("expected error from refreshToken")
	}
	if !strings.Contains(err.Error(), "refresh IAM token") {
		t.Errorf("error should mention refresh IAM token, got: %v", err)
	}
}

// --- withAuth tests ---

func TestWithAuth_Success(t *testing.T) {
	sp := &fakeSecretProvider{
		token:         "test-token",
		riaasEndpoint: "https://us-south.iaas.cloud.ibm.com/v1",
	}

	client, err := NewClientWithProvider(sp, "us-south")
	if err != nil {
		t.Fatalf("NewClientWithProvider() error = %v", err)
	}

	err = client.withAuth(context.Background(), "test-op")
	if err != nil {
		t.Fatalf("withAuth() error = %v", err)
	}
}

func TestWithAuth_ContextCancelled(t *testing.T) {
	sp := &fakeSecretProvider{
		token:         "test-token",
		riaasEndpoint: "https://us-south.iaas.cloud.ibm.com/v1",
	}

	client, err := NewClientWithProvider(sp, "us-south")
	if err != nil {
		t.Fatalf("NewClientWithProvider() error = %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err = client.withAuth(ctx, "test-op")
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestWithAuth_TokenRefreshError(t *testing.T) {
	sp := &fakeSecretProvider{
		token:         "test-token",
		riaasEndpoint: "https://us-south.iaas.cloud.ibm.com/v1",
	}

	client, err := NewClientWithProvider(sp, "us-south")
	if err != nil {
		t.Fatalf("NewClientWithProvider() error = %v", err)
	}

	// Make token refresh fail.
	sp.tokenErr = fmt.Errorf("auth failure")

	err = client.withAuth(context.Background(), "test-op")
	if err == nil {
		t.Fatal("expected error from token refresh failure")
	}
}

// --- GetFileShare tests ---

func TestGetFileShare_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-abc", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		body := shareJSONWithMountTargets(
			"share-abc", "pool-share-1", "stable", "dp2", "us-south-1", 500, 3000,
			[]map[string]interface{}{
				mountTargetRef("mt-1", "pool-share-1-mt"),
			},
		)
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("/shares/share-abc/mount_targets/mt-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-1", "pool-share-1-mt", "", "10.240.0.5"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	info, err := client.GetFileShare(context.Background(), "share-abc")
	if err != nil {
		t.Fatalf("GetFileShare() error = %v", err)
	}

	if info.ID != "share-abc" {
		t.Errorf("ID = %q, want %q", info.ID, "share-abc")
	}
	if info.Name != "pool-share-1" {
		t.Errorf("Name = %q, want %q", info.Name, "pool-share-1")
	}
	if info.LifecycleState != "stable" {
		t.Errorf("LifecycleState = %q, want %q", info.LifecycleState, "stable")
	}
	if info.SizeGB != 500 {
		t.Errorf("SizeGB = %d, want %d", info.SizeGB, 500)
	}
	if info.IOPS != 3000 {
		t.Errorf("IOPS = %d, want %d", info.IOPS, 3000)
	}
	if info.Profile != "dp2" {
		t.Errorf("Profile = %q, want %q", info.Profile, "dp2")
	}
	if info.Zone != "us-south-1" {
		t.Errorf("Zone = %q, want %q", info.Zone, "us-south-1")
	}
	if len(info.MountTargets) != 1 {
		t.Fatalf("MountTargets count = %d, want 1", len(info.MountTargets))
	}
	if info.MountTargets[0].IPAddress != "10.240.0.5" {
		t.Errorf("MountTarget IP = %q, want %q", info.MountTargets[0].IPAddress, "10.240.0.5")
	}
}

func TestGetFileShare_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-missing", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"errors": []map[string]interface{}{
				{"code": "not_found", "message": "share not found"},
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	_, err := client.GetFileShare(context.Background(), "share-missing")
	if err == nil {
		t.Fatal("expected error for not found share")
	}
	if !errors.Is(err, ErrShareNotFound) {
		t.Errorf("expected ErrShareNotFound, got: %v", err)
	}
}

func TestGetFileShare_ServerError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-err", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"errors": []map[string]interface{}{
				{"code": "internal_error", "message": "server error"},
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	_, err := client.GetFileShare(context.Background(), "share-err")
	if err == nil {
		t.Fatal("expected error for server error")
	}
}

func TestGetFileShare_MountPathExtraction(t *testing.T) {
	// Test VPC-mode mount target (MountPath instead of PrimaryIP).
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-vpc", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		body := shareJSONWithMountTargets(
			"share-vpc", "vpc-share", "stable", "dp2", "us-south-1", 1000, 5000,
			[]map[string]interface{}{
				mountTargetRef("mt-vpc-1", "vpc-share-mt"),
			},
		)
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("/shares/share-vpc/mount_targets/mt-vpc-1", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		// VPC mode: IP comes from mount_path, not primary_ip.
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-vpc-1", "vpc-share-mt", "fsf-dal1234.adn.networklayer.com:/share_export", ""))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	info, err := client.GetFileShare(context.Background(), "share-vpc")
	if err != nil {
		t.Fatalf("GetFileShare() error = %v", err)
	}

	if len(info.MountTargets) != 1 {
		t.Fatalf("MountTargets count = %d, want 1", len(info.MountTargets))
	}
	if info.MountTargets[0].IPAddress != "fsf-dal1234.adn.networklayer.com" {
		t.Errorf("MountTarget IP = %q, want %q", info.MountTargets[0].IPAddress, "fsf-dal1234.adn.networklayer.com")
	}
}

func TestGetFileShare_MountTargetFetchError(t *testing.T) {
	// When getMountTarget fails, GetFileShare should skip the mount target.
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-mt-err", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		body := shareJSONWithMountTargets(
			"share-mt-err", "mt-err-share", "stable", "dp2", "us-south-1", 100, 1000,
			[]map[string]interface{}{
				mountTargetRef("mt-fail", "mt-fail-name"),
			},
		)
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("/shares/share-mt-err/mount_targets/mt-fail", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"errors": []map[string]interface{}{
				{"code": "internal_error", "message": "mount target fetch failed"},
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	info, err := client.GetFileShare(context.Background(), "share-mt-err")
	if err != nil {
		t.Fatalf("GetFileShare() error = %v", err)
	}

	// Mount targets should be empty because the fetch failed and was skipped.
	if len(info.MountTargets) != 0 {
		t.Errorf("MountTargets count = %d, want 0 (fetch failed)", len(info.MountTargets))
	}
}

func TestGetFileShare_NoMountTargets(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-no-mt", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		body := shareJSON("share-no-mt", "no-mt-share", "stable", "dp2", "us-south-1", 100, 1000)
		writeJSON(w, http.StatusOK, body)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	info, err := client.GetFileShare(context.Background(), "share-no-mt")
	if err != nil {
		t.Fatalf("GetFileShare() error = %v", err)
	}
	if len(info.MountTargets) != 0 {
		t.Errorf("MountTargets count = %d, want 0", len(info.MountTargets))
	}
}

func TestGetFileShare_AuthError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-auth", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusUnauthorized, map[string]interface{}{
			"errors": []map[string]interface{}{
				{"code": "not_authorized", "message": "unauthorized"},
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	_, err := client.GetFileShare(context.Background(), "share-auth")
	if err == nil {
		t.Fatal("expected auth error")
	}
	if !errors.Is(err, ErrAuthentication) {
		t.Errorf("expected ErrAuthentication, got: %v", err)
	}
}

func TestGetFileShare_RateLimitError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-rl", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusTooManyRequests, map[string]interface{}{
			"errors": []map[string]interface{}{
				{"code": "too_many_requests", "message": "rate limited"},
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	_, err := client.GetFileShare(context.Background(), "share-rl")
	if err == nil {
		t.Fatal("expected rate limit error")
	}
	if !errors.Is(err, ErrAPIRateLimit) {
		t.Errorf("expected ErrAPIRateLimit, got: %v", err)
	}
}

// --- CreateFileShare tests ---

func TestCreateFileShare_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		body := shareJSONWithMountTargets(
			"share-new", "test-pool-share", "stable", "dp2", "us-south-1", 1000, 3000,
			[]map[string]interface{}{
				mountTargetRef("mt-new", "test-pool-share-mt"),
			},
		)
		writeJSON(w, http.StatusCreated, body)
	})
	mux.HandleFunc("/shares/share-new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		body := shareJSONWithMountTargets(
			"share-new", "test-pool-share", "stable", "dp2", "us-south-1", 1000, 3000,
			[]map[string]interface{}{
				mountTargetRef("mt-new", "test-pool-share-mt"),
			},
		)
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("/shares/share-new/mount_targets/mt-new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-new", "test-pool-share-mt", "", "10.240.0.10"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	input := CreateShareInput{
		Name:            "test-pool-share",
		Zone:            "us-south-1",
		Profile:         "dp2",
		SizeGB:          1000,
		ResourceGroupID: "rg-test-123",
		Tags:            []string{"pool:test-pool"},
		VPCId:           "vpc-123",
	}

	info, err := client.CreateFileShare(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateFileShare() error = %v", err)
	}

	if info.ID != "share-new" {
		t.Errorf("ID = %q, want %q", info.ID, "share-new")
	}
	if info.Name != "test-pool-share" {
		t.Errorf("Name = %q, want %q", info.Name, "test-pool-share")
	}
	if info.SizeGB != 1000 {
		t.Errorf("SizeGB = %d, want %d", info.SizeGB, 1000)
	}
}

func TestCreateFileShare_WithIOPS(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		body := shareJSON("share-iops", "iops-share", "stable", "custom", "us-south-1", 500, 10000)
		writeJSON(w, http.StatusCreated, body)
	})
	mux.HandleFunc("/shares/share-iops", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		body := shareJSON("share-iops", "iops-share", "stable", "custom", "us-south-1", 500, 10000)
		writeJSON(w, http.StatusOK, body)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	iops := int64(10000)
	input := CreateShareInput{
		Name:    "iops-share",
		Zone:    "us-south-1",
		Profile: "custom",
		SizeGB:  500,
		IOPS:    &iops,
	}

	info, err := client.CreateFileShare(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateFileShare() error = %v", err)
	}
	if info.IOPS != 10000 {
		t.Errorf("IOPS = %d, want %d", info.IOPS, 10000)
	}
}

func TestCreateFileShare_WithEncryptInTransit(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		// Verify the request body includes transit encryption.
		var reqBody map[string]interface{}
		json.NewDecoder(r.Body).Decode(&reqBody) //nolint:errcheck
		if mts, ok := reqBody["mount_targets"].([]interface{}); ok && len(mts) > 0 {
			mt := mts[0].(map[string]interface{})
			if mt["transit_encryption"] != "user_managed" {
				writeJSON(w, http.StatusBadRequest, nil)
				return
			}
		}
		body := shareJSON("share-eit", "eit-share", "stable", "dp2", "us-south-1", 200, 1000)
		writeJSON(w, http.StatusCreated, body)
	})
	mux.HandleFunc("/shares/share-eit", func(w http.ResponseWriter, r *http.Request) {
		body := shareJSON("share-eit", "eit-share", "stable", "dp2", "us-south-1", 200, 1000)
		writeJSON(w, http.StatusOK, body)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	input := CreateShareInput{
		Name:             "eit-share",
		Zone:             "us-south-1",
		Profile:          "dp2",
		SizeGB:           200,
		VPCId:            "vpc-123",
		EncryptInTransit: true,
	}

	info, err := client.CreateFileShare(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateFileShare() error = %v", err)
	}
	if info.ID != "share-eit" {
		t.Errorf("ID = %q, want %q", info.ID, "share-eit")
	}
}

func TestCreateFileShare_AlreadyExists(t *testing.T) {
	// Tests the idempotent path: API returns 400 "already exists", client
	// should look up the share by name and return it.
	var listCalls int32

	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"errors":[{"code":"already_exists","message":"share name already exists"}]}`)
			return
		}
		if r.Method == http.MethodGet {
			atomic.AddInt32(&listCalls, 1)
			// Return the existing share by name lookup.
			result := map[string]interface{}{
				"shares": []map[string]interface{}{
					shareJSON("share-existing", "existing-share", "stable", "dp2", "us-south-1", 500, 3000),
				},
				"total_count": 1,
			}
			writeJSON(w, http.StatusOK, result)
			return
		}
	})
	mux.HandleFunc("/shares/share-existing", func(w http.ResponseWriter, r *http.Request) {
		body := shareJSON("share-existing", "existing-share", "stable", "dp2", "us-south-1", 500, 3000)
		writeJSON(w, http.StatusOK, body)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	input := CreateShareInput{
		Name:    "existing-share",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  500,
	}

	info, err := client.CreateFileShare(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateFileShare() error = %v", err)
	}
	if info.ID != "share-existing" {
		t.Errorf("ID = %q, want %q", info.ID, "share-existing")
	}
	if atomic.LoadInt32(&listCalls) == 0 {
		t.Error("expected list call for name lookup")
	}
}

func TestCreateFileShare_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"errors": []map[string]interface{}{
				{"code": "internal_error", "message": "server error"},
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	input := CreateShareInput{
		Name:    "fail-share",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  500,
	}

	_, err := client.CreateFileShare(context.Background(), input)
	if err == nil {
		t.Fatal("expected error from CreateFileShare")
	}
}

func TestCreateFileShare_ShareNotStable(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		body := shareJSON("share-pend", "pending-share", "pending", "dp2", "us-south-1", 500, 3000)
		writeJSON(w, http.StatusCreated, body)
	})
	mux.HandleFunc("/shares/share-pend", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		// Always return "failed" to trigger the waitForShareStable failure.
		body := shareJSON("share-pend", "pending-share", "failed", "dp2", "us-south-1", 500, 3000)
		writeJSON(w, http.StatusOK, body)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	input := CreateShareInput{
		Name:    "pending-share",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  500,
	}

	_, err := client.CreateFileShare(context.Background(), input)
	if err == nil {
		t.Fatal("expected error for non-stable share")
	}
	if !strings.Contains(err.Error(), "did not become stable") {
		t.Errorf("error should mention 'did not become stable', got: %v", err)
	}
}

func TestCreateFileShare_AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.NewServeMux()) // empty mux, unused
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	// Make token refresh fail.
	client.secretProvider = &fakeSecretProvider{
		tokenErr: fmt.Errorf("auth expired"),
	}

	input := CreateShareInput{
		Name:    "auth-fail-share",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  500,
	}

	_, err := client.CreateFileShare(context.Background(), input)
	if err == nil {
		t.Fatal("expected auth error from CreateFileShare")
	}
}

// --- ExpandFileShare tests ---

func TestExpandFileShare_Success(t *testing.T) {
	var expandCalled int32

	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-expand", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			atomic.AddInt32(&expandCalled, 1)
			body := shareJSON("share-expand", "expand-share", "updating", "dp2", "us-south-1", 1000, 3000)
			writeJSON(w, http.StatusOK, body)
			return
		}
		if r.Method == http.MethodGet {
			body := shareJSON("share-expand", "expand-share", "stable", "dp2", "us-south-1", 1000, 3000)
			writeJSON(w, http.StatusOK, body)
			return
		}
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	err := client.ExpandFileShare(context.Background(), "share-expand", 1000)
	if err != nil {
		t.Fatalf("ExpandFileShare() error = %v", err)
	}
	if atomic.LoadInt32(&expandCalled) != 1 {
		t.Errorf("expected 1 PATCH call, got %d", atomic.LoadInt32(&expandCalled))
	}
}

func TestExpandFileShare_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-expand-err", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			writeJSON(w, http.StatusBadRequest, map[string]interface{}{
				"errors": []map[string]interface{}{
					{"code": "invalid_size", "message": "new size must be larger than current"},
				},
			})
			return
		}
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	err := client.ExpandFileShare(context.Background(), "share-expand-err", 100)
	if err == nil {
		t.Fatal("expected error from ExpandFileShare")
	}
}

func TestExpandFileShare_AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.NewServeMux())
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)
	client.secretProvider = &fakeSecretProvider{tokenErr: fmt.Errorf("auth expired")}

	err := client.ExpandFileShare(context.Background(), "share-x", 1000)
	if err == nil {
		t.Fatal("expected auth error from ExpandFileShare")
	}
}

// --- DeleteFileShare tests ---

func TestDeleteFileShare_Success(t *testing.T) {
	var deleteCalled int32

	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-del", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			body := shareJSONWithMountTargets(
				"share-del", "del-share", "stable", "dp2", "us-south-1", 500, 3000,
				[]map[string]interface{}{
					mountTargetRef("mt-del", "del-share-mt"),
				},
			)
			writeJSON(w, http.StatusOK, body)
			return
		}
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&deleteCalled, 1)
			body := shareJSON("share-del", "del-share", "deleting", "dp2", "us-south-1", 500, 3000)
			writeJSON(w, http.StatusAccepted, body)
			return
		}
	})
	mux.HandleFunc("/shares/share-del/mount_targets/mt-del", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, mountTargetJSON("mt-del", "del-share-mt", "", "10.240.0.1"))
			return
		}
		if r.Method == http.MethodDelete {
			writeJSON(w, http.StatusNoContent, nil)
			return
		}
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	err := client.DeleteFileShare(context.Background(), "share-del")
	if err != nil {
		t.Fatalf("DeleteFileShare() error = %v", err)
	}
	if atomic.LoadInt32(&deleteCalled) != 1 {
		t.Errorf("expected 1 DELETE call, got %d", atomic.LoadInt32(&deleteCalled))
	}
}

func TestDeleteFileShare_AlreadyGone(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-gone", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"errors": []map[string]interface{}{
					{"code": "not_found", "message": "share not found"},
				},
			})
			return
		}
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	err := client.DeleteFileShare(context.Background(), "share-gone")
	if err != nil {
		t.Fatalf("DeleteFileShare() should be idempotent for already deleted, got: %v", err)
	}
}

func TestDeleteFileShare_DeleteReturns404(t *testing.T) {
	// The share exists when checked but returns 404 during delete (race condition).
	var getCalls int32
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-race", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			call := atomic.AddInt32(&getCalls, 1)
			if call == 1 {
				// First GET (from DeleteFileShare's initial check): share exists.
				body := shareJSON("share-race", "race-share", "stable", "dp2", "us-south-1", 100, 1000)
				writeJSON(w, http.StatusOK, body)
				return
			}
			// Subsequent GETs (from GetFileShare during delete): not found.
			writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"errors": []map[string]interface{}{
					{"code": "not_found", "message": "not found"},
				},
			})
			return
		}
		if r.Method == http.MethodDelete {
			writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"errors": []map[string]interface{}{
					{"code": "not_found", "message": "share already deleted"},
				},
			})
			return
		}
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	err := client.DeleteFileShare(context.Background(), "share-race")
	if err != nil {
		t.Fatalf("DeleteFileShare() should handle 404 on delete, got: %v", err)
	}
}

func TestDeleteFileShare_AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.NewServeMux())
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)
	client.secretProvider = &fakeSecretProvider{tokenErr: fmt.Errorf("auth expired")}

	err := client.DeleteFileShare(context.Background(), "share-x")
	if err == nil {
		t.Fatal("expected auth error from DeleteFileShare")
	}
}

// --- ListFileShares tests ---

func TestListFileShares_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		result := map[string]interface{}{
			"shares": []map[string]interface{}{
				shareJSONWithMountTargets("share-1", "pool-s1", "stable", "dp2", "us-south-1", 500, 3000,
					[]map[string]interface{}{mountTargetRef("mt-1", "pool-s1-mt")}),
				shareJSONWithMountTargets("share-2", "pool-s2", "stable", "dp2", "us-south-1", 1000, 5000,
					[]map[string]interface{}{mountTargetRef("mt-2", "pool-s2-mt")}),
			},
			"total_count": 2,
		}
		writeJSON(w, http.StatusOK, result)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	shares, err := client.ListFileShares(context.Background(), "rg-test-123", nil)
	if err != nil {
		t.Fatalf("ListFileShares() error = %v", err)
	}
	if len(shares) != 2 {
		t.Fatalf("shares count = %d, want 2", len(shares))
	}
	if shares[0].ID != "share-1" && shares[1].ID != "share-1" {
		t.Error("expected share-1 in results")
	}
	// Verify mount target refs are populated (ID and Name only, no IP).
	for _, s := range shares {
		if len(s.MountTargets) != 1 {
			t.Errorf("share %s should have 1 mount target ref, got %d", s.ID, len(s.MountTargets))
		}
	}
}

func TestListFileShares_Empty(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		result := map[string]interface{}{
			"shares":      []interface{}{},
			"total_count": 0,
		}
		writeJSON(w, http.StatusOK, result)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	shares, err := client.ListFileShares(context.Background(), "rg-test-123", nil)
	if err != nil {
		t.Fatalf("ListFileShares() error = %v", err)
	}
	if len(shares) != 0 {
		t.Errorf("shares count = %d, want 0", len(shares))
	}
}

func TestListFileShares_Pagination(t *testing.T) {
	var pageCalls int32

	// We need to know the server URL before setting up handlers,
	// so use a variable that gets set after server creation.
	var serverURL string

	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		page := atomic.AddInt32(&pageCalls, 1)
		start := r.URL.Query().Get("start")

		if page == 1 && start == "" {
			// First page with a "next" link.
			result := map[string]interface{}{
				"shares": []map[string]interface{}{
					shareJSON("share-p1", "page1-share", "stable", "dp2", "us-south-1", 500, 3000),
				},
				"total_count": 2,
				"next": map[string]interface{}{
					"href": serverURL + "/shares?start=page2token&limit=100",
				},
			}
			writeJSON(w, http.StatusOK, result)
			return
		}
		// Second (final) page -- no "next" field.
		result := map[string]interface{}{
			"shares": []map[string]interface{}{
				shareJSON("share-p2", "page2-share", "stable", "dp2", "us-south-1", 1000, 5000),
			},
			"total_count": 2,
		}
		writeJSON(w, http.StatusOK, result)
	})

	server := httptest.NewServer(mux)
	defer server.Close()
	serverURL = server.URL

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	shares, err := client.ListFileShares(context.Background(), "rg-test-123", nil)
	if err != nil {
		t.Fatalf("ListFileShares() error = %v", err)
	}
	if len(shares) != 2 {
		t.Fatalf("shares count = %d, want 2", len(shares))
	}
	if atomic.LoadInt32(&pageCalls) < 2 {
		t.Errorf("expected at least 2 page calls, got %d", atomic.LoadInt32(&pageCalls))
	}
}

func TestListFileShares_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"errors": []map[string]interface{}{
				{"code": "internal_error", "message": "server error"},
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	_, err := client.ListFileShares(context.Background(), "rg-test-123", nil)
	if err == nil {
		t.Fatal("expected error from ListFileShares")
	}
}

func TestListFileShares_AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.NewServeMux())
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)
	client.secretProvider = &fakeSecretProvider{tokenErr: fmt.Errorf("auth expired")}

	_, err := client.ListFileShares(context.Background(), "rg-test-123", nil)
	if err == nil {
		t.Fatal("expected auth error from ListFileShares")
	}
}

func TestListFileShares_NoResourceGroup(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		// Verify no resource_group.id query parameter.
		if rg := r.URL.Query().Get("resource_group.id"); rg != "" {
			t.Errorf("expected no resource_group.id parameter, got %q", rg)
		}
		result := map[string]interface{}{
			"shares":      []interface{}{},
			"total_count": 0,
		}
		writeJSON(w, http.StatusOK, result)
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	_, err := client.ListFileShares(context.Background(), "", nil)
	if err != nil {
		t.Fatalf("ListFileShares() error = %v", err)
	}
}

// --- CreateShareMountTarget tests ---

func TestCreateShareMountTarget_Success(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-mt/mount_targets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		writeJSON(w, http.StatusCreated, mountTargetJSON("mt-new", "cross-zone-mt", "", ""))
	})
	mux.HandleFunc("/shares/share-mt/mount_targets/mt-new", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		// Return with IP populated.
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-new", "cross-zone-mt", "", "10.240.1.5"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	input := CreateMountTargetInput{
		Name:  "cross-zone-mt",
		VPCId: "vpc-123",
	}

	mt, err := client.CreateShareMountTarget(context.Background(), "share-mt", input)
	if err != nil {
		t.Fatalf("CreateShareMountTarget() error = %v", err)
	}
	if mt.ID != "mt-new" {
		t.Errorf("ID = %q, want %q", mt.ID, "mt-new")
	}
	if mt.IPAddress != "10.240.1.5" {
		t.Errorf("IPAddress = %q, want %q", mt.IPAddress, "10.240.1.5")
	}
}

func TestCreateShareMountTarget_WithEncryption(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-enc/mount_targets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		writeJSON(w, http.StatusCreated, mountTargetJSON("mt-enc", "enc-mt", "", ""))
	})
	mux.HandleFunc("/shares/share-enc/mount_targets/mt-enc", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-enc", "enc-mt", "", "10.240.2.1"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	input := CreateMountTargetInput{
		Name:             "enc-mt",
		VPCId:            "vpc-123",
		EncryptInTransit: true,
	}

	mt, err := client.CreateShareMountTarget(context.Background(), "share-enc", input)
	if err != nil {
		t.Fatalf("CreateShareMountTarget() error = %v", err)
	}
	if mt.ID != "mt-enc" {
		t.Errorf("ID = %q, want %q", mt.ID, "mt-enc")
	}
}

func TestCreateShareMountTarget_APIError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-mt-err/mount_targets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, map[string]interface{}{
			"errors": []map[string]interface{}{
				{"code": "already_exists", "message": "mount target already exists for this VPC"},
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	input := CreateMountTargetInput{
		Name:  "dup-mt",
		VPCId: "vpc-123",
	}

	_, err := client.CreateShareMountTarget(context.Background(), "share-mt-err", input)
	if err == nil {
		t.Fatal("expected error from CreateShareMountTarget")
	}
}

func TestCreateShareMountTarget_AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.NewServeMux())
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)
	client.secretProvider = &fakeSecretProvider{tokenErr: fmt.Errorf("auth expired")}

	input := CreateMountTargetInput{
		Name:  "fail-mt",
		VPCId: "vpc-123",
	}

	_, err := client.CreateShareMountTarget(context.Background(), "share-x", input)
	if err == nil {
		t.Fatal("expected auth error from CreateShareMountTarget")
	}
}

func TestCreateShareMountTarget_IPPollingTimeout(t *testing.T) {
	// Test the case where the mount target IP never becomes available.
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-poll/mount_targets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		writeJSON(w, http.StatusCreated, mountTargetJSON("mt-poll", "poll-mt", "", ""))
	})
	mux.HandleFunc("/shares/share-poll/mount_targets/mt-poll", func(w http.ResponseWriter, r *http.Request) {
		// Always return empty IP to trigger timeout.
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-poll", "poll-mt", "", ""))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)
	// Use very short poll interval to speed up the test, but we need a real
	// timeout mechanism. The method uses a 5-minute deadline. Override pollInterval
	// to be very short but we also need the test to finish quickly. The function
	// checks time.Now().After(deadline) so we can't really control the 5-minute
	// timeout easily. Instead, cancel the context after a short time.
	client.pollInterval = 1 * time.Millisecond

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	input := CreateMountTargetInput{
		Name:  "poll-mt",
		VPCId: "vpc-123",
	}

	_, err := client.CreateShareMountTarget(ctx, "share-poll", input)
	if err == nil {
		t.Fatal("expected timeout/cancellation error")
	}
}

func TestCreateShareMountTarget_IPPollingGetError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-ge/mount_targets", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		writeJSON(w, http.StatusCreated, mountTargetJSON("mt-ge", "ge-mt", "", ""))
	})
	mux.HandleFunc("/shares/share-ge/mount_targets/mt-ge", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		// Return an error during polling.
		writeJSON(w, http.StatusInternalServerError, map[string]interface{}{
			"errors": []map[string]interface{}{
				{"code": "internal_error", "message": "temporary error"},
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)
	client.pollInterval = 1 * time.Millisecond

	input := CreateMountTargetInput{
		Name:  "ge-mt",
		VPCId: "vpc-123",
	}

	_, err := client.CreateShareMountTarget(context.Background(), "share-ge", input)
	if err == nil {
		t.Fatal("expected error when getMountTarget fails during polling")
	}
}

// --- getMountTarget tests ---

func TestGetMountTarget_PrimaryIP(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-1/mount_targets/mt-1", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-1", "mt-name", "", "10.240.0.1"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	mt, err := client.getMountTarget(context.Background(), "share-1", "mt-1")
	if err != nil {
		t.Fatalf("getMountTarget() error = %v", err)
	}
	if mt.IPAddress != "10.240.0.1" {
		t.Errorf("IPAddress = %q, want %q", mt.IPAddress, "10.240.0.1")
	}
}

func TestGetMountTarget_MountPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-1/mount_targets/mt-2", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-2", "mt-vpc", "fsf-dal1234.adn.networklayer.com:/export", ""))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	mt, err := client.getMountTarget(context.Background(), "share-1", "mt-2")
	if err != nil {
		t.Fatalf("getMountTarget() error = %v", err)
	}
	if mt.IPAddress != "fsf-dal1234.adn.networklayer.com" {
		t.Errorf("IPAddress = %q, want %q", mt.IPAddress, "fsf-dal1234.adn.networklayer.com")
	}
}

func TestGetMountTarget_NoIPOrMountPath(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-1/mount_targets/mt-3", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-3", "mt-noip", "", ""))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	mt, err := client.getMountTarget(context.Background(), "share-1", "mt-3")
	if err != nil {
		t.Fatalf("getMountTarget() error = %v", err)
	}
	if mt.IPAddress != "" {
		t.Errorf("IPAddress = %q, want empty", mt.IPAddress)
	}
}

func TestGetMountTarget_NotFound(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-1/mount_targets/mt-missing", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]interface{}{
			"errors": []map[string]interface{}{
				{"code": "not_found", "message": "mount target not found"},
			},
		})
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	_, err := client.getMountTarget(context.Background(), "share-1", "mt-missing")
	if err == nil {
		t.Fatal("expected error for missing mount target")
	}
	if !errors.Is(err, ErrShareNotFound) {
		t.Errorf("expected ErrShareNotFound, got: %v", err)
	}
}

func TestGetMountTarget_AuthFailure(t *testing.T) {
	server := httptest.NewServer(http.NewServeMux())
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)
	client.secretProvider = &fakeSecretProvider{tokenErr: fmt.Errorf("auth expired")}

	_, err := client.getMountTarget(context.Background(), "share-1", "mt-1")
	if err == nil {
		t.Fatal("expected auth error from getMountTarget")
	}
}

// --- recordAPIMetrics tests ---

func TestRecordAPIMetrics(t *testing.T) {
	// Test that recordAPIMetrics does not panic for success and error cases.
	// We cannot easily assert counter values without a custom registry, but
	// we verify it does not panic.
	start := time.Now()
	recordAPIMetrics("test_op_success", start, nil)
	recordAPIMetrics("test_op_error", start, fmt.Errorf("test error"))
}

// --- Context cancellation tests for CRUD methods ---

func TestGetFileShare_ContextCancelled(t *testing.T) {
	// Server that delays response (but we cancel before it responds).
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-ctx", func(w http.ResponseWriter, r *http.Request) {
		// Wait for the request context to be done (client cancelled).
		<-r.Context().Done()
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, err := client.GetFileShare(ctx, "share-ctx")
	if err == nil {
		t.Fatal("expected error from context cancellation")
	}
}

// --- Multiple mount targets on GetFileShare ---

func TestGetFileShare_MultipleMountTargets(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-multi", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		body := shareJSONWithMountTargets(
			"share-multi", "multi-mt-share", "stable", "dp2", "us-south-1", 500, 3000,
			[]map[string]interface{}{
				mountTargetRef("mt-a", "mt-zone1"),
				mountTargetRef("mt-b", "mt-zone2"),
			},
		)
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("/shares/share-multi/mount_targets/mt-a", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-a", "mt-zone1", "", "10.240.0.1"))
	})
	mux.HandleFunc("/shares/share-multi/mount_targets/mt-b", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-b", "mt-zone2", "", "10.240.1.1"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	info, err := client.GetFileShare(context.Background(), "share-multi")
	if err != nil {
		t.Fatalf("GetFileShare() error = %v", err)
	}
	if len(info.MountTargets) != 2 {
		t.Fatalf("MountTargets count = %d, want 2", len(info.MountTargets))
	}

	// Verify both IPs are present.
	ips := map[string]bool{}
	for _, mt := range info.MountTargets {
		ips[mt.IPAddress] = true
	}
	if !ips["10.240.0.1"] || !ips["10.240.1.1"] {
		t.Errorf("expected both IPs, got: %v", ips)
	}
}

// --- CreateFileShare with mount target IP polling ---

func TestCreateFileShare_MountTargetIPPolling(t *testing.T) {
	// Test the path where the share becomes stable but mount target IP
	// is not yet available, requiring polling.
	var mtGetCalls int32

	mux := http.NewServeMux()
	mux.HandleFunc("/shares", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		body := shareJSONWithMountTargets(
			"share-poll-ip", "poll-ip-share", "stable", "dp2", "us-south-1", 500, 3000,
			[]map[string]interface{}{
				mountTargetRef("mt-poll-ip", "poll-ip-mt"),
			},
		)
		writeJSON(w, http.StatusCreated, body)
	})
	mux.HandleFunc("/shares/share-poll-ip", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			writeJSON(w, http.StatusMethodNotAllowed, nil)
			return
		}
		body := shareJSONWithMountTargets(
			"share-poll-ip", "poll-ip-share", "stable", "dp2", "us-south-1", 500, 3000,
			[]map[string]interface{}{
				mountTargetRef("mt-poll-ip", "poll-ip-mt"),
			},
		)
		writeJSON(w, http.StatusOK, body)
	})
	mux.HandleFunc("/shares/share-poll-ip/mount_targets/mt-poll-ip", func(w http.ResponseWriter, _ *http.Request) {
		calls := atomic.AddInt32(&mtGetCalls, 1)
		if calls < 3 {
			// First two calls: IP not yet available.
			writeJSON(w, http.StatusOK, mountTargetJSON("mt-poll-ip", "poll-ip-mt", "", ""))
			return
		}
		// Third call: IP available.
		writeJSON(w, http.StatusOK, mountTargetJSON("mt-poll-ip", "poll-ip-mt", "", "10.240.0.99"))
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	// Use getShareFunc to return stable + mount target without IP to trigger
	// the mount target IP polling path.
	client.getShareFunc = func(_ context.Context, _ string) (*ShareInfo, error) {
		return &ShareInfo{
			ID:             "share-poll-ip",
			Name:           "poll-ip-share",
			LifecycleState: "stable",
			SizeGB:         500,
			IOPS:           3000,
			MountTargets: []MountTargetInfo{
				{ID: "mt-poll-ip", Name: "poll-ip-mt", IPAddress: ""},
			},
		}, nil
	}

	input := CreateShareInput{
		Name:    "poll-ip-share",
		Zone:    "us-south-1",
		Profile: "dp2",
		SizeGB:  500,
		VPCId:   "vpc-123",
	}

	info, err := client.CreateFileShare(context.Background(), input)
	if err != nil {
		t.Fatalf("CreateFileShare() error = %v", err)
	}

	// The mount target IP should have been populated by polling.
	if len(info.MountTargets) == 0 {
		t.Fatal("expected at least one mount target")
	}
	if info.MountTargets[0].IPAddress != "10.240.0.99" {
		t.Errorf("MountTarget IP = %q, want %q", info.MountTargets[0].IPAddress, "10.240.0.99")
	}
}

// --- DeleteFileShare with mount target deletion errors ---

func TestDeleteFileShare_MountTargetDeleteError(t *testing.T) {
	// When mount target deletion fails (may already be deleted), DeleteFileShare
	// should still proceed to delete the share.
	var shareDeleteCalled int32

	mux := http.NewServeMux()
	mux.HandleFunc("/shares/share-mt-del-err", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			body := shareJSONWithMountTargets(
				"share-mt-del-err", "del-err-share", "stable", "dp2", "us-south-1", 100, 1000,
				[]map[string]interface{}{
					mountTargetRef("mt-del-err", "del-err-mt"),
				},
			)
			writeJSON(w, http.StatusOK, body)
			return
		}
		if r.Method == http.MethodDelete {
			atomic.AddInt32(&shareDeleteCalled, 1)
			writeJSON(w, http.StatusAccepted, nil)
			return
		}
	})
	mux.HandleFunc("/shares/share-mt-del-err/mount_targets/mt-del-err", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			writeJSON(w, http.StatusOK, mountTargetJSON("mt-del-err", "del-err-mt", "", "10.240.0.1"))
			return
		}
		if r.Method == http.MethodDelete {
			// Return error — mount target already deleted.
			writeJSON(w, http.StatusNotFound, map[string]interface{}{
				"errors": []map[string]interface{}{
					{"code": "not_found", "message": "mount target already deleted"},
				},
			})
			return
		}
	})

	server := httptest.NewServer(mux)
	defer server.Close()

	svc := newTestVPCService(t, server.URL)
	client := newTestClient(t, svc)

	err := client.DeleteFileShare(context.Background(), "share-mt-del-err")
	if err != nil {
		t.Fatalf("DeleteFileShare() error = %v", err)
	}
	if atomic.LoadInt32(&shareDeleteCalled) != 1 {
		t.Errorf("expected share delete to be called, got %d", atomic.LoadInt32(&shareDeleteCalled))
	}
}

// --- test helpers ---

func strPtr(s string) *string {
	return &s
}
