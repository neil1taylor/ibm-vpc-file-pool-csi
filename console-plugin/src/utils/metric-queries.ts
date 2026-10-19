import { METRICS } from '../constants';

const p95 = (name: string, window: string, by: string) =>
  `histogram_quantile(0.95, sum(rate(${name}_bucket[${window}])) by (${by}))`;

const apiP95 = p95(METRICS.VPC_API_CALL_DURATION, '5m', 'le');
const allocationP95 = p95(METRICS.ALLOCATION_DURATION, '5m', 'le');

export const metricQueries = {
  allocationP95ByPool: p95(METRICS.ALLOCATION_DURATION, '5m', 'pool, le'),
  apiP95,
  cloneMedian: `histogram_quantile(0.5, sum(rate(${METRICS.CLONE_DURATION}_bucket[1h])) by (le))`,
  apiP95Ms: `${apiP95} * 1000`,
  allocationP95Ms: `${allocationP95} * 1000`,
};
