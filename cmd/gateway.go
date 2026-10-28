package main

import (
	"context"
	"crypto/tls"
	"errors"
	"net/http"
	"os"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/gateway"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// gatewayOptions are the flags of the backup gateway.
type gatewayOptions struct {
	listen      string
	certFile    string
	keyFile     string
	sessionWait time.Duration
	maxDownload int64
	// maxOpenRestores limits the file restores one user has that have not ended.
	maxOpenRestores int
	authCache       time.Duration
	// browseNamespace holds the session Secrets and helper pods.
	browseNamespace string
}

// runGateway serves the browse API for users. It checks every caller with the
// Kubernetes API and relays calls to the helper pods.
func runGateway(o gatewayOptions) {
	if o.certFile == "" || o.keyFile == "" {
		klog.ErrorS(nil, "The gateway needs --gateway-tls-cert-file and --gateway-tls-key-file: it is never served in plain HTTP")
		os.Exit(1)
	}
	loader, err := gateway.NewCertLoader(o.certFile, o.keyFile)
	if err != nil {
		klog.ErrorS(err, "Cannot load the gateway certificate")
		os.Exit(1)
	}
	cfg := ctrl.GetConfigOrDie()
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	if serr := v1alpha1.AddToScheme(scheme); serr != nil {
		klog.ErrorS(serr, "Failed to add CRD types to scheme")
		os.Exit(1)
	}
	// No cache: the gateway reads a few objects per call and must see them now.
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		klog.ErrorS(err, "Failed to create the Kubernetes client")
		os.Exit(1)
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.ErrorS(err, "Failed to create the Kubernetes clientset")
		os.Exit(1)
	}
	auth := gateway.NewKubeAuth(cs)
	g := gateway.New(c, auth, auth, gateway.Config{
		SessionNamespace: o.browseNamespace,
		EventNamespace:   driverNamespace,
		SessionWait:      o.sessionWait,
		MaxDownloadBytes: o.maxDownload,
		MaxOpenRestores:  o.maxOpenRestores,
		AuthCacheTTL:     o.authCache,
	})
	srv := &http.Server{
		Addr:              o.listen,
		Handler:           g.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         &tls.Config{GetCertificate: loader.GetCertificate, MinVersion: tls.VersionTLS12},
	}
	ctx := ctrl.SetupSignalHandler()
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdown)
	}()
	klog.InfoS("Starting the backup gateway", "listen", o.listen)
	if err := srv.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		klog.ErrorS(err, "The backup gateway failed")
		os.Exit(1)
	}
}
