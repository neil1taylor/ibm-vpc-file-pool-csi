import { PoolBench } from '../types';
import { chartWindow, ibmOpsPerIO, runCharts } from './poolbench-queries';

const pb = (): PoolBench => ({
  metadata: { name: 't10k' },
  spec: {
    shareSizeGB: 3900, iops: 10000,
    load: { blockMix: '4k:50 16k:30 64k:20' },
    knee: { latencyLimitMs: 20, slowVMPercent: 25 },
  },
  status: { phase: 'Running', startTime: '2026-10-25T08:00:00Z', poolName: 'poolbench-t10k' },
});

describe('ibmOpsPerIO', () => {
  it('counts one IBM operation for every 16 KiB of an I/O, as the Go service does', () => {
    expect(ibmOpsPerIO('4k:50 16k:30 64k:20')).toBe(1.6);
    expect(ibmOpsPerIO('64k:100')).toBe(4);
    expect(ibmOpsPerIO('1m:100')).toBe(64);
  });

  it('falls back to 1 for a mix it cannot read', () => {
    expect(ibmOpsPerIO('nonsense')).toBe(1);
    expect(ibmOpsPerIO(undefined)).toBe(1.6);
  });
});

describe('runCharts', () => {
  it('has the five graphs of the spec', () => {
    expect(runCharts(pb()).map((c) => c.title)).toEqual(['VMs running', 'IOPS', 'Latency', 'Throughput', 'Slow VMs']);
  });

  it('reads the run and its pool only', () => {
    for (const c of runCharts(pb())) {
      for (const s of c.series) {
        expect(s.query).toMatch(/run="t10k"|pool="poolbench-t10k"/);
      }
    }
  });

  it('draws the share limit, the IBM operations and the latency limit', () => {
    const [, iops, latency, , slow] = runCharts(pb());
    expect(iops.lines).toEqual([{ label: 'Share limit', y: 10000 }]);
    expect(iops.series.find((s) => s.label === 'IBM operations')?.query).toBe(
      'sum(poolbench_vm_iops{run="t10k",phase="steady"}) * 1.6',
    );
    expect(latency.lines).toEqual([{ label: 'Limit 20 ms', y: 20 }]);
    expect(latency.series.map((s) => s.label)).toEqual(['Median VM p95', 'Worst VM p95', 'Share']);
    expect(slow.lines).toEqual([{ label: 'Knee 25%', y: 25 }]);
    expect(slow.series[0].query).toContain('> 0.02');
  });

  it('counts the Linux, Windows and not-ready VMs', () => {
    const [vms] = runCharts(pb());
    expect(vms.series.map((s) => s.label)).toEqual(['Linux', 'Windows', 'Not ready']);
  });
});

describe('chartWindow', () => {
  it('runs from the start of the run to now while it runs', () => {
    const now = new Date('2026-10-25T10:00:00Z');
    const w = chartWindow(pb(), now)!;
    expect(w.start).toBe(Date.parse('2026-10-25T08:00:00Z') / 1000);
    expect(w.end).toBe(now.getTime() / 1000);
    expect(w.step).toBe(30);
  });

  it('runs to the end of a run that has ended, with a longer step for a long run', () => {
    const p = pb();
    p.status = { ...p.status, phase: 'Complete', endTime: '2026-10-25T18:00:00Z' };
    const w = chartWindow(p, new Date('2026-10-26T00:00:00Z'))!;
    expect(w.end).toBe(Date.parse('2026-10-25T18:00:00Z') / 1000);
    expect(w.step).toBe(150);
  });

  it('is empty before the run starts', () => {
    const p = pb();
    p.status = {};
    expect(chartWindow(p, new Date())).toBeUndefined();
  });
});
