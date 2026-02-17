package main

import (
	"context"
	"flag"
	"os"
	"strings"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	ctrlwebhook "sigs.k8s.io/controller-runtime/pkg/webhook"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/driver"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/hooks"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	_ "github.com/IBM/ibm-vpc-file-pool-csi/pkg/metrics" // Register Prometheus collectors via init()
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
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
		cloneWorkerInterval time.Duration
	)

	flag.StringVar(&endpoint, "endpoint", "unix:///csi/csi.sock", "CSI endpoint")
	flag.StringVar(&nodeID, "node-id", "", "Node ID")
	flag.StringVar(&mode, "mode", "controller", "Driver mode: controller or node")
	flag.StringVar(&region, "region", "", "IBM Cloud region (e.g. us-south); auto-discovered from secret provider if omitted")
	flag.StringVar(&vpcID, "vpc-id", "", "IBM Cloud VPC ID for creating file share mount targets")
	flag.StringVar(&subnetID, "subnet-id", "", "IBM Cloud subnet ID for creating file share mount targets")
	flag.DurationVar(&cloneWorkerInterval, "clone-worker-interval", pool.DefaultCloneWorkerInterval, "Interval between clone worker poll cycles")

	klog.InitFlags(nil)
	flag.Parse()

	klog.InfoS("Starting IBM VPC File Pool CSI Driver", "version", version, "mode", mode)

	if mode == "controller" {
		runController(endpoint, nodeID, region, vpcID, subnetID, cloneWorkerInterval)
	} else {
		runNode(endpoint, nodeID, mode)
	}
}

func runController(endpoint, nodeID, region, vpcID, subnetID string, cloneWorkerInterval time.Duration) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add CRD types to scheme")
		os.Exit(1)
	}
	if err := corev1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add corev1 types to scheme")
		os.Exit(1)
	}

	restConfig := ctrl.GetConfigOrDie()
	mgr, err := ctrl.NewManager(restConfig, ctrl.Options{
		Scheme:                  scheme,
		LeaderElection:          true,
		LeaderElectionID:        "ibm-vpc-file-pool-csi-leader",
		LeaderElectionNamespace: "kube-system",
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
	if vpcID == "" {
		if cm, cmErr := clientset.CoreV1().ConfigMaps("kube-system").Get(ctx, "ibm-cloud-provider-data", metav1.GetOptions{}); cmErr == nil {
			if v := cm.Data["vpc_id"]; v != "" {
				vpcID = v
				klog.V(2).InfoS("Auto-discovered VPC ID", "source", "ibm-cloud-provider-data", "vpcID", vpcID)
			}
		}
	}
	if subnetID == "" {
		if cm, cmErr := clientset.CoreV1().ConfigMaps("kube-system").Get(ctx, "ibm-cloud-provider-data", metav1.GetOptions{}); cmErr == nil {
			if v := cm.Data["vpc_subnet_ids"]; v != "" {
				subnetID = strings.Split(strings.TrimSpace(v), ",")[0]
				klog.V(2).InfoS("Auto-discovered subnet ID", "source", "ibm-cloud-provider-data", "subnetID", subnetID)
			}
		}
	}

	// Create real VPC API client using cluster credentials.
	// Region is auto-discovered from the RIAAS endpoint if not set via flag.
	vpcClient, err := ibmcloud.NewClient(clientset, region)
	if err != nil {
		klog.ErrorS(err, "Failed to create VPC API client", "region", region)
		os.Exit(1)
	}

	// Get default resource group from secret provider for pools that don't specify one.
	defaultResourceGroup := vpcClient.ResourceGroupID()

	reconciler := pool.NewFileSharePoolReconciler(k8sClient, vpcClient)
	reconciler.SetVPCConfig(vpcID, subnetID, defaultResourceGroup)
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

	stagingBasePath := "/var/lib/kubelet/plugins/vpc-file-pool.csi.ibm.io/staging"

	poolManager := pool.NewManager(k8sClient, vpcClient, nil, stagingBasePath)
	poolManager.SetDefaultResourceGroup(defaultResourceGroup)

	// Run CSI gRPC server in a goroutine
	d, err := driver.NewDriver(driver.Config{
		Name:        driver.DriverName,
		Version:     version,
		NodeID:      nodeID,
		Endpoint:    endpoint,
		Mode:        "controller",
		K8sClient:   k8sClient,
		PoolManager: poolManager,
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

	// Start the background clone worker for async volume clones.
	// It uses real NFS operations to perform the actual cp -a copies.
	// The signal context ensures graceful shutdown alongside the manager.
	signalCtx := ctrl.SetupSignalHandler()
	nfsOps := pool.NewRealNFSOperations()
	cloneWorker := pool.NewCloneWorker(k8sClient, nfsOps, stagingBasePath)
	if cloneWorkerInterval > 0 {
		cloneWorker.SetInterval(cloneWorkerInterval)
	}
	go cloneWorker.Run(signalCtx)

	// Start the background replication controller for cross-region DR.
	replController := pool.NewReplicationController(k8sClient, nfsOps)
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
