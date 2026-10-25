package poolbench

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/hooks"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/nodemetrics"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

// serviceAccountTokenFile is where a pod finds its service account token.
const serviceAccountTokenFile = "/var/run/secrets/kubernetes.io/serviceaccount/token" // #nosec G101 -- a path, not a credential

// Options are the settings of the PoolBench service.
type Options struct {
	// Namespace is the driver's namespace, where the service runs.
	Namespace string
	// CollectorAddr is where the collector listens for the test VMs.
	CollectorAddr string
	// CollectorURL is how the test VMs reach the collector.
	CollectorURL string
	// MetricsAddr and MetricsCertDir serve the metrics over HTTPS.
	MetricsAddr    string
	MetricsCertDir string
	// ResultsAddr serves the raw results to the console's plugin proxy over
	// HTTPS, with the certificate in MetricsCertDir.
	ResultsAddr string
	HealthAddr  string
	// ResultsDir holds the raw results (a persistent volume).
	ResultsDir string
	// Interval is the time between two passes of the runner.
	Interval time.Duration
	// RunRole, ServiceAccount: see Runner.
	RunRole        string
	ServiceAccount string
	// NodeMetricsService is the node agents' metrics Service, and
	// NodeMetricsCA the file with the CA that signed their certificate.
	NodeMetricsService string
	NodeMetricsCA      string
}

// hookExec adapts the hooks package's pod exec to an Executor.
type hookExec struct{ e hooks.PodExecer }

func (h hookExec) Exec(ctx context.Context, namespace, pod, container string, cmd []string) (string, error) {
	out, errOut, err := h.e.Exec(ctx, namespace, pod, container, cmd)
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(errOut))
	}
	return out, nil
}

// leaderRunnable runs a function only on the elected replica.
type leaderRunnable func(ctx context.Context) error

var _ manager.LeaderElectionRunnable = leaderRunnable(nil)

func (f leaderRunnable) Start(ctx context.Context) error { return f(ctx) }
func (leaderRunnable) NeedLeaderElection() bool          { return true }

func agentHTTPClient(caFile, serverName string) (*http.Client, error) {
	cfg := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: serverName}
	if caFile != "" {
		pem, err := os.ReadFile(caFile) //nolint:gosec // the path is a flag of the service, set by the chart
		if err != nil {
			return nil, fmt.Errorf("read the node agents' CA: %w", err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("the node agents' CA file has no certificate")
		}
		cfg.RootCAs = pool
	}
	return &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{TLSClientConfig: cfg}}, nil
}

// Run runs the PoolBench service until ctx ends: the runner, which takes
// every PoolBench through its stages, and the collector the test VMs send
// their results to. Both run only on the elected replica, because the
// runner reads the collector's store.
func Run(ctx context.Context, cfg *rest.Config, opts Options) error {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return err
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		return err
	}
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme:                  scheme,
		LeaderElection:          true,
		LeaderElectionID:        "ibm-vpc-file-pool-csi-poolbench",
		LeaderElectionNamespace: opts.Namespace,
		HealthProbeBindAddress:  opts.HealthAddr,
		Metrics:                 nodemetrics.SecureOptions(opts.MetricsAddr, opts.MetricsCertDir),
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}
	if herr := mgr.AddHealthzCheck("ping", healthz.Ping); herr != nil {
		return herr
	}
	// The runner reads few objects at long intervals, and some (pods in every
	// namespace) would be costly to cache, so it reads the API directly.
	c, err := client.New(cfg, client.Options{Scheme: scheme, Mapper: mgr.GetRESTMapper()})
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	clientset, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		return fmt.Errorf("create clientset: %w", err)
	}
	httpClient, err := agentHTTPClient(opts.NodeMetricsCA, opts.NodeMetricsService+"."+opts.Namespace+".svc")
	if err != nil {
		return err
	}
	if merr := os.MkdirAll(opts.ResultsDir, 0o750); merr != nil {
		return fmt.Errorf("make the results folder: %w", merr)
	}
	store, err := NewStore(opts.ResultsDir, NewMetrics(ctrlmetrics.Registry))
	if err != nil {
		return err
	}
	runner := &Runner{
		Client: c, Store: store, CollectorURL: opts.CollectorURL,
		Starter: &VirshStarter{Client: c, Exec: hookExec{hooks.NewPodExecer(clientset, cfg)}},
		RunRole: opts.RunRole, ServiceAccount: opts.ServiceAccount, ServiceNamespace: opts.Namespace,
		Now: time.Now,
	}
	// Without node agent metrics the steps have no share-side numbers; the
	// run's log says so at each step.
	if opts.NodeMetricsService != "" {
		runner.Shares = &NodeAgentShares{Client: c, Namespace: opts.Namespace, Service: opts.NodeMetricsService,
			HTTP: httpClient, TokenFile: serviceAccountTokenFile}
	}
	if aerr := mgr.Add(leaderRunnable(func(ctx context.Context) error {
		t := time.NewTicker(opts.Interval)
		defer t.Stop()
		for {
			runner.Pass(ctx)
			select {
			case <-ctx.Done():
				return nil
			case <-t.C:
			}
		}
	})); aerr != nil {
		return aerr
	}
	srv := &http.Server{Addr: opts.CollectorAddr, Handler: &Collector{Store: store, Runs: runner}, ReadHeaderTimeout: 10 * time.Second}
	if aerr := mgr.Add(leaderRunnable(func(ctx context.Context) error {
		go func() {
			<-ctx.Done()
			sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = srv.Shutdown(sctx)
		}()
		klog.InfoS("PoolBench collector listening", "addr", opts.CollectorAddr)
		if serr := srv.ListenAndServe(); serr != nil && !errors.Is(serr, http.ErrServerClosed) {
			return serr
		}
		return nil
	})); aerr != nil {
		return aerr
	}
	if opts.ResultsAddr != "" && opts.MetricsCertDir != "" {
		results := &http.Server{
			Addr:              opts.ResultsAddr,
			Handler:           &ResultsServer{Store: store, Auth: NewKubeAuth(clientset)},
			ReadHeaderTimeout: 10 * time.Second,
			TLSConfig:         &tls.Config{MinVersion: tls.VersionTLS12},
		}
		if aerr := mgr.Add(leaderRunnable(func(ctx context.Context) error {
			go func() {
				<-ctx.Done()
				sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				_ = results.Shutdown(sctx)
			}()
			klog.InfoS("PoolBench results server listening", "addr", opts.ResultsAddr)
			serr := results.ListenAndServeTLS(filepath.Join(opts.MetricsCertDir, "tls.crt"), filepath.Join(opts.MetricsCertDir, "tls.key"))
			if serr != nil && !errors.Is(serr, http.ErrServerClosed) {
				return serr
			}
			return nil
		})); aerr != nil {
			return aerr
		}
	}
	return mgr.Start(ctx)
}
