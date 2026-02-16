package ibmcloud

import (
	"context"
	"errors"
	"time"
)

// Sentinel errors for VPC API operations.
var (
	ErrShareNotFound       = errors.New("VPC file share not found")
	ErrShareNotStable      = errors.New("VPC file share is not in stable state")
	ErrShareCreationFailed = errors.New("VPC file share creation failed")
	ErrAPIRateLimit        = errors.New("VPC API rate limit exceeded")
	ErrAuthentication      = errors.New("VPC API authentication failed")
)

// VPCFileClient defines the interface for VPC file share operations.
// The real implementation wraps vpc-go-sdk; tests use the fake in pkg/ibmcloud/fake/.
type VPCFileClient interface {
	CreateFileShare(ctx context.Context, input CreateShareInput) (*ShareInfo, error)
	GetFileShare(ctx context.Context, shareID string) (*ShareInfo, error)
	ExpandFileShare(ctx context.Context, shareID string, newSizeGB int64) error
	DeleteFileShare(ctx context.Context, shareID string) error
	ListFileShares(ctx context.Context, resourceGroupID string, tags []string) ([]*ShareInfo, error)
	CreateShareMountTarget(ctx context.Context, shareID string, input CreateMountTargetInput) (*MountTargetInfo, error)
}

// CreateShareInput contains the parameters for creating a new VPC file share.
type CreateShareInput struct {
	Name             string
	Zone             string
	Profile          string
	SizeGB           int64
	IOPS             *int64
	ResourceGroupID  string
	Tags             []string
	EncryptInTransit bool
	VPCId            string
	SubnetID         string
	SecurityGroupIDs []string
}

// ShareInfo contains metadata about a VPC file share.
type ShareInfo struct {
	ID             string
	Name           string
	LifecycleState string // "pending", "stable", "failed", etc.
	SizeGB         int64
	IOPS           int64
	Profile        string
	Zone           string
	MountTargets   []MountTargetInfo
	CreatedAt      time.Time
}

// CreateMountTargetInput contains the parameters for creating a mount target on an existing share.
type CreateMountTargetInput struct {
	Name             string
	VPCId            string
	SubnetID         string
	EncryptInTransit bool
}

// MountTargetInfo contains metadata about a VPC file share mount target.
type MountTargetInfo struct {
	ID        string
	Name      string
	IPAddress string // NFS server IP to mount
}
