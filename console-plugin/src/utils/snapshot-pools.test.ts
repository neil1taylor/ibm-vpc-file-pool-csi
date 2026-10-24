import { BackupPolicy } from '../types';
import { StorageClass } from './k8s-objects';
import { isSnapshotPool, snapshotPoolNames, volumeClasses } from './snapshot-pools';

const DRIVER = 'vpc-file-pool.csi.ibm.io';
const policy = (name: string, snapshotPool: string) => ({ metadata: { name }, spec: { snapshotPool } }) as BackupPolicy;
const sc = (name: string, pool: string, provisioner = DRIVER): StorageClass => ({
  metadata: { name },
  provisioner,
  parameters: { pool },
});

describe('snapshot pools', () => {
  const policies = [policy('nightly', 'backups'), policy('weekly', 'backups'), policy('vm', 'vm-backups')];

  it('names each pool that a policy uses as its snapshot pool', () => {
    expect([...snapshotPoolNames(policies)].sort()).toEqual(['backups', 'vm-backups']);
    expect(isSnapshotPool('backups', policies)).toBe(true);
    expect(isSnapshotPool('data', policies)).toBe(false);
  });

  it('offers only classes of this driver whose pool is not a snapshot pool', () => {
    const classes = [sc('data', 'data'), sc('backups', 'backups'), sc('tier-fast', 'vm-backups'), sc('gp', 'gp', 'other')];
    expect(volumeClasses(classes, policies).map((c) => c.metadata?.name)).toEqual(['data']);
  });

  it('offers every class of this driver when there are no policies', () => {
    expect(volumeClasses([sc('data', 'data'), sc('backups', 'backups')], []).map((c) => c.metadata?.name)).toEqual([
      'data',
      'backups',
    ]);
  });
});
