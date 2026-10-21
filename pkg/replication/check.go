package replication

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
)

// The reasons a receiver check can fail. They are short and plain on purpose:
// the console shows them to the operator as they are.
const (
	ReasonCannotConnect       = "cannot connect"
	ReasonCertNotTrusted      = "certificate not trusted"
	ReasonTokenRefused        = "token refused"
	ReasonStorageNotReachable = "receiver storage not reachable"
	ReasonReceiverTooOld      = "receiver has no auth-check call; update it"
	ReasonBadInput            = "bad input"
)

// CheckError is why a receiver check failed.
type CheckError struct {
	// Reason is one of the Reason constants.
	Reason string
	// Err is the cause, for logs.
	Err error
}

func (e *CheckError) Error() string {
	if e.Err == nil {
		return e.Reason
	}
	return fmt.Sprintf("%s: %v", e.Reason, e.Err)
}

func (e *CheckError) Unwrap() error { return e.Err }

// Check proves three things in order: the receiver answers, its certificate is
// trusted, and it accepts the token. It changes nothing on the receiver.
func (c *SyncClient) Check(ctx context.Context) error {
	// The health call needs no token, so it shows a connection or certificate problem first.
	status, err := c.checkGet(ctx, "/api/v1/health", false)
	if err != nil {
		return err
	}
	if status == http.StatusServiceUnavailable {
		return &CheckError{Reason: ReasonStorageNotReachable}
	}
	if status != http.StatusOK {
		return &CheckError{Reason: ReasonCannotConnect, Err: fmt.Errorf("health call returned HTTP %d", status)}
	}

	status, err = c.checkGet(ctx, "/api/v1/auth-check", true)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
		return nil
	case http.StatusUnauthorized:
		return &CheckError{Reason: ReasonTokenRefused}
	case http.StatusServiceUnavailable:
		return &CheckError{Reason: ReasonStorageNotReachable}
	case http.StatusNotFound:
		return &CheckError{Reason: ReasonReceiverTooOld}
	default:
		return &CheckError{Reason: ReasonCannotConnect, Err: fmt.Errorf("auth-check call returned HTTP %d", status)}
	}
}

// checkGet makes one GET and returns the HTTP status. A transport failure is a CheckError.
func (c *SyncClient) checkGet(ctx context.Context, path string, withToken bool) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.endpoint+path, nil)
	if err != nil {
		return 0, &CheckError{Reason: ReasonBadInput, Err: err}
	}
	if withToken {
		req.Header.Set("Authorization", "Bearer "+c.authToken)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		var certErr *tls.CertificateVerificationError
		if errors.As(err, &certErr) {
			return 0, &CheckError{Reason: ReasonCertNotTrusted, Err: err}
		}
		return 0, &CheckError{Reason: ReasonCannotConnect, Err: err}
	}
	defer closeBody(resp.Body)
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, nil
}

// CheckReceiver runs Check with the token and the CA certificate read from files.
// An empty caCertFile means the system trust store.
func CheckReceiver(ctx context.Context, endpoint, tokenFile, caCertFile string) error {
	tokenBytes, err := os.ReadFile(tokenFile) //nolint:gosec // G304: path comes from the operator-set --auth-token-file flag
	if err != nil {
		return &CheckError{Reason: ReasonBadInput, Err: fmt.Errorf("read token file: %w", err)}
	}
	var caPEM []byte
	if caCertFile != "" {
		caPEM, err = os.ReadFile(caCertFile) //nolint:gosec // G304: path comes from the operator-set --ca-cert-file flag
		if err != nil {
			return &CheckError{Reason: ReasonBadInput, Err: fmt.Errorf("read CA file: %w", err)}
		}
	}
	client, err := NewSyncClientWithCA(endpoint, strings.TrimSpace(string(tokenBytes)), caPEM)
	if err != nil {
		return &CheckError{Reason: ReasonBadInput, Err: err}
	}
	return client.Check(ctx)
}
