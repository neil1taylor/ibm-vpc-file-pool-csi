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
