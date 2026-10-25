import React from 'react';
import { render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import PoolBenchListPage from './PoolBenchListPage';

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

const runs = [
  {
    metadata: { name: 't3000', creationTimestamp: '2026-10-24T20:00:00Z' },
    spec: { shareSizeGB: 3900, iops: 3000 },
    status: {
      phase: 'Complete', kneeVMs: 45, recommendedVMs: 30, vms: 45,
      startTime: '2026-10-24T20:00:00Z', endTime: '2026-10-25T02:00:00Z',
    },
  },
  {
    metadata: { name: 't10k', creationTimestamp: '2026-10-25T03:00:00Z' },
    spec: { shareSizeGB: 3900, iops: 10000 },
    status: { phase: 'Running', vms: 60, windowsVMs: 20, step: 4, totalSteps: 6, startTime: '2026-10-25T03:00:00Z' },
  },
  {
    metadata: { name: 'bad' },
    spec: { shareSizeGB: 1000, iops: 5000 },
    status: { phase: 'Failed', message: 'The image openshift-virtualization-os-images/win2k22-perf does not exist.' },
  },
];

beforeEach(() => {
  jest.clearAllMocks();
  (useK8sWatchResource as jest.Mock).mockReturnValue([runs, true, undefined]);
});

describe('PoolBenchListPage', () => {
  it('shows one row per run with the share, state, VMs, knee and recommended count', () => {
    render(<PoolBenchListPage />);
    const row = screen.getByRole('link', { name: 't3000' }).closest('tr')!;
    expect(row.textContent).toContain('3900 GB');
    expect(row.textContent).toContain('3000 IOPS');
    expect(row.textContent).toContain('Complete');
    expect(row.textContent).toContain('45');
    expect(row.textContent).toContain('30');
    expect(row.textContent).toContain('6h 0m');
    expect(screen.getByRole('link', { name: 't3000' }).getAttribute('href')).toBe('/vpc-file-pools/poolbench/t3000');
  });

  it('shows the step of a running run and a dash for a knee not found yet', () => {
    render(<PoolBenchListPage />);
    const row = screen.getByRole('link', { name: 't10k' }).closest('tr')!;
    expect(row.textContent).toContain('Running');
    expect(row.textContent).toContain('step 4 of 6');
    expect(row.textContent).toContain('60');
  });

  it('says in plain words why a run failed', () => {
    render(<PoolBenchListPage />);
    const row = screen.getByRole('link', { name: 'bad' }).closest('tr')!;
    expect(row.textContent).toContain('win2k22-perf does not exist');
  });

  it('explains what PoolBench is when there is no run', () => {
    (useK8sWatchResource as jest.Mock).mockReturnValue([[], true, undefined]);
    render(<PoolBenchListPage />);
    expect(screen.getByText(/No runs yet/)).toBeTruthy();
    expect(screen.getAllByText(/how many VMs one share carries/).length).toBeGreaterThan(0);
  });

  it('says when the PoolBench resource is not installed', () => {
    (useK8sWatchResource as jest.Mock).mockReturnValue([undefined, true, { message: 'the server could not find the requested resource' }]);
    render(<PoolBenchListPage />);
    expect(screen.getByText(/PoolBench is not installed/)).toBeTruthy();
  });
});
