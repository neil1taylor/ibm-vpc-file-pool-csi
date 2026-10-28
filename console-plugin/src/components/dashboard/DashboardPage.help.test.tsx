import React from 'react';
import { render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import DashboardPage from './DashboardPage';

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
  useLocation: () => ({ pathname: '/vpc-file-pools/overview' }),
}));
jest.mock('../../utils/use-pool-metrics', () => ({
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
}));
jest.mock('./CapacityDonut', () => ({ __esModule: true, default: () => null }));

beforeEach(() => {
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind: { kind: string } }) => [
    req.groupVersionKind.kind === 'ReplicationPolicy'
      ? [{ metadata: { name: 'rp-1' }, spec: { sourcePoolName: 'p' } }]
      : [],
    true,
    undefined,
  ]);
});

describe('DashboardPage help', () => {
  it('gives every number card a labelled help button', () => {
    render(<DashboardPage />);
    const titles = [
      'Total Pools',
      'Total Capacity',
      'Total SubVolumes',
      'Active Alerts',
      'Allocation Rate',
      'P95 Allocation Latency',
      'VPC API Health',
      'Aggregate Capacity',
      'Pool Capacity',
      'Replication Lag',
      'Sync Rate',
      'Consecutive Failures',
      'Snapshots',
      'Clones',
      'Group Snapshots',
      'Firing Alerts',
    ];
    for (const t of titles) {
      expect(screen.getByRole('button', { name: `About ${t}` })).toBeTruthy();
    }
  });

  it('labels the clone time as the median, which is what it shows', () => {
    render(<DashboardPage />);
    expect(screen.getByText('Median duration')).toBeTruthy();
    expect(screen.queryByText('Avg duration')).toBeNull();
  });
});
