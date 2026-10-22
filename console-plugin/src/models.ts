import { K8sModel } from '@openshift-console/dynamic-plugin-sdk';
import { API_GROUP, API_VERSION } from './constants';

export const FileSharePoolModel: K8sModel = {
  apiGroup: API_GROUP,
  apiVersion: API_VERSION,
  kind: 'FileSharePool',
  plural: 'filesharepools',
  abbr: 'FSP',
  label: 'FileSharePool',
  labelPlural: 'FileSharePools',
  namespaced: false,
};

export const SubVolumeModel: K8sModel = {
  apiGroup: API_GROUP,
  apiVersion: API_VERSION,
  kind: 'SubVolume',
  plural: 'subvolumes',
  abbr: 'SV',
  label: 'SubVolume',
  labelPlural: 'SubVolumes',
  namespaced: false,
};

export const SnapshotModel: K8sModel = {
  apiGroup: API_GROUP,
  apiVersion: API_VERSION,
  kind: 'Snapshot',
  plural: 'snapshots',
  abbr: 'SNAP',
  label: 'Snapshot',
  labelPlural: 'Snapshots',
  namespaced: false,
};

export const VolumeGroupSnapshotModel: K8sModel = {
  apiGroup: API_GROUP,
  apiVersion: API_VERSION,
  kind: 'VolumeGroupSnapshot',
  plural: 'volumegroupsnapshots',
  abbr: 'VGS',
  label: 'VolumeGroupSnapshot',
  labelPlural: 'VolumeGroupSnapshots',
  namespaced: false,
};

export const ReplicationPolicyModel: K8sModel = {
  apiGroup: API_GROUP,
  apiVersion: API_VERSION,
  kind: 'ReplicationPolicy',
  plural: 'replicationpolicies',
  abbr: 'RP',
  label: 'ReplicationPolicy',
  labelPlural: 'ReplicationPolicies',
  namespaced: false,
};

export const BackupPolicyModel: K8sModel = {
  apiGroup: API_GROUP,
  apiVersion: API_VERSION,
  kind: 'BackupPolicy',
  plural: 'backuppolicies',
  abbr: 'BP',
  label: 'BackupPolicy',
  labelPlural: 'BackupPolicies',
  namespaced: false,
};

export const BackupRestoreModel: K8sModel = {
  apiGroup: API_GROUP,
  apiVersion: API_VERSION,
  kind: 'BackupRestore',
  plural: 'backuprestores',
  abbr: 'BR',
  label: 'BackupRestore',
  labelPlural: 'BackupRestores',
  namespaced: false,
};

// ── Standard Kubernetes kinds used by the create pages ──

export const PersistentVolumeClaimModel: K8sModel = {
  apiVersion: 'v1',
  kind: 'PersistentVolumeClaim',
  plural: 'persistentvolumeclaims',
  abbr: 'PVC',
  label: 'PersistentVolumeClaim',
  labelPlural: 'PersistentVolumeClaims',
  namespaced: true,
};

export const PersistentVolumeModel: K8sModel = {
  apiVersion: 'v1',
  kind: 'PersistentVolume',
  plural: 'persistentvolumes',
  abbr: 'PV',
  label: 'PersistentVolume',
  labelPlural: 'PersistentVolumes',
  namespaced: false,
};

export const DeploymentModel: K8sModel = {
  apiGroup: 'apps',
  apiVersion: 'v1',
  kind: 'Deployment',
  plural: 'deployments',
  abbr: 'D',
  label: 'Deployment',
  labelPlural: 'Deployments',
  namespaced: true,
};

export const JobModel: K8sModel = {
  apiGroup: 'batch',
  apiVersion: 'v1',
  kind: 'Job',
  plural: 'jobs',
  abbr: 'J',
  label: 'Job',
  labelPlural: 'Jobs',
  namespaced: true,
};

export const SecretModel: K8sModel = {
  apiVersion: 'v1',
  kind: 'Secret',
  plural: 'secrets',
  abbr: 'S',
  label: 'Secret',
  labelPlural: 'Secrets',
  namespaced: true,
};

export const PodModel: K8sModel = {
  apiVersion: 'v1',
  kind: 'Pod',
  plural: 'pods',
  abbr: 'P',
  label: 'Pod',
  labelPlural: 'Pods',
  namespaced: true,
};

export const StorageClassModel: K8sModel = {
  apiGroup: 'storage.k8s.io',
  apiVersion: 'v1',
  kind: 'StorageClass',
  plural: 'storageclasses',
  abbr: 'SC',
  label: 'StorageClass',
  labelPlural: 'StorageClasses',
  namespaced: false,
};

export const VolumeSnapshotModel: K8sModel = {
  apiGroup: 'snapshot.storage.k8s.io',
  apiVersion: 'v1',
  kind: 'VolumeSnapshot',
  plural: 'volumesnapshots',
  abbr: 'VS',
  label: 'VolumeSnapshot',
  labelPlural: 'VolumeSnapshots',
  namespaced: true,
};

export const VolumeSnapshotClassModel: K8sModel = {
  apiGroup: 'snapshot.storage.k8s.io',
  apiVersion: 'v1',
  kind: 'VolumeSnapshotClass',
  plural: 'volumesnapshotclasses',
  abbr: 'VSC',
  label: 'VolumeSnapshotClass',
  labelPlural: 'VolumeSnapshotClasses',
  namespaced: false,
};

export const KubeVolumeGroupSnapshotModel: K8sModel = {
  apiGroup: 'groupsnapshot.storage.k8s.io',
  apiVersion: 'v1beta1',
  kind: 'VolumeGroupSnapshot',
  plural: 'volumegroupsnapshots',
  abbr: 'VGS',
  label: 'VolumeGroupSnapshot',
  labelPlural: 'VolumeGroupSnapshots',
  namespaced: true,
};

export const VolumeGroupSnapshotClassModel: K8sModel = {
  apiGroup: 'groupsnapshot.storage.k8s.io',
  apiVersion: 'v1beta1',
  kind: 'VolumeGroupSnapshotClass',
  plural: 'volumegroupsnapshotclasses',
  abbr: 'VGSC',
  label: 'VolumeGroupSnapshotClass',
  labelPlural: 'VolumeGroupSnapshotClasses',
  namespaced: false,
};

export const VolumeSnapshotContentModel: K8sModel = {
  apiGroup: 'snapshot.storage.k8s.io',
  apiVersion: 'v1',
  kind: 'VolumeSnapshotContent',
  plural: 'volumesnapshotcontents',
  abbr: 'VSC',
  label: 'VolumeSnapshotContent',
  labelPlural: 'VolumeSnapshotContents',
  namespaced: false,
};

export const VirtualMachineModel: K8sModel = {
  apiGroup: 'kubevirt.io',
  apiVersion: 'v1',
  kind: 'VirtualMachine',
  plural: 'virtualmachines',
  abbr: 'VM',
  label: 'VirtualMachine',
  labelPlural: 'VirtualMachines',
  namespaced: true,
};
