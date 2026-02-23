import { consoleFetchJSON } from '@openshift-console/dynamic-plugin-sdk';
import { useEffect, useState, useCallback } from 'react';
import { METRICS } from '../constants';

const PROMETHEUS_BASE = '/api/prometheus/api/v1';

interface PrometheusResult {
  metric: Record<string, string>;
  value: [number, string];
}

interface PrometheusQueryResponse {
  status: string;
  data: {
    resultType: string;
    result: PrometheusResult[];
  };
}

interface PoolMetrics {
  capacityGB: number;
  allocatedGB: number;
  shareCount: number;
  pvcCount: number;
  allocationRate1h: number;
  p95LatencySeconds: number;
  replicationLagSeconds: number;
}

interface AggregateMetrics {
  apiCallRate5m: number;
  apiErrorRate5m: number;
  apiP95LatencySeconds: number;
  snapshotCount: number;
  cloneCount: number;
  cloneAvgDurationSeconds: number;
  groupSnapshotCount: number;
  replicationSyncRate5m: number;
  replicationConsecutiveFailures: Record<string, number>;
}

interface MetricsState {
  metrics: Record<string, PoolMetrics>;
  aggregate: AggregateMetrics;
  loading: boolean;
  error: string | null;
}

async function queryPrometheus(query: string): Promise<PrometheusResult[]> {
  try {
    const resp = (await consoleFetchJSON(
      `${PROMETHEUS_BASE}/query?query=${encodeURIComponent(query)}`,
    )) as PrometheusQueryResponse;
    if (resp.status === 'success') {
      return resp.data.result;
    }
    return [];
  } catch {
    return [];
  }
}

function resultToNumber(results: PrometheusResult[], pool?: string): number {
  const match = pool
    ? results.find((r) => r.metric.pool === pool)
    : results[0];
  return match ? parseFloat(match.value[1]) : 0;
}

function resultSum(results: PrometheusResult[]): number {
  return results.reduce((sum, r) => sum + parseFloat(r.value[1] || '0'), 0);
}

function resultsByLabel(results: PrometheusResult[], label: string): Record<string, number> {
  const map: Record<string, number> = {};
  for (const r of results) {
    const key = r.metric[label] || 'unknown';
    map[key] = parseFloat(r.value[1] || '0');
  }
  return map;
}

const emptyAggregate: AggregateMetrics = {
  apiCallRate5m: 0,
  apiErrorRate5m: 0,
  apiP95LatencySeconds: 0,
  snapshotCount: 0,
  cloneCount: 0,
  cloneAvgDurationSeconds: 0,
  groupSnapshotCount: 0,
  replicationSyncRate5m: 0,
  replicationConsecutiveFailures: {},
};

export function usePoolMetrics(poolNames: string[], refreshInterval = 30000): MetricsState {
  const [state, setState] = useState<MetricsState>({
    metrics: {},
    aggregate: emptyAggregate,
    loading: true,
    error: null,
  });

  const fetchMetrics = useCallback(async () => {
    try {
      const [
        capacity, allocated, shares, pvcs, rate, latency, lag,
        apiCallRate, apiErrorRate, apiLatency,
        snapshots, clones, cloneDuration, groupSnapshots,
        replicationSyncRate, replicationFailures,
      ] = await Promise.all([
        // Per-pool metrics
        queryPrometheus(METRICS.POOL_CAPACITY_GB),
        queryPrometheus(METRICS.POOL_ALLOCATED_GB),
        queryPrometheus(METRICS.POOL_SHARE_COUNT),
        queryPrometheus(METRICS.POOL_PVC_COUNT),
        queryPrometheus(`rate(${METRICS.ALLOCATIONS_TOTAL}[1h])`),
        queryPrometheus(`histogram_quantile(0.95, rate(${METRICS.ALLOCATION_DURATION}[5m]))`),
        queryPrometheus(METRICS.REPLICATION_LAG),
        // Aggregate metrics
        queryPrometheus(`sum(rate(${METRICS.VPC_API_CALLS_TOTAL}[5m]))`),
        queryPrometheus(`sum(rate(${METRICS.VPC_API_CALLS_TOTAL}{status="error"}[5m]))`),
        queryPrometheus(`histogram_quantile(0.95, sum(rate(${METRICS.VPC_API_CALL_DURATION}[5m])) by (le))`),
        queryPrometheus(METRICS.SNAPSHOTS_TOTAL),
        queryPrometheus(METRICS.CLONES_TOTAL),
        queryPrometheus(`histogram_quantile(0.5, rate(${METRICS.CLONE_DURATION}[1h]))`),
        queryPrometheus(METRICS.GROUP_SNAPSHOTS_TOTAL),
        queryPrometheus(`sum(rate(${METRICS.REPLICATION_SYNC_TOTAL}[5m]))`),
        queryPrometheus(METRICS.REPLICATION_CONSECUTIVE_FAILURES),
      ]);

      const metrics: Record<string, PoolMetrics> = {};
      for (const pool of poolNames) {
        metrics[pool] = {
          capacityGB: resultToNumber(capacity, pool),
          allocatedGB: resultToNumber(allocated, pool),
          shareCount: resultToNumber(shares, pool),
          pvcCount: resultToNumber(pvcs, pool),
          allocationRate1h: resultToNumber(rate, pool),
          p95LatencySeconds: resultToNumber(latency, pool),
          replicationLagSeconds: resultToNumber(lag, pool),
        };
      }

      const aggregate: AggregateMetrics = {
        apiCallRate5m: resultToNumber(apiCallRate) * 60,
        apiErrorRate5m: resultToNumber(apiErrorRate) * 60,
        apiP95LatencySeconds: resultToNumber(apiLatency),
        snapshotCount: resultSum(snapshots),
        cloneCount: resultSum(clones),
        cloneAvgDurationSeconds: resultToNumber(cloneDuration),
        groupSnapshotCount: resultSum(groupSnapshots),
        replicationSyncRate5m: resultToNumber(replicationSyncRate) * 60,
        replicationConsecutiveFailures: resultsByLabel(replicationFailures, 'policy'),
      };

      setState({ metrics, aggregate, loading: false, error: null });
    } catch (err) {
      setState((prev) => ({
        ...prev,
        loading: false,
        error: err instanceof Error ? err.message : 'Failed to fetch metrics',
      }));
    }
  }, [poolNames]);

  useEffect(() => {
    fetchMetrics();
    const interval = setInterval(fetchMetrics, refreshInterval);
    return () => clearInterval(interval);
  }, [fetchMetrics, refreshInterval]);

  return state;
}

export function usePrometheusQuery(query: string, refreshInterval = 30000) {
  const [results, setResults] = useState<PrometheusResult[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;

    const fetch = async () => {
      try {
        const data = await queryPrometheus(query);
        if (!cancelled) {
          setResults(data);
          setLoading(false);
          setError(null);
        }
      } catch (err) {
        if (!cancelled) {
          setLoading(false);
          setError(err instanceof Error ? err.message : 'Query failed');
        }
      }
    };

    fetch();
    const interval = setInterval(fetch, refreshInterval);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [query, refreshInterval]);

  return { results, loading, error };
}

// --- Prometheus range query types and hook ---

export interface TimeRange {
  label: string;
  seconds: number;
  step: string;
}

export const TIME_RANGES: TimeRange[] = [
  { label: '1h', seconds: 3600, step: '30s' },
  { label: '6h', seconds: 21600, step: '120s' },
  { label: '24h', seconds: 86400, step: '300s' },
  { label: '7d', seconds: 604800, step: '1800s' },
];

export interface PrometheusRangeResult {
  metric: Record<string, string>;
  values: [number, string][];
}

interface PrometheusRangeQueryResponse {
  status: string;
  data: {
    resultType: string;
    result: PrometheusRangeResult[];
  };
}

export function usePrometheusRange(
  query: string,
  range: TimeRange,
  refreshInterval = 60000,
): { series: PrometheusRangeResult[]; loading: boolean; error: string | null } {
  const [series, setSeries] = useState<PrometheusRangeResult[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState<string | null>(null);

  useEffect(() => {
    let cancelled = false;

    const fetchRange = async () => {
      try {
        const end = Math.floor(Date.now() / 1000);
        const start = end - range.seconds;
        const url = `${PROMETHEUS_BASE}/query_range?query=${encodeURIComponent(query)}&start=${start}&end=${end}&step=${range.step}`;
        const resp = (await consoleFetchJSON(url)) as PrometheusRangeQueryResponse;
        if (!cancelled && resp.status === 'success') {
          setSeries(resp.data.result);
          setLoading(false);
          setError(null);
        }
      } catch (err) {
        if (!cancelled) {
          setLoading(false);
          setError(err instanceof Error ? err.message : 'Range query failed');
        }
      }
    };

    fetchRange();
    const interval = setInterval(fetchRange, refreshInterval);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [query, range.seconds, range.step, refreshInterval]);

  return { series, loading, error };
}
