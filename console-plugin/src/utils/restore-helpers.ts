import { Snapshot, SubVolume } from '../types';
import {
  CSI_DRIVER_NAME,
  PersistentVolumeClaim,
  StorageClass,
  VirtualMachineLike,
  VolumeSnapshotContent,
  isValidK8sName,
  storageClassFor,
} from './k8s-objects';

export interface VolumeSnapshotRef {
  name: string;
  namespace: string;
}

export interface RestoreForm {
  name: string;
  sizeGiB: number;
  storageClass: string;
  accessMode: string;
  volumeMode: string;
}

export interface RestoreErrors {
  name?: string;
  size?: string;
}

export type RestoreProgress =
  | { state: 'waiting' }
  | { state: 'copying'; percent: number; copiedBytes: number; totalBytes: number }
  | { state: 'ready' }
  | { state: 'failed'; error: string };

/**
 * Finds the VolumeSnapshot that points to a driver Snapshot. The content's
 * snapshotHandle is "<pool>/<share ID>/<Snapshot name>".
 */
export function findVolumeSnapshotRef(
  snapshot: Snapshot,
  contents: VolumeSnapshotContent[],
): VolumeSnapshotRef | undefined {
  for (const c of contents) {
    const parts = c.status?.snapshotHandle?.split('/');
    if (parts?.length === 3 && parts[0] === snapshot.spec?.poolName && parts[2] === snapshot.metadata?.name) {
      return { name: c.spec.volumeSnapshotRef.name, namespace: c.spec.volumeSnapshotRef.namespace };
    }
  }
  return undefined;
}

/** Why the restore action is off, or undefined when it is on. */
export function restoreDisabledReason(snapshot: Snapshot, ref: VolumeSnapshotRef | undefined): string | undefined {
  if (!snapshot.status?.readyToUse) {
    return 'The snapshot is not ready to use yet.';
  }
  if (!ref) {
    return 'No VolumeSnapshot points to this snapshot (it was made directly as a Snapshot object). Create a VolumeSnapshot first.';
  }
  return undefined;
}

export function restoreDefaults(input: {
  snapshot: Snapshot;
  sourcePVC: PersistentVolumeClaim | undefined;
  sourcePVCName: string;
  storageClasses: StorageClass[];
}): RestoreForm {
  const { snapshot, sourcePVC, sourcePVCName, storageClasses } = input;
  const ours = storageClasses.filter((sc) => sc.provisioner === CSI_DRIVER_NAME).map((sc) => sc.metadata?.name || '');
  const poolClass = storageClassFor(snapshot.spec?.poolName || '');
  return {
    name: `${sourcePVCName}-restored`,
    sizeGiB: Math.max(1, Math.ceil(snapshot.spec?.sizeGB || 0)),
    storageClass: sourcePVC?.spec?.storageClassName || (ours.includes(poolClass) ? poolClass : ours[0] || poolClass),
    accessMode: sourcePVC?.spec?.accessModes?.[0] || 'ReadWriteMany',
    volumeMode: sourcePVC?.spec?.volumeMode || 'Filesystem',
  };
}

export function validateRestore(
  form: RestoreForm,
  context: { existingNames: string[]; minGiB: number; minLabel?: string },
): RestoreErrors {
  const errors: RestoreErrors = {};
  if (!isValidK8sName(form.name)) {
    errors.name = 'Use lower-case letters, digits and dashes.';
  } else if (context.existingNames.includes(form.name)) {
    errors.name = 'A claim with this name already exists in the namespace.';
  }
  if (!Number.isInteger(form.sizeGiB)) {
    errors.size = 'The size must be a whole number of GiB.';
  } else if (form.sizeGiB < context.minGiB) {
    errors.size = `The size must be at least ${context.minGiB} GiB (${context.minLabel || "the snapshot's"} size).`;
  }
  return errors;
}

export function buildRestorePVC(form: RestoreForm, ref: VolumeSnapshotRef): PersistentVolumeClaim {
  return {
    apiVersion: 'v1',
    kind: 'PersistentVolumeClaim',
    metadata: { name: form.name, namespace: ref.namespace },
    spec: {
      accessModes: [form.accessMode],
      storageClassName: form.storageClass,
      volumeMode: form.volumeMode,
      resources: { requests: { storage: `${form.sizeGiB}Gi` } },
      dataSource: { apiGroup: 'snapshot.storage.k8s.io', kind: 'VolumeSnapshot', name: ref.name },
    },
  };
}

/** Why the clone action is off, or undefined when it is on. */
export function cloneDisabledReason(subvolume: SubVolume): string | undefined {
  if (!subvolume.spec?.pvcName) {
    return 'This volume has no claim to clone.';
  }
  if (subvolume.status?.phase !== 'Bound') {
    return `The volume is not Bound yet (phase: ${subvolume.status?.phase || 'unknown'}).`;
  }
  if (subvolume.status?.cloneStatus === 'InProgress') {
    return 'A copy into this volume is still running.';
  }
  return undefined;
}

export function cloneDefaults(input: {
  subvolume: SubVolume;
  sourcePVC: PersistentVolumeClaim | undefined;
  storageClasses: StorageClass[];
}): RestoreForm {
  const { subvolume, sourcePVC, storageClasses } = input;
  const ours = storageClasses.filter((sc) => sc.provisioner === CSI_DRIVER_NAME).map((sc) => sc.metadata?.name || '');
  const poolClass = storageClassFor(subvolume.spec.poolName);
  return {
    name: `${subvolume.spec.pvcName}-clone`,
    sizeGiB: Math.max(1, Math.ceil(subvolume.spec.requestedGB || 0)),
    storageClass: sourcePVC?.spec?.storageClassName || (ours.includes(poolClass) ? poolClass : ours[0] || poolClass),
    accessMode: sourcePVC?.spec?.accessModes?.[0] || 'ReadWriteMany',
    volumeMode: sourcePVC?.spec?.volumeMode || 'Filesystem',
  };
}

/** A claim whose data source is another claim: the driver copies the source volume into it. */
export function buildClonePVC(form: RestoreForm, source: { name: string; namespace: string }): PersistentVolumeClaim {
  return {
    apiVersion: 'v1',
    kind: 'PersistentVolumeClaim',
    metadata: { name: form.name, namespace: source.namespace },
    spec: {
      accessModes: [form.accessMode],
      storageClassName: form.storageClass,
      volumeMode: form.volumeMode,
      resources: { requests: { storage: `${form.sizeGiB}Gi` } },
      dataSource: { kind: 'PersistentVolumeClaim', name: source.name },
    },
  };
}

/** True when a VM in the namespace mounts the claim, directly or through a DataVolume of the same name. */
export function vmUsesPVC(vms: VirtualMachineLike[], namespace: string, pvcName: string): boolean {
  return vms.some(
    (vm) =>
      vm.metadata?.namespace === namespace &&
      (vm.spec?.template?.spec?.volumes || []).some(
        (v) => v.persistentVolumeClaim?.claimName === pvcName || v.dataVolume?.name === pvcName,
      ),
  );
}

/** What the restore banner shows, from the new claim and its SubVolume. */
export function restoreProgress(
  pvc: PersistentVolumeClaim | undefined,
  subvolume: SubVolume | undefined,
  subvolumeLoaded = true,
): RestoreProgress {
  if (!pvc) return { state: 'waiting' };
  // Until the SubVolume is read, its copy state is not known, so the claim being Bound says nothing.
  if (pvc.spec?.volumeName && !subvolumeLoaded) return { state: 'waiting' };
  const clone = subvolume?.status;
  // A clone's claim is Bound at once, while its copy still runs; only the
  // SubVolume says when the copy has ended.
  const copyEnded = !clone?.cloneStatus || clone.cloneStatus === 'Complete';
  if (pvc.status?.phase === 'Bound' && copyEnded) return { state: 'ready' };
  if (!clone?.cloneStatus) return { state: 'waiting' };
  if (clone.cloneStatus === 'Failed') {
    return { state: 'failed', error: clone.cloneProgress?.error || 'The copy failed.' };
  }
  const copied = clone.cloneProgress?.bytesCopied || 0;
  const total = clone.cloneProgress?.totalBytes || 0;
  return {
    state: 'copying',
    percent: total > 0 ? Math.min(100, Math.floor((copied / total) * 100)) : 0,
    copiedBytes: copied,
    totalBytes: total,
  };
}
