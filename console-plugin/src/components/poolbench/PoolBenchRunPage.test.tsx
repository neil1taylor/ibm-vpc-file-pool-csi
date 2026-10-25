import React from 'react';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { consoleFetch, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import PoolBenchRunPage from './PoolBenchRunPage';
import { PoolBench } from '../../types';

jest.mock('./RunCharts', () => ({ __esModule: true, default: () => <div data-testid="run-charts" /> }));
jest.mock('../../utils/use-run-metrics', () => ({
  useVMNumbers: () => ({ 'vm-001': { iops: 29, p95Ms: 4.2 } }),
}));

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
  useParams: () => ({ name: 't3000' }),
  useHistory: () => ({ push: jest.fn() }),
}));

const run = (): PoolBench => ({
  metadata: { name: 't3000' },
  spec: {
    shareSizeGB: 3900, iops: 3000, stepVMs: 15, maxVMs: 90, windowsEvery: 3,
    settleMinutes: 15, measureMinutes: 45,
    load: { steadyIOPS: 30, burstIOPS: 300, burstSeconds: 45, periodSeconds: 600, readPercent: 70, blockMix: '4k:50 16k:30 64k:20', areaGiB: 3 },
    knee: { latencyLimitMs: 20, slowVMPercent: 25, stallSeconds: 5 },
  },
  status: {
    phase: 'Running', stage: 'Measuring', step: 3, totalSteps: 6, vms: 45, windowsVMs: 15, readyVMs: 45,
    namespace: 'poolbench-t3000', poolName: 'poolbench-t3000',
    startTime: '2026-10-25T08:00:00Z',
    stageStarted: new Date(Date.now() - 25 * 60000).toISOString(),
    stageEnds: new Date(Date.now() + 20 * 60000).toISOString(),
    kneeVMs: 0,
    recommendedVMs: 30,
    steps: [
      { vms: 15, windowsVMs: 5, shareIOPS: 768, guestIOPS: 785, ibmOps: 1256, shareLatencyMicros: 300, p50Micros: 600, p95Micros: 1500, p99Micros: 4000, slowPercent: 0, stalls: 0, readySeconds: 360, pass: true, ended: '2026-10-25T09:00:00Z' },
      { vms: 30, windowsVMs: 10, shareIOPS: 1526, guestIOPS: 1585, ibmOps: 2536, shareLatencyMicros: 400, p95Micros: 2100, slowPercent: 0, stalls: 0, readySeconds: 420, pass: true, ended: '2026-10-25T10:00:00Z' },
      { vms: 45, windowsVMs: 15, pass: false },
    ],
    log: [
      { time: '2026-10-25T08:00:00Z', kind: 'Info', message: 'Run started: 3900 GB share at 3000 IOPS' },
      { time: '2026-10-25T09:00:00Z', kind: 'Control', message: 'Paused' },
      { time: '2026-10-25T09:05:00Z', kind: 'Warning', message: 'Cannot read the share counters' },
    ],
  },
});

const vmis = [
  { metadata: { name: 'vm-001', labels: { 'storage.ibmcloud.io/poolbench-os': 'linux' } }, status: { phase: 'Running', nodeName: 'w1' } },
  { metadata: { name: 'vm-003', labels: { 'storage.ibmcloud.io/poolbench-os': 'windows' } }, status: { phase: 'Scheduling' } },
];

let current: PoolBench;

beforeEach(() => {
  jest.clearAllMocks();
  current = run();
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { kind?: string; groupVersionKind?: { kind: string } } | null) => {
    if (!req) return [undefined, true, undefined];
    if (req.groupVersionKind?.kind === 'VirtualMachineInstance') return [vmis, true, undefined];
    return [current, true, undefined];
  });
});

describe('PoolBenchRunPage', () => {
  it('shows the live status line with the step, the VMs running and the time left', () => {
    render(<PoolBenchRunPage />);
    expect(screen.getByText(/Step 3 of 6: 45 VMs running \(15 Windows\), measuring, 20 min left/)).toBeTruthy();
    expect(screen.getByText(/Expected end/)).toBeTruthy();
  });

  it('shows the settings of the run in plain words', () => {
    render(<PoolBenchRunPage />);
    const card = screen.getByTestId('poolbench-settings');
    expect(card.textContent).toContain('3900 GB at 3000 IOPS');
    expect(card.textContent).toContain('15 VMs per step, up to 90');
    expect(card.textContent).toContain('1 in 3 Windows');
    expect(card.textContent).toContain('30 IOPS steady, 300 IOPS bursts of 45 s every 600 s');
    expect(card.textContent).toContain('p95 over 20 ms on more than 25% of the VMs');
  });

  it('shows one row per step, like HCIBench, with the step in progress last', () => {
    render(<PoolBenchRunPage />);
    const table = screen.getByRole('grid', { name: 'Step results' });
    const rows = within(table).getAllByRole('row');
    expect(rows).toHaveLength(4);
    expect(rows[1].textContent).toContain('15 (5 Windows)');
    expect(rows[1].textContent).toContain('768');
    expect(rows[1].textContent).toContain('42%');
    expect(rows[1].textContent).toContain('1.5 ms');
    expect(rows[1].textContent).toContain('Pass');
    expect(rows[2].textContent).toContain('Recommended');
    expect(rows[3].textContent).toContain('In progress');
  });

  it('shows the controls of a running run', () => {
    render(<PoolBenchRunPage />);
    expect(screen.getByRole('button', { name: 'Pause' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Stop and clean up' })).toBeTruthy();
  });

  it('shows the live graphs', () => {
    render(<PoolBenchRunPage />);
    expect(screen.getByTestId('run-charts')).toBeTruthy();
  });

  it('lists the test VMs with their OS, node, state, last IOPS and p95', () => {
    render(<PoolBenchRunPage />);
    const table = screen.getByRole('grid', { name: 'Test VMs' });
    const row = within(table).getByText('vm-001').closest('tr')!;
    expect(row.textContent).toContain('29');
    expect(row.textContent).toContain('4.2 ms');
    expect(table.textContent).toContain('vm-001');
    expect(table.textContent).toContain('Linux');
    expect(table.textContent).toContain('w1');
    expect(table.textContent).toContain('Windows');
    expect(table.textContent).toContain('Scheduling');
  });

  it("opens a VM's own result lines", async () => {
    (consoleFetch as jest.Mock).mockResolvedValue({ text: () => Promise.resolve('PERF-READY\nPERF 1700000600 steady {}\n') });
    render(<PoolBenchRunPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Lines of vm-001' }));
    await waitFor(() => expect(screen.getByText(/PERF 1700000600 steady/)).toBeTruthy());
    expect((consoleFetch as jest.Mock).mock.calls[0][0]).toBe(
      '/api/proxy/plugin/ibm-vpc-file-pool-csi/poolbench/v1/runs/t3000/vms/vm-001/log',
    );
  });

  it('shows the run log, newest first', () => {
    render(<PoolBenchRunPage />);
    const log = screen.getByTestId('poolbench-log');
    const text = log.textContent || '';
    expect(text.indexOf('Cannot read the share counters')).toBeLessThan(text.indexOf('Run started'));
    expect(text).toContain('Warning');
  });

  it('explains the result when the run is complete', () => {
    current = run();
    const steps = [...(current.status?.steps || [])];
    steps[2] = { ...steps[1], vms: 45, windowsVMs: 15, pass: false, slowPercent: 76 };
    current.status = { ...current.status, phase: 'Complete', stage: undefined, kneeVMs: 45, recommendedVMs: 30, message: 'Knee at 45 VMs.', steps };
    render(<PoolBenchRunPage />);
    expect(screen.getByText(/Users would notice at 45 VMs on this 3900 GB share at 3000 IOPS. Recommended: up to 30 VMs./)).toBeTruthy();
    const rows = within(screen.getByRole('grid', { name: 'Step results' })).getAllByRole('row');
    expect(rows[3].textContent).toContain('Knee');
    expect(rows[3].textContent).toContain('Fail');
  });

  it('warns while the test VMs and pool are kept after the end', () => {
    current = run();
    current.spec = { ...current.spec, keepAfterEnd: true };
    current.status = { ...current.status, phase: 'Complete', cleanedUp: false };
    render(<PoolBenchRunPage />);
    expect(screen.getByText(/kept after the end, and they cost money/)).toBeTruthy();
  });
});

describe('PoolBenchRunPage help', () => {
  it('explains every card, column and setting for a new user', () => {
    render(<PoolBenchRunPage />);
    for (const t of ['Step results', 'Settings', 'Test VMs', 'Run log']) {
      expect(screen.getByLabelText(`About ${t}`)).toBeTruthy();
    }
    for (const c of ['VMs', 'Share IOPS', 'Guest IOPS', 'IBM operations', 'Share latency', 'p50', 'p95', 'p99', 'Slow VMs', 'Stalls', 'Ready after', 'Result']) {
      expect(screen.getAllByLabelText(`About the ${c} column`).length).toBeGreaterThan(0);
    }
    for (const c of ['OS', 'Node', 'State', 'Last IOPS', 'Last p95']) {
      expect(screen.getAllByLabelText(`About the ${c} column`).length).toBeGreaterThan(0);
    }
    for (const t of ['Share', 'VMs', 'Load per VM', 'Each step', 'Knee rule']) {
      expect(within(screen.getByTestId('poolbench-settings')).getByLabelText(`About ${t}`)).toBeTruthy();
    }
  });

  it('explains the state of the run next to its name', () => {
    render(<PoolBenchRunPage />);
    expect(screen.getByLabelText('About the State of a run')).toBeTruthy();
  });
});
