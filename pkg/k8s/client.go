package k8s

import (
	"context"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
)

// Client defines the interface for Kubernetes CRD operations used by
// the pool manager and CSI controller.
type Client interface {
	// FileSharePool operations
	GetFileSharePool(ctx context.Context, name string) (*v1alpha1.FileSharePool, error)
	UpdateFileSharePoolStatus(ctx context.Context, pool *v1alpha1.FileSharePool) error
	UpdateFileSharePool(ctx context.Context, pool *v1alpha1.FileSharePool) error

	// Node operations
	GetNodeZone(ctx context.Context, nodeName string) (string, error)

	// ConfigMap operations
	GetConfigMapValue(ctx context.Context, namespace, name, key string) (string, error)

	// SubVolume operations
	GetSubVolume(ctx context.Context, name string) (*v1alpha1.SubVolume, error)
	ListSubVolumes(ctx context.Context, poolName string) ([]v1alpha1.SubVolume, error)
	ListSubVolumesByShare(ctx context.Context, shareID string) ([]v1alpha1.SubVolume, error)
	CreateSubVolume(ctx context.Context, sv *v1alpha1.SubVolume) error
	UpdateSubVolume(ctx context.Context, sv *v1alpha1.SubVolume) error
	UpdateSubVolumeStatus(ctx context.Context, sv *v1alpha1.SubVolume) error
	DeleteSubVolume(ctx context.Context, name string) error

	// Snapshot operations
	GetSnapshot(ctx context.Context, name string) (*v1alpha1.Snapshot, error)
	ListSnapshots(ctx context.Context, poolName string) ([]v1alpha1.Snapshot, error)
	ListSnapshotsByShare(ctx context.Context, shareID string) ([]v1alpha1.Snapshot, error)
	ListSnapshotsBySource(ctx context.Context, sourceSubVolume string) ([]v1alpha1.Snapshot, error)
	CreateSnapshot(ctx context.Context, snap *v1alpha1.Snapshot) error
	UpdateSnapshot(ctx context.Context, snap *v1alpha1.Snapshot) error
	UpdateSnapshotStatus(ctx context.Context, snap *v1alpha1.Snapshot) error
	DeleteSnapshot(ctx context.Context, name string) error
}
