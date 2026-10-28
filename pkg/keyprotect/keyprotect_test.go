package keyprotect

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const (
	testInstance = "11111111-2222-3333-4444-555555555555"
	testRootKey  = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	testAPIKey   = "test-api-key-value"
	// testBackupKey is the readable key. It must never show up in a log or error.
	testBackupKey = "SENTINEL-readable-backup-key-0123456789abcdef"
)

// fakeKeyProtect is an IAM token endpoint and a Key Protect wrap/unwrap endpoint.
type fakeKeyProtect struct {
	t        *testing.T
	iam, kms *httptest.Server
	tokens   int
	status   int // when not 0, the status of every Key Protect answer
}

func newFakeKeyProtect(t *testing.T) *fakeKeyProtect {
	f := &fakeKeyProtect{t: t}
	f.iam = httptest.NewServer(http.HandlerFunc(f.serveIAM))
	f.kms = httptest.NewServer(http.HandlerFunc(f.serveKMS))
	t.Cleanup(f.iam.Close)
	t.Cleanup(f.kms.Close)
	return f
}

func (f *fakeKeyProtect) serveIAM(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != "/identity/token" {
		http.NotFound(w, r)
		return
	}
	_ = r.ParseForm()
	if r.PostForm.Get("grant_type") != "urn:ibm:params:oauth:grant-type:apikey" || r.PostForm.Get("apikey") != testAPIKey {
		http.Error(w, `{"errorMessage":"bad api key"}`, http.StatusBadRequest)
		return
	}
	f.tokens++
	_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "iam-token", "expires_in": 3600})
}

func (f *fakeKeyProtect) serveKMS(w http.ResponseWriter, r *http.Request) {
	if f.status != 0 {
		http.Error(w, `{"resources":[{"errorMsg":"down"}]}`, f.status)
		return
	}
	if r.Header.Get("Authorization") != "Bearer iam-token" || r.Header.Get("Bluemix-Instance") != testInstance {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost || !strings.HasPrefix(r.URL.Path, "/api/v2/keys/") {
		http.NotFound(w, r)
		return
	}
	if strings.TrimPrefix(r.URL.Path, "/api/v2/keys/") != testRootKey {
		http.Error(w, `{"resources":[{"errorMsg":"key not found"}]}`, http.StatusNotFound)
		return
	}
	var body map[string]string
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.URL.Query().Get("action") {
	case "wrap":
		_ = json.NewEncoder(w).Encode(map[string]string{"ciphertext": "wrapped." + body["plaintext"]})
	case "unwrap":
		pt, ok := strings.CutPrefix(body["ciphertext"], "wrapped.")
		if !ok {
			http.Error(w, `{"resources":[{"errorMsg":"cannot unwrap"}]}`, http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"plaintext": pt})
	default:
		http.Error(w, "bad action", http.StatusBadRequest)
	}
}

func (f *fakeKeyProtect) client(rootKey string) *Client {
	f.t.Helper()
	c, err := NewWithURLs(Config{Region: "eu-de", InstanceID: testInstance, RootKeyID: rootKey, APIKey: testAPIKey}, f.iam.URL, f.kms.URL, f.kms.Client())
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func TestWrapThenUnwrap_RoundTripsTheKey(t *testing.T) {
	f := newFakeKeyProtect(t)
	c := f.client(testRootKey)
	wrapped, err := c.Wrap(context.Background(), testBackupKey)
	if err != nil {
		t.Fatalf("Wrap: %v", err)
	}
	if strings.Contains(wrapped, testBackupKey) {
		t.Fatalf("the wrapped key holds the readable key")
	}
	got, err := c.Unwrap(context.Background(), wrapped)
	if err != nil || got != testBackupKey {
		t.Fatalf("Unwrap = %q, %v; want the original key", got, err)
	}
}

func TestUnwrap_ReusesTheIAMToken(t *testing.T) {
	f := newFakeKeyProtect(t)
	c := f.client(testRootKey)
	wrapped, _ := c.Wrap(context.Background(), testBackupKey)
	for i := 0; i < 3; i++ {
		if _, err := c.Unwrap(context.Background(), wrapped); err != nil {
			t.Fatal(err)
		}
	}
	if f.tokens != 1 {
		t.Errorf("asked IAM for %d tokens, want 1", f.tokens)
	}
}

func TestUnwrap_WrongRootKeyFails(t *testing.T) {
	f := newFakeKeyProtect(t)
	wrapped, _ := f.client(testRootKey).Wrap(context.Background(), testBackupKey)
	_, err := f.client("99999999-9999-9999-9999-999999999999").Unwrap(context.Background(), wrapped)
	if err == nil {
		t.Fatal("Unwrap with the wrong root key worked")
	}
	if errors.Is(err, ErrUnreachable) {
		t.Errorf("a wrong root key is not 'unreachable': %v", err)
	}
}

func TestUnwrap_BadAPIKeyFails(t *testing.T) {
	f := newFakeKeyProtect(t)
	c, _ := NewWithURLs(Config{Region: "eu-de", InstanceID: testInstance, RootKeyID: testRootKey, APIKey: "wrong"}, f.iam.URL, f.kms.URL, f.kms.Client())
	_, err := c.Unwrap(context.Background(), "wrapped.x")
	if err == nil || errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v; want a plain failure, not unreachable", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Errorf("the error holds the API key: %v", err)
	}
}

func TestUnwrap_ServerDownIsUnreachable(t *testing.T) {
	f := newFakeKeyProtect(t)
	c := f.client(testRootKey)
	f.kms.Close()
	if _, err := c.Unwrap(context.Background(), "wrapped.x"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v; want ErrUnreachable", err)
	}
}

func TestUnwrap_ServerErrorIsUnreachable(t *testing.T) {
	f := newFakeKeyProtect(t)
	f.status = http.StatusServiceUnavailable
	if _, err := f.client(testRootKey).Unwrap(context.Background(), "wrapped.x"); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("err = %v; want ErrUnreachable", err)
	}
}

func TestNew_RefusesBadConfig(t *testing.T) {
	good := Config{Region: "eu-de", InstanceID: testInstance, RootKeyID: testRootKey, APIKey: testAPIKey}
	cases := map[string]func(*Config){
		"no region":         func(c *Config) { c.Region = "" },
		"region with slash": func(c *Config) { c.Region = "eu-de/evil" },
		"region with host":  func(c *Config) { c.Region = "evil.example.com#" },
		"no instance":       func(c *Config) { c.InstanceID = "" },
		"no root key":       func(c *Config) { c.RootKeyID = "" },
		"root key path":     func(c *Config) { c.RootKeyID = "../x" },
		"no api key":        func(c *Config) { c.APIKey = "" },
	}
	for name, mod := range cases {
		c := good
		mod(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s: New accepted it", name)
		}
	}
	if _, err := New(good); err != nil {
		t.Errorf("good config: %v", err)
	}
}

type stubUnwrapper struct {
	key string
	err error
}

func (s stubUnwrapper) Unwrap(context.Context, string) (string, error) { return s.key, s.err }

func TestResolve_UsesKeyProtect(t *testing.T) {
	var log bytes.Buffer
	key, err := Resolve(context.Background(), stubUnwrapper{key: testBackupKey}, "wrapped", "EMERGENCY-KEY", &log)
	if err != nil || key != testBackupKey {
		t.Fatalf("Resolve = %q, %v", key, err)
	}
}

func TestResolve_KeyProtectDownWithEmergencyKeyUsesIt(t *testing.T) {
	var log bytes.Buffer
	key, err := Resolve(context.Background(), stubUnwrapper{err: ErrUnreachable}, "wrapped", "EMERGENCY-KEY\n", &log)
	if err != nil || key != "EMERGENCY-KEY" {
		t.Fatalf("Resolve = %q, %v; want the emergency key without its newline", key, err)
	}
	if !strings.Contains(log.String(), "emergency") {
		t.Errorf("the log does not say the emergency key was used: %q", log.String())
	}
	if strings.Contains(log.String(), "EMERGENCY-KEY") {
		t.Errorf("the log holds the key: %q", log.String())
	}
}

func TestResolve_KeyProtectDownWithoutEmergencyKeyIsAClearError(t *testing.T) {
	_, err := Resolve(context.Background(), stubUnwrapper{err: ErrUnreachable}, "wrapped", "", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "emergency") || !strings.Contains(err.Error(), "Key Protect") {
		t.Fatalf("err = %v; want a message about Key Protect and the emergency key", err)
	}
}

func TestResolve_AWrongRootKeyDoesNotFallBack(t *testing.T) {
	_, err := Resolve(context.Background(), stubUnwrapper{err: errors.New("unwrap refused (HTTP 400)")}, "wrapped", "EMERGENCY-KEY", io.Discard)
	if err == nil {
		t.Fatal("a refused unwrap silently used the emergency key")
	}
}

func TestResolve_NoWrappedKeyAndNoEmergencyKeyFails(t *testing.T) {
	if _, err := Resolve(context.Background(), nil, "", "", io.Discard); err == nil {
		t.Error("no wrapped key and no emergency key must fail")
	}
}

func TestResolve_NeverLogsOrReturnsTheKeyInErrors(t *testing.T) {
	f := newFakeKeyProtect(t)
	wrapped, _ := f.client(testRootKey).Wrap(context.Background(), testBackupKey)
	var log bytes.Buffer
	_, err := Resolve(context.Background(), f.client("99999999-9999-9999-9999-999999999999"), wrapped, "", &log)
	if err == nil {
		t.Fatal("expected failure")
	}
	for _, s := range []string{log.String(), err.Error()} {
		if strings.Contains(s, testBackupKey) || strings.Contains(s, base64.StdEncoding.EncodeToString([]byte(testBackupKey))) {
			t.Errorf("the key is in %q", s)
		}
	}
}
