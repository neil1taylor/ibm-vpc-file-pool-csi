package k8s

import (
	"context"
	"fmt"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// realClient implements Client by wrapping a controller-runtime client.Client.
// No mutex is needed — controller-runtime's client.Client is thread-safe.
type realClient struct {
	client client.Client
}

// NewClient creates a real Client wrapping a controller-runtime client.Client.
func NewClient(c client.Client) Client {
	return &realClient{client: c}
}

// --- FileSharePool operations ---

func (r *realClient) GetFileSharePool(ctx context.Context, name string) (*v1alpha1.FileSharePool, error) {
	pool := &v1alpha1.FileSharePool{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, pool); err != nil {
		return nil, err
	}
	return pool, nil
}

func (r *realClient) UpdateFileSharePoolStatus(ctx context.Context, pool *v1alpha1.FileSharePool) error {
	return r.client.Status().Update(ctx, pool)
}

func (r *realClient) UpdateFileSharePool(ctx context.Context, pool *v1alpha1.FileSharePool) error {
	return r.client.Update(ctx, pool)
}

// --- SubVolume operations ---

func (r *realClient) GetSubVolume(ctx context.Context, name string) (*v1alpha1.SubVolume, error) {
	sv := &v1alpha1.SubVolume{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, sv); err != nil {
		return nil, err
	}
	return sv, nil
}

func (r *realClient) ListSubVolumes(ctx context.Context, poolName string) ([]v1alpha1.SubVolume, error) {
	list := &v1alpha1.SubVolumeList{}
	if err := r.client.List(ctx, list, client.MatchingLabels{
		"storage.ibmcloud.io/pool": poolName,
	}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *realClient) ListSubVolumesByShare(ctx context.Context, shareID string) ([]v1alpha1.SubVolume, error) {
	list := &v1alpha1.SubVolumeList{}
	if err := r.client.List(ctx, list, client.MatchingLabels{
		"storage.ibmcloud.io/share-id": shareID,
	}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *realClient) CreateSubVolume(ctx context.Context, sv *v1alpha1.SubVolume) error {
	return r.client.Create(ctx, sv)
}

func (r *realClient) UpdateSubVolume(ctx context.Context, sv *v1alpha1.SubVolume) error {
	return r.client.Update(ctx, sv)
}

func (r *realClient) UpdateSubVolumeStatus(ctx context.Context, sv *v1alpha1.SubVolume) error {
	return r.client.Status().Update(ctx, sv)
}

func (r *realClient) DeleteSubVolume(ctx context.Context, name string) error {
	sv := &v1alpha1.SubVolume{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, sv); err != nil {
		return err
	}
	return r.client.Delete(ctx, sv)
}

// --- Snapshot operations ---

func (r *realClient) GetSnapshot(ctx context.Context, name string) (*v1alpha1.Snapshot, error) {
	snap := &v1alpha1.Snapshot{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, snap); err != nil {
		return nil, err
	}
	return snap, nil
}

func (r *realClient) ListSnapshots(ctx context.Context, poolName string) ([]v1alpha1.Snapshot, error) {
	list := &v1alpha1.SnapshotList{}
	if err := r.client.List(ctx, list, client.MatchingLabels{
		"storage.ibmcloud.io/pool": poolName,
	}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *realClient) ListSnapshotsByShare(ctx context.Context, shareID string) ([]v1alpha1.Snapshot, error) {
	list := &v1alpha1.SnapshotList{}
	if err := r.client.List(ctx, list, client.MatchingLabels{
		"storage.ibmcloud.io/share-id": shareID,
	}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *realClient) ListSnapshotsBySource(ctx context.Context, sourceSubVolume string) ([]v1alpha1.Snapshot, error) {
	list := &v1alpha1.SnapshotList{}
	if err := r.client.List(ctx, list, client.MatchingLabels{
		"storage.ibmcloud.io/source-subvolume": sourceSubVolume,
	}); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *realClient) CreateSnapshot(ctx context.Context, snap *v1alpha1.Snapshot) error {
	return r.client.Create(ctx, snap)
}

func (r *realClient) UpdateSnapshot(ctx context.Context, snap *v1alpha1.Snapshot) error {
	return r.client.Update(ctx, snap)
}

func (r *realClient) UpdateSnapshotStatus(ctx context.Context, snap *v1alpha1.Snapshot) error {
	return r.client.Status().Update(ctx, snap)
}

func (r *realClient) DeleteSnapshot(ctx context.Context, name string) error {
	snap := &v1alpha1.Snapshot{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, snap); err != nil {
		return err
	}
	return r.client.Delete(ctx, snap)
}

// --- ConfigMap operations ---

func (r *realClient) GetConfigMapValue(ctx context.Context, namespace, name, key string) (string, error) {
	cm := &corev1.ConfigMap{}
	if err := r.client.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, cm); err != nil {
		return "", err
	}
	val, ok := cm.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in configmap %s/%s", key, namespace, name)
	}
	return val, nil
}

// --- Node operations ---

func (r *realClient) GetNodeZone(ctx context.Context, nodeName string) (string, error) {
	node := &corev1.Node{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: nodeName}, node); err != nil {
		return "", err
	}

	if zone, ok := node.Labels["topology.kubernetes.io/zone"]; ok {
		return zone, nil
	}
	if zone, ok := node.Labels["failure-domain.beta.kubernetes.io/zone"]; ok {
		return zone, nil
	}

	return "", fmt.Errorf("node %q has no zone label", nodeName)
}
