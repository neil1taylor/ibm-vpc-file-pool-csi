package nodemetrics

import (
	"net/http"

	"k8s.io/client-go/rest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
)

// NewSecureServer serves the process's metrics registry (the one the collector
// is registered in) over HTTPS on addr. Each request must carry a bearer token
// that the API server accepts (TokenReview) and whose user may "get" the
// non-resource URL /metrics (SubjectAccessReview). If certDir is set, the server
// uses tls.crt and tls.key from it (and reloads them when they change), so
// callers can check the certificate; otherwise it makes a self-signed one.
func NewSecureServer(addr, certDir string, cfg *rest.Config, httpClient *http.Client) (metricsserver.Server, error) {
	return metricsserver.NewServer(metricsserver.Options{
		BindAddress:    addr,
		SecureServing:  true,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
		CertDir:        certDir,
	}, cfg, httpClient)
}
