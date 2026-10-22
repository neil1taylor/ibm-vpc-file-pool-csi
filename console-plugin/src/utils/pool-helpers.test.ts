import { FileSharePool, PoolShareStatus } from '../types';
import {
  drainConsequences,
  drainListAfter,
  hasUsableShare,
  isDraining,
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

describe('drainListAfter', () => {
  it('adds a share to the drain list once', () => {
    expect(drainListAfter(undefined, 'share-1', 'drain')).toEqual(['share-1']);
    expect(drainListAfter(['share-2'], 'share-1', 'drain')).toEqual(['share-2', 'share-1']);
    expect(drainListAfter(['share-1'], 'share-1', 'drain')).toEqual(['share-1']);
  });

  it('takes a share off the list and keeps the others', () => {
    expect(drainListAfter(['share-1', 'share-2'], 'share-1', 'return')).toEqual(['share-2']);
    expect(drainListAfter(['share-1'], 'share-1', 'return')).toEqual([]);
    expect(drainListAfter(undefined, 'share-1', 'return')).toEqual([]);
  });
});

describe('isDraining', () => {
  it('is true for a share on the pool drain list', () => {
    const pool = { spec: { drainShares: ['share-1'] } } as unknown as FileSharePool;
    expect(isDraining(pool, 'share-1')).toBe(true);
    expect(isDraining(pool, 'share-2')).toBe(false);
    expect(isDraining({ spec: {} } as unknown as FileSharePool, 'share-1')).toBe(false);
  });
});

describe('drainConsequences', () => {
  const share = (pvcCount: number) => ({ shareID: 's', shareName: 'n', pvcCount }) as PoolShareStatus;

  it('says what draining does and that no data moves', () => {
    const text = drainConsequences(share(3), 'drain').join(' ');
    expect(text).toMatch(/new volumes/i);
    expect(text).toMatch(/3 existing volumes/);
    expect(text).toMatch(/no data (is )?moved?|does not move/i);
  });

  it('uses the singular for one volume and says so for none', () => {
    expect(drainConsequences(share(1), 'drain').join(' ')).toMatch(/1 existing volume\b/);
    expect(drainConsequences(share(0), 'drain').join(' ')).not.toMatch(/existing volume/);
  });

  it('for a return, warns to keep the share on the list while deleting it', () => {
    const text = drainConsequences(share(0), 'return').join(' ');
    expect(text).toMatch(/new volumes again/i);
    expect(text).toMatch(/deleting the share/i);
  });
});
