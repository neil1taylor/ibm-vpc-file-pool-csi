import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import MonitoringPage from './MonitoringPage';
import { metricQueries } from '../../utils/metric-queries';

jest.mock('../../utils/use-pool-metrics', () => ({
  TIME_RANGES: [{ label: '1h', seconds: 3600, step: 60 }],
  usePoolMetrics: () => ({
    metrics: {},
    aggregate: {
      apiCallRate5m: 0,
      apiErrorRate5m: 0,
      apiP95LatencySeconds: 0,
      snapshotCount: 0,
      cloneCount: 0,
      cloneAvgDurationSeconds: 0,
      groupSnapshotCount: 0,
      replicationSyncRate5m: 0,
      replicationConsecutiveFailures: {},
    },
    loading: false,
    error: null,
  }),
  checkPrometheusConnection: () => Promise.resolve({ connected: true, hasMetrics: true }),
}));

const charts: string[] = [];
jest.mock('./TimeSeriesChart', () => ({
  __esModule: true,
  default: ({ query }: { query: string }) => {
    charts.push(query);
    return <div data-query={query} />;
  },
}));
jest.mock('../dashboard/CapacityDonut', () => ({ __esModule: true, default: () => null }));
jest.mock('../common/CapacityBar', () => ({ __esModule: true, default: () => null }));

beforeEach(() => {
  charts.length = 0;
  (useK8sWatchResource as jest.Mock).mockReturnValue([[], true, undefined]);
});

describe('MonitoringPage group snapshot and CDI section', () => {
  it('is closed at first', () => {
    render(<MonitoringPage />);
    expect(screen.getByText('Show Group Snapshot and CDI Activity')).toBeTruthy();
    expect(screen.getByText('Group Snapshot Members (average)').closest('[hidden]')).toBeTruthy();
  });

  it('charts the inconsistency window, the member count and the CDI limit raises when opened', () => {
    render(<MonitoringPage />);
    fireEvent.click(screen.getByText('Show Group Snapshot and CDI Activity'));
    expect(screen.getByText('Group Snapshot Inconsistency Window (P95)')).toBeTruthy();
    expect(screen.getByText('Group Snapshot Members (average)')).toBeTruthy();
    expect(screen.getByText('CDI Copy Limit Raises (per hour)')).toBeTruthy();
    expect(charts).toEqual(
      expect.arrayContaining([metricQueries.groupSnapshotWindowP95Ms, metricQueries.groupSnapshotMembersAvg, metricQueries.cdiLimitRaises1h]),
    );
  });

  it('explains why a CDI raise matters', () => {
    render(<MonitoringPage />);
    fireEvent.click(screen.getByText('Show Group Snapshot and CDI Activity'));
    expect(screen.getByText(/something reset the limit/i)).toBeTruthy();
  });
});
