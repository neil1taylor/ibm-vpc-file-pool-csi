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
// non-resource URL /metrics (SubjectAccessReview). The certificate is
// self-signed and made at start.
func NewSecureServer(addr string, cfg *rest.Config, httpClient *http.Client) (metricsserver.Server, error) {
	return metricsserver.NewServer(metricsserver.Options{
		BindAddress:    addr,
		SecureServing:  true,
		FilterProvider: filters.WithAuthenticationAndAuthorization,
	}, cfg, httpClient)
}
