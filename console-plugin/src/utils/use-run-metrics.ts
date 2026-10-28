import { consoleFetchJSON } from '@openshift-console/dynamic-plugin-sdk';
import { useEffect, useMemo, useState } from 'react';
import { ChartSpec, ChartWindow } from './poolbench-queries';

const PROMETHEUS_BASE = '/api/prometheus/api/v1';
/** How often the graphs of a running run are read again. */
const REFRESH_MS = 30000;

export interface Point {
  x: number;
  y: number;
}

export interface SeriesData {
  label: string;
  points: Point[];
}

interface RangeResponse {
  status: string;
  data: { result: { metric: Record<string, string>; values: [number, string][] }[] };
}

interface VectorResponse {
  status: string;
  data: { result: { metric: Record<string, string>; value: [number, string] }[] };
}

/** Points of the first series of a range answer; NaN and Inf mean no data and are left out. */
const toPoints = (resp: RangeResponse): Point[] =>
  (resp.data.result[0]?.values || [])
    .map(([x, v]) => ({ x, y: parseFloat(v) }))
    .filter((p) => Number.isFinite(p.y));

/**
 * Reads every series of the run's graphs over the window, and again every 30
 * seconds while the run is live.
 */
export const useRunMetrics = (
  specs: ChartSpec[],
  win: ChartWindow | undefined,
  live: boolean,
): { charts: Record<string, SeriesData[]>; loading: boolean; error: string | null } => {
  const [charts, setCharts] = useState<Record<string, SeriesData[]>>({});
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);
  const key = useMemo(() => JSON.stringify(specs.map((s) => s.series)), [specs]);

  useEffect(() => {
    if (!win) return undefined;
    let cancelled = false;
    const read = async () => {
      const end = live ? Math.floor(Date.now() / 1000) : win.end;
      try {
        const out: Record<string, SeriesData[]> = {};
        for (const spec of specs) {
          out[spec.title] = await Promise.all(
            spec.series.map(async (s) => {
              const url = `${PROMETHEUS_BASE}/query_range?query=${encodeURIComponent(s.query)}&start=${win.start}&end=${end}&step=${win.step}`;
              const resp = (await consoleFetchJSON(url)) as RangeResponse;
              return { label: s.label, points: resp.status === 'success' ? toPoints(resp) : [] };
            }),
          );
        }
        if (!cancelled) {
          setCharts(out);
          setError(null);
        }
      } catch (err) {
        if (!cancelled) setError(err instanceof Error ? err.message : 'Prometheus could not be read');
      } finally {
        if (!cancelled) setLoading(false);
      }
    };
    read();
    const timer = live ? setInterval(read, REFRESH_MS) : undefined;
    return () => {
      cancelled = true;
      if (timer) clearInterval(timer);
    };
    // eslint-disable-next-line react-hooks/exhaustive-deps -- key stands for specs
  }, [key, win?.start, win?.end, win?.step, live]);

  return { charts, loading, error };
};

/** The last steady IOPS and p95 of each test VM of a run, read every 30 seconds while live. */
export const useVMNumbers = (run: string, live: boolean): Record<string, { iops: number; p95Ms: number }> => {
  const [numbers, setNumbers] = useState<Record<string, { iops: number; p95Ms: number }>>({});
  useEffect(() => {
    if (!live) return undefined;
    let cancelled = false;
    const query = async (q: string) =>
      (await consoleFetchJSON(`${PROMETHEUS_BASE}/query?query=${encodeURIComponent(q)}`)) as VectorResponse;
    const read = async () => {
      try {
        const [iops, p95] = await Promise.all([
          query(`poolbench_vm_iops{run="${run}",phase="steady"}`),
          query(`poolbench_vm_latency_seconds{run="${run}",phase="steady",quantile="0.95"}`),
        ]);
        const out: Record<string, { iops: number; p95Ms: number }> = {};
        for (const r of iops.data.result) out[r.metric.vm] = { iops: parseFloat(r.value[1]), p95Ms: NaN };
        for (const r of p95.data.result) {
          out[r.metric.vm] = { iops: out[r.metric.vm]?.iops ?? NaN, p95Ms: Math.round(parseFloat(r.value[1]) * 10000) / 10 };
        }
        if (!cancelled) setNumbers(out);
      } catch {
        // The VM table still shows the VMs; the numbers stay as they were.
      }
    };
    read();
    const timer = setInterval(read, REFRESH_MS);
    return () => {
      cancelled = true;
      clearInterval(timer);
    };
  }, [run, live]);
  return numbers;
};
