import {
  BackupPolicyModel,
  BackupRestoreModel,
  KubeVolumeGroupSnapshotModel,
  PersistentVolumeClaimModel,
  StorageClassModel,
  VolumeGroupSnapshotClassModel,
  VolumeSnapshotClassModel,
  VolumeSnapshotModel,
} from './models';

describe('Kubernetes models', () => {
  it('describe the core and snapshot APIs the create pages use', () => {
    expect(PersistentVolumeClaimModel).toMatchObject({ apiVersion: 'v1', plural: 'persistentvolumeclaims', namespaced: true });
    expect(StorageClassModel).toMatchObject({ apiGroup: 'storage.k8s.io', plural: 'storageclasses', namespaced: false });
    expect(VolumeSnapshotModel).toMatchObject({ apiGroup: 'snapshot.storage.k8s.io', apiVersion: 'v1', namespaced: true });
    expect(VolumeSnapshotClassModel).toMatchObject({ apiGroup: 'snapshot.storage.k8s.io', namespaced: false });
    expect(KubeVolumeGroupSnapshotModel).toMatchObject({
      apiGroup: 'groupsnapshot.storage.k8s.io',
      apiVersion: 'v1beta1',
      namespaced: true,
    });
    expect(VolumeGroupSnapshotClassModel).toMatchObject({ apiGroup: 'groupsnapshot.storage.k8s.io', namespaced: false });
  });
});

describe('Backup models', () => {
  it('describe the cluster-scoped backup resources of the driver', () => {
    expect(BackupPolicyModel).toMatchObject({
      apiGroup: 'storage.ibmcloud.io',
      apiVersion: 'v1alpha1',
      kind: 'BackupPolicy',
      plural: 'backuppolicies',
      namespaced: false,
    });
    expect(BackupRestoreModel).toMatchObject({
      apiGroup: 'storage.ibmcloud.io',
      apiVersion: 'v1alpha1',
      kind: 'BackupRestore',
      plural: 'backuprestores',
      namespaced: false,
    });
  });
});
