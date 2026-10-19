import { FileSharePool, PoolShareStatus } from '../types';
import {
  hasUsableShare,
  parseSecurityGroupIDs,
  poolPhaseReason,
  shareHealth,
  shareIsUsable,
} from './pool-helpers';

function share(overrides: Partial<PoolShareStatus> = {}): PoolShareStatus {
  return {
    shareID: 's1',
    shareName: 'share-1',
    mountTargetIP: '10.0.0.4',
    mountTargetID: 'mt1',
    totalGB: 100,
    allocatedGB: 0,
    pvcCount: 0,
    state: 'stable',
    zone: 'eu-de-1',
    ...overrides,
  };
}

function pool(status: FileSharePool['status'], name = 'p'): FileSharePool {
  return {
    metadata: { name },
    spec: {} as FileSharePool['spec'],
    status,
  };
}

describe('shareHealth', () => {
  it('is a success for a stable share with a mount target', () => {
    expect(shareHealth(share())).toEqual({ label: 'stable', status: 'success' });
  });

  it('warns when a stable share has no mount target', () => {
    expect(shareHealth(share({ mountTargetIP: '' }))).toEqual({
      label: 'No mount target',
      status: 'warning',
    });
  });

  it('uses the state colour for other states', () => {
    expect(shareHealth(share({ state: 'creating', mountTargetIP: '' }))).toEqual({
      label: 'creating',
      status: 'info',
    });
    expect(shareHealth(share({ state: 'degraded' }))).toEqual({
      label: 'degraded',
      status: 'warning',
    });
  });
});

describe('shareIsUsable / hasUsableShare', () => {
  it('needs a stable state and a mount target IP', () => {
    expect(shareIsUsable(share())).toBe(true);
    expect(shareIsUsable(share({ mountTargetIP: '' }))).toBe(false);
    expect(shareIsUsable(share({ state: 'draining' }))).toBe(false);
  });

  it('is true when at least one share is usable', () => {
    expect(hasUsableShare(pool({ shares: [share({ mountTargetIP: '' }), share()] } as never))).toBe(true);
    expect(hasUsableShare(pool({ shares: [share({ mountTargetIP: '' })] } as never))).toBe(false);
    expect(hasUsableShare(pool(undefined))).toBe(false);
  });
});

describe('poolPhaseReason', () => {
  const conditions = [
    { type: 'SharesReady', status: 'False', reason: 'NoMountTarget', message: 'share-1 has no mount target' },
    { type: 'CapacityAvailable', status: 'False', reason: 'NoCapacity', message: 'No free space' },
  ];

  it('is undefined when the pool is Ready', () => {
    expect(poolPhaseReason(pool({ phase: 'Ready', conditions } as never))).toBeUndefined();
  });

  it('uses the SharesReady message when the phase is not Ready', () => {
    expect(poolPhaseReason(pool({ phase: 'Degraded', conditions } as never))).toBe(
      'share-1 has no mount target',
    );
  });

  it('uses the CapacityAvailable message when the phase is Full', () => {
    expect(poolPhaseReason(pool({ phase: 'Full', conditions } as never))).toBe('No free space');
  });

  it('ignores conditions that are True or have no message', () => {
    const ok = [{ type: 'SharesReady', status: 'True', message: 'fine' }];
    expect(poolPhaseReason(pool({ phase: 'Initializing', conditions: ok } as never))).toBeUndefined();
    expect(poolPhaseReason(pool({ phase: 'Degraded' } as never))).toBeUndefined();
  });
});

describe('parseSecurityGroupIDs', () => {
  it('splits on newlines and commas and trims', () => {
    expect(parseSecurityGroupIDs('sg-1, sg-2\n  sg-3 \n\n')).toEqual(['sg-1', 'sg-2', 'sg-3']);
  });

  it('returns an empty list for blank input', () => {
    expect(parseSecurityGroupIDs('  \n ')).toEqual([]);
  });
});
