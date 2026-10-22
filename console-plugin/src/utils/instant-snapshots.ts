import { K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';
import { SubVolume } from '../types';
import { VirtualMachineLike } from './k8s-objects';
import { vmUsesPVC } from './restore-helpers';

/** KubeVirt feature gate that instant VM snapshots need. */
export const SIDECAR_FEATURE_GATE = 'Sidecar';

export type InstantSnapshotState = 'on' | 'off' | 'unknown';

export interface KubeVirtLike extends K8sResourceCommon {
  spec?: { configuration?: { developerConfiguration?: { featureGates?: string[] } } };
}

/**
 * Whether VM disk snapshots are instant: on when a KubeVirt has the Sidecar feature gate. Unknown while
 * loading, when the user may not read KubeVirt, or when there is no KubeVirt.
 */
export function instantSnapshotState(
  kubevirts: KubeVirtLike[] | undefined,
  loaded: boolean,
  error: unknown,
): InstantSnapshotState {
  if (error || !loaded || !kubevirts?.length) return 'unknown';
  const on = kubevirts.some((kv) =>
    (kv.spec?.configuration?.developerConfiguration?.featureGates || []).includes(SIDECAR_FEATURE_GATE),
  );
  return on ? 'on' : 'off';
}

export interface DiskWithInstantSnapshots {
  namespace: string;
  claim: string;
  snapshots: number;
  vm?: string;
}

/** The VM disks that have instant snapshots, with the VM that uses each one when it can be found. */
export function disksWithInstantSnapshots(
  subvolumes: SubVolume[] | undefined,
  vms: VirtualMachineLike[] | undefined,
): DiskWithInstantSnapshots[] {
  const vmFor = (namespace: string, claim: string) =>
    (vms || []).find((vm) => vmUsesPVC([vm], namespace, claim))?.metadata?.name;

  return (subvolumes || [])
    .filter((sv) => (sv.status?.diskChain?.length || 0) > 1)
    .map((sv) => ({
      namespace: sv.spec.pvcNamespace,
      claim: sv.spec.pvcName,
      snapshots: (sv.status?.diskChain?.length || 1) - 1,
      vm: vmFor(sv.spec.pvcNamespace, sv.spec.pvcName),
    }))
    .sort(
      (a, b) =>
        a.namespace.localeCompare(b.namespace) ||
        (a.vm || '').localeCompare(b.vm || '') ||
        a.claim.localeCompare(b.claim),
    );
}
