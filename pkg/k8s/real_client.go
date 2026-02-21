package k8s

import (
	"context"
	"fmt"

	v1alpha1 "github.com/IBM/ibm-vpc-file-pool-csi/api/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
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

func (r *realClient) ListFileSharePools(ctx context.Context) ([]v1alpha1.FileSharePool, error) {
	list := &v1alpha1.FileSharePoolList{}
	if err := r.client.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
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

func (r *realClient) ListCloneSubVolumes(ctx context.Context) ([]v1alpha1.SubVolume, error) {
	list := &v1alpha1.SubVolumeList{}
	if err := r.client.List(ctx, list, client.HasLabels{
		"storage.ibmcloud.io/clone-source",
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

// --- VolumeGroupSnapshot operations ---

func (r *realClient) GetVolumeGroupSnapshot(ctx context.Context, name string) (*v1alpha1.VolumeGroupSnapshot, error) {
	vgs := &v1alpha1.VolumeGroupSnapshot{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, vgs); err != nil {
		return nil, err
	}
	return vgs, nil
}

func (r *realClient) CreateVolumeGroupSnapshot(ctx context.Context, vgs *v1alpha1.VolumeGroupSnapshot) error {
	return r.client.Create(ctx, vgs)
}

func (r *realClient) UpdateVolumeGroupSnapshotStatus(ctx context.Context, vgs *v1alpha1.VolumeGroupSnapshot) error {
	return r.client.Status().Update(ctx, vgs)
}

func (r *realClient) DeleteVolumeGroupSnapshot(ctx context.Context, name string) error {
	vgs := &v1alpha1.VolumeGroupSnapshot{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, vgs); err != nil {
		return err
	}
	return r.client.Delete(ctx, vgs)
}

func (r *realClient) ListVolumeGroupSnapshots(ctx context.Context, poolName string) ([]v1alpha1.VolumeGroupSnapshot, error) {
	list := &v1alpha1.VolumeGroupSnapshotList{}
	if err := r.client.List(ctx, list, client.MatchingLabels{
		"storage.ibmcloud.io/pool": poolName,
	}); err != nil {
		return nil, err
	}
	return list.Items, nil
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

// --- ReplicationPolicy operations ---

func (r *realClient) GetReplicationPolicy(ctx context.Context, name string) (*v1alpha1.ReplicationPolicy, error) {
	rp := &v1alpha1.ReplicationPolicy{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, rp); err != nil {
		return nil, err
	}
	return rp, nil
}

func (r *realClient) ListReplicationPolicies(ctx context.Context) ([]v1alpha1.ReplicationPolicy, error) {
	list := &v1alpha1.ReplicationPolicyList{}
	if err := r.client.List(ctx, list); err != nil {
		return nil, err
	}
	return list.Items, nil
}

func (r *realClient) CreateReplicationPolicy(ctx context.Context, rp *v1alpha1.ReplicationPolicy) error {
	return r.client.Create(ctx, rp)
}

func (r *realClient) UpdateReplicationPolicyStatus(ctx context.Context, rp *v1alpha1.ReplicationPolicy) error {
	return r.client.Status().Update(ctx, rp)
}

func (r *realClient) DeleteReplicationPolicy(ctx context.Context, name string) error {
	rp := &v1alpha1.ReplicationPolicy{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, rp); err != nil {
		return err
	}
	return r.client.Delete(ctx, rp)
}

// --- StorageClass operations ---

func (r *realClient) GetStorageClass(ctx context.Context, name string) (*storagev1.StorageClass, error) {
	sc := &storagev1.StorageClass{}
	if err := r.client.Get(ctx, types.NamespacedName{Name: name}, sc); err != nil {
		return nil, err
	}
	return sc, nil
}

func (r *realClient) CreateStorageClass(ctx context.Context, sc *storagev1.StorageClass) error {
	return r.client.Create(ctx, sc)
}
