// Package keyprotect is a small client for the IBM Key Protect REST API. It does
// the two actions the backup key needs: wrap (encrypt) and unwrap (decrypt) a
// key with a root key that never leaves Key Protect. It uses only net/http.
//
// Nothing here logs. Errors hold the HTTP status and Key Protect's own short
// message, never the API key, the readable key or the wrapped key.
package keyprotect

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

const (
	iamURL       = "https://iam.cloud.ibm.com"
	requestLimit = 30 * time.Second
	tokenMargin  = time.Minute
	maxBody      = 1 << 20
)

// ErrUnreachable means Key Protect or IAM could not be reached or answered with
// a server error. It is the one case in which an emergency key may be used: any
// other error (a wrong root key, a bad API key) must be fixed, not bypassed.
var ErrUnreachable = errors.New("the Key Protect service is unreachable")

var (
	regionPattern = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)
	idPattern     = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]*$`)
)

// Config says which root key to use and how to log in.
type Config struct {
	// Region is the Key Protect region, such as eu-de.
	Region string
	// InstanceID is the GUID of the Key Protect instance.
	InstanceID string
	// RootKeyID is the ID of the root key.
	RootKeyID string
	// APIKey is the IBM Cloud API key that has access to the root key.
	APIKey string
}

func (c Config) validate() error {
	switch {
	case !regionPattern.MatchString(c.Region):
		return fmt.Errorf("the Key Protect region %q is not a region name such as eu-de", c.Region)
	case !idPattern.MatchString(c.InstanceID):
		return errors.New("the Key Protect instance ID is missing or not valid")
	case !idPattern.MatchString(c.RootKeyID):
		return errors.New("the Key Protect root key ID is missing or not valid")
	case c.APIKey == "":
		return errors.New("the IBM Cloud API key for Key Protect is missing")
	}
	return nil
}

// Client calls Key Protect as the owner of an API key.
type Client struct {
	cfg    Config
	iam    string
	kms    string
	client *http.Client

	mu      sync.Mutex
	token   string
	expires time.Time
}

// New makes a client for the public Key Protect and IAM endpoints.
func New(cfg Config) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return newClient(cfg, iamURL, "https://"+cfg.Region+".kms.cloud.ibm.com", &http.Client{Timeout: requestLimit}), nil
}

// NewWithURLs makes a client for other endpoints. It is for tests; the Jobs use
// New, so the address of Key Protect cannot be changed from outside.
func NewWithURLs(cfg Config, iam, kms string, hc *http.Client) (*Client, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return newClient(cfg, iam, kms, hc), nil
}

func newClient(cfg Config, iam, kms string, hc *http.Client) *Client {
	return &Client{cfg: cfg, iam: strings.TrimRight(iam, "/"), kms: strings.TrimRight(kms, "/"), client: hc}
}

// Wrap has Key Protect encrypt key with the root key. The result is safe to
// store.
func (c *Client) Wrap(ctx context.Context, key string) (string, error) {
	var out struct {
		Ciphertext string `json:"ciphertext"`
	}
	body := map[string]string{"plaintext": base64.StdEncoding.EncodeToString([]byte(key))}
	if err := c.action(ctx, "wrap", body, &out); err != nil {
		return "", err
	}
	if out.Ciphertext == "" {
		return "", errors.New("wrap: the Key Protect answer holds no wrapped key")
	}
	return out.Ciphertext, nil
}

// Unwrap has Key Protect decrypt a wrapped key and returns the readable key.
func (c *Client) Unwrap(ctx context.Context, wrapped string) (string, error) {
	var out struct {
		Plaintext string `json:"plaintext"`
	}
	if err := c.action(ctx, "unwrap", map[string]string{"ciphertext": wrapped}, &out); err != nil {
		return "", err
	}
	key, err := base64.StdEncoding.DecodeString(out.Plaintext)
	if err != nil || len(key) == 0 {
		return "", errors.New("unwrap: the Key Protect answer holds no key")
	}
	return string(key), nil
}

func (c *Client) action(ctx context.Context, name string, body, out any) error {
	token, err := c.bearer(ctx)
	if err != nil {
		return err
	}
	payload, _ := json.Marshal(body)
	u := c.kms + "/api/v2/keys/" + url.PathEscape(c.cfg.RootKeyID) + "?action=" + name
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, strings.NewReader(string(payload)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Bluemix-Instance", c.cfg.InstanceID)
	req.Header.Set("Content-Type", "application/vnd.ibm.kms.key_action+json")
	return c.do(req, "Key Protect "+name, out)
}

func (c *Client) bearer(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" && time.Now().Before(c.expires) {
		return c.token, nil
	}
	form := url.Values{"grant_type": {"urn:ibm:params:oauth:grant-type:apikey"}, "apikey": {c.cfg.APIKey}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.iam+"/identity/token", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
	}
	if err = c.do(req, "IAM token", &out); err != nil {
		return "", err
	}
	if out.AccessToken == "" {
		return "", errors.New("IAM token: the answer holds no token")
	}
	c.token = out.AccessToken
	c.expires = time.Now().Add(time.Duration(out.ExpiresIn)*time.Second - tokenMargin)
	return c.token, nil
}

func (c *Client) do(req *http.Request, what string, out any) error {
	resp, err := c.client.Do(req)
	if err != nil {
		// Do not wrap err: for a URL error it can hold the request URL only, but
		// keep the message short and free of anything we sent.
		var ue *url.Error
		if errors.As(err, &ue) {
			err = ue.Err
		}
		return fmt.Errorf("%s: %w: %v", what, ErrUnreachable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	switch {
	case resp.StatusCode >= 500 || resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%s: %w (HTTP %d)", what, ErrUnreachable, resp.StatusCode)
	case resp.StatusCode/100 != 2:
		return fmt.Errorf("%s was refused (HTTP %d)%s", what, resp.StatusCode, serverMessage(data))
	}
	if err = json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("%s: the answer is not valid JSON", what)
	}
	return nil
}

// serverMessage returns Key Protect's short error text, if it has one.
func serverMessage(data []byte) string {
	var e struct {
		Resources []struct {
			ErrorMsg string `json:"errorMsg"`
		} `json:"resources"`
		ErrorMessage string `json:"errorMessage"`
	}
	if json.Unmarshal(data, &e) != nil {
		return ""
	}
	msg := e.ErrorMessage
	if len(e.Resources) > 0 && e.Resources[0].ErrorMsg != "" {
		msg = e.Resources[0].ErrorMsg
	}
	if len(msg) > 200 {
		msg = msg[:200]
	}
	if msg == "" {
		return ""
	}
	return ": " + msg
}
