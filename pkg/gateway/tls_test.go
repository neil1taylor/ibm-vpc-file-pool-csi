package gateway

import (
	"bytes"
	"crypto/tls"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/browse"
)

func writePair(t *testing.T, dir string) []byte {
	t.Helper()
	cert, key, err := browse.NewSessionCert(time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.crt"), cert, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tls.key"), key, 0o600); err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestCertLoaderFollowsRotation(t *testing.T) {
	dir := t.TempDir()
	first := writePair(t, dir)
	l, err := NewCertLoader(filepath.Join(dir, "tls.crt"), filepath.Join(dir, "tls.key"))
	if err != nil {
		t.Fatal(err)
	}
	c, err := l.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil || c == nil {
		t.Fatalf("first: %v", err)
	}
	firstDER := c.Certificate[0]
	second := writePair(t, dir)
	if bytes.Equal(first, second) {
		t.Fatal("same certificate twice")
	}
	// Make the new files look newer than the load.
	future := time.Now().Add(time.Minute)
	_ = os.Chtimes(filepath.Join(dir, "tls.crt"), future, future)
	_ = os.Chtimes(filepath.Join(dir, "tls.key"), future, future)
	c2, err := l.GetCertificate(&tls.ClientHelloInfo{})
	if err != nil || bytes.Equal(c2.Certificate[0], firstDER) {
		t.Errorf("the rotated certificate was not used (%v)", err)
	}
}

func TestCertLoaderRefusesABadPair(t *testing.T) {
	dir := t.TempDir()
	if _, err := NewCertLoader(filepath.Join(dir, "nope.crt"), filepath.Join(dir, "nope.key")); err == nil {
		t.Error("missing files must be refused")
	}
}
