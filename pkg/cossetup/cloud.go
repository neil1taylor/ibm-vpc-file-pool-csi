package cossetup

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
)

// Doer is the part of http.Client that this package uses. Tests give it a
// client for an httptest server.
type Doer interface {
	Do(*http.Request) (*http.Response, error)
}

// Endpoints are the base URLs of the IBM Cloud APIs. Tests point them at a
// fake server.
type Endpoints struct {
	ResourceController string
	ResourceManager    string
	IAM                string
	CBR                string
	// COS gives the S3 base URL of a region.
	COS func(region string) string
	// KeyProtect gives the Key Protect base URL of a region.
	KeyProtect func(region string) string
}

// DefaultEndpoints are the public IBM Cloud endpoints.
func DefaultEndpoints() Endpoints {
	return Endpoints{
		ResourceController: "https://resource-controller.cloud.ibm.com",
		ResourceManager:    "https://resource-controller.cloud.ibm.com",
		IAM:                "https://iam.cloud.ibm.com",
		CBR:                "https://cbr.cloud.ibm.com",
		COS:                func(region string) string { return "https://s3." + region + ".cloud-object-storage.appdomain.cloud" },
		KeyProtect:         func(region string) string { return "https://" + region + ".kms.cloud.ibm.com" },
	}
}

// TokenSource gives the caller's own IAM access token. The token is held in
// memory for one run. It is never stored and never printed.
type TokenSource interface {
	Token(ctx context.Context) (string, error)
}

// StaticToken is a token that is already known.
type StaticToken string

// Token returns the token.
func (t StaticToken) Token(context.Context) (string, error) { return string(t), nil }

// EnvironmentToken finds the token as the IBM Cloud tools do: IBMCLOUD_IAM_TOKEN,
// then an API key in IBMCLOUD_API_KEY or IC_API_KEY (exchanged for a token),
// then the login of the ibmcloud CLI.
type EnvironmentToken struct {
	HTTP Doer
	IAM  string
}

// Token returns a bearer token without the "Bearer " prefix.
func (e EnvironmentToken) Token(ctx context.Context) (string, error) {
	if t := os.Getenv("IBMCLOUD_IAM_TOKEN"); t != "" {
		return strings.TrimPrefix(t, "Bearer "), nil
	}
	for _, name := range []string{"IBMCLOUD_API_KEY", "IC_API_KEY"} {
		if key := os.Getenv(name); key != "" {
			return e.exchange(ctx, key)
		}
	}
	out, err := exec.CommandContext(ctx, "ibmcloud", "iam", "oauth-tokens", "--output", "json").Output()
	if err != nil {
		return "", fmt.Errorf("no IBM Cloud login found: set IBMCLOUD_API_KEY, or run `ibmcloud login` first")
	}
	var parsed struct {
		Token string `json:"iam_token"`
	}
	if err = json.Unmarshal(out, &parsed); err != nil || parsed.Token == "" {
		return "", fmt.Errorf("`ibmcloud iam oauth-tokens` gave no token: run `ibmcloud login` again")
	}
	return strings.TrimPrefix(parsed.Token, "Bearer "), nil
}

func (e EnvironmentToken) exchange(ctx context.Context, apiKey string) (string, error) {
	form := url.Values{"grant_type": {"urn:ibm:params:oauth:grant-type:apikey"}, "apikey": {apiKey}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, e.IAM+"/identity/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	resp, err := e.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("exchanging the API key for a token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("exchanging the API key for a token: HTTP %d", resp.StatusCode)
	}
	var parsed struct {
		Token string `json:"access_token"`
	}
	if err = json.Unmarshal(body, &parsed); err != nil || parsed.Token == "" {
		return "", fmt.Errorf("exchanging the API key for a token: no token in the answer")
	}
	return parsed.Token, nil
}

// AddressFinder tells the address that IBM Cloud sees for the caller. The
// CBR zone must hold it, or the caller is locked out of the bucket.
type AddressFinder interface {
	Address(ctx context.Context) (string, error)
}

// PublicAddress asks a public service for the caller's address.
type PublicAddress struct {
	HTTP Doer
	URL  string
}

// Address returns the caller's public address.
func (p PublicAddress) Address(ctx context.Context) (string, error) {
	u := p.URL
	if u == "" {
		u = "https://api.ipify.org"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", err
	}
	resp, err := p.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("finding your public address (pass --my-address to give it): %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return strings.TrimSpace(string(b)), nil
}

// accountID reads the account from the claims of a token. The claims are not
// checked here: IBM Cloud checks the token on every call.
func accountID(token string) (string, error) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return "", fmt.Errorf("the IAM token is not a JWT")
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return "", fmt.Errorf("the IAM token is not a JWT: %w", err)
	}
	var claims struct {
		Account struct {
			BSS string `json:"bss"`
		} `json:"account"`
	}
	if err = json.Unmarshal(raw, &claims); err != nil || claims.Account.BSS == "" {
		return "", fmt.Errorf("the IAM token names no account")
	}
	return claims.Account.BSS, nil
}

// reply is one answer of an API.
type reply struct {
	status int
	body   []byte
}

func (r reply) ok() bool { return r.status >= 200 && r.status < 300 }

func (r reply) decode(v any) error {
	if err := json.Unmarshal(r.body, v); err != nil {
		return fmt.Errorf("unreadable answer: %w", err)
	}
	return nil
}

// call sends one request with the caller's token. body, when not nil, is sent
// as JSON; a []byte is sent as it is. It returns the answer whatever the
// status; the caller decides what a status means. Errors never hold the token.
func (r *run) call(ctx context.Context, method, rawURL string, body any, headers map[string]string) (reply, error) {
	var rd io.Reader
	switch b := body.(type) {
	case nil:
	case []byte:
		rd = bytes.NewReader(b)
	default:
		enc, err := json.Marshal(body)
		if err != nil {
			return reply{}, err
		}
		rd = bytes.NewReader(enc)
		if headers == nil {
			headers = map[string]string{}
		}
		if _, set := headers["Content-Type"]; !set {
			headers["Content-Type"] = "application/json"
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, rawURL, rd)
	if err != nil {
		return reply{}, err
	}
	req.Header.Set("Authorization", "Bearer "+r.token)
	req.Header.Set("Accept", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := r.s.HTTP.Do(req)
	if err != nil {
		return reply{}, fmt.Errorf("%s %s: %w", method, redactURL(rawURL), err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return reply{status: resp.StatusCode, body: data}, nil
}

func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "(url)"
	}
	return u.Scheme + "://" + u.Host + u.Path
}

// must turns an unexpected status into an error. what says what was tried.
func must(what string, rep reply, err error) (reply, error) {
	if err != nil {
		return rep, err
	}
	if !rep.ok() {
		msg := strings.TrimSpace(string(rep.body))
		if len(msg) > 300 {
			msg = msg[:300] + "..."
		}
		return rep, fmt.Errorf("%s: HTTP %d: %s", what, rep.status, msg)
	}
	return rep, nil
}
