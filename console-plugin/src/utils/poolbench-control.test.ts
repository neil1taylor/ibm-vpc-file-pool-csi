import { PoolBench } from '../types';
import { availableActions, controlPatch, reportMarkdown, stepsCSV } from './poolbench-control';

const pb = (status: PoolBench['status'], control: PoolBench['spec']['control'] = {}, keep = false): PoolBench => ({
  metadata: { name: 't3000' },
  spec: {
    shareSizeGB: 3900, iops: 3000, stepVMs: 15, maxVMs: 90, windowsEvery: 3, settleMinutes: 15, measureMinutes: 45,
    load: { steadyIOPS: 30, burstIOPS: 300, burstSeconds: 45, periodSeconds: 600, readPercent: 70, blockMix: '4k:50 16k:30 64k:20' },
    knee: { latencyLimitMs: 20, slowVMPercent: 25, stallSeconds: 5 },
    keepAfterEnd: keep,
    control,
  },
  status,
});

describe('availableActions', () => {
  it('offers pause, hold, measure longer, skip and stop while measuring', () => {
    expect(availableActions(pb({ phase: 'Running', stage: 'Measuring', step: 2 }))).toEqual([
      'pause', 'hold', 'extend', 'skip', 'stop',
    ]);
  });

  it('offers resume while paused, and release while held', () => {
    expect(availableActions(pb({ phase: 'Paused', stage: 'Measuring', step: 2 }, { paused: true }))).toContain('resume');
    expect(availableActions(pb({ phase: 'Running', stage: 'Measuring', step: 2 }, { hold: true }))).toContain('release');
  });

  it('offers only pause and stop while the VMs are made', () => {
    expect(availableActions(pb({ phase: 'Running', stage: 'Creating', step: 2 }))).toEqual(['pause', 'stop']);
  });

  it('offers a cleanup of a run whose VMs were kept', () => {
    expect(availableActions(pb({ phase: 'Complete', cleanedUp: false }, {}, true))).toEqual(['cleanup']);
    expect(availableActions(pb({ phase: 'Complete', cleanedUp: true }, {}, true))).toEqual([]);
  });

  it('offers nothing once a stop is asked', () => {
    expect(availableActions(pb({ phase: 'CleaningUp' }, { stop: true }))).toEqual([]);
  });
});

describe('controlPatch', () => {
  const run = pb({ phase: 'Running', stage: 'Measuring', step: 2 }, { extendStep: 2, extendMinutes: 15 });

  it('sets one field of spec.control and keeps the others', () => {
    expect(controlPatch(run, 'pause')).toEqual([
      { op: 'add', path: '/spec/control', value: { extendStep: 2, extendMinutes: 15, paused: true } },
    ]);
    expect(controlPatch(run, 'skip')[0].value).toMatchObject({ skipStep: 2 });
    expect(controlPatch(run, 'stop')[0].value).toMatchObject({ stop: true });
    expect(controlPatch(run, 'cleanup')[0].value).toMatchObject({ stop: true });
  });

  it('adds minutes to this step, counting what was added before', () => {
    expect(controlPatch(run, 'extend', 15)[0].value).toMatchObject({ extendStep: 2, extendMinutes: 30 });
    const fresh = pb({ phase: 'Running', stage: 'Measuring', step: 3 }, { extendStep: 2, extendMinutes: 15 });
    expect(controlPatch(fresh, 'extend', 15)[0].value).toMatchObject({ extendStep: 3, extendMinutes: 15 });
  });
});

const done = pb({
  phase: 'Complete',
  kneeVMs: 45,
  recommendedVMs: 30,
  startTime: '2026-10-24T20:00:00Z',
  endTime: '2026-10-25T02:00:00Z',
  steps: [
    { vms: 15, windowsVMs: 5, shareIOPS: 768, guestIOPS: 785, ibmOps: 1256, shareLatencyMicros: 300, p50Micros: 600, p95Micros: 1500, p99Micros: 4000, throughputKBps: 20000, slowPercent: 0, stalls: 0, readySeconds: 360, pass: true, ended: 'x' },
    { vms: 45, windowsVMs: 15, shareIOPS: 2300, guestIOPS: 2350, ibmOps: 3760, slowPercent: 76, stalls: 0, pass: false, ended: 'y' },
  ],
});

describe('stepsCSV', () => {
  it('has one row per step with every number of the table', () => {
    const lines = stepsCSV(done).trim().split('\n');
    expect(lines[0]).toBe(
      'vms,windows_vms,share_iops,guest_iops,ibm_ops,ibm_ops_pct_of_limit,share_latency_ms,p50_ms,p95_ms,p99_ms,throughput_mb_s,slow_vms_pct,stalls,ready_s,result,mark',
    );
    expect(lines[1]).toBe('15,5,768,785,1256,42,0.3,0.6,1.5,4.0,20.5,0,0,360,Pass,');
    expect(lines[2]).toBe('45,15,2300,2350,3760,125,,,,,,76,0,,Fail,Knee');
  });
});

describe('reportMarkdown', () => {
  it('has the result, the settings and the step table', () => {
    const md = reportMarkdown(done);
    expect(md).toContain('# PoolBench run t3000');
    expect(md).toContain('Users would notice at 45 VMs on this 3900 GB share at 3000 IOPS. Recommended: up to 30 VMs.');
    expect(md).toContain('| VMs | Share IOPS |');
    expect(md).toContain('| 45 (15 Windows) | 2300 |');
    expect(md).toContain('30 IOPS steady, 300 IOPS bursts of 45 s every 600 s');
  });
});
