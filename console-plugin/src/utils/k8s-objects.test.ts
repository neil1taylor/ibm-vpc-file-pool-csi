import {
  CSI_DRIVER_NAME,
  buildPVC,
  buildVolumeGroupSnapshot,
  buildVolumeSnapshot,
  findClassByDriver,
  errorMessage,
  isAlreadyExistsError,
  isNotFoundError,
  isValidK8sName,
  parseMatchLabels,
  pvcPath,
  storageClassFor,
  volumeSnapshotPath,
  volumeGroupSnapshotPath,
  poolPVCs,
  PersistentVolumeClaim,
} from './k8s-objects';

describe('storageClassFor', () => {
  it('is the pool name for a pool without tiers', () => {
    expect(storageClassFor('pool-a')).toBe('pool-a');
  });

  it('is <pool>-<tier> for a tiered pool', () => {
    expect(storageClassFor('pool-a', 'gold')).toBe('pool-a-gold');
  });
});

describe('buildPVC', () => {
  it('builds a ReadWriteMany claim in the pool storage class', () => {
    expect(buildPVC({ namespace: 'apps', name: 'data', poolName: 'pool-a', sizeGiB: 20 })).toEqual({
      apiVersion: 'v1',
      kind: 'PersistentVolumeClaim',
      metadata: { name: 'data', namespace: 'apps' },
      spec: {
        accessModes: ['ReadWriteMany'],
        storageClassName: 'pool-a',
        resources: { requests: { storage: '20Gi' } },
      },
    });
  });

  it('uses the tier storage class', () => {
    const pvc = buildPVC({ namespace: 'a', name: 'b', poolName: 'p', tier: 'fast', sizeGiB: 1 });
    expect(pvc.spec.storageClassName).toBe('p-fast');
  });
});

describe('buildVolumeSnapshot', () => {
  it('snapshots a PVC with the given class', () => {
    expect(
      buildVolumeSnapshot({ namespace: 'apps', name: 'snap', pvcName: 'data', className: 'ibm-vpc-file-pool' }),
    ).toEqual({
      apiVersion: 'snapshot.storage.k8s.io/v1',
      kind: 'VolumeSnapshot',
      metadata: { name: 'snap', namespace: 'apps' },
      spec: {
        volumeSnapshotClassName: 'ibm-vpc-file-pool',
        source: { persistentVolumeClaimName: 'data' },
      },
    });
  });
});

describe('buildVolumeGroupSnapshot', () => {
  it('selects PVCs by label', () => {
    expect(
      buildVolumeGroupSnapshot({
        namespace: 'apps',
        name: 'grp',
        matchLabels: { app: 'db' },
        className: 'grp-class',
      }),
    ).toEqual({
      apiVersion: 'groupsnapshot.storage.k8s.io/v1beta1',
      kind: 'VolumeGroupSnapshot',
      metadata: { name: 'grp', namespace: 'apps' },
      spec: {
        volumeGroupSnapshotClassName: 'grp-class',
        source: { selector: { matchLabels: { app: 'db' } } },
      },
    });
  });
});

describe('parseMatchLabels', () => {
  it('parses key=value lines', () => {
    expect(parseMatchLabels('app=db\n tier = data \n')).toEqual({ app: 'db', tier: 'data' });
  });

  it('is empty for blank input and ignores lines without a key', () => {
    expect(parseMatchLabels(' \n=x\nnovalue')).toEqual({});
  });
});

describe('findClassByDriver', () => {
  const classes = [
    { metadata: { name: 'other' }, driver: 'other.csi' },
    { metadata: { name: 'ibm-vpc-file-pool' }, driver: CSI_DRIVER_NAME },
  ];

  it('finds the class by driver, not by name', () => {
    expect(findClassByDriver(classes)?.metadata?.name).toBe('ibm-vpc-file-pool');
  });

  it('is undefined when no class uses the driver', () => {
    expect(findClassByDriver([classes[0]])).toBeUndefined();
  });
});

describe('poolPVCs', () => {
  const storageClasses = [
    { metadata: { name: 'pool-a' }, provisioner: CSI_DRIVER_NAME },
    { metadata: { name: 'gp' }, provisioner: 'other' },
  ];
  const pvc = (name: string, sc?: string): PersistentVolumeClaim => ({
    metadata: { name, namespace: 'apps' },
    spec: { accessModes: ['ReadWriteMany'], storageClassName: sc, resources: { requests: { storage: '1Gi' } } },
  });

  it('keeps only claims whose storage class uses the driver', () => {
    const result = poolPVCs([pvc('a', 'pool-a'), pvc('b', 'gp'), pvc('c'), pvc('d', 'missing')], storageClasses);
    expect(result.map((p) => p.metadata?.name)).toEqual(['a']);
  });
});

describe('paths', () => {
  it('links to the standard console pages', () => {
    expect(pvcPath('apps', 'data')).toBe('/k8s/ns/apps/persistentvolumeclaims/data');
    expect(volumeSnapshotPath('apps', 'snap')).toBe(
      '/k8s/ns/apps/snapshot.storage.k8s.io~v1~VolumeSnapshot/snap',
    );
    expect(volumeGroupSnapshotPath('apps', 'grp')).toBe(
      '/k8s/ns/apps/groupsnapshot.storage.k8s.io~v1beta1~VolumeGroupSnapshot/grp',
    );
  });
});

describe('isNotFoundError', () => {
  it('recognises 404 in the shapes the SDK produces', () => {
    expect(isNotFoundError({ code: 404 })).toBe(true);
    expect(isNotFoundError({ response: { status: 404 } })).toBe(true);
    expect(isNotFoundError({ json: { code: 404 } })).toBe(true);
  });

  it('rejects other errors', () => {
    expect(isNotFoundError({ code: 403 })).toBe(false);
    expect(isNotFoundError(undefined)).toBe(false);
    expect(isNotFoundError('x')).toBe(false);
  });
});

describe('isValidK8sName', () => {
  it('accepts DNS-1123 labels', () => {
    expect(isValidK8sName('my-volume-1')).toBe(true);
  });

  it('rejects empty, upper-case, leading-dash and over-long names', () => {
    expect(isValidK8sName('')).toBe(false);
    expect(isValidK8sName('My')).toBe(false);
    expect(isValidK8sName('-a')).toBe(false);
    expect(isValidK8sName('a'.repeat(64))).toBe(false);
  });
});

describe('errorMessage', () => {
  it('reads the message from Error objects and plain objects', () => {
    expect(errorMessage(new Error('boom'), 'fallback')).toBe('boom');
    expect(errorMessage({ message: 'denied' }, 'fallback')).toBe('denied');
  });

  it('uses the fallback for anything else', () => {
    expect(errorMessage(undefined, 'fallback')).toBe('fallback');
  });
});

describe('isAlreadyExistsError', () => {
  it('is true for a 409 in any of the shapes the console client gives', () => {
    expect(isAlreadyExistsError({ code: 409 })).toBe(true);
    expect(isAlreadyExistsError({ response: { status: 409 } })).toBe(true);
    expect(isAlreadyExistsError({ json: { code: 409 } })).toBe(true);
  });

  it('is false for other errors', () => {
    expect(isAlreadyExistsError({ code: 404 })).toBe(false);
    expect(isAlreadyExistsError(new Error('boom'))).toBe(false);
    expect(isAlreadyExistsError(undefined)).toBe(false);
  });
});
