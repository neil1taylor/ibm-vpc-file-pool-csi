package main

import (
	"flag"
	"os"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/driver"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/ibmcloud"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
)

var version = "dev"

func main() {
	var (
		endpoint string
		nodeID   string
		mode     string
		region   string
		vpcID    string
		subnetID string
	)

	flag.StringVar(&endpoint, "endpoint", "unix:///csi/csi.sock", "CSI endpoint")
	flag.StringVar(&nodeID, "node-id", "", "Node ID")
	flag.StringVar(&mode, "mode", "controller", "Driver mode: controller or node")
	flag.StringVar(&region, "region", "", "IBM Cloud region (e.g. us-south), required in controller mode")
	flag.StringVar(&vpcID, "vpc-id", "", "IBM Cloud VPC ID for creating file share mount targets")
	flag.StringVar(&subnetID, "subnet-id", "", "IBM Cloud subnet ID for creating file share mount targets")

	klog.InitFlags(nil)
	flag.Parse()

	klog.InfoS("Starting IBM VPC File Pool CSI Driver", "version", version, "mode", mode)

	if mode == "controller" {
		if region == "" {
			klog.ErrorS(nil, "--region flag is required in controller mode")
			os.Exit(1)
		}
		runController(endpoint, nodeID, region, vpcID, subnetID)
	} else {
		runNode(endpoint, nodeID, mode)
	}
}

func runController(endpoint, nodeID, region, vpcID, subnetID string) {
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

	// Create real VPC API client using cluster credentials.
	vpcClient, err := ibmcloud.NewClient(clientset, region)
	if err != nil {
		klog.ErrorS(err, "Failed to create VPC API client", "region", region)
		os.Exit(1)
	}

	reconciler := pool.NewFileSharePoolReconciler(k8sClient, vpcClient)
	reconciler.SetVPCConfig(vpcID, subnetID)
	if err := reconciler.SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "Failed to set up reconciler")
		os.Exit(1)
	}

	stagingBasePath := "/var/lib/kubelet/plugins/vpc-file-pool.csi.ibm.io/staging"

	// Run CSI gRPC server in a goroutine
	d, err := driver.NewDriver(driver.Config{
		Name:        driver.DriverName,
		Version:     version,
		NodeID:      nodeID,
		Endpoint:    endpoint,
		Mode:        "controller",
		K8sClient:   k8sClient,
		PoolManager: pool.NewManager(k8sClient, vpcClient, nil, stagingBasePath),
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

	// mgr.Start blocks until signal or error
	klog.InfoS("Starting controller-runtime manager with leader election")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
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
