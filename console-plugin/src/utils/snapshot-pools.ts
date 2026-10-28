import { BackupPolicy } from '../types';
import { CSI_DRIVER_NAME, StorageClass } from './k8s-objects';

// A snapshot pool must hold only backups: its BackupPolicy stops backing up
// while the pool holds a volume, and the driver refuses to make one there.

/** The pools that BackupPolicies use as their snapshot pool. */
export function snapshotPoolNames(policies: BackupPolicy[]): Set<string> {
  return new Set(policies.map((p) => p.spec?.snapshotPool).filter((name): name is string => !!name));
}

export function isSnapshotPool(poolName: string, policies: BackupPolicy[]): boolean {
  return snapshotPoolNames(policies).has(poolName);
}

/** The storage classes of this driver that a new volume may use: not those of a snapshot pool. */
export function volumeClasses(classes: StorageClass[], policies: BackupPolicy[]): StorageClass[] {
  const snapshotPools = snapshotPoolNames(policies);
  return classes.filter((sc) => sc.provisioner === CSI_DRIVER_NAME && !snapshotPools.has(sc.parameters?.pool || ''));
}
