import { FileSharePool } from '../types';
import { PersistentVolumeClaim, StorageClass } from './k8s-objects';
import {
  MigrationPodLike,
  migrationProgress,
  migrationRows,
  migrationTargets,
  parseGiB,
  podsUsingPVC,
  sourceClasses,
} from './migrate-helpers';

const DRIVER = 'vpc-file-pool.csi.ibm.io';

const pvc = (name: string, over: Partial<PersistentVolumeClaim['spec']> = {}, phase = 'Bound', namespace = 'apps'): PersistentVolumeClaim => ({
  metadata: { name, namespace },
  spec: { accessModes: ['ReadWriteMany'], storageClassName: 'ibmc-vpc-file-3000-iops', resources: { requests: { storage: '200Gi' } }, ...over },
  status: { phase },
});

const pod = (name: string, claims: string[], phase = 'Running', namespace = 'apps', labels?: Record<string, string>): MigrationPodLike => ({
  metadata: { name, namespace, labels },
  spec: { volumes: claims.map((c) => ({ persistentVolumeClaim: { claimName: c } })) },
  status: { phase },
});

describe('parseGiB', () => {
  it('reads binary and decimal suffixes and rounds up to a whole GiB', () => {
    expect(parseGiB('200Gi')).toBe(200);
    expect(parseGiB('1Ti')).toBe(1024);
    expect(parseGiB('500Mi')).toBe(1);
    expect(parseGiB('214748364800')).toBe(200);
    expect(parseGiB('100G')).toBe(94);
  });

  it('is 0 for a size it cannot read', () => {
    expect(parseGiB('')).toBe(0);
    expect(parseGiB('lots')).toBe(0);
  });
});

describe('podsUsingPVC', () => {
  it('lists running and pending pods of the namespace that mount the claim', () => {
    const pods = [pod('web', ['data']), pod('other-ns', ['data'], 'Running', 'elsewhere'), pod('other-claim', ['x'])];
    expect(podsUsingPVC(pods, 'apps', 'data')).toEqual(['web']);
  });

  it('ignores pods that have finished and migration pods', () => {
    const pods = [
      pod('done', ['data'], 'Succeeded'),
      pod('dead', ['data'], 'Failed'),
      pod('migrate-data', ['data'], 'Running', 'apps', { 'vpc-file-pool-csi.ibm.io/migration': 'true' }),
    ];
    expect(podsUsingPVC(pods, 'apps', 'data')).toEqual([]);
  });
});

describe('sourceClasses', () => {
  it('offers the storage classes the claims use, not the classes of this driver', () => {
    const classes: StorageClass[] = [
      { metadata: { name: 'ibmc-vpc-file-3000-iops' }, provisioner: 'vpc.file.csi.ibm.io' },
      { metadata: { name: 'pool-a' }, provisioner: DRIVER },
      { metadata: { name: 'unused' }, provisioner: 'other' },
    ];
    const pvcs = [pvc('a'), pvc('b', { storageClassName: 'pool-a' }), pvc('c', { storageClassName: 'ibmc-vpc-file-3000-iops' })];
    expect(sourceClasses(pvcs, classes)).toEqual(['ibmc-vpc-file-3000-iops']);
  });
});

describe('migrationRows', () => {
  const input = (pvcs: PersistentVolumeClaim[], pods: MigrationPodLike[] = []) => ({
    pvcs,
    pods,
    namespace: 'apps',
    sourceClass: 'ibmc-vpc-file-3000-iops',
  });

  it('lists the claims of the namespace and class, with size and phase', () => {
    const rows = migrationRows(input([pvc('data'), pvc('other-class', { storageClassName: 'gp' }), pvc('x', {}, 'Bound', 'elsewhere')]));
    expect(rows).toEqual([{ name: 'data', sizeGiB: 200, phase: 'Bound', pods: [], canMove: true }]);
  });

  it('cannot move a claim that a pod uses, and names the pods', () => {
    const [row] = migrationRows(input([pvc('data')], [pod('web', ['data'])]));
    expect(row.canMove).toBe(false);
    expect(row.pods).toEqual(['web']);
    expect(row.reason).toMatch(/In use by: web/);
  });

  it('cannot move a claim that is not Bound', () => {
    const [row] = migrationRows(input([pvc('data', {}, 'Pending')]));
    expect(row.canMove).toBe(false);
    expect(row.reason).toMatch(/not Bound/);
  });

  it('cannot move a claim whose new claim name is taken', () => {
    const rows = migrationRows(input([pvc('data'), pvc('data-pooled', { storageClassName: 'pool-a' })]));
    const row = rows.find((r) => r.name === 'data');
    expect(row?.canMove).toBe(false);
    expect(row?.reason).toMatch(/data-pooled already exists/);
  });
});

describe('migrationTargets', () => {
  const classes: StorageClass[] = [
    { metadata: { name: 'pool-a' }, provisioner: DRIVER },
    { metadata: { name: 'pool-b-gold' }, provisioner: DRIVER },
  ];
  const pool = (name: string, over: Record<string, unknown> = {}, status = { totalCapacityGB: 200, totalAllocatedGB: 150 }) =>
    ({ metadata: { name }, spec: { ...over }, status }) as unknown as FileSharePool;

  it('offers a pool by its storage class, with the free space', () => {
    expect(migrationTargets([pool('pool-a')], classes)).toEqual([
      { storageClass: 'pool-a', pool: 'pool-a', label: 'pool-a', freeGB: 50 },
    ]);
  });

  it('offers each tier of a tiered pool', () => {
    const tiered = pool('pool-b', { tiers: [{ name: 'gold' }, { name: 'silver' }] });
    expect(migrationTargets([tiered], classes).map((t) => [t.storageClass, t.label])).toEqual([['pool-b-gold', 'pool-b / gold']]);
  });

  it('leaves out a pool that has no storage class yet', () => {
    expect(migrationTargets([pool('pool-z')], classes)).toEqual([]);
  });
});

describe('migrationProgress', () => {
  const target = (phase: string) => pvc('data-pooled', { storageClassName: 'pool-a' }, phase);

  it('has not started when there is no new claim', () => {
    expect(migrationProgress(undefined, undefined)).toEqual({ state: 'not-started' });
  });

  it('waits for the new claim to bind', () => {
    expect(migrationProgress(target('Pending'), undefined)).toEqual({ state: 'binding' });
  });

  it('is starting when the claim is Bound and the pod does not exist yet', () => {
    expect(migrationProgress(target('Bound'), undefined)).toEqual({ state: 'starting' });
  });

  it('is copying while the pod is Pending or Running', () => {
    expect(migrationProgress(target('Bound'), pod('migrate-data', [], 'Running'))).toEqual({ state: 'copying', podPhase: 'Running' });
    expect(migrationProgress(target('Bound'), pod('migrate-data', [], 'Pending'))).toEqual({ state: 'copying', podPhase: 'Pending' });
  });

  it('is done when the pod Succeeded', () => {
    expect(migrationProgress(target('Bound'), pod('migrate-data', [], 'Succeeded'))).toEqual({ state: 'done' });
  });

  it('is failed when the pod Failed, with the exit code', () => {
    const failed = pod('migrate-data', [], 'Failed');
    failed.status = { phase: 'Failed', containerStatuses: [{ state: { terminated: { exitCode: 1 } } }] };
    expect(migrationProgress(target('Bound'), failed)).toEqual({ state: 'failed', exitCode: 1 });
  });
});
