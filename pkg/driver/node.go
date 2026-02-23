package driver

import (
	"context"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"

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
func (d *Driver) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	server := req.GetVolumeContext()["server"]
	sharePath := req.GetVolumeContext()["share"]
	stagingPath := req.GetStagingTargetPath()

	// Use zone-local IP when available for cross-zone access
	if nodeZone := d.cachedNodeZone(ctx); nodeZone != "" {
		if zoneIP := req.GetVolumeContext()["server."+nodeZone]; zoneIP != "" {
			server = zoneIP
		}
	}

	if d.mountCache.IsMounted(stagingPath) {
		return &csi.NodeStageVolumeResponse{}, nil
	}

	source := fmt.Sprintf("%s:%s", server, sharePath)

	mountOptions := mergeNFSMountOptions(
		[]string{"nfsvers=4.1", "soft", "timeo=600", "retrans=3", "sec=sys"},
		req.GetVolumeCapability().GetMount().GetMountFlags(),
	)

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
func (d *Driver) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {
	stagingPath := req.GetStagingTargetPath()
	targetPath := req.GetTargetPath()
	subDir := req.GetVolumeContext()["subDir"]

	// SECURITY: Validate subDir path
	if err := util.ValidateSubDir(subDir); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid subDir path: %s", subDir)
	}

	// Check if this is a clone that is still in progress.
	// The clone source label is set at allocation time, but CloneStatus may not
	// be set yet when the clone worker hasn't processed it. We must block in
	// both cases to prevent mounting an empty directory before the clone completes.
	if d.k8sClient != nil {
		_, _, pvName, parseErr := parseVolumeID(req.GetVolumeId())
		if parseErr == nil {
			sv, svErr := d.k8sClient.GetSubVolume(ctx, pvName)
			if svErr == nil {
				isClone := sv.Labels["storage.ibmcloud.io/clone-source"] != ""
				cloneStatus := sv.Status.CloneStatus

				if isClone {
					switch cloneStatus {
					case "Complete":
						// Clone is done, proceed with normal mount
					case "Failed":
						errMsg := "clone failed"
						if sv.Status.CloneProgress != nil && sv.Status.CloneProgress.Error != "" {
							errMsg = sv.Status.CloneProgress.Error
						}
						return nil, status.Errorf(codes.Internal, "clone failed: %s", errMsg)
					default:
						// Pending, InProgress, or empty (not yet processed by clone worker)
						klog.V(4).InfoS("Clone gate: blocking mount until clone completes",
							"subVolume", pvName, "cloneStatus", cloneStatus)
						return nil, status.Errorf(codes.Unavailable,
							"clone is %s, not ready for mount (retry later)", cloneStatus)
					}
				}
			}
		}
	}

	sourcePath := filepath.Join(stagingPath, subDir)

	// Create the subdirectory on the NFS share if it doesn't exist yet.
	// The controller only records the SubVolume CR; actual mkdir happens here
	// on the node where the NFS share is mounted.
	if _, err := os.Stat(sourcePath); os.IsNotExist(err) {
		perm := os.FileMode(0755)
		if p := req.GetVolumeContext()["permissions"]; p != "" {
			if parsed, err := strconv.ParseUint(p, 8, 32); err == nil {
				perm = os.FileMode(parsed)
			}
		}
		// Parse ownership
		uidVal, gidVal := -1, -1
		if u := req.GetVolumeContext()["uid"]; u != "" {
			if v, err := strconv.Atoi(u); err == nil {
				uidVal = v
			}
		}
		if g := req.GetVolumeContext()["gid"]; g != "" {
			if v, err := strconv.Atoi(g); err == nil {
				gidVal = v
			}
		}

		if err := os.MkdirAll(sourcePath, perm); err != nil {
			return nil, status.Errorf(codes.Internal, "failed to create subdirectory %s: %v", subDir, err)
		}
		// Chown is best-effort: works with sec=sys (the default), but kept
		// non-fatal for resilience if custom mount options omit sec=sys.
		if uidVal >= 0 || gidVal >= 0 {
			if err := os.Chown(sourcePath, uidVal, gidVal); err != nil {
				klog.V(4).InfoS("Chown failed (best-effort)", "path", sourcePath, "uid", uidVal, "gid", gidVal, "error", err)
			}
		}
		// Ensure exact permissions regardless of umask.
		if err := os.Chmod(sourcePath, perm); err != nil {
			klog.ErrorS(err, "Failed to chmod subdirectory", "path", sourcePath, "perm", perm)
		}
		klog.V(2).InfoS("Created subdirectory on NFS share", "path", sourcePath, "perm", perm, "uid", uidVal, "gid", gidVal)
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

// NodeGetVolumeStats reports per-PVC capacity and usage.
//
// It looks up the SubVolume CR to get the PVC's requested size (quota),
// then walks the bind-mount directory to compute actual disk usage.
// This gives meaningful per-PVC stats instead of repeated share-level numbers.
// Falls back to share-level statfs if the SubVolume lookup fails.
func (d *Driver) NodeGetVolumeStats(ctx context.Context, req *csi.NodeGetVolumeStatsRequest) (*csi.NodeGetVolumeStatsResponse, error) {
	volumePath := req.GetVolumePath()

	// Always get share-level stats for inode reporting and fallback.
	var stat unix.Statfs_t
	if err := unix.Statfs(volumePath, &stat); err != nil {
		return nil, status.Errorf(codes.Internal, "statfs failed: %v", err)
	}

	totalInodes := int64(stat.Files) //nolint:gosec // safe: inode count won't overflow int64
	freeInodes := int64(stat.Ffree)  //nolint:gosec // safe: inode count won't overflow int64
	usedInodes := totalInodes - freeInodes

	// Try per-PVC usage: look up SubVolume CR for RequestedGB, walk dir for actual usage.
	volumeID := req.GetVolumeId()
	_, _, pvName, parseErr := parseVolumeID(volumeID)
	if parseErr == nil && d.k8sClient != nil {
		sv, svErr := d.k8sClient.GetSubVolume(ctx, pvName)
		if svErr == nil && sv.Spec.RequestedGB > 0 {
			usedBytes, walkErr := dirUsageBytes(volumePath)
			if walkErr == nil {
				const gb = 1024 * 1024 * 1024
				totalBytes := sv.Spec.RequestedGB * gb
				available := totalBytes - usedBytes
				if available < 0 {
					available = 0
				}

				klog.V(6).InfoS("Per-PVC volume stats",
					"pvName", pvName,
					"requestedGB", sv.Spec.RequestedGB,
					"usedBytes", usedBytes,
				)

				return &csi.NodeGetVolumeStatsResponse{
					Usage: []*csi.VolumeUsage{
						{
							Available: available,
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
			klog.V(4).InfoS("Directory walk failed, falling back to statfs", "path", volumePath, "error", walkErr)
		}
	}

	// Fallback: share-level statfs
	totalBytes := int64(stat.Blocks) * int64(stat.Bsize) //nolint:gosec // safe: filesystem stats won't overflow
	freeBytes := int64(stat.Bfree) * int64(stat.Bsize)   //nolint:gosec // safe: filesystem stats won't overflow
	usedBytes := totalBytes - freeBytes

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

// dirUsageBytes walks a directory tree and returns the total size of all regular files.
func dirUsageBytes(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			info, infoErr := d.Info()
			if infoErr != nil {
				return infoErr
			}
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// mergeNFSMountOptions merges custom mount flags with safe NFS defaults.
// Custom flags override defaults with the same key (e.g. "timeo=300" overrides
// "timeo=600"). The "soft" default is always preserved unless the custom flags
// explicitly include "hard".
func mergeNFSMountOptions(defaults, custom []string) []string {
	if len(custom) == 0 {
		return defaults
	}

	// optKey extracts the key portion of a mount option (before '=').
	optKey := func(opt string) string {
		for i, c := range opt {
			if c == '=' {
				return opt[:i]
			}
		}
		return opt
	}

	// Build a map of default options keyed by their prefix.
	merged := make(map[string]string, len(defaults)+len(custom))
	order := make([]string, 0, len(defaults)+len(custom))
	for _, opt := range defaults {
		key := optKey(opt)
		merged[key] = opt
		order = append(order, key)
	}

	// Apply custom options, overriding defaults with the same key.
	hasHard := false
	for _, opt := range custom {
		key := optKey(opt)
		if key == "hard" {
			hasHard = true
		}
		if _, exists := merged[key]; !exists {
			order = append(order, key)
		}
		merged[key] = opt
	}

	// If "hard" was explicitly specified, remove "soft" (they're mutually exclusive).
	if hasHard {
		delete(merged, "soft")
	}

	result := make([]string, 0, len(merged))
	for _, key := range order {
		if opt, ok := merged[key]; ok {
			result = append(result, opt)
		}
	}
	return result
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
