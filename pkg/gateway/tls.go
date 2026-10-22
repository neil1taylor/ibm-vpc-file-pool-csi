package gateway

import (
	"crypto/tls"
	"os"
	"sync"
	"time"
)

// CertLoader gives the serving certificate, and reads the files again when
// they change, so a rotated certificate is used without a restart.
type CertLoader struct {
	certFile, keyFile string

	mu    sync.Mutex
	cert  *tls.Certificate
	mtime time.Time
}

// NewCertLoader reads the certificate once, to fail early on a bad pair.
func NewCertLoader(certFile, keyFile string) (*CertLoader, error) {
	l := &CertLoader{certFile: certFile, keyFile: keyFile}
	if err := l.load(); err != nil {
		return nil, err
	}
	return l, nil
}

func (l *CertLoader) load() error {
	pair, err := tls.LoadX509KeyPair(l.certFile, l.keyFile)
	if err != nil {
		return err
	}
	l.cert = &pair
	l.mtime = newest(l.certFile, l.keyFile)
	return nil
}

func newest(files ...string) time.Time {
	var t time.Time
	for _, f := range files {
		if fi, err := os.Stat(f); err == nil && fi.ModTime().After(t) {
			t = fi.ModTime()
		}
	}
	return t
}

// GetCertificate is for tls.Config.GetCertificate. If the files changed and the
// new pair cannot be read, the old certificate stays in use.
func (l *CertLoader) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if newest(l.certFile, l.keyFile).After(l.mtime) {
		_ = l.load()
	}
	return l.cert, nil
}
