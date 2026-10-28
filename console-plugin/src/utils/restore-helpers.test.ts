import { Snapshot, SubVolume } from '../types';
import {
  PersistentVolumeClaim,
  StorageClass,
  VolumeSnapshotContent,
  VirtualMachineLike,
} from './k8s-objects';
import {
  buildClonePVC,
  buildRestorePVC,
  cloneDefaults,
  cloneDisabledReason,
  findVolumeSnapshotRef,
  restoreDefaults,
  restoreDisabledReason,
  restoreProgress,
  validateRestore,
  vmUsesPVC,
} from './restore-helpers';

const DRIVER = 'vpc-file-pool.csi.ibm.io';

const snapshot = (over: Partial<Snapshot> = {}): Snapshot =>
  ({
    metadata: { name: 'snapshot-abc' },
    spec: { poolName: 'pool-a', sizeGB: 10, sourceSubVolume: 'pvc-1' },
    status: { readyToUse: true, phase: 'Ready' },
    ...over,
  }) as Snapshot;

const content = (handle: string | undefined, ref = { name: 'daily', namespace: 'apps' }): VolumeSnapshotContent => ({
  metadata: { name: 'snapcontent-1' },
  spec: { volumeSnapshotRef: ref },
  status: handle === undefined ? undefined : { snapshotHandle: handle },
});

describe('findVolumeSnapshotRef', () => {
  it('finds the VolumeSnapshot whose content handle ends in the Snapshot name, in the same pool', () => {
    const ref = findVolumeSnapshotRef(snapshot(), [content('pool-a/share-9/snapshot-abc')]);
    expect(ref).toEqual({ name: 'daily', namespace: 'apps' });
  });

  it('ignores a handle for another pool or another Snapshot', () => {
    expect(findVolumeSnapshotRef(snapshot(), [content('pool-b/share-9/snapshot-abc')])).toBeUndefined();
    expect(findVolumeSnapshotRef(snapshot(), [content('pool-a/share-9/snapshot-other')])).toBeUndefined();
  });

  it('ignores a content with no handle or a handle that is not pool/share/name', () => {
    expect(findVolumeSnapshotRef(snapshot(), [content(undefined)])).toBeUndefined();
    expect(findVolumeSnapshotRef(snapshot(), [content('snapshot-abc')])).toBeUndefined();
  });
});

describe('restoreDisabledReason', () => {
  const ref = { name: 'daily', namespace: 'apps' };

  it('allows a ready snapshot that has a VolumeSnapshot', () => {
    expect(restoreDisabledReason(snapshot(), ref)).toBeUndefined();
  });

  it('says why when the snapshot is not ready', () => {
    const s = snapshot({ status: { readyToUse: false, phase: 'Creating' } });
    expect(restoreDisabledReason(s, ref)).toMatch(/not ready/i);
  });

  it('says to create a VolumeSnapshot when none points to the snapshot', () => {
    expect(restoreDisabledReason(snapshot(), undefined)).toMatch(/create a VolumeSnapshot first/i);
  });
});

describe('restoreDefaults', () => {
  const classes: StorageClass[] = [
    { metadata: { name: 'pool-a' }, provisioner: DRIVER },
    { metadata: { name: 'pool-b' }, provisioner: DRIVER },
    { metadata: { name: 'gp' }, provisioner: 'other' },
  ];
  const source: PersistentVolumeClaim = {
    metadata: { name: 'data', namespace: 'apps' },
    spec: {
      accessModes: ['ReadWriteOnce'],
      storageClassName: 'pool-b',
      volumeMode: 'Block',
      resources: { requests: { storage: '10Gi' } },
    },
  };

  it('copies the source claim when it still exists', () => {
    expect(restoreDefaults({ snapshot: snapshot(), sourcePVC: source, sourcePVCName: 'data', storageClasses: classes })).toEqual({
      name: 'data-restored',
      sizeGiB: 10,
      storageClass: 'pool-b',
      accessMode: 'ReadWriteOnce',
      volumeMode: 'Block',
    });
  });

  it('falls back to the pool class, ReadWriteMany and Filesystem when the source claim is gone', () => {
    expect(restoreDefaults({ snapshot: snapshot(), sourcePVC: undefined, sourcePVCName: 'data', storageClasses: classes })).toEqual({
      name: 'data-restored',
      sizeGiB: 10,
      storageClass: 'pool-a',
      accessMode: 'ReadWriteMany',
      volumeMode: 'Filesystem',
    });
  });

  it('rounds the size up to a whole GiB, with a minimum of 1', () => {
    const small = snapshot({ spec: { ...snapshot().spec, sizeGB: 0.2 } });
    expect(restoreDefaults({ snapshot: small, sourcePVC: undefined, sourcePVCName: 'data', storageClasses: classes }).sizeGiB).toBe(1);
  });
});

describe('validateRestore', () => {
  const form = { name: 'data-restored', sizeGiB: 10, storageClass: 'pool-a', accessMode: 'ReadWriteMany', volumeMode: 'Filesystem' };

  it('accepts a good form', () => {
    expect(validateRestore(form, { existingNames: ['data'], minGiB: 10 })).toEqual({});
  });

  it('refuses a bad name and a name that is taken', () => {
    expect(validateRestore({ ...form, name: 'Bad_Name' }, { existingNames: [], minGiB: 10 }).name).toBeTruthy();
    expect(validateRestore({ ...form, name: 'data' }, { existingNames: ['data'], minGiB: 10 }).name).toMatch(/already exists/);
  });

  it('refuses a size under the snapshot size, and a size that is not a whole number', () => {
    expect(validateRestore({ ...form, sizeGiB: 9 }, { existingNames: [], minGiB: 10 }).size).toMatch(/at least 10 GiB/);
    expect(validateRestore({ ...form, sizeGiB: 10.5 }, { existingNames: [], minGiB: 10 }).size).toBeTruthy();
  });

  it('allows a bigger size', () => {
    expect(validateRestore({ ...form, sizeGiB: 50 }, { existingNames: [], minGiB: 10 })).toEqual({});
  });
});

describe('buildRestorePVC', () => {
  it('makes a claim in the snapshot namespace with the VolumeSnapshot as its data source', () => {
    const pvc = buildRestorePVC(
      { name: 'data-restored', sizeGiB: 20, storageClass: 'pool-a', accessMode: 'ReadWriteMany', volumeMode: 'Filesystem' },
      { name: 'daily', namespace: 'apps' },
    );
    expect(pvc).toEqual({
      apiVersion: 'v1',
      kind: 'PersistentVolumeClaim',
      metadata: { name: 'data-restored', namespace: 'apps' },
      spec: {
        accessModes: ['ReadWriteMany'],
        storageClassName: 'pool-a',
        volumeMode: 'Filesystem',
        resources: { requests: { storage: '20Gi' } },
        dataSource: { apiGroup: 'snapshot.storage.k8s.io', kind: 'VolumeSnapshot', name: 'daily' },
      },
    });
  });
});

describe('vmUsesPVC', () => {
  const vm = (
    volumes: Array<{ persistentVolumeClaim?: { claimName?: string }; dataVolume?: { name?: string } }>,
    namespace = 'apps',
  ): VirtualMachineLike => ({
    metadata: { name: 'vm1', namespace },
    spec: { template: { spec: { volumes } } },
  });

  it('is true when a VM in the namespace mounts the claim directly or through a DataVolume of the same name', () => {
    expect(vmUsesPVC([vm([{ persistentVolumeClaim: { claimName: 'data' } }])], 'apps', 'data')).toBe(true);
    expect(vmUsesPVC([vm([{ dataVolume: { name: 'data' } }])], 'apps', 'data')).toBe(true);
  });

  it('is false for another claim, another namespace, or no VMs', () => {
    expect(vmUsesPVC([vm([{ persistentVolumeClaim: { claimName: 'other' } }])], 'apps', 'data')).toBe(false);
    expect(vmUsesPVC([vm([{ persistentVolumeClaim: { claimName: 'data' } }], 'elsewhere')], 'apps', 'data')).toBe(false);
    expect(vmUsesPVC([], 'apps', 'data')).toBe(false);
  });
});

describe('restoreProgress', () => {
  const pvc = (phase: string, volumeName?: string): PersistentVolumeClaim => ({
    metadata: { name: 'data-restored', namespace: 'apps' },
    spec: { accessModes: [], resources: { requests: { storage: '10Gi' } }, volumeName },
    status: { phase },
  });
  const sub = (status: SubVolume['status']): SubVolume => ({ metadata: { name: 'pvc-x' }, spec: {} as SubVolume['spec'], status });

  it('is waiting while the claim does not exist yet', () => {
    expect(restoreProgress(undefined, undefined)).toEqual({ state: 'waiting' });
  });

  it('is ready when the claim is Bound', () => {
    expect(restoreProgress(pvc('Bound', 'pvc-x'), sub({ cloneStatus: 'Complete' }))).toEqual({ state: 'ready' });
  });

  it('is waiting when the claim is Bound but the SubVolume has not been read yet', () => {
    expect(restoreProgress(pvc('Bound', 'pvc-x'), undefined, false)).toEqual({ state: 'waiting' });
  });

  it('shows the percent copied while the claim is Pending and the copy runs', () => {
    const p = restoreProgress(pvc('Pending', 'pvc-x'), sub({ cloneStatus: 'InProgress', cloneProgress: { bytesCopied: 5, totalBytes: 20 } }));
    expect(p).toEqual({ state: 'copying', percent: 25, copiedBytes: 5, totalBytes: 20 });
  });

  it('shows the clone error when the copy failed', () => {
    const p = restoreProgress(pvc('Pending', 'pvc-x'), sub({ cloneStatus: 'Failed', cloneProgress: { bytesCopied: 0, totalBytes: 20, error: 'rsync failed' } }));
    expect(p).toEqual({ state: 'failed', error: 'rsync failed' });
  });

  it('is waiting while the claim is Pending and no SubVolume exists yet', () => {
    expect(restoreProgress(pvc('Pending'), undefined)).toEqual({ state: 'waiting' });
  });

  it('shows the copy, not ready, while a clone claim is Bound and the copy runs', () => {
    const p = restoreProgress(pvc('Bound', 'pvc-x'), sub({ cloneStatus: 'InProgress', cloneProgress: { bytesCopied: 10, totalBytes: 20 } }));
    expect(p).toEqual({ state: 'copying', percent: 50, copiedBytes: 10, totalBytes: 20 });
  });

  it('shows the copy, not ready, while a clone claim is Bound and the copy has not started', () => {
    expect(restoreProgress(pvc('Bound', 'pvc-x'), sub({ cloneStatus: 'Pending' }))).toEqual({
      state: 'copying',
      percent: 0,
      copiedBytes: 0,
      totalBytes: 0,
    });
  });

  it('shows the clone error when a Bound clone claim failed its copy', () => {
    const p = restoreProgress(pvc('Bound', 'pvc-x'), sub({ cloneStatus: 'Failed', cloneProgress: { bytesCopied: 0, totalBytes: 20, error: 'cp failed' } }));
    expect(p).toEqual({ state: 'failed', error: 'cp failed' });
  });
});

describe('clone helpers', () => {
  const classes: StorageClass[] = [
    { metadata: { name: 'pool-a' }, provisioner: DRIVER },
    { metadata: { name: 'pool-b' }, provisioner: DRIVER },
  ];
  const subvolume = (over: Partial<SubVolume> = {}): SubVolume =>
    ({
      metadata: { name: 'pvc-x' },
      spec: { poolName: 'pool-a', pvcName: 'data', pvcNamespace: 'apps', requestedGB: 10 },
      status: { phase: 'Bound' },
      ...over,
    }) as SubVolume;
  const claim: PersistentVolumeClaim = {
    metadata: { name: 'data', namespace: 'apps' },
    spec: { accessModes: ['ReadWriteOnce'], storageClassName: 'pool-b', volumeMode: 'Block', resources: { requests: { storage: '10Gi' } } },
  };

  it('allows a Bound volume', () => {
    expect(cloneDisabledReason(subvolume())).toBeUndefined();
  });

  it('says why when the volume is not Bound', () => {
    expect(cloneDisabledReason(subvolume({ status: { phase: 'Creating' } }))).toMatch(/not Bound/);
    expect(cloneDisabledReason(subvolume({ status: undefined }))).toMatch(/not Bound/);
  });

  it('says why when a copy into the volume is still running', () => {
    expect(cloneDisabledReason(subvolume({ status: { phase: 'Bound', cloneStatus: 'InProgress' } }))).toMatch(/still running/);
  });

  it('says why when the volume has no claim', () => {
    const sv = subvolume();
    sv.spec.pvcName = '';
    expect(cloneDisabledReason(sv)).toMatch(/no claim/);
  });

  it('defaults to <claim>-clone, the volume size, and the source claim settings', () => {
    expect(cloneDefaults({ subvolume: subvolume(), sourcePVC: claim, storageClasses: classes })).toEqual({
      name: 'data-clone',
      sizeGiB: 10,
      storageClass: 'pool-b',
      accessMode: 'ReadWriteOnce',
      volumeMode: 'Block',
    });
  });

  it('falls back to the pool class, ReadWriteMany and Filesystem when the claim is gone', () => {
    expect(cloneDefaults({ subvolume: subvolume(), sourcePVC: undefined, storageClasses: classes })).toEqual({
      name: 'data-clone',
      sizeGiB: 10,
      storageClass: 'pool-a',
      accessMode: 'ReadWriteMany',
      volumeMode: 'Filesystem',
    });
  });

  it('makes a claim in the source namespace with the source claim as its data source and no apiGroup', () => {
    const pvc = buildClonePVC(
      { name: 'data-clone', sizeGiB: 20, storageClass: 'pool-a', accessMode: 'ReadWriteMany', volumeMode: 'Filesystem' },
      { name: 'data', namespace: 'apps' },
    );
    expect(pvc.metadata).toEqual({ name: 'data-clone', namespace: 'apps' });
    expect(pvc.spec.dataSource).toEqual({ kind: 'PersistentVolumeClaim', name: 'data' });
    expect(pvc.spec.resources.requests.storage).toBe('20Gi');
  });

  it('names the volume, not the snapshot, in the size message', () => {
    const form = { name: 'data-clone', sizeGiB: 5, storageClass: 'pool-a', accessMode: 'ReadWriteMany', volumeMode: 'Filesystem' };
    expect(validateRestore(form, { existingNames: [], minGiB: 10, minLabel: "the volume's" }).size).toMatch(/at least 10 GiB \(the volume's size\)/);
  });
});
