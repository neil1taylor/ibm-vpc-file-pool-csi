import { METRICS } from '../constants';
import { metricQueries } from './metric-queries';

describe('metricQueries', () => {
  it('queries allocation latency per pool from the _bucket series', () => {
    expect(metricQueries.allocationP95ByPool).toBe(
      `histogram_quantile(0.95, sum(rate(${METRICS.ALLOCATION_DURATION}_bucket[5m])) by (pool, le))`,
    );
  });

  it('queries API latency from the _bucket series', () => {
    expect(metricQueries.apiP95).toBe(
      `histogram_quantile(0.95, sum(rate(${METRICS.VPC_API_CALL_DURATION}_bucket[5m])) by (le))`,
    );
  });

  it('queries median clone duration from the _bucket series', () => {
    expect(metricQueries.cloneMedian).toBe(
      `histogram_quantile(0.5, sum(rate(${METRICS.CLONE_DURATION}_bucket[1h])) by (le))`,
    );
  });

  it('charts use the same _bucket form, scaled to milliseconds', () => {
    expect(metricQueries.apiP95Ms).toBe(`${metricQueries.apiP95} * 1000`);
    expect(metricQueries.allocationP95Ms).toBe(
      `histogram_quantile(0.95, sum(rate(${METRICS.ALLOCATION_DURATION}_bucket[5m])) by (le)) * 1000`,
    );
  });

  it('never queries a histogram by its bare name', () => {
    for (const q of Object.values(metricQueries)) {
      expect(q).not.toMatch(/_seconds\[/);
    }
  });
});
