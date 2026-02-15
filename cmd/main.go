package main

import (
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/driver"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
)

var version = "dev"

func main() {
	var (
		endpoint string
		nodeID   string
		mode     string
	)

	flag.StringVar(&endpoint, "endpoint", "unix:///csi/csi.sock", "CSI endpoint")
	flag.StringVar(&nodeID, "node-id", "", "Node ID")
	flag.StringVar(&mode, "mode", "controller", "Driver mode: controller or node")

	klog.InitFlags(nil)
	flag.Parse()

	klog.InfoS("Starting IBM VPC File Pool CSI Driver", "version", version, "mode", mode)

	if mode == "controller" {
		runController(endpoint, nodeID)
	} else {
		runNode(endpoint, nodeID, mode)
	}
}

func runController(endpoint, nodeID string) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		klog.ErrorS(err, "Failed to add CRD types to scheme")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                  scheme,
		LeaderElection:          true,
		LeaderElectionID:        "ibm-vpc-file-pool-csi-leader",
		LeaderElectionNamespace: "kube-system",
	})
	if err != nil {
		klog.ErrorS(err, "Failed to create controller-runtime manager")
		os.Exit(1)
	}

	// TODO: Create real k8s.Client and ibmcloud.VPCFileClient implementations.
	// These are separate tasks per API-KEY-SETUP.md and will be wired here once implemented.
	// For now, pass nil — the reconciler will panic if actually invoked without real clients.
	reconciler := pool.NewFileSharePoolReconciler(nil, nil)
	if err := reconciler.SetupWithManager(mgr); err != nil {
		klog.ErrorS(err, "Failed to set up reconciler")
		os.Exit(1)
	}

	// Run CSI gRPC server in a goroutine
	d, err := driver.NewDriver(driver.Config{
		Name:     driver.DriverName,
		Version:  version,
		NodeID:   nodeID,
		Endpoint: endpoint,
		Mode:     "controller",
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
	d, err := driver.NewDriver(driver.Config{
		Name:     driver.DriverName,
		Version:  version,
		NodeID:   nodeID,
		Endpoint: endpoint,
		Mode:     mode,
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
