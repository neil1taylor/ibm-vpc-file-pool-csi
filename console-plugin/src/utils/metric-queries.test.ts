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

  it('charts the group snapshot inconsistency window from the _bucket series, already in milliseconds', () => {
    expect(metricQueries.groupSnapshotWindowP95Ms).toBe(
      `histogram_quantile(0.95, sum(rate(${METRICS.GROUP_SNAPSHOT_INCONSISTENCY_WINDOW_MS}_bucket[5m])) by (le))`,
    );
  });

  it('charts the average number of members per group snapshot from the _sum and _count series', () => {
    expect(metricQueries.groupSnapshotMembersAvg).toBe(
      `sum(rate(${METRICS.GROUP_SNAPSHOT_MEMBER_COUNT}_sum[1h])) / sum(rate(${METRICS.GROUP_SNAPSHOT_MEMBER_COUNT}_count[1h]))`,
    );
  });

  it('counts the times the controller raised the CDI copy limit in the last hour', () => {
    expect(metricQueries.cdiLimitRaises1h).toBe(`sum(increase(${METRICS.CDI_COPY_LIMIT_RAISED_TOTAL}[1h]))`);
  });
});
