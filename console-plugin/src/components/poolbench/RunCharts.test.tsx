import React from 'react';
import { render, screen } from '@testing-library/react';
import RunCharts, { clock } from './RunCharts';
import { useRunMetrics } from '../../utils/use-run-metrics';
import { PoolBench } from '../../types';

jest.mock('../../utils/use-run-metrics', () => ({ useRunMetrics: jest.fn() }));

// The charts draw SVG through Victory; the test checks what is given to them.
jest.mock('@patternfly/react-charts', () => ({
  Chart: ({ children, ariaTitle }: { children: React.ReactNode; ariaTitle?: string }) => (
    <div data-testid="chart" aria-label={ariaTitle}>
      {children}
    </div>
  ),
  ChartAxis: () => null,
  ChartGroup: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  ChartLegend: () => null,
  ChartLine: ({ name, data }: { name: string; data: unknown[] }) => (
    <div data-testid="line">
      {name}:{data.length}
    </div>
  ),
  ChartVoronoiContainer: () => null,
  ChartThemeColor: { multiOrdered: 'multi' },
}));

const pb: PoolBench = {
  metadata: { name: 't10k' },
  spec: { shareSizeGB: 3900, iops: 10000, knee: { latencyLimitMs: 20, slowVMPercent: 25 } },
  status: {
    phase: 'Running',
    startTime: '2026-10-25T08:00:00Z',
    steps: [
      { vms: 15, pass: true, started: '2026-10-25T08:20:00Z', ended: '2026-10-25T09:05:00Z' },
      { vms: 30, pass: false, started: '2026-10-25T09:30:00Z' },
    ],
  },
};

beforeEach(() => {
  (useRunMetrics as jest.Mock).mockReturnValue({
    charts: {
      IOPS: [
        { label: 'Share', points: [{ x: 1761379200, y: 700 }, { x: 1761379230, y: 768 }] },
        { label: 'Guests', points: [] },
      ],
    },
    loading: false,
    error: null,
  });
});

describe('RunCharts', () => {
  it('draws the five graphs', () => {
    render(<RunCharts pb={pb} />);
    for (const t of ['VMs running', 'IOPS', 'Latency', 'Throughput', 'Slow VMs']) {
      expect(screen.getByText(t)).toBeTruthy();
    }
  });

  it('draws each series with data, and the share limit line', () => {
    render(<RunCharts pb={pb} />);
    const lines = screen.getAllByTestId('line').map((l) => l.textContent);
    expect(lines).toContain('Share:2');
    expect(lines).toContain('Share limit:2');
    expect(lines).not.toContain('Guests:0');
  });

  it('marks the start of each step', () => {
    render(<RunCharts pb={pb} />);
    const lines = screen.getAllByTestId('line').map((l) => l.textContent);
    expect(lines).toContain('Step 1 (15 VMs):2');
    expect(lines).toContain('Step 2 (30 VMs):2');
  });

  it('says there is no data yet for a graph without points', () => {
    render(<RunCharts pb={pb} />);
    expect(screen.getAllByText(/No data yet/).length).toBe(4);
  });

  it('says when Prometheus cannot be read', () => {
    (useRunMetrics as jest.Mock).mockReturnValue({ charts: {}, loading: false, error: '403 Forbidden' });
    render(<RunCharts pb={pb} />);
    expect(screen.getByText(/The graphs need Prometheus: 403 Forbidden/)).toBeTruthy();
  });

  it('draws nothing before the run starts', () => {
    render(<RunCharts pb={{ ...pb, status: {} }} />);
    expect(screen.getByText(/The graphs start when the run starts/)).toBeTruthy();
  });
});

describe('clock', () => {
  const at = new Date(2026, 9, 25, 9, 5);

  it('writes the time of a Date', () => {
    expect(clock(at)).toBe('09:05');
  });

  it('writes the time of a number of milliseconds, which is what the axis hands over', () => {
    expect(clock(at.getTime())).toBe('09:05');
  });
});
