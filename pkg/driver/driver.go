package driver

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc"
	"k8s.io/klog/v2"
	mount "k8s.io/mount-utils"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/k8s"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
)

// DriverName is the CSI driver name registered with Kubernetes.
const DriverName = "vpc-file-pool.csi.ibm.io"

// Config holds the configuration for creating a new Driver.
type Config struct {
	Name     string
	Version  string
	NodeID   string
	Endpoint string
	Mode     string // "controller" or "node"
}

// Driver implements the CSI Identity, Controller, and Node services.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	name        string
	version     string
	nodeID      string
	endpoint    string
	mode        string
	poolManager pool.PoolManager
	k8sClient   k8s.Client
	mounter     mount.Interface
	mountCache  *util.MountCache
}

// NewDriver creates a new CSI driver instance.
// Dependencies (poolManager, k8sClient, mounter) are nil in the scaffold;
// they will be wired up during full implementation.
func NewDriver(cfg Config) (*Driver, error) {
	return &Driver{
		name:       cfg.Name,
		version:    cfg.Version,
		nodeID:     cfg.NodeID,
		endpoint:   cfg.Endpoint,
		mode:       cfg.Mode,
		mountCache: util.NewMountCache(),
	}, nil
}

// Run starts the gRPC server and registers CSI services based on mode.
func (d *Driver) Run() error {
	addr := d.endpoint
	if strings.HasPrefix(addr, "unix://") {
		addr = strings.TrimPrefix(addr, "unix://")
	}

	// Remove existing socket file
	if err := os.Remove(addr); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to remove existing socket: %w", err)
	}

	listener, err := net.Listen("unix", addr)
	if err != nil {
		return fmt.Errorf("failed to listen on %s: %w", addr, err)
	}

	server := grpc.NewServer(
		grpc.UnaryInterceptor(logInterceptor),
	)

	csi.RegisterIdentityServer(server, d)

	if d.isController() {
		csi.RegisterControllerServer(server, d)
	}

	if d.isNode() {
		csi.RegisterNodeServer(server, d)
	}

	klog.InfoS("CSI driver serving", "endpoint", addr, "mode", d.mode)
	return server.Serve(listener)
}

func (d *Driver) isController() bool {
	return d.mode == "controller"
}

func (d *Driver) isNode() bool {
	return d.mode == "node"
}

func logInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	klog.V(4).InfoS("gRPC call", "method", info.FullMethod)
	resp, err := handler(ctx, req)
	if err != nil {
		klog.ErrorS(err, "gRPC call failed", "method", info.FullMethod)
	}
	return resp, err
}

// parseVolumeID splits a volume ID into its components.
// Format: {pool-name}/{share-vpc-id}/{pv-name}
func parseVolumeID(volumeID string) (poolName, shareID, pvName string, err error) {
	parts := strings.SplitN(volumeID, "/", 3)
	if len(parts) != 3 {
		return "", "", "", fmt.Errorf("volume ID must have 3 parts separated by '/', got: %s", volumeID)
	}
	return parts[0], parts[1], parts[2], nil
}
