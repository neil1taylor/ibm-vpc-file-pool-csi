import { K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';
import { FileSharePool } from '../types';
import { CSI_DRIVER_NAME, PersistentVolumeClaim, StorageClass, storageClassFor } from './k8s-objects';
import { LABEL_MIGRATION, migrationNames } from './migrate-objects';

/** The parts of a Pod the migration page reads. */
export interface MigrationPodLike extends K8sResourceCommon {
  spec?: { volumes?: Array<{ persistentVolumeClaim?: { claimName?: string } }> };
  status?: {
    phase?: string;
    containerStatuses?: Array<{ state?: { terminated?: { exitCode?: number } } }>;
  };
}

export interface MigrationRow {
  name: string;
  sizeGiB: number;
  phase: string;
  pods: string[];
  canMove: boolean;
  reason?: string;
}

export interface MigrationTarget {
  storageClass: string;
  pool: string;
  label: string;
  freeGB?: number;
}

export type MigrationProgress =
  | { state: 'not-started' }
  | { state: 'binding' }
  | { state: 'starting' }
  | { state: 'copying'; podPhase: string }
  | { state: 'done' }
  | { state: 'failed'; exitCode?: number };

const BINARY: Record<string, number> = { Ki: 2 ** 10, Mi: 2 ** 20, Gi: 2 ** 30, Ti: 2 ** 40, Pi: 2 ** 50 };
const DECIMAL: Record<string, number> = { k: 1e3, K: 1e3, M: 1e6, G: 1e9, T: 1e12, P: 1e15 };

/** A Kubernetes size ("200Gi", "100G", plain bytes) in whole GiB, rounded up. 0 when it cannot be read. */
export function parseGiB(quantity: string): number {
  const match = /^(\d+(?:\.\d+)?)([A-Za-z]{0,2})$/.exec(quantity.trim());
  if (!match) return 0;
  const unit = match[2];
  const factor = unit === '' ? 1 : BINARY[unit] ?? DECIMAL[unit];
  if (!factor) return 0;
  return Math.ceil((Number(match[1]) * factor) / 2 ** 30);
}

/** Names of the running or pending pods in the namespace that mount the claim. Finished pods and migration pods do not count. */
export function podsUsingPVC(pods: MigrationPodLike[], namespace: string, claim: string): string[] {
  return pods
    .filter(
      (p) =>
        p.metadata?.namespace === namespace &&
        p.metadata?.labels?.[LABEL_MIGRATION] !== 'true' &&
        p.status?.phase !== 'Succeeded' &&
        p.status?.phase !== 'Failed' &&
        (p.spec?.volumes || []).some((v) => v.persistentVolumeClaim?.claimName === claim),
    )
    .map((p) => p.metadata?.name || '');
}

/** Storage classes that claims use and that are not classes of this driver. */
export function sourceClasses(pvcs: PersistentVolumeClaim[], storageClasses: StorageClass[]): string[] {
  const ours = new Set(storageClasses.filter((sc) => sc.provisioner === CSI_DRIVER_NAME).map((sc) => sc.metadata?.name));
  const used = new Set(pvcs.map((p) => p.spec?.storageClassName).filter((n): n is string => !!n && !ours.has(n)));
  return [...used].sort();
}

/** The claims of a namespace and storage class, each with whether it can move now, and if not, why. */
export function migrationRows(input: {
  pvcs: PersistentVolumeClaim[];
  pods: MigrationPodLike[];
  namespace: string;
  sourceClass: string;
}): MigrationRow[] {
  const inNamespace = input.pvcs.filter((p) => p.metadata?.namespace === input.namespace);
  const taken = new Set(inNamespace.map((p) => p.metadata?.name));
  return inNamespace
    .filter((p) => p.spec?.storageClassName === input.sourceClass)
    .map((p) => {
      const name = p.metadata?.name || '';
      const phase = p.status?.phase || 'Unknown';
      const pods = podsUsingPVC(input.pods, input.namespace, name);
      const target = migrationNames(name).targetPVC;
      let reason: string | undefined;
      if (phase !== 'Bound') {
        reason = `The claim is not Bound (phase: ${phase}).`;
      } else if (pods.length > 0) {
        reason = `In use by: ${pods.join(', ')}. Stop the application first.`;
      } else if (taken.has(target)) {
        reason = `A claim named ${target} already exists.`;
      }
      return {
        name,
        sizeGiB: parseGiB(p.spec?.resources?.requests?.storage || ''),
        phase,
        pods,
        canMove: !reason,
        reason,
      };
    });
}

/** The pools (and tiers) a claim can move into: those with a storage class. The free space is the pool's. */
export function migrationTargets(pools: FileSharePool[], storageClasses: StorageClass[]): MigrationTarget[] {
  const classes = new Set(storageClasses.filter((sc) => sc.provisioner === CSI_DRIVER_NAME).map((sc) => sc.metadata?.name));
  const targets: MigrationTarget[] = [];
  for (const pool of pools) {
    const name = pool.metadata?.name || '';
    const freeGB = pool.status ? pool.status.totalCapacityGB - pool.status.totalAllocatedGB : undefined;
    const tiers = pool.spec?.tiers || [];
    if (tiers.length === 0) {
      if (classes.has(storageClassFor(name))) targets.push({ storageClass: name, pool: name, label: name, freeGB });
      continue;
    }
    for (const tier of tiers) {
      const storageClass = storageClassFor(name, tier.name);
      if (classes.has(storageClass)) targets.push({ storageClass, pool: name, label: `${name} / ${tier.name}`, freeGB });
    }
  }
  return targets;
}

/** What a row shows, from the new claim and the migration pod. */
export function migrationProgress(target: PersistentVolumeClaim | undefined, pod: MigrationPodLike | undefined): MigrationProgress {
  if (!target) return { state: 'not-started' };
  if (!pod) return target.status?.phase === 'Bound' ? { state: 'starting' } : { state: 'binding' };
  switch (pod.status?.phase) {
    case 'Succeeded':
      return { state: 'done' };
    case 'Failed':
      return { state: 'failed', exitCode: pod.status.containerStatuses?.[0]?.state?.terminated?.exitCode };
    default:
      return { state: 'copying', podPhase: pod.status?.phase || 'Pending' };
  }
}
