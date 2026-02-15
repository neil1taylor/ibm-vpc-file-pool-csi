package driver

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"golang.org/x/sys/unix"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/util"
)

// NodeGetCapabilities reports the supported node RPCs.
func (d *Driver) NodeGetCapabilities(_ context.Context, _ *csi.NodeGetCapabilitiesRequest) (*csi.NodeGetCapabilitiesResponse, error) {
	return &csi.NodeGetCapabilitiesResponse{
		Capabilities: []*csi.NodeServiceCapability{
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_STAGE_UNSTAGE_VOLUME,
					},
				},
			},
			{
				Type: &csi.NodeServiceCapability_Rpc{
					Rpc: &csi.NodeServiceCapability_RPC{
						Type: csi.NodeServiceCapability_RPC_GET_VOLUME_STATS,
					},
				},
			},
		},
	}, nil
}

// NodeStageVolume mounts the NFS share to a staging directory.
// Called once per share per node (not once per PVC).
func (d *Driver) NodeStageVolume(_ context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	server := req.GetVolumeContext()["server"]
	sharePath := req.GetVolumeContext()["share"]
	stagingPath := req.GetStagingTargetPath()

	if d.mountCache.IsMounted(stagingPath) {
		return &csi.NodeStageVolumeResponse{}, nil
	}

	source := fmt.Sprintf("%s:%s", server, sharePath)

	mountOptions := []string{"nfsvers=4.1", "soft", "timeo=600", "retrans=3"}
	if opts := req.GetVolumeCapability().GetMount().GetMountFlags(); len(opts) > 0 {
		mountOptions = opts
	}

	if err := os.MkdirAll(stagingPath, 0750); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create staging dir: %v", err)
	}

	if err := d.mounter.Mount(source, stagingPath, "nfs4", mountOptions); err != nil {
		return nil, status.Errorf(codes.Internal, "NFS mount failed: %v", err)
	}

	d.mountCache.Add(stagingPath, server, sharePath)

	klog.V(2).InfoS("Staged volume", "source", source, "stagingPath", stagingPath)
	return &csi.NodeStageVolumeResponse{}, nil
}

// NodePublishVolume bind-mounts the specific subdirectory into the pod's volume path.
func (d *Driver) NodePublishVolume(_ context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	stagingPath := req.GetStagingTargetPath()
	targetPath := req.GetTargetPath()
	subDir := req.GetVolumeContext()["subDir"]

	// SECURITY: Validate subDir path
	if err := util.ValidateSubDir(subDir); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid subDir path: %s", subDir)
	}

	sourcePath := filepath.Join(stagingPath, subDir)

	if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
		return nil, status.Errorf(codes.NotFound, "subdirectory %s does not exist on share", subDir)
	}

	if err := os.MkdirAll(targetPath, 0750); err != nil {
		return nil, status.Errorf(codes.Internal, "failed to create target dir: %v", err)
	}

	mountOptions := []string{"bind"}
	if req.GetReadonly() {
		mountOptions = append(mountOptions, "ro")
	}

	if err := d.mounter.Mount(sourcePath, targetPath, "", mountOptions); err != nil {
		return nil, status.Errorf(codes.Internal, "bind mount failed: %v", err)
	}

	klog.V(2).InfoS("Published volume", "subDir", subDir, "targetPath", targetPath)
	return &csi.NodePublishVolumeResponse{}, nil
}

// NodeUnpublishVolume unmounts the bind-mount from the pod path.
func (d *Driver) NodeUnpublishVolume(_ context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	targetPath := req.GetTargetPath()

	if err := d.mounter.Unmount(targetPath); err != nil {
		return nil, status.Errorf(codes.Internal, "unmount failed: %v", err)
	}

	if err := os.Remove(targetPath); err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "failed to remove target dir: %v", err)
	}

	klog.V(2).InfoS("Unpublished volume", "targetPath", targetPath)
	return &csi.NodeUnpublishVolumeResponse{}, nil
}

// NodeUnstageVolume unmounts the NFS share from the staging directory.
func (d *Driver) NodeUnstageVolume(_ context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	stagingPath := req.GetStagingTargetPath()

	if err := d.mounter.Unmount(stagingPath); err != nil {
		return nil, status.Errorf(codes.Internal, "NFS unmount failed: %v", err)
	}

	d.mountCache.Remove(stagingPath)

	if err := os.Remove(stagingPath); err != nil && !os.IsNotExist(err) {
		return nil, status.Errorf(codes.Internal, "failed to remove staging dir: %v", err)
	}

	klog.V(2).InfoS("Unstaged volume", "stagingPath", stagingPath)
	return &csi.NodeUnstageVolumeResponse{}, nil
}

// NodeGetVolumeStats reports capacity/usage for the mount point via statfs.
func (d *Driver) NodeGetVolumeStats(_ context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumePath := req.GetVolumePath()

	var stat unix.Statfs_t
	if err := unix.Statfs(volumePath, &stat); err != nil {
		return nil, status.Errorf(codes.Internal, "statfs failed: %v", err)
	}

	totalBytes := int64(stat.Blocks) * int64(stat.Bsize)
	freeBytes := int64(stat.Bfree) * int64(stat.Bsize)
	usedBytes := totalBytes - freeBytes

	totalInodes := int64(stat.Files)
	freeInodes := int64(stat.Ffree)
	usedInodes := totalInodes - freeInodes

	return &csi.NodeGetVolumeStatsResponse{
		Usage: []*csi.VolumeUsage{
			{
				Available: freeBytes,
				Total:     totalBytes,
				Used:      usedBytes,
				Unit:      csi.VolumeUsage_BYTES,
			},
			{
				Available: freeInodes,
				Total:     totalInodes,
				Used:      usedInodes,
				Unit:      csi.VolumeUsage_INODES,
			},
		},
	}, nil
}

// NodeGetInfo returns the node ID and accessible topology (zone).
func (d *Driver) NodeGetInfo(ctx context.Context, _ *csi.NodeGetInfoRequest) (*csi.NodeGetInfoResponse, error) {
	zone, err := d.getNodeZone(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to determine node zone: %v", err)
	}

	return &csi.NodeGetInfoResponse{
		NodeId: d.nodeID,
		AccessibleTopology: &csi.Topology{
			Segments: map[string]string{
				"topology.kubernetes.io/zone": zone,
			},
		},
	}, nil
}

// getNodeZone reads the zone from node labels via the Kubernetes API.
func (d *Driver) getNodeZone(ctx context.Context) (string, error) {
	if d.k8sClient == nil {
		return "", fmt.Errorf("k8s client not configured")
	}
	zone, err := d.k8sClient.GetNodeZone(ctx, d.nodeID)
	if err != nil {
		return "", fmt.Errorf("get node zone for %q: %w", d.nodeID, err)
	}
	if zone == "" {
		return "", fmt.Errorf("node %q has no topology.kubernetes.io/zone label", d.nodeID)
	}
	return zone, nil
}
