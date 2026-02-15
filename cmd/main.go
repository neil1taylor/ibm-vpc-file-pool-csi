package main

import (
	"flag"
	"os"

	"k8s.io/klog/v2"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/driver"
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
