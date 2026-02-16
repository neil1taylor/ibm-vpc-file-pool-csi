package driver

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	csi "github.com/container-storage-interface/spec/lib/go/csi"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/klog/v2"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	"github.com/IBM/ibm-vpc-file-pool-csi/pkg/pool"
)

// ControllerGetCapabilities reports the supported controller RPCs.
func (d *Driver) ControllerGetCapabilities(_ context.Context, _ *csi.ControllerGetCapabilitiesRequest) (*csi.ControllerGetCapabilitiesResponse, error) {
	return &csi.ControllerGetCapabilitiesResponse{
		Capabilities: []*csi.ControllerServiceCapability{
			newControllerCap(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME),
			newControllerCap(csi.ControllerServiceCapability_RPC_EXPAND_VOLUME),
		},
	}, nil
}

// CreateVolume allocates a subdirectory on a pooled VPC file share.
func (d *Driver) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if req.GetName() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume name is required")
	}

	params := req.GetParameters()
	poolName := params["pool"]
	if poolName == "" {
		return nil, status.Error(codes.InvalidArgument, "StorageClass parameter 'pool' is required")
	}

	// Extract requested size
	requiredBytes := req.GetCapacityRange().GetRequiredBytes()
	requiredGB := (requiredBytes + (1<<30 - 1)) / (1 << 30)
	if requiredGB < 1 {
		requiredGB = 1
	}

	// Check for idempotency — does a SubVolume CR already exist for this volume name?
	if d.k8sClient != nil {
		existing, err := d.k8sClient.GetSubVolume(ctx, req.GetName())
		if err == nil && existing != nil {
			return &csi.CreateVolumeResponse{
				Volume: subVolumeToCSIVolume(existing),
			}, nil
		}
	}

	// Extract topology preference (zone)
	zone := ""
	if req.GetAccessibilityRequirements() != nil {
		for _, topo := range req.GetAccessibilityRequirements().GetPreferred() {
			if z, ok := topo.GetSegments()["topology.kubernetes.io/zone"]; ok {
				zone = z
				break
			}
		}
	}

	// Delegate to Pool Manager
	allocReq := pool.AllocationRequest{
		PVName:       req.GetName(),
		PVCName:      params["csi.storage.k8s.io/pvc/name"],
		PVCNamespace: params["csi.storage.k8s.io/pvc/namespace"],
		PoolName:     poolName,
		RequestedGB:  requiredGB,
		Zone:         zone,
		Tier:         params["tier"],
		UID:          parseOptionalInt64(params["uid"]),
		GID:          parseOptionalInt64(params["gid"]),
		Permissions:  params["permissions"],
	}

	result, err := d.poolManager.Allocate(ctx, allocReq)
	if err != nil {
		switch {
		case errors.Is(err, pool.ErrPoolNotFound):
			return nil, status.Errorf(codes.NotFound, "pool %q not found", poolName)
		case errors.Is(err, pool.ErrPoolExhausted):
			return nil, status.Errorf(codes.ResourceExhausted, "pool %q has no available capacity", poolName)
		case errors.Is(err, pool.ErrShareCreationPending):
			return nil, status.Errorf(codes.Unavailable, "pool %q is expanding, retry shortly", poolName)
		default:
			return nil, status.Errorf(codes.Internal, "allocation failed: %v", err)
		}
	}

	volumeID := fmt.Sprintf("%s/%s/%s", poolName, result.ShareID, req.GetName())

	klog.V(2).InfoS("Created volume",
		"volumeID", volumeID,
		"pool", poolName,
		"shareID", result.ShareID,
		"subPath", result.SubPath,
		"requestedGB", requiredGB,
	)

	volCtx := map[string]string{
		"server":  result.MountTargetIP,
		"share":   result.SharePath,
		"subDir":  result.SubPath,
		"pool":    poolName,
		"shareID": result.ShareID,
	}
	if result.Permissions != "" {
		volCtx["permissions"] = result.Permissions
	}
	if result.UID != nil {
		volCtx["uid"] = strconv.FormatInt(*result.UID, 10)
	}
	if result.GID != nil {
		volCtx["gid"] = strconv.FormatInt(*result.GID, 10)
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			VolumeId:      volumeID,
			CapacityBytes: requiredGB * (1 << 30),
			VolumeContext: volCtx,
			AccessibleTopology: []*csi.Topology{
				{
					Segments: map[string]string{
						"topology.kubernetes.io/zone": zone,
					},
				},
			},
		},
	}, nil
}

// DeleteVolume removes a subdirectory allocation.
func (d *Driver) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	_, _, pvName, err := parseVolumeID(volumeID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume ID: %v", err)
	}

	err = d.poolManager.Deallocate(ctx, pvName)
	if err != nil {
		if errors.Is(err, pool.ErrSubVolumeNotFound) {
			return &csi.DeleteVolumeResponse{}, nil
		}
		return nil, status.Errorf(codes.Internal, "deallocation failed: %v", err)
	}

	klog.V(2).InfoS("Deleted volume", "volumeID", volumeID)
	return &csi.DeleteVolumeResponse{}, nil
}

// ControllerExpandVolume updates the allocation size for an existing SubVolume.
func (d *Driver) ControllerExpandVolume(ctx context.Context, req *csi.ControllerExpandVolumeRequest) (*csi.ControllerExpandVolumeResponse, error) {
	volumeID := req.GetVolumeId()
	if volumeID == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	_, _, pvName, err := parseVolumeID(volumeID)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "invalid volume ID: %v", err)
	}

	requiredBytes := req.GetCapacityRange().GetRequiredBytes()
	requiredGB := (requiredBytes + (1<<30 - 1)) / (1 << 30)

	err = d.poolManager.Expand(ctx, pvName, requiredGB)
	if err != nil {
		if errors.Is(err, pool.ErrInsufficientShareCapacity) {
			return nil, status.Errorf(codes.ResourceExhausted, "share does not have enough remaining capacity")
		}
		return nil, status.Errorf(codes.Internal, "expand failed: %v", err)
	}

	return &csi.ControllerExpandVolumeResponse{
		CapacityBytes:         requiredGB * (1 << 30),
		NodeExpansionRequired: false,
	}, nil
}

// ValidateVolumeCapabilities checks if the requested capabilities are supported.
// NFS supports all access modes.
func (d *Driver) ValidateVolumeCapabilities(_ context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	if req.GetVolumeId() == "" {
		return nil, status.Error(codes.InvalidArgument, "volume ID is required")
	}

	supported := []*csi.VolumeCapability_AccessMode{
		{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_WRITER},
		{Mode: csi.VolumeCapability_AccessMode_SINGLE_NODE_READER_ONLY},
		{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_READER_ONLY},
		{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_SINGLE_WRITER},
		{Mode: csi.VolumeCapability_AccessMode_MULTI_NODE_MULTI_WRITER},
	}

	for _, cap := range req.GetVolumeCapabilities() {
		found := false
		for _, s := range supported {
			if cap.GetAccessMode().GetMode() == s.Mode {
				found = true
				break
			}
		}
		if !found {
			return &csi.ValidateVolumeCapabilitiesResponse{}, nil
		}
	}

	return &csi.ValidateVolumeCapabilitiesResponse{
		Confirmed: &csi.ValidateVolumeCapabilitiesResponse_Confirmed{
			VolumeCapabilities: req.GetVolumeCapabilities(),
		},
	}, nil
}

// subVolumeToCSIVolume converts a SubVolume CR to a CSI Volume for idempotent responses.
func subVolumeToCSIVolume(sv *v1alpha1.SubVolume) *csi.Volume {
	volumeID := fmt.Sprintf("%s/%s/%s", sv.Spec.PoolName, sv.Spec.ShareID, sv.Spec.PVName)
	return &csi.Volume{
		VolumeId:      volumeID,
		CapacityBytes: sv.Spec.RequestedGB * (1 << 30),
		VolumeContext: map[string]string{
			"server":  sv.Spec.ShareMountTargetIP,
			"share":   "/",
			"subDir":  sv.Spec.SubPath,
			"pool":    sv.Spec.PoolName,
			"shareID": sv.Spec.ShareID,
		},
	}
}

// parseOptionalInt64 parses a string to *int64, returning nil on empty or error.
func parseOptionalInt64(s string) *int64 {
	if s == "" {
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return nil
	}
	return &v
}

func newControllerCap(cap csi.ControllerServiceCapability_RPC_Type) *csi.ControllerServiceCapability {
	return &csi.ControllerServiceCapability{
		Type: &csi.ControllerServiceCapability_Rpc{
			Rpc: &csi.ControllerServiceCapability_RPC{
				Type: cap,
			},
		},
	}
}
