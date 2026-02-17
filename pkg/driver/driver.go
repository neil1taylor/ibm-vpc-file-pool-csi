package driver

import (
	"context"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

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
	Name        string
	Version     string
	NodeID      string
	Endpoint    string
	Mode        string // "controller" or "node"
	Mounter     mount.Interface
	K8sClient   k8s.Client
	PoolManager pool.PoolManager
	Ready       <-chan struct{} // Closed when VPC client is initialized; nil = always ready
}

// Driver implements the CSI Identity, Controller, and Node services.
type Driver struct {
	csi.UnimplementedIdentityServer
	csi.UnimplementedControllerServer
	csi.UnimplementedNodeServer

	name         string
	version      string
	nodeID       string
	endpoint     string
	mode         string
	poolManager  pool.PoolManager
	k8sClient    k8s.Client
	mounter      mount.Interface
	mountCache   *util.MountCache
	ready        <-chan struct{}
	nodeZone     string
	nodeZoneOnce sync.Once
}

// NewDriver creates a new CSI driver instance.
func NewDriver(cfg Config) (*Driver, error) {
	mounter := cfg.Mounter
	if mounter == nil {
		mounter = mount.New("")
	}

	ready := cfg.Ready
	if ready == nil {
		// nil means always ready (node mode, tests)
		ch := make(chan struct{})
		close(ch)
		ready = ch
	}

	return &Driver{
		name:        cfg.Name,
		version:     cfg.Version,
		nodeID:      cfg.NodeID,
		endpoint:    cfg.Endpoint,
		mode:        cfg.Mode,
		poolManager: cfg.PoolManager,
		k8sClient:   cfg.K8sClient,
		mounter:     mounter,
		mountCache:  util.NewMountCache(),
		ready:       ready,
	}, nil
}

// isReady returns true if the driver has completed initialization.
func (d *Driver) isReady() bool {
	select {
	case <-d.ready:
		return true
	default:
		return false
	}
}

// Run starts the gRPC server and registers CSI services based on mode.
func (d *Driver) Run() error {
	addr := strings.TrimPrefix(d.endpoint, "unix://")

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

// cachedNodeZone lazily caches the node's zone from the Kubernetes API.
// Returns empty string if zone detection fails (best-effort for cross-zone).
func (d *Driver) cachedNodeZone(ctx context.Context) string {
	d.nodeZoneOnce.Do(func() {
		zone, err := d.getNodeZone(ctx)
		if err != nil {
			klog.V(4).InfoS("Failed to cache node zone, cross-zone IP selection disabled", "error", err)
			return
		}
		d.nodeZone = zone
	})
	return d.nodeZone
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
	return util.ParseVolumeID(volumeID)
}
