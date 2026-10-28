import { PoolBench } from '../types';

const DEFAULT_BLOCK_MIX = '4k:50 16k:30 64k:20';

/** Points per graph; the step grows for a long run so a graph stays light. */
const POINTS = 240;
const MIN_STEP_SECONDS = 30;

/**
 * How many share operations IBM counts, on average, for one I/O of the block mix:
 * one for every 16 KiB of an I/O. The same rule as the Go service (IBMOpsPerIO).
 * A mix it cannot read counts 1.
 */
export const ibmOpsPerIO = (blockMix: string | undefined): number => {
  let total = 0;
  let percent = 0;
  for (const entry of (blockMix ?? DEFAULT_BLOCK_MIX).trim().split(/\s+/)) {
    const m = /^(\d+)([km]):(\d+)$/i.exec(entry);
    if (!m) return 1;
    const kib = Number(m[1]) * (m[2].toLowerCase() === 'm' ? 1024 : 1);
    total += (Number(m[3]) / 100) * Math.ceil(kib / 16);
    percent += Number(m[3]);
  }
  return percent === 100 ? Math.round(total * 10) / 10 : 1;
};

export interface ChartSeries {
  label: string;
  query: string;
}

export interface ChartSpec {
  title: string;
  yLabel: string;
  series: ChartSeries[];
  /** Flat lines to compare with, such as the share's IOPS limit. */
  lines: { label: string; y: number }[];
}

/** The five live graphs of a run, as Prometheus queries. */
export const runCharts = (pb: PoolBench): ChartSpec[] => {
  const run = `run="${pb.metadata?.name}"`;
  const pool = `pool="${pb.status?.poolName || `poolbench-${pb.metadata?.name}`}"`;
  const rw = 'op=~"read|write"';
  const p95 = `poolbench_vm_latency_seconds{${run},phase="steady",quantile="0.95"}`;
  const limitMs = pb.spec.knee?.latencyLimitMs ?? 20;
  const slowPct = pb.spec.knee?.slowVMPercent ?? 25;
  const shareOps = `sum(rate(vpc_file_pool_share_nfs_operations_total{${pool},${rw}}[5m]))`;
  return [
    {
      title: 'VMs running',
      yLabel: 'VMs',
      series: [
        { label: 'Linux', query: `poolbench_run_vms{${run},os="linux"}` },
        { label: 'Windows', query: `poolbench_run_vms{${run},os="windows"}` },
        { label: 'Not ready', query: `sum(poolbench_run_vms{${run}}) - sum(poolbench_run_ready_vms{${run}})` },
      ],
      lines: [],
    },
    {
      title: 'IOPS',
      yLabel: 'IOPS',
      series: [
        { label: 'Share', query: shareOps },
        { label: 'Guests', query: `sum(poolbench_vm_iops{${run},phase="steady"})` },
        { label: 'IBM operations', query: `sum(poolbench_vm_iops{${run},phase="steady"}) * ${ibmOpsPerIO(pb.spec.load?.blockMix)}` },
      ],
      lines: [{ label: 'Share limit', y: pb.spec.iops }],
    },
    {
      title: 'Latency',
      yLabel: 'ms',
      series: [
        { label: 'Median VM p95', query: `1000 * quantile(0.5, ${p95})` },
        { label: 'Worst VM p95', query: `1000 * max(${p95})` },
        {
          label: 'Share',
          query: `1000 * sum(rate(vpc_file_pool_share_nfs_execute_seconds_total{${pool},${rw}}[5m])) / ${shareOps}`,
        },
      ],
      lines: [{ label: `Limit ${limitMs} ms`, y: limitMs }],
    },
    {
      title: 'Throughput',
      yLabel: 'MB/s',
      series: [{ label: 'Guests', query: `sum(poolbench_vm_throughput_bytes_per_second{${run},phase="steady"}) / 1e6` }],
      lines: [],
    },
    {
      title: 'Slow VMs',
      yLabel: '% of VMs',
      series: [
        {
          label: `p95 over ${limitMs} ms`,
          query: `100 * (count(${p95} > ${limitMs / 1000}) or vector(0)) / count(${p95})`,
        },
      ],
      lines: [{ label: `Knee ${slowPct}%`, y: slowPct }],
    },
  ];
};

export interface ChartWindow {
  start: number;
  end: number;
  step: number;
}

/** The time the graphs show: the whole run, to now while it runs. In seconds. */
export const chartWindow = (pb: PoolBench, now: Date): ChartWindow | undefined => {
  const startTime = pb.status?.startTime;
  if (!startTime) return undefined;
  const start = Date.parse(startTime) / 1000;
  const end = (pb.status?.endTime ? Date.parse(pb.status.endTime) : now.getTime()) / 1000;
  return { start, end, step: Math.max(MIN_STEP_SECONDS, Math.ceil((end - start) / POINTS)) };
};
