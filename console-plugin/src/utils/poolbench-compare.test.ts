import { PoolBench } from '../types';
import { compareRows, compareSeries, runLabel } from './poolbench-compare';

const run = (name: string, iops: number, steps: [number, number, number, boolean][], knee = 0): PoolBench => ({
  metadata: { name },
  spec: { shareSizeGB: 3900, iops },
  status: {
    phase: 'Complete',
    kneeVMs: knee,
    steps: steps.map(([vms, p95Micros, slowPercent, pass]) => ({ vms, p95Micros, slowPercent, pass, ended: 'x' })),
  },
});

const t3000 = run('t3000', 3000, [[15, 1500, 0, true], [30, 2100, 0, true], [45, 25000, 76, false]], 45);
const t10k = run('t10k', 10000, [[15, 1400, 0, true], [30, 1600, 0, true], [45, 1700, 0, true], [60, 1800, 0, true]]);

describe('runLabel', () => {
  it('names a run by its share', () => {
    expect(runLabel(t3000)).toBe('t3000 (3900 GB, 3000 IOPS)');
  });
});

describe('compareRows', () => {
  it('has one row per VM count of any run, and a cell per run', () => {
    const rows = compareRows([t3000, t10k]);
    expect(rows.map((r) => r.vms)).toEqual([15, 30, 45, 60]);
    expect(rows[2].cells).toEqual([
      { p95Ms: 25, slowPercent: 76, pass: false, knee: true },
      { p95Ms: 1.7, slowPercent: 0, pass: true, knee: false },
    ]);
    expect(rows[3].cells[0]).toBeUndefined();
  });

  it('leaves out a step still in progress', () => {
    const r = run('live', 3000, [[15, 1500, 0, true]]);
    r.status!.steps!.push({ vms: 30, pass: false });
    expect(compareRows([r]).map((x) => x.vms)).toEqual([15]);
  });
});

describe('compareSeries', () => {
  it('gives each run its p95 against the VM count', () => {
    expect(compareSeries([t3000, t10k])).toEqual([
      { label: 't3000 (3900 GB, 3000 IOPS)', points: [{ x: 15, y: 1.5 }, { x: 30, y: 2.1 }, { x: 45, y: 25 }] },
      { label: 't10k (3900 GB, 10000 IOPS)', points: [{ x: 15, y: 1.4 }, { x: 30, y: 1.6 }, { x: 45, y: 1.7 }, { x: 60, y: 1.8 }] },
    ]);
  });
});
