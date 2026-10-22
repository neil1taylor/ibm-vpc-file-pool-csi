import { K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';

export const CSI_DRIVER_NAME = 'vpc-file-pool.csi.ibm.io';

export interface PersistentVolumeClaim extends K8sResourceCommon {
  spec: {
    accessModes: string[];
    storageClassName?: string;
    volumeMode?: string;
    volumeName?: string;
    resources: { requests: { storage: string } };
    dataSource?: { apiGroup?: string; kind: string; name: string };
  };
  status?: { phase?: string };
}

export interface StorageClass extends K8sResourceCommon {
  provisioner: string;
}

/** A VolumeSnapshotClass or VolumeGroupSnapshotClass. */
export interface DriverClass extends K8sResourceCommon {
  driver: string;
}

export interface VolumeSnapshot extends K8sResourceCommon {
  spec: {
    volumeSnapshotClassName: string;
    source: { persistentVolumeClaimName: string };
  };
}

/** The cluster-scoped object that ties a VolumeSnapshot to the driver's snapshot. */
export interface VolumeSnapshotContent extends K8sResourceCommon {
  spec: { volumeSnapshotRef: { name: string; namespace: string } };
  status?: { snapshotHandle?: string };
}

/** The parts of a KubeVirt VirtualMachine the console reads. */
export interface VirtualMachineLike extends K8sResourceCommon {
  spec?: {
    template?: {
      spec?: {
        volumes?: Array<{
          persistentVolumeClaim?: { claimName?: string };
          dataVolume?: { name?: string };
        }>;
      };
    };
  };
}

export interface KubeVolumeGroupSnapshot extends K8sResourceCommon {
  spec: {
    volumeGroupSnapshotClassName: string;
    source: { selector: { matchLabels: Record<string, string> } };
  };
}

/** The driver creates one StorageClass per pool, or one per tier for a tiered pool. */
export function storageClassFor(poolName: string, tier?: string): string {
  return tier ? `${poolName}-${tier}` : poolName;
}

export function buildPVC(input: {
  namespace: string;
  name: string;
  poolName: string;
  tier?: string;
  sizeGiB: number;
}): PersistentVolumeClaim {
  return {
    apiVersion: 'v1',
    kind: 'PersistentVolumeClaim',
    metadata: { name: input.name, namespace: input.namespace },
    spec: {
      accessModes: ['ReadWriteMany'],
      storageClassName: storageClassFor(input.poolName, input.tier),
      resources: { requests: { storage: `${input.sizeGiB}Gi` } },
    },
  };
}

export function buildVolumeSnapshot(input: {
  namespace: string;
  name: string;
  pvcName: string;
  className: string;
}): VolumeSnapshot {
  return {
    apiVersion: 'snapshot.storage.k8s.io/v1',
    kind: 'VolumeSnapshot',
    metadata: { name: input.name, namespace: input.namespace },
    spec: {
      volumeSnapshotClassName: input.className,
      source: { persistentVolumeClaimName: input.pvcName },
    },
  };
}

export function buildVolumeGroupSnapshot(input: {
  namespace: string;
  name: string;
  matchLabels: Record<string, string>;
  className: string;
}): KubeVolumeGroupSnapshot {
  return {
    apiVersion: 'groupsnapshot.storage.k8s.io/v1beta1',
    kind: 'VolumeGroupSnapshot',
    metadata: { name: input.name, namespace: input.namespace },
    spec: {
      volumeGroupSnapshotClassName: input.className,
      source: { selector: { matchLabels: input.matchLabels } },
    },
  };
}

/** Parses one key=value pair per line. Lines without a key or a "=" are ignored. */
export function parseMatchLabels(input: string): Record<string, string> {
  const labels: Record<string, string> = {};
  for (const line of input.split('\n')) {
    const index = line.indexOf('=');
    if (index < 0) continue;
    const key = line.slice(0, index).trim();
    if (key) labels[key] = line.slice(index + 1).trim();
  }
  return labels;
}

export function findClassByDriver(classes: DriverClass[]): DriverClass | undefined {
  return classes.find((c) => c.driver === CSI_DRIVER_NAME);
}

/** Claims whose storage class is provisioned by this driver. */
export function poolPVCs(
  pvcs: PersistentVolumeClaim[],
  storageClasses: StorageClass[],
): PersistentVolumeClaim[] {
  const ours = new Set(
    storageClasses.filter((sc) => sc.provisioner === CSI_DRIVER_NAME).map((sc) => sc.metadata?.name),
  );
  return pvcs.filter((p) => !!p.spec?.storageClassName && ours.has(p.spec.storageClassName));
}

export function pvcPath(namespace: string, name: string): string {
  return `/k8s/ns/${namespace}/persistentvolumeclaims/${name}`;
}

export function volumeSnapshotPath(namespace: string, name: string): string {
  return `/k8s/ns/${namespace}/snapshot.storage.k8s.io~v1~VolumeSnapshot/${name}`;
}

export function volumeGroupSnapshotPath(namespace: string, name: string): string {
  return `/k8s/ns/${namespace}/groupsnapshot.storage.k8s.io~v1beta1~VolumeGroupSnapshot/${name}`;
}

/** True when an SDK error means the API or resource does not exist. */
export function isNotFoundError(err: unknown): boolean {
  if (typeof err !== 'object' || err === null) return false;
  const e = err as { code?: number; response?: { status?: number }; json?: { code?: number } };
  return e.code === 404 || e.response?.status === 404 || e.json?.code === 404;
}

export function isAlreadyExistsError(err: unknown): boolean {
  if (typeof err !== 'object' || err === null) return false;
  const e = err as { code?: number; response?: { status?: number }; json?: { code?: number } };
  return e.code === 409 || e.response?.status === 409 || e.json?.code === 409;
}

/** DNS-1123 label: lower-case letters, digits and dashes, at most 63 characters. */
export function isValidK8sName(name: string): boolean {
  return name.length <= 63 && /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(name);
}

export function errorMessage(err: unknown, fallback: string): string {
  if (err instanceof Error) return err.message;
  if (typeof err === 'object' && err !== null && 'message' in err) {
    return String((err as { message: unknown }).message);
  }
  return fallback;
}
