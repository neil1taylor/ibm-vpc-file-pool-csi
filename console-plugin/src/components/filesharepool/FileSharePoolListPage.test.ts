import { getIOPS, getIOPSNumber } from './FileSharePoolListPage';
import { FileSharePool } from '../../types';

function makePool(overrides: Partial<FileSharePool['spec']> = {}): FileSharePool {
  return {
    apiVersion: 'storage.ibmcloud.io/v1alpha1',
    kind: 'FileSharePool',
    metadata: { name: 'test-pool' },
    spec: {
      zone: 'us-south-1',
      profile: 'dp2',
      shareSizeGB: 100,
      maxShares: 10,
      initialShares: 1,
      autoExpand: true,
      expandThresholdPercent: 80,
      allocationStrategy: 'spread',
      encryptionInTransit: false,
      defaultPermissions: '0755',
      ...overrides,
    },
  };
}

describe('getIOPS', () => {
  it('returns formatted iops when spec.iops is set and > 0', () => {
    const pool = makePool({ iops: 5000 });
    expect(getIOPS(pool)).toBe('5,000');
  });

  it('derives IOPS from dp2 profile (100 IOPS/GB)', () => {
    const pool = makePool({ profile: 'dp2', shareSizeGB: 300 });
    expect(getIOPS(pool)).toBe('30,000');
  });

  it('returns "-" for non-dp2 profile without explicit iops', () => {
    const pool = makePool({ profile: 'custom', iops: undefined });
    expect(getIOPS(pool)).toBe('-');
  });

  it('prefers explicit iops over dp2 derivation', () => {
    const pool = makePool({ profile: 'dp2', shareSizeGB: 100, iops: 3000 });
    expect(getIOPS(pool)).toBe('3,000');
  });

  it('returns "-" when iops is 0', () => {
    const pool = makePool({ iops: 0, profile: 'custom' });
    expect(getIOPS(pool)).toBe('-');
  });
});

describe('getIOPSNumber', () => {
  it('returns explicit iops when set and > 0', () => {
    const pool = makePool({ iops: 5000 });
    expect(getIOPSNumber(pool)).toBe(5000);
  });

  it('derives IOPS from dp2 profile for sorting', () => {
    const pool = makePool({ profile: 'dp2', shareSizeGB: 300 });
    expect(getIOPSNumber(pool)).toBe(30000);
  });

  it('returns 0 for non-dp2 profile without explicit iops', () => {
    const pool = makePool({ profile: 'custom', iops: undefined });
    expect(getIOPSNumber(pool)).toBe(0);
  });

  it('prefers explicit iops over dp2 derivation', () => {
    const pool = makePool({ profile: 'dp2', shareSizeGB: 100, iops: 3000 });
    expect(getIOPSNumber(pool)).toBe(3000);
  });
});
