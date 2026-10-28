import { PoolBench } from '../types';

export interface CompareCell {
  p95Ms: number;
  slowPercent: number;
  pass: boolean;
  knee: boolean;
}

export interface CompareRow {
  vms: number;
  /** One cell per run, in the order given; undefined where the run has no such step. */
  cells: (CompareCell | undefined)[];
}

/** A run named by its share, for the column heads and the legend. */
export const runLabel = (pb: PoolBench): string =>
  `${pb.metadata?.name} (${pb.spec.shareSizeGB} GB, ${pb.spec.iops} IOPS)`;

const measured = (pb: PoolBench) => (pb.status?.steps || []).filter((s) => s.ended);

const toMs = (micros?: number): number => Math.round((micros ?? 0) / 100) / 10;

/** The steps of several runs side by side: one row per VM count that any run measured. */
export const compareRows = (runs: PoolBench[]): CompareRow[] => {
  const counts = Array.from(new Set(runs.flatMap((pb) => measured(pb).map((s) => s.vms)))).sort((a, b) => a - b);
  return counts.map((vms) => ({
    vms,
    cells: runs.map((pb) => {
      const s = measured(pb).find((x) => x.vms === vms);
      return s
        ? { p95Ms: toMs(s.p95Micros), slowPercent: s.slowPercent ?? 0, pass: s.pass, knee: pb.status?.kneeVMs === vms }
        : undefined;
    }),
  }));
};

/** Each run's median VM p95 (ms) against the VM count, for one graph of all runs. */
export const compareSeries = (runs: PoolBench[]): { label: string; points: { x: number; y: number }[] }[] =>
  runs.map((pb) => ({ label: runLabel(pb), points: measured(pb).map((s) => ({ x: s.vms, y: toMs(s.p95Micros) })) }));
