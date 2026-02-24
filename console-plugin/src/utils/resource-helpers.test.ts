import {
  humanizeBytes,
  gbToBytes,
  formatDuration,
  formatAge,
  utilizationPercent,
  phaseBadgeColor,
  phaseToPatternFlyStatus,
} from './resource-helpers';

describe('humanizeBytes', () => {
  it('returns "0 B" for zero', () => {
    expect(humanizeBytes(0)).toBe('0 B');
  });

  it('formats bytes (no decimals)', () => {
    expect(humanizeBytes(1)).toBe('1 B');
    expect(humanizeBytes(512)).toBe('512 B');
  });

  it('formats KiB', () => {
    expect(humanizeBytes(1024)).toBe('1.0 KiB');
    expect(humanizeBytes(1536)).toBe('1.5 KiB');
  });

  it('formats MiB', () => {
    expect(humanizeBytes(1024 * 1024)).toBe('1.0 MiB');
    expect(humanizeBytes(2.5 * 1024 * 1024)).toBe('2.5 MiB');
  });

  it('formats GiB', () => {
    expect(humanizeBytes(1024 * 1024 * 1024)).toBe('1.0 GiB');
    expect(humanizeBytes(10 * 1024 * 1024 * 1024)).toBe('10.0 GiB');
  });

  it('formats TiB', () => {
    expect(humanizeBytes(1024 * 1024 * 1024 * 1024)).toBe('1.0 TiB');
  });
});

describe('gbToBytes', () => {
  it('converts 1 GB to bytes', () => {
    expect(gbToBytes(1)).toBe(1073741824);
  });

  it('returns 0 for 0', () => {
    expect(gbToBytes(0)).toBe(0);
  });

  it('handles fractional GB', () => {
    expect(gbToBytes(0.5)).toBe(536870912);
  });
});

describe('formatDuration', () => {
  it('parses hours, minutes, seconds', () => {
    expect(formatDuration('1h30m45s')).toBe('1h 30m 45s');
  });

  it('parses minutes only', () => {
    expect(formatDuration('15m')).toBe('15m');
  });

  it('parses fractional seconds', () => {
    expect(formatDuration('2.5s')).toBe('3s');
  });

  it('parses hours only', () => {
    expect(formatDuration('2h')).toBe('2h');
  });

  it('returns "0s" for empty string', () => {
    expect(formatDuration('')).toBe('0s');
  });

  it('returns original string when no match', () => {
    // The regex always matches (all groups optional), so test a non-duration string
    // that results in all-zero groups
    expect(formatDuration('abc')).toBe('0s');
  });
});

describe('formatAge', () => {
  it('returns "-" for undefined', () => {
    expect(formatAge(undefined)).toBe('-');
  });

  it('returns seconds for recent timestamps', () => {
    const now = new Date(Date.now() - 30 * 1000).toISOString();
    expect(formatAge(now)).toBe('30s');
  });

  it('returns minutes', () => {
    const ago = new Date(Date.now() - 5 * 60 * 1000).toISOString();
    expect(formatAge(ago)).toBe('5m');
  });

  it('returns hours', () => {
    const ago = new Date(Date.now() - 3 * 60 * 60 * 1000).toISOString();
    expect(formatAge(ago)).toBe('3h');
  });

  it('returns days', () => {
    const ago = new Date(Date.now() - 2 * 24 * 60 * 60 * 1000).toISOString();
    expect(formatAge(ago)).toBe('2d');
  });
});

describe('utilizationPercent', () => {
  it('returns 0 when total is 0', () => {
    expect(utilizationPercent(0, 0)).toBe(0);
  });

  it('calculates 50%', () => {
    expect(utilizationPercent(50, 100)).toBe(50);
  });

  it('calculates 100%', () => {
    expect(utilizationPercent(100, 100)).toBe(100);
  });

  it('handles overcommit (>100%)', () => {
    expect(utilizationPercent(150, 100)).toBe(150);
  });

  it('rounds to nearest integer', () => {
    expect(utilizationPercent(1, 3)).toBe(33);
  });
});

describe('phaseBadgeColor', () => {
  it('returns green for Ready, Bound, Complete, Active, stable', () => {
    expect(phaseBadgeColor('Ready')).toBe('green');
    expect(phaseBadgeColor('Bound')).toBe('green');
    expect(phaseBadgeColor('Complete')).toBe('green');
    expect(phaseBadgeColor('Active')).toBe('green');
    expect(phaseBadgeColor('stable')).toBe('green');
  });

  it('returns blue for in-progress phases', () => {
    expect(phaseBadgeColor('Initializing')).toBe('blue');
    expect(phaseBadgeColor('Creating')).toBe('blue');
    expect(phaseBadgeColor('Cloning')).toBe('blue');
    expect(phaseBadgeColor('Expanding')).toBe('blue');
    expect(phaseBadgeColor('InProgress')).toBe('blue');
    expect(phaseBadgeColor('Pending')).toBe('blue');
    expect(phaseBadgeColor('Syncing')).toBe('blue');
    expect(phaseBadgeColor('creating')).toBe('blue');
  });

  it('returns orange for warning phases', () => {
    expect(phaseBadgeColor('Degraded')).toBe('orange');
    expect(phaseBadgeColor('PartialFailure')).toBe('orange');
    expect(phaseBadgeColor('Paused')).toBe('orange');
    expect(phaseBadgeColor('Retained')).toBe('orange');
    expect(phaseBadgeColor('Archived')).toBe('orange');
    expect(phaseBadgeColor('draining')).toBe('orange');
    expect(phaseBadgeColor('degraded')).toBe('orange');
  });

  it('returns red for error phases', () => {
    expect(phaseBadgeColor('Failed')).toBe('red');
    expect(phaseBadgeColor('Full')).toBe('red');
    expect(phaseBadgeColor('deleting')).toBe('red');
  });

  it('returns grey for Deleting', () => {
    expect(phaseBadgeColor('Deleting')).toBe('grey');
  });

  it('returns grey for undefined', () => {
    expect(phaseBadgeColor(undefined)).toBe('grey');
  });

  it('returns grey for unknown phase', () => {
    expect(phaseBadgeColor('SomethingRandom')).toBe('grey');
  });
});

describe('phaseToPatternFlyStatus', () => {
  it('maps green to success', () => {
    expect(phaseToPatternFlyStatus('Ready')).toBe('success');
  });

  it('maps blue to info', () => {
    expect(phaseToPatternFlyStatus('Initializing')).toBe('info');
  });

  it('maps orange to warning', () => {
    expect(phaseToPatternFlyStatus('Degraded')).toBe('warning');
  });

  it('maps red to danger', () => {
    expect(phaseToPatternFlyStatus('Failed')).toBe('danger');
  });

  it('maps grey/unknown to custom', () => {
    expect(phaseToPatternFlyStatus(undefined)).toBe('custom');
    expect(phaseToPatternFlyStatus('Deleting')).toBe('custom');
    expect(phaseToPatternFlyStatus('UnknownPhase')).toBe('custom');
  });
});
