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

/** The ConfigMap the Helm chart renders; the name is fixed in the chart and the namespace is the driver's. */
export const SETTINGS_CONFIGMAP = { name: 'ibm-vpc-file-pool-csi-settings', namespace: 'kube-system' };

export interface SettingsConfigMapLike extends K8sResourceCommon {
  data?: Record<string, string>;
}

/** The driver's vmSnapshots.method ("auto" or "copy"), or undefined when the chart does not publish it. */
export function vmSnapshotMethod(settings: SettingsConfigMapLike | undefined): string | undefined {
  return settings?.data?.vmSnapshotMethod || undefined;
}

export type SnapshotMode =
  | 'instant' // method auto and the gate is on
  | 'copy-setting' // the driver is set to copy
  | 'copy-gate' // method auto, but the gate is off
  | 'unknown' // method auto, and the gate cannot be read
  | 'unknown-setting'; // the driver does not report its setting, or reports one the console does not know

/**
 * How the driver takes VM disk snapshots, from its own setting and the KubeVirt gate. It says "instant" only when
 * both allow it, and never guesses when the setting is missing.
 */
export function snapshotMode(method: string | undefined, gate: InstantSnapshotState): SnapshotMode {
  if (method === 'copy') return 'copy-setting';
  if (method !== 'auto') return 'unknown-setting';
  if (gate === 'unknown') return 'unknown';
  return gate === 'on' ? 'instant' : 'copy-gate';
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
