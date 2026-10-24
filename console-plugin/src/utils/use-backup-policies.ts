import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { BackupPolicyModel } from '../models';
import { BackupPolicy } from '../types';

/** All BackupPolicies, or none while they load or when the user may not list them. */
export function useBackupPolicies(): BackupPolicy[] {
  const [policies] = useK8sWatchResource<BackupPolicy[]>({
    groupVersionKind: { group: BackupPolicyModel.apiGroup, version: BackupPolicyModel.apiVersion, kind: BackupPolicyModel.kind },
    isList: true,
  });
  return policies || [];
}
