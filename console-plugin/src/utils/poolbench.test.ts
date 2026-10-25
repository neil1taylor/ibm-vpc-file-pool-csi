import { PoolBench } from '../types';
import {
  bannerText,
  expectedEnd,
  isActive,
  ms,
  percentOfLimit,
  resultLabel,
  stepMarks,
  verdictText,
} from './poolbench';

const NOW = new Date('2026-10-25T10:00:00Z');
const at = (minutes: number) => new Date(NOW.getTime() + minutes * 60000).toISOString();

const running = (): PoolBench => ({
  metadata: { name: 't10k' },
  spec: { shareSizeGB: 3900, iops: 10000, stepVMs: 15, maxVMs: 90, settleMinutes: 15, measureMinutes: 45 },
  status: {
    phase: 'Running',
    stage: 'Measuring',
    step: 3,
    totalSteps: 6,
    vms: 45,
    windowsVMs: 15,
    readyVMs: 45,
    stageStarted: at(-25),
    stageEnds: at(20),
  },
});

describe('bannerText', () => {
  it('says the step, the VMs running and the time left, in plain words', () => {
    expect(bannerText(running(), NOW)).toBe('Step 3 of 6: 45 VMs running (15 Windows), measuring, 20 min left');
  });

  it('counts the VMs that are not ready yet while making them', () => {
    const pb = running();
    pb.status = { ...pb.status, stage: 'Creating', readyVMs: 38, stageEnds: undefined };
    expect(bannerText(pb, NOW)).toBe('Step 3 of 6: 45 VMs (15 Windows), 38 ready, making the new VMs');
  });

  it('says when the run is paused', () => {
    const pb = running();
    pb.status = { ...pb.status, phase: 'Paused' };
    expect(bannerText(pb, NOW)).toMatch(/^Paused at step 3 of 6: 45 VMs/);
  });

  it('uses the message of a run that is not running', () => {
    const pb = running();
    pb.status = { phase: 'Complete', message: 'Knee at 45 VMs. Recommended: 30 VMs on this share.' };
    expect(bannerText(pb, NOW)).toBe('Knee at 45 VMs. Recommended: 30 VMs on this share.');
  });
});

describe('expectedEnd', () => {
  it('adds the rest of this stage and each step to come', () => {
    // 20 min left of step 3, then 3 more steps of about 5 + 15 + 45 minutes.
    expect(expectedEnd(running(), NOW)?.toISOString()).toBe(at(20 + 3 * 65));
  });

  it('is unknown for a run that has ended', () => {
    const pb = running();
    pb.status = { phase: 'Complete' };
    expect(expectedEnd(pb, NOW)).toBeUndefined();
  });
});

describe('isActive', () => {
  it('is true while a run can still change', () => {
    for (const phase of ['Pending', 'Checking', 'Running', 'Paused', 'CleaningUp'] as const) {
      expect(isActive({ ...running(), status: { phase } })).toBe(true);
    }
    for (const phase of ['Complete', 'Stopped', 'Failed'] as const) {
      expect(isActive({ ...running(), status: { phase } })).toBe(false);
    }
  });
});

describe('formatting', () => {
  it('shows microseconds as milliseconds with one decimal', () => {
    expect(ms(4260)).toBe('4.3 ms');
    expect(ms(undefined)).toBe('-');
  });

  it('shows IBM operations as a percent of the share limit', () => {
    expect(percentOfLimit(3760, 3000)).toBe('125%');
    expect(percentOfLimit(undefined, 3000)).toBe('-');
  });

  it('labels each step result', () => {
    expect(resultLabel({ vms: 15, pass: true })).toBe('Pass');
    expect(resultLabel({ vms: 45, pass: false })).toBe('Fail');
    expect(resultLabel({ vms: 30, pass: true, cutShort: true })).toBe('Pass (cut short)');
  });
});

describe('stepMarks', () => {
  it('marks the knee and the recommended step', () => {
    const pb = running();
    pb.status = { ...pb.status, kneeVMs: 45, recommendedVMs: 30 };
    expect(stepMarks(pb, 45)).toEqual(['Knee']);
    expect(stepMarks(pb, 30)).toEqual(['Recommended']);
    expect(stepMarks(pb, 15)).toEqual([]);
  });
});

describe('verdictText', () => {
  it('explains a knee and the recommendation in one sentence', () => {
    const pb = running();
    pb.status = { phase: 'Complete', kneeVMs: 45, recommendedVMs: 30, steps: [{ vms: 15, pass: true }, { vms: 30, pass: true }, { vms: 45, pass: false }] };
    expect(verdictText(pb)).toBe(
      'Users would notice at 45 VMs on this 3900 GB share at 10000 IOPS. Recommended: up to 30 VMs.',
    );
  });

  it('says when no knee was found', () => {
    const pb = running();
    pb.status = { phase: 'Complete', kneeVMs: 0, recommendedVMs: 90, steps: [{ vms: 90, pass: true }] };
    expect(verdictText(pb)).toBe('No knee up to 90 VMs: the share carried every step. The real limit is higher.');
  });

  it('says when the first step was already too much', () => {
    const pb = running();
    pb.status = { phase: 'Complete', kneeVMs: 15, recommendedVMs: 0, steps: [{ vms: 15, pass: false }] };
    expect(verdictText(pb)).toBe('Users would notice already at 15 VMs, the first step. Try a faster share or smaller steps.');
  });

  it('is empty before a step is measured', () => {
    expect(verdictText(running())).toBe('');
  });
});
