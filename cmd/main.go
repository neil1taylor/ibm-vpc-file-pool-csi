package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/driver"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/hooks"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	_ "github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics" // Register Prometheus collectors via init()
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/replication"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/vmcopy"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/webhook"
)

var version = "dev"

// driverNamespace is where the Helm chart installs the driver. Leader
// election and share data Jobs use it.
const driverNamespace = pool.DefaultDriverNamespace

func main() {
	var (
		endpoint                string
		nodeID                  string
		mode                    string
		region                  string
		vpcID                   string
		subnetID                string
		kubeletDir              string
		cloneWorkerInterval     time.Duration
		goldenImageSyncInterval time.Duration
		cdiCopyPodMemoryLimit   string
		webhooks                webhookOptions

		// Receiver mode flags.
		receiverBasePath      string
		receiverAuthTokenFile string
		receiverListenAddr    string
		receiverTLSCertFile   string
		receiverTLSKeyFile    string

		// Replication controller flags.
		driverImage string

		// Sync-client mode flags.
		receiverEndpoint string
		authTokenFile    string
		caCertFile       string
		sourcePath       string
		destBasePath     string
		subPath          string
		subvolumeName    string
		metadataJSON     string

		// Apply-vm-disk mode flags.
		stagingPath string
		diskPath    string
		vmCopy      vmCopyFlags

		// Send-vm-disk mode flags (it also uses the sync-client flags).
		vmGroup string
		copyID  string
	)

	flag.StringVar(&endpoint, "endpoint", "unix:///csi/csi.sock", "CSI endpoint")
	flag.StringVar(&nodeID, "node-id", "", "Node ID")
	flag.StringVar(&mode, "mode", "controller", "Driver mode: controller, node, receiver, sync-client, check-receiver, apply-vm-disk, or send-vm-disk")
	flag.StringVar(&region, "region", "", "IBM Cloud region (e.g. us-south); auto-discovered from secret provider if omitted")
	flag.StringVar(&vpcID, "vpc-id", "", "IBM Cloud VPC ID for creating file share mount targets")
	flag.StringVar(&subnetID, "subnet-id", "", "Comma-separated IBM Cloud worker subnet IDs; each share's mount target goes in the one in the pool's zone")
	flag.StringVar(&kubeletDir, "kubelet-dir", "/var/lib/kubelet", "Kubelet root directory (ROKS uses /var/data/kubelet)")
	flag.DurationVar(&cloneWorkerInterval, "clone-worker-interval", pool.DefaultCloneWorkerInterval, "Interval between clone worker poll cycles")
	flag.DurationVar(&goldenImageSyncInterval, "golden-image-sync-interval", pool.DefaultGoldenImageInterval, "Interval between golden image syncer poll cycles")
	flag.StringVar(&cdiCopyPodMemoryLimit, "cdi-copy-pod-memory-limit", "8Gi", "Keep the memory limit of CDI's copy pods (OpenShift Virtualization) at least this high; empty turns the check off")

	// Replication controller flags.
	flag.StringVar(&driverImage, "driver-image", "", "Driver container image for receiver-mode sync-client Jobs")
	flag.BoolVar(&webhooks.enabled, "enable-webhooks", true, "Serve the validating admission webhooks (needs a serving certificate)")
	flag.IntVar(&webhooks.port, "webhook-port", 9443, "Port of the admission webhook server")

	// Receiver flags.
	flag.StringVar(&receiverBasePath, "receiver-base-path", "/data", "NFS mount point inside receiver container")
	flag.StringVar(&receiverAuthTokenFile, "receiver-auth-token-file", "/etc/replication/token", "Path to file containing bearer token for receiver auth")
	flag.StringVar(&receiverListenAddr, "receiver-listen-addr", ":8443", "HTTP listen address for receiver")
	flag.StringVar(&receiverTLSCertFile, "receiver-tls-cert-file", "", "TLS certificate file for receiver HTTPS (empty = plain HTTP)")
	flag.StringVar(&receiverTLSKeyFile, "receiver-tls-key-file", "", "TLS private key file for receiver HTTPS (empty = plain HTTP)")

	// Sync-client flags.
	flag.StringVar(&receiverEndpoint, "receiver-endpoint", "", "Destination receiver URL for sync-client mode")
	flag.StringVar(&authTokenFile, "auth-token-file", "/etc/replication/token", "Path to file containing bearer token for sync-client auth")
	flag.StringVar(&caCertFile, "ca-cert-file", "", "CA certificate file for verifying receiver TLS cert (empty = system trust store)")
	flag.StringVar(&sourcePath, "source-path", "", "Mounted source NFS path for sync-client mode")
	flag.StringVar(&destBasePath, "dest-base-path", "", "Destination base path for sync-client mode")
	flag.StringVar(&subPath, "sub-path", "", "SubVolume sub path for sync-client mode")
	flag.StringVar(&subvolumeName, "subvolume-name", "", "SubVolume CR name for sync-client mode")
	flag.StringVar(&metadataJSON, "metadata-json", "", "Serialized SubVolumeMetadata JSON for sync-client mode")

	// Apply-vm-disk flags.
	flag.StringVar(&stagingPath, "staging-path", "", "VM disk copy staging file for apply-vm-disk mode")
	flag.StringVar(&diskPath, "disk-path", "", "Disk image to replace in apply-vm-disk mode")
	flag.StringVar(&vmCopy.from, "from", "", "Moment the far-site disk shows now, RFC 3339 (apply-vm-disk mode; empty for a first copy)")
	flag.StringVar(&vmCopy.to, "to", "", "Moment the copy shows, RFC 3339 (apply-vm-disk mode)")
	flag.BoolVar(&vmCopy.full, "full", false, "The copy holds the whole disk (apply-vm-disk mode)")
	flag.DurationVar(&vmCopy.keep, "keep", 24*time.Hour, "How long restore points are kept (apply-vm-disk mode)")

	flag.StringVar(&vmGroup, "vm-group", "", "namespace/name of the VM that owns the disk (send-vm-disk mode)")
	flag.StringVar(&copyID, "copy-id", "", "Name of this copy attempt; a repeat with the same name resumes (send-vm-disk mode)")

	klog.InitFlags(nil)
	flag.Parse()

	klog.InfoS("Starting IBM VPC File Pool CSI Driver", "version", version, "mode", mode)

	switch mode {
	case "controller":
		runController(endpoint, nodeID, region, vpcID, subnetID, kubeletDir, cloneWorkerInterval, goldenImageSyncInterval, cdiCopyPodMemoryLimit, driverImage, webhooks)
	case "receiver":
		runReceiver(receiverBasePath, receiverAuthTokenFile, receiverListenAddr, receiverTLSCertFile, receiverTLSKeyFile)
	case "sync-client":
		runSyncClient(receiverEndpoint, authTokenFile, sourcePath, destBasePath, subPath, subvolumeName, metadataJSON, caCertFile)
	case "check-receiver":
		runCheckReceiver(receiverEndpoint, authTokenFile, caCertFile)
	case "apply-vm-disk":
		runApplyVMDisk(stagingPath, diskPath, vmCopy)
	case "send-vm-disk":
		runSendVMDisk(receiverEndpoint, authTokenFile, caCertFile, stagingPath, destBasePath, subPath, subvolumeName, copyID, vmGroup, metadataJSON, vmCopy)
	default:
		runNode(endpoint, nodeID, mode)
	}
}

// webhookOptions configures the admission webhook server.
type webhookOptions struct {
	enabled bool
	port    int
}

func runController(endpoint, nodeID, region, vpcID, subnetID, kubeletDir string, cloneWorkerInterval, goldenImageSyncInterval time.Duration, cdiCopyPodMemoryLimit, driverImage string, webhooks webhookOptions) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add CRD types to scheme")
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add corev1 types to scheme")
		os.Exit(1)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add storagev1 types to scheme")
		os.Exit(1)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add batchv1 types to scheme")
		os.Exit(1)
	}

	// readyCh is closed once the VPC client is initialized. It gates:
	// - CSI controller methods (return Unavailable until ready)
	// - /readyz health check (returns 503 until ready)
	// - Probe() identity RPC (returns Ready=false until ready)
	readyCh := make(chan struct{})

	restConfig := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                  scheme,
		LeaderElection:          true,
		LeaderElectionID:        "ibm-vpc-file-pool-csi-leader",
		LeaderElectionNamespace: driverNamespace,
		HealthProbeBindAddress:  ":8081",
		Metrics: metricsserver.Options{
			BindAddress: ":8080",
		},
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{
			Port:    webhooks.port,
			CertDir: "/tmp/k8s-webhook-server/serving-certs",
		}),
	})
	if err != nil {
		klog.ErrorS(err, "Failed to create controller-runtime manager")
		os.Exit(1)
	}

	// Register health checks.
	if err = mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		klog.ErrorS(err, "Failed to register healthz check")
		os.Exit(1)
	}
	if err = mgr.AddReadyzCheck("vpc-client", func(_ *http.Request) error {
		select {
		case <-readyCh:
			return nil
		default:
			return fmt.Errorf("VPC client not yet initialized")
		}
	}); err != nil {
		klog.ErrorS(err, "Failed to register readyz check")
		os.Exit(1)
	}

	k8sClient := k8s.NewClient(mgr.GetClient())

	// Create kubernetes.Clientset for secret-common-lib.
	clientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		klog.ErrorS(err, "Failed to create kubernetes clientset")
		os.Exit(1)
	}

	ctx := context.Background()
	netCfg := discoverNetworkConfig(ctx, clientset, vpcID, subnetID)
	network := pool.NewShareNetwork(netCfg.vpcID, netCfg.subnetIDs, netCfg.clusterID)

	// Create reconciler and pool manager with nil VPC client — will be set
	// once the secret-provider sidecar is ready.
	reconciler := pool.NewFileSharePoolReconciler(k8sClient, nil)
	if err = reconciler.SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "Failed to set up reconciler")
		os.Exit(1)
	}

	// Register validating webhooks for CRD resources. The manager starts the
	// webhook server only when a webhook is registered, so with webhooks
	// disabled the controller needs no serving certificate.
	if webhooks.enabled {
		if err = registerWebhooks(mgr); err != nil {
			klog.ErrorS(err, "Failed to register webhooks")
			os.Exit(1)
		}
		klog.V(2).InfoS("Registered validating webhooks for all CRD types", "port", webhooks.port)
	} else {
		klog.InfoS("Admission webhooks disabled")
	}

	stagingBasePath := filepath.Join(kubeletDir, "plugins/vpc-file-pool.csi.ibm.io/staging")

	poolManager := pool.NewManager(k8sClient, nil, nil, stagingBasePath)
	// The controller has no NFS access, so folder removal and copies on
	// shares run as Jobs in the driver's namespace. The runner reads without
	// the cache: a stale read could report a copy as stopped too early.
	shareJobClient, err := client.New(restConfig, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		klog.ErrorS(err, "Failed to create the share data Job client")
		os.Exit(1)
	}
	poolManager.SetShareData(pool.NewShareJobRunner(shareJobClient, driverNamespace, pool.DefaultCloneImage))

	// Run CSI gRPC server in a goroutine — serves immediately, but controller
	// methods return Unavailable until readyCh is closed.
	d, err := driver.NewDriver(driver.Config{
		Name:        driver.DriverName,
		Version:     version,
		NodeID:      nodeID,
		Endpoint:    endpoint,
		Mode:        "controller",
		K8sClient:   k8sClient,
		PoolManager: poolManager,
		Ready:       readyCh,
	})
	if err != nil {
		klog.ErrorS(err, "Failed to create driver")
		os.Exit(1)
	}

	go func() {
		if runErr := d.Run(); runErr != nil {
			klog.ErrorS(runErr, "CSI gRPC server failed")
			os.Exit(1)
		}
	}()

	// Initialize VPC client in the background. The secret-provider sidecar may
	// take up to 150s to become ready. This goroutine retries until it succeeds,
	// then injects the client into the reconciler and pool manager and signals
	// readiness by closing readyCh.
	go func() {
		var vpcClient *ibmcloud.Client
		var initErr error
		for attempt := 1; attempt <= 30; attempt++ {
			func() {
				defer func() {
					if r := recover(); r != nil {
						initErr = fmt.Errorf("secret provider panic: %v", r)
					}
				}()
				vpcClient, initErr = ibmcloud.NewClient(clientset, region)
			}()
			if initErr == nil {
				break
			}
			klog.V(2).InfoS("Waiting for secret provider sidecar", "attempt", attempt, "err", initErr)
			time.Sleep(5 * time.Second)
		}
		if initErr != nil {
			klog.ErrorS(initErr, "Failed to create VPC API client after retries — controller will not become ready", "region", region)
			return
		}

		defaultResourceGroup := vpcClient.ResourceGroupID()

		poolManager.SetVPCClient(vpcClient)
		poolManager.SetDefaultResourceGroup(defaultResourceGroup)
		poolManager.SetShareNetwork(network)
		reconciler.SetVPCClient(vpcClient)
		reconciler.SetVPCConfig(network, defaultResourceGroup)

		close(readyCh)
		klog.InfoS("VPC client initialized, controller is ready")
	}()

	// Start the background clone worker for async volume clones.
	// It creates Kubernetes Jobs to perform cp -a (controller pod has no NFS access).
	// The signal context ensures graceful shutdown alongside the manager.
	signalCtx := ctrl.SetupSignalHandler()
	nfsOps := pool.NewRealNFSOperations()
	cloneWorker := pool.NewCloneWorker(k8sClient, shareJobClient, pool.DefaultCloneImage)
	if cloneWorkerInterval > 0 {
		cloneWorker.SetInterval(cloneWorkerInterval)
	}
	go cloneWorker.Run(signalCtx)

	// Start the background golden image syncer for KubeVirt VM templates.
	goldenImageSyncer := pool.NewGoldenImageSyncer(k8sClient, mgr.GetClient())
	if goldenImageSyncInterval > 0 {
		goldenImageSyncer.SetInterval(goldenImageSyncInterval)
	}
	reconciler.SetGoldenImageRemover(goldenImageSyncer)

	// Keep CDI's copy pods big enough to write onto NFS. Runs on the leader only.
	if cdiCopyPodMemoryLimit != "" {
		if addErr := mgr.Add(pool.NewCDILimitKeeper(shareJobClient, cdiCopyPodMemoryLimit)); addErr != nil {
			klog.ErrorS(addErr, "Failed to add the CDI copy limit check")
			os.Exit(1)
		}
	}
	go goldenImageSyncer.Run(signalCtx)

	// Start the background replication controller for cross-region DR. It
	// reads without the cache: the cache would watch every pod in the cluster,
	// and the controller may only get and list pods.
	replClient, err := client.New(restConfig, client.Options{Scheme: mgr.GetScheme()})
	if err != nil {
		klog.ErrorS(err, "Failed to create the replication client")
		os.Exit(1)
	}
	replController := pool.NewReplicationController(k8sClient, replClient, nfsOps)
	hookOrchestrator := hooks.NewOrchestrator(
		hooks.NewExecHook(clientset, restConfig),
		hooks.NewHTTPHook(nil),
	)
	replController.SetOrchestrator(hookOrchestrator)
	replController.SetDriverImage(driverImage)
	replController.SetReplicationImage(driverImage)
	// Copies of running VM disks run commands in the VM pods, as hooks do.
	replController.SetVMCopier(vmcopy.NewCopier(hooks.NewPodExecer(clientset, restConfig)))
	go replController.Run(signalCtx)

	// mgr.Start blocks until signal or error
	klog.InfoS("Starting controller-runtime manager with leader election")
	if err := mgr.Start(signalCtx); err != nil {
		klog.ErrorS(err, "Controller manager failed")
		os.Exit(1)
	}
}

func runReceiver(basePath, authTokenFile, listenAddr, tlsCertFile, tlsKeyFile string) {
	// Read auth token from file.
	tokenBytes, err := os.ReadFile(authTokenFile) //nolint:gosec // G304: path comes from the operator-set --auth-token-file flag
	if err != nil {
		klog.ErrorS(err, "Failed to read receiver auth token file", "path", authTokenFile)
		os.Exit(1)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		klog.ErrorS(nil, "Receiver auth token file is empty", "path", authTokenFile)
		os.Exit(1)
	}

	receiver := replication.NewReceiver(basePath, token, listenAddr)
	if tlsCertFile != "" && tlsKeyFile != "" {
		receiver.SetTLS(tlsCertFile, tlsKeyFile)
	}

	signalCtx := ctrl.SetupSignalHandler()
	klog.InfoS("Starting replication receiver", "basePath", basePath, "listenAddr", listenAddr, "tls", tlsCertFile != "")
	if err := receiver.Start(signalCtx); err != nil {
		klog.ErrorS(err, "Replication receiver failed")
		os.Exit(1)
	}
}

// newSyncClient reads the auth token and the optional CA certificate and makes
// the client. It exits if either file is not usable.
func newSyncClient(endpoint, authTokenFile, caCertFile string) *replication.SyncClient {
	// Read auth token from file.
	tokenBytes, err := os.ReadFile(authTokenFile) //nolint:gosec // G304: path comes from the operator-set --auth-token-file flag
	if err != nil {
		klog.ErrorS(err, "Failed to read auth token file", "path", authTokenFile)
		os.Exit(1)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		klog.ErrorS(nil, "Auth token file is empty", "path", authTokenFile)
		os.Exit(1)
	}
	if caCertFile == "" {
		return replication.NewSyncClient(endpoint, token)
	}
	caCertPEM, err := os.ReadFile(caCertFile) //nolint:gosec // G304: path comes from the operator-set CA cert flag
	if err != nil {
		klog.ErrorS(err, "Failed to read CA cert file", "path", caCertFile)
		os.Exit(1)
	}
	client, err := replication.NewSyncClientWithCA(endpoint, token, caCertPEM)
	if err != nil {
		klog.ErrorS(err, "Invalid CA cert file", "path", caCertFile)
		os.Exit(1)
	}
	klog.V(2).InfoS("Using custom CA certificate for receiver TLS", "caCertFile", caCertFile)
	return client
}

func runSyncClient(endpoint, authTokenFile, sourcePath, destBasePath, subPath, svName, metadataJSON, caCertFile string) {
	if endpoint == "" {
		klog.ErrorS(nil, "receiver-endpoint is required for sync-client mode")
		os.Exit(1)
	}
	if sourcePath == "" || destBasePath == "" || subPath == "" || svName == "" {
		klog.ErrorS(nil, "source-path, dest-base-path, sub-path, and subvolume-name are required for sync-client mode")
		os.Exit(1)
	}

	client := newSyncClient(endpoint, authTokenFile, caCertFile)

	var metadata []byte
	if metadataJSON != "" {
		metadata = []byte(metadataJSON)
	}

	ctx := context.Background()
	klog.InfoS("Starting sync-client",
		"endpoint", endpoint,
		"sourcePath", sourcePath,
		"destBasePath", destBasePath,
		"subPath", subPath,
		"subVolume", svName,
	)

	bytesWritten, err := client.SyncSubVolume(ctx, sourcePath, destBasePath, subPath, svName, metadata)
	if err != nil {
		klog.ErrorS(err, "Sync-client failed")
		os.Exit(1)
	}

	klog.InfoS("Sync-client completed successfully", "subVolume", svName, "bytesWritten", bytesWritten)

	// Write bytesWritten to termination-log so the replication controller can
	// read it from the completed pod's container status.
	_ = os.WriteFile("/dev/termination-log", []byte(fmt.Sprintf(`{"bytesWritten":%d}`, bytesWritten)), 0o600)
}

// checkReceiverResult is the one result line of a receiver check ("ok", or the short reason) and the exit code.
func checkReceiverResult(err error) (string, int) {
	if err == nil {
		return "ok", 0
	}
	var checkErr *replication.CheckError
	if errors.As(err, &checkErr) {
		return checkErr.Reason, 1
	}
	return err.Error(), 1
}

// runCheckReceiver tests a receiver the way a replication Job reaches it: the address, the
// certificate and the token. It writes one result line to the output and to the termination log
// (so the console can read it from the Pod status), and exits 1 when the check fails.
func runCheckReceiver(endpoint, authTokenFile, caCertFile string) {
	if endpoint == "" {
		klog.ErrorS(nil, "receiver-endpoint is required for check-receiver mode")
		os.Exit(1)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	line, code := checkReceiverResult(replication.CheckReceiver(ctx, endpoint, authTokenFile, caCertFile))
	fmt.Println(line)
	_ = os.WriteFile("/dev/termination-log", []byte(line), 0o600)
	if code != 0 {
		os.Exit(code)
	}
}

func runNode(endpoint, nodeID, mode string) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add CRD types to scheme")
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add corev1 types to scheme")
		os.Exit(1)
	}
	if err := storagev1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add storagev1 types to scheme")
		os.Exit(1)
	}

	restConfig := ctrl.GetConfigOrDie()
	ctrlClient, err := client.New(restConfig, client.Options{Scheme: scheme})
	if err != nil {
		klog.ErrorS(err, "Failed to create controller-runtime client")
		os.Exit(1)
	}
	k8sClient := k8s.NewClient(ctrlClient)

	d, err := driver.NewDriver(driver.Config{
		Name:      driver.DriverName,
		Version:   version,
		NodeID:    nodeID,
		Endpoint:  endpoint,
		Mode:      mode,
		K8sClient: k8sClient,
	})
	if err != nil {
		klog.ErrorS(err, "Failed to create driver")
		os.Exit(1)
	}

	if err := d.Run(); err != nil {
		klog.ErrorS(err, "Driver failed")
		os.Exit(1)
	}
}

// networkConfig is the cluster network that share mount targets join.
type networkConfig struct {
	vpcID     string
	subnetIDs []string
	clusterID string
}

// discoverNetworkConfig fills in the VPC ID and worker subnets from the
// ibm-cloud-provider-data configmap unless the flags set them, and the cluster
// ID from the cluster-info configmap. Both configmaps exist on ROKS and ROVS.
// Uses the clientset (not the manager client) because the manager cache is not
// started yet.
func discoverNetworkConfig(ctx context.Context, clientset kubernetes.Interface, vpcID, subnetIDs string) networkConfig {
	if vpcID == "" || subnetIDs == "" {
		if cm, err := clientset.CoreV1().ConfigMaps("kube-system").Get(ctx, "ibm-cloud-provider-data", metav1.GetOptions{}); err == nil {
			if vpcID == "" {
				vpcID = cm.Data["vpc_id"]
			}
			if subnetIDs == "" {
				subnetIDs = cm.Data["vpc_subnet_ids"]
			}
		} else {
			klog.ErrorS(err, "Cannot read ibm-cloud-provider-data; set --vpc-id and --subnet-id")
		}
	}

	cfg := networkConfig{vpcID: vpcID}
	for _, id := range strings.Split(subnetIDs, ",") {
		if id = strings.TrimSpace(id); id != "" {
			cfg.subnetIDs = append(cfg.subnetIDs, id)
		}
	}
	if cm, err := clientset.CoreV1().ConfigMaps("kube-system").Get(ctx, "cluster-info", metav1.GetOptions{}); err == nil {
		cfg.clusterID = cm.Data["cluster_id"]
	} else {
		klog.ErrorS(err, "Cannot read cluster-info; pools must set securityGroupIDs")
	}

	klog.V(2).InfoS("Share network", "vpcID", cfg.vpcID, "subnetIDs", cfg.subnetIDs, "clusterID", cfg.clusterID)
	return cfg
}

// registerWebhooks registers the validating webhook for each CRD type.
func registerWebhooks(mgr ctrl.Manager) error {
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.FileSharePool{}).
		WithValidator(&webhook.FileSharePoolValidator{}).Complete(); err != nil {
		return fmt.Errorf("register FileSharePool webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.SubVolume{}).
		WithValidator(&webhook.SubVolumeValidator{}).Complete(); err != nil {
		return fmt.Errorf("register SubVolume webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.ReplicationPolicy{}).
		WithValidator(&webhook.ReplicationPolicyValidator{}).Complete(); err != nil {
		return fmt.Errorf("register ReplicationPolicy webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.Snapshot{}).
		WithValidator(&webhook.SnapshotValidator{}).Complete(); err != nil {
		return fmt.Errorf("register Snapshot webhook: %w", err)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.VolumeGroupSnapshot{}).
		WithValidator(&webhook.VolumeGroupSnapshotValidator{}).Complete(); err != nil {
		return fmt.Errorf("register VolumeGroupSnapshot webhook: %w", err)
	}
	return nil
}

// vmCopyFlags are the apply-vm-disk flags that describe the copy.
type vmCopyFlags struct {
	from, to string
	full     bool
	keep     time.Duration
}

// applyOptions turns the flags into replication.ApplyOptions.
func (f vmCopyFlags) applyOptions(now time.Time) (replication.ApplyOptions, error) {
	opts := replication.ApplyOptions{Full: f.full, Keep: f.keep, Now: now}
	var err error
	if opts.To, err = time.Parse(time.RFC3339Nano, f.to); err != nil {
		return opts, fmt.Errorf("--to: %w", err)
	}
	if f.from != "" {
		if opts.From, err = time.Parse(time.RFC3339Nano, f.from); err != nil {
			return opts, fmt.Errorf("--from: %w", err)
		}
	}
	return opts, nil
}

// applyVMDiskMessage is the termination message of an apply-vm-disk Job.
func applyVMDiskMessage(res replication.VMDiskResult) string {
	msg := struct {
		BytesWritten       int64      `json:"bytesWritten"`
		RestorePoints      int        `json:"restorePoints"`
		OldestRestorePoint *time.Time `json:"oldestRestorePoint,omitempty"`
	}{res.ChangedBytes, res.RestorePoints, res.OldestRestorePoint}
	b, _ := json.Marshal(msg)
	return string(b)
}

// runApplyVMDisk applies a staged VM disk copy to the far-site disk. A
// replication Job runs it.
func runApplyVMDisk(stagingPath, diskPath string, f vmCopyFlags) {
	if stagingPath == "" || diskPath == "" {
		klog.ErrorS(nil, "staging-path and disk-path are required for apply-vm-disk mode")
		os.Exit(1)
	}
	opts, err := f.applyOptions(time.Now())
	if err != nil {
		klog.ErrorS(err, "Invalid apply-vm-disk flags")
		os.Exit(1)
	}
	res, err := replication.ApplyVMDisk(stagingPath, diskPath, opts)
	if err != nil {
		klog.ErrorS(err, "Failed to apply VM disk copy", "staging", stagingPath, "disk", diskPath)
		os.Exit(1)
	}
	klog.InfoS("Applied VM disk copy", "disk", diskPath, "changedBytes", res.ChangedBytes,
		"rolledBack", res.RolledBack, "restorePoints", res.RestorePoints)
	// The replication controller reads the result from the termination log.
	_ = os.WriteFile("/dev/termination-log", []byte(applyVMDiskMessage(res)), 0o600)
}

// sendVMDiskArgs turns the flags into the arguments of replication.SendVMDisk.
func sendVMDiskArgs(f vmCopyFlags, destBasePath, subPath, svName, copyID, vmGroup string) (replication.VMSendArgs, error) {
	opts, err := f.applyOptions(time.Now())
	if err != nil {
		return replication.VMSendArgs{}, err
	}
	a := replication.VMSendArgs{DestBasePath: destBasePath, SubPath: subPath, SVName: svName, CopyID: copyID,
		Full: f.full, To: opts.To, VMGroup: vmGroup, Keep: f.keep}
	if f.from != "" {
		a.From = &opts.From
	}
	return a, nil
}

// sendVMDiskMessage is the termination message of a send-vm-disk Job. A copy
// the receiver refused because its disk is at another moment is reported, not
// failed, so the Job is not retried: the controller makes the next copy full.
func sendVMDiskMessage(res replication.VMSendResult, wrongBase *replication.WrongBaseError) string {
	msg := struct {
		BytesWritten       int64      `json:"bytesWritten"`
		RestorePoints      int32      `json:"restorePoints"`
		OldestRestorePoint *time.Time `json:"oldestRestorePoint,omitempty"`
		WrongBase          bool       `json:"wrongBase,omitempty"`
		DiskMoment         string     `json:"diskMoment,omitempty"`
	}{res.SentBytes, res.RestorePoints, res.OldestRestorePoint, false, ""}
	if wrongBase != nil {
		msg.WrongBase, msg.DiskMoment = true, wrongBase.DiskMoment
	}
	b, _ := json.Marshal(msg)
	return string(b)
}

// runSendVMDisk sends a staged VM disk copy to a receiver at the far site, and
// the volume's metadata file. A replication Job runs it.
func runSendVMDisk(endpoint, authTokenFile, caCertFile, stagingPath, destBasePath, subPath, svName, copyID, vmGroup, metadataJSON string, f vmCopyFlags) {
	if endpoint == "" || stagingPath == "" || destBasePath == "" || subPath == "" || svName == "" || copyID == "" {
		klog.ErrorS(nil, "receiver-endpoint, staging-path, dest-base-path, sub-path, subvolume-name and copy-id are required for send-vm-disk mode")
		os.Exit(1)
	}
	args, err := sendVMDiskArgs(f, destBasePath, subPath, svName, copyID, vmGroup)
	if err != nil {
		klog.ErrorS(err, "Invalid send-vm-disk flags")
		os.Exit(1)
	}
	client := newSyncClient(endpoint, authTokenFile, caCertFile)
	ctx := context.Background()
	// The metadata goes first, as in direct NFS mode. If the Job is retried
	// after the staging file was deleted, nothing is left to send.
	if metadataJSON != "" {
		if err = client.UploadVMMetadata(ctx, destBasePath, subPath, svName, []byte(metadataJSON)); err != nil {
			klog.ErrorS(err, "Failed to upload the volume metadata", "subVolume", svName)
			os.Exit(1)
		}
	}
	res, err := replication.SendVMDiskFile(ctx, client, stagingPath, args)
	var wrongBase *replication.WrongBaseError
	if errors.As(err, &wrongBase) {
		klog.ErrorS(err, "The far-site disk is not at the moment this copy starts from; the next copy will be a full copy", "subVolume", svName)
		_ = os.WriteFile("/dev/termination-log", []byte(sendVMDiskMessage(res, wrongBase)), 0o600)
		return
	}
	if err != nil {
		klog.ErrorS(err, "Failed to send VM disk copy", "staging", stagingPath, "subVolume", svName)
		os.Exit(1)
	}
	klog.InfoS("Sent VM disk copy", "subVolume", svName, "sentBytes", res.SentBytes, "changedBytes", res.ChangedBytes,
		"restorePoints", res.RestorePoints, "alreadyApplied", res.AlreadyApplied)
	_ = os.WriteFile("/dev/termination-log", []byte(sendVMDiskMessage(res, nil)), 0o600)
}
