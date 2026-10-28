package browse

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func tlsServer(t *testing.T, certPEM, keyPEM []byte) *httptest.Server {
	t.Helper()
	cfg, err := ServerTLSConfig(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, "ok") }))
	ts.TLS = cfg
	ts.StartTLS()
	t.Cleanup(ts.Close)
	return ts
}

func get(c *http.Client, url string) error {
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d", resp.StatusCode)
	}
	return nil
}

func TestPinnedClientAcceptsOnlyThePinnedCertificate(t *testing.T) {
	cert, key, err := NewSessionCert(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	otherCert, otherKey, err := NewSessionCert(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	good := tlsServer(t, cert, key)
	other := tlsServer(t, otherCert, otherKey)

	c, err := PinnedClient(cert)
	if err != nil {
		t.Fatal(err)
	}
	// The address is 127.0.0.1, not the session name: the pin decides.
	if err := get(c, good.URL); err != nil {
		t.Errorf("the pinned server was refused: %v", err)
	}
	if err := get(c, other.URL); err == nil {
		t.Error("a server with a different session certificate was accepted")
	}
	plain := httptest.NewServer(http.NotFoundHandler())
	defer plain.Close()
	if err := get(c, "https://"+plain.Listener.Addr().String()); err == nil {
		t.Error("a server without TLS was accepted")
	}
	if err := get(c, "http://"+good.Listener.Addr().String()); err == nil {
		t.Error("plain HTTP to a TLS server worked")
	}
}

func TestPinnedClientRefusesAnExpiredCertificate(t *testing.T) {
	cert, key, err := NewSessionCert(-time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	ts := tlsServer(t, cert, key)
	c, _ := PinnedClient(cert)
	if err := get(c, ts.URL); err == nil {
		t.Error("an expired certificate was accepted")
	}
}

func TestPinnedClientRefusesBadPEM(t *testing.T) {
	if _, err := PinnedClient([]byte("not a certificate")); err == nil {
		t.Error("bad PEM must be refused")
	}
	if _, err := ServerTLSConfig([]byte("x"), []byte("y")); err == nil {
		t.Error("bad key pair must be refused")
	}
}
