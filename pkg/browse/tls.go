package browse

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"
)

// SessionServerName is the name in every session certificate and the name the
// client asks for. The pod address is not known when the certificate is made,
// so trust comes from the pinned certificate, not from the address.
const SessionServerName = "browse-session"

// NewSessionCert makes a key and a self-signed certificate for one browse
// session, both as PEM. The certificate is its own root, so a client can trust
// exactly this one.
func NewSessionCert(validFor time.Duration) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 120))
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: SessionServerName},
		DNSNames:              []string{SessionServerName},
		NotBefore:             now.Add(-5 * time.Minute),
		NotAfter:              now.Add(validFor),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLen:            0,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, err
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	return certPEM, keyPEM, nil
}

// PinnedClient returns an HTTP client that trusts exactly one certificate, the
// session certificate, and nothing else: not the system roots, and no other
// certificate even if it is also valid for the same name. It asks for the name
// SessionServerName whatever address it connects to.
func PinnedClient(certPEM []byte) (*http.Client, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(certPEM) {
		return nil, errors.New("the session certificate is not valid PEM")
	}
	return &http.Client{Transport: &http.Transport{
		TLSClientConfig: &tls.Config{
			RootCAs:    pool,
			ServerName: SessionServerName,
			MinVersion: tls.VersionTLS13,
		},
	}}, nil
}

// ServerTLSConfig builds the helper's TLS settings from the certificate and key.
func ServerTLSConfig(certPEM, keyPEM []byte) (*tls.Config, error) {
	pair, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, fmt.Errorf("the session certificate and key: %w", err)
	}
	return &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS13}, nil
}
