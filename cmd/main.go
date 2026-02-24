package main

import (
	"context"
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
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/webhook"
)

var version = "dev"

func main() {
	var (
		endpoint            string
		nodeID              string
		mode                string
		region              string
		vpcID               string
		subnetID            string
		kubeletDir          string
		cloneWorkerInterval      time.Duration
		goldenImageSyncInterval  time.Duration

		// Receiver mode flags.
		receiverBasePath      string
		receiverAuthTokenFile string
		receiverListenAddr    string

		// Sync-client mode flags.
		receiverEndpoint string
		authTokenFile    string
		sourcePath       string
		destBasePath     string
		subPath          string
		subvolumeName    string
		metadataJSON     string
	)

	flag.StringVar(&endpoint, "endpoint", "unix:///csi/csi.sock", "CSI endpoint")
	flag.StringVar(&nodeID, "node-id", "", "Node ID")
	flag.StringVar(&mode, "mode", "controller", "Driver mode: controller, node, receiver, or sync-client")
	flag.StringVar(&region, "region", "", "IBM Cloud region (e.g. us-south); auto-discovered from secret provider if omitted")
	flag.StringVar(&vpcID, "vpc-id", "", "IBM Cloud VPC ID for creating file share mount targets")
	flag.StringVar(&subnetID, "subnet-id", "", "IBM Cloud subnet ID for creating file share mount targets")
	flag.StringVar(&kubeletDir, "kubelet-dir", "/var/lib/kubelet", "Kubelet root directory (ROKS uses /var/data/kubelet)")
	flag.DurationVar(&cloneWorkerInterval, "clone-worker-interval", pool.DefaultCloneWorkerInterval, "Interval between clone worker poll cycles")
	flag.DurationVar(&goldenImageSyncInterval, "golden-image-sync-interval", pool.DefaultGoldenImageInterval, "Interval between golden image syncer poll cycles")

	// Receiver flags.
	flag.StringVar(&receiverBasePath, "receiver-base-path", "/data", "NFS mount point inside receiver container")
	flag.StringVar(&receiverAuthTokenFile, "receiver-auth-token-file", "/etc/replication/token", "Path to file containing bearer token for receiver auth")
	flag.StringVar(&receiverListenAddr, "receiver-listen-addr", ":8443", "HTTP listen address for receiver")

	// Sync-client flags.
	flag.StringVar(&receiverEndpoint, "receiver-endpoint", "", "Destination receiver URL for sync-client mode")
	flag.StringVar(&authTokenFile, "auth-token-file", "/etc/replication/token", "Path to file containing bearer token for sync-client auth")
	flag.StringVar(&sourcePath, "source-path", "", "Mounted source NFS path for sync-client mode")
	flag.StringVar(&destBasePath, "dest-base-path", "", "Destination base path for sync-client mode")
	flag.StringVar(&subPath, "sub-path", "", "SubVolume sub path for sync-client mode")
	flag.StringVar(&subvolumeName, "subvolume-name", "", "SubVolume CR name for sync-client mode")
	flag.StringVar(&metadataJSON, "metadata-json", "", "Serialized SubVolumeMetadata JSON for sync-client mode")

	klog.InitFlags(nil)
	flag.Parse()

	klog.InfoS("Starting IBM VPC File Pool CSI Driver", "version", version, "mode", mode)

	switch mode {
	case "controller":
		runController(endpoint, nodeID, region, vpcID, subnetID, kubeletDir, cloneWorkerInterval, goldenImageSyncInterval)
	case "receiver":
		runReceiver(receiverBasePath, receiverAuthTokenFile, receiverListenAddr)
	case "sync-client":
		runSyncClient(receiverEndpoint, authTokenFile, sourcePath, destBasePath, subPath, subvolumeName, metadataJSON)
	default:
		runNode(endpoint, nodeID, mode)
	}
}

func runController(endpoint, nodeID, region, vpcID, subnetID, kubeletDir string, cloneWorkerInterval, goldenImageSyncInterval time.Duration) {
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
		LeaderElectionNamespace: "kube-system",
		HealthProbeBindAddress:  ":8081",
		Metrics: metricsserver.Options{
			BindAddress: ":8080",
		},
		WebhookServer: ctrlwebhook.NewServer(ctrlwebhook.Options{
			Port:    9443,
			CertDir: "/tmp/k8s-webhook-server/serving-certs",
		}),
	})
	if err != nil {
		klog.ErrorS(err, "Failed to create controller-runtime manager")
		os.Exit(1)
	}

	// Register health checks.
	if err := mgr.AddHealthzCheck("ping", healthz.Ping); err != nil {
		klog.ErrorS(err, "Failed to register healthz check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("vpc-client", func(_ *http.Request) error {
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

	// Auto-discover VPC config from ibm-cloud-provider-data configmap.
	// Use clientset (not mgr.GetClient()) because the manager cache isn't started yet.
	ctx := context.Background()
	if vpcID == "" || subnetID == "" {
		if cm, cmErr := clientset.CoreV1().ConfigMaps("kube-system").Get(ctx, "ibm-cloud-provider-data", metav1.GetOptions{}); cmErr == nil {
			if vpcID == "" {
				if v := cm.Data["vpc_id"]; v != "" {
					vpcID = v
					klog.V(2).InfoS("Auto-discovered VPC ID", "source", "ibm-cloud-provider-data", "vpcID", vpcID)
				}
			}
			if subnetID == "" {
				if v := cm.Data["vpc_subnet_ids"]; v != "" {
					subnetID = strings.Split(strings.TrimSpace(v), ",")[0]
					klog.V(2).InfoS("Auto-discovered subnet ID", "source", "ibm-cloud-provider-data", "subnetID", subnetID)
				}
			}
		}
	}

	// Create reconciler and pool manager with nil VPC client — will be set
	// once the secret-provider sidecar is ready.
	reconciler := pool.NewFileSharePoolReconciler(k8sClient, nil)
	if err := reconciler.SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "Failed to set up reconciler")
		os.Exit(1)
	}

	// Register validating webhooks for CRD resources.
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.FileSharePool{}).
		WithValidator(&webhook.FileSharePoolValidator{}).
		Complete(); err != nil {
		klog.ErrorS(err, "Failed to register FileSharePool webhook")
		os.Exit(1)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.SubVolume{}).
		WithValidator(&webhook.SubVolumeValidator{}).
		Complete(); err != nil {
		klog.ErrorS(err, "Failed to register SubVolume webhook")
		os.Exit(1)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.ReplicationPolicy{}).
		WithValidator(&webhook.ReplicationPolicyValidator{}).
		Complete(); err != nil {
		klog.ErrorS(err, "Failed to register ReplicationPolicy webhook")
		os.Exit(1)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.Snapshot{}).
		WithValidator(&webhook.SnapshotValidator{}).
		Complete(); err != nil {
		klog.ErrorS(err, "Failed to register Snapshot webhook")
		os.Exit(1)
	}
	if err := ctrl.NewWebhookManagedBy(mgr, &v1alpha1.VolumeGroupSnapshot{}).
		WithValidator(&webhook.VolumeGroupSnapshotValidator{}).
		Complete(); err != nil {
		klog.ErrorS(err, "Failed to register VolumeGroupSnapshot webhook")
		os.Exit(1)
	}
	klog.V(2).InfoS("Registered validating webhooks for all CRD types")

	stagingBasePath := filepath.Join(kubeletDir, "plugins/vpc-file-pool.csi.ibm.io/staging")

	poolManager := pool.NewManager(k8sClient, nil, nil, stagingBasePath)

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
		if err := d.Run(); err != nil {
			klog.ErrorS(err, "CSI gRPC server failed")
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
		reconciler.SetVPCClient(vpcClient)
		reconciler.SetVPCConfig(vpcID, subnetID, defaultResourceGroup)

		close(readyCh)
		klog.InfoS("VPC client initialized, controller is ready")
	}()

	// Start the background clone worker for async volume clones.
	// It creates Kubernetes Jobs to perform cp -a (controller pod has no NFS access).
	// The signal context ensures graceful shutdown alongside the manager.
	signalCtx := ctrl.SetupSignalHandler()
	nfsOps := pool.NewRealNFSOperations()
	cloneWorker := pool.NewCloneWorker(k8sClient, mgr.GetClient(), pool.DefaultCloneImage)
	if cloneWorkerInterval > 0 {
		cloneWorker.SetInterval(cloneWorkerInterval)
	}
	go cloneWorker.Run(signalCtx)

	// Start the background golden image syncer for KubeVirt VM templates.
	goldenImageSyncer := pool.NewGoldenImageSyncer(k8sClient, mgr.GetClient())
	if goldenImageSyncInterval > 0 {
		goldenImageSyncer.SetInterval(goldenImageSyncInterval)
	}
	go goldenImageSyncer.Run(signalCtx)

	// Start the background replication controller for cross-region DR.
	replController := pool.NewReplicationController(k8sClient, mgr.GetClient(), nfsOps)
	hookOrchestrator := hooks.NewOrchestrator(
		hooks.NewExecHook(clientset, restConfig),
		hooks.NewHTTPHook(nil),
	)
	replController.SetOrchestrator(hookOrchestrator)
	go replController.Run(signalCtx)

	// mgr.Start blocks until signal or error
	klog.InfoS("Starting controller-runtime manager with leader election")
	if err := mgr.Start(signalCtx); err != nil {
		klog.ErrorS(err, "Controller manager failed")
		os.Exit(1)
	}
}

func runReceiver(basePath, authTokenFile, listenAddr string) {
	// Read auth token from file.
	tokenBytes, err := os.ReadFile(authTokenFile)
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

	signalCtx := ctrl.SetupSignalHandler()
	klog.InfoS("Starting replication receiver", "basePath", basePath, "listenAddr", listenAddr)
	if err := receiver.Start(signalCtx); err != nil {
		klog.ErrorS(err, "Replication receiver failed")
		os.Exit(1)
	}
}

func runSyncClient(endpoint, authTokenFile, sourcePath, destBasePath, subPath, svName, metadataJSON string) {
	if endpoint == "" {
		klog.ErrorS(nil, "receiver-endpoint is required for sync-client mode")
		os.Exit(1)
	}
	if sourcePath == "" || destBasePath == "" || subPath == "" || svName == "" {
		klog.ErrorS(nil, "source-path, dest-base-path, sub-path, and subvolume-name are required for sync-client mode")
		os.Exit(1)
	}

	// Read auth token from file.
	tokenBytes, err := os.ReadFile(authTokenFile)
	if err != nil {
		klog.ErrorS(err, "Failed to read auth token file", "path", authTokenFile)
		os.Exit(1)
	}
	token := strings.TrimSpace(string(tokenBytes))
	if token == "" {
		klog.ErrorS(nil, "Auth token file is empty", "path", authTokenFile)
		os.Exit(1)
	}

	client := replication.NewSyncClient(endpoint, token)

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

	if err := client.SyncSubVolume(ctx, sourcePath, destBasePath, subPath, svName, metadata); err != nil {
		klog.ErrorS(err, "Sync-client failed")
		os.Exit(1)
	}

	klog.InfoS("Sync-client completed successfully", "subVolume", svName)
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
