import React from 'react';
import { render, screen, within } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import PoolBenchComparePage from './PoolBenchComparePage';

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
  useLocation: () => ({ search: '?runs=t3000,t10k' }),
}));

jest.mock('@patternfly/react-charts', () => ({
  Chart: ({ children }: { children: React.ReactNode }) => <div data-testid="chart">{children}</div>,
  ChartAxis: ({ style }: { style?: { tickLabels?: { fill?: string } } }) => (
    <div data-testid="axis" data-tick={style?.tickLabels?.fill} />
  ),
  ChartGroup: ({ children }: { children: React.ReactNode }) => <div>{children}</div>,
  ChartLegend: () => null,
  ChartLine: ({ name, data }: { name: string; data: unknown[] }) => (
    <div data-testid="line">
      {name}:{data.length}
    </div>
  ),
  ChartScatter: () => null,
  ChartVoronoiContainer: () => null,
  ChartThemeColor: { multiOrdered: 'multi' },
}));

const run = (name: string, iops: number, steps: [number, number, number, boolean][], knee = 0) => ({
  metadata: { name },
  spec: { shareSizeGB: 3900, iops },
  status: {
    phase: 'Complete',
    kneeVMs: knee,
    recommendedVMs: knee ? 30 : 60,
    steps: steps.map(([vms, p95Micros, slowPercent, pass]) => ({ vms, p95Micros, slowPercent, pass, ended: 'x' })),
  },
});

beforeEach(() => {
  (useK8sWatchResource as jest.Mock).mockReturnValue([
    [
      run('t3000', 3000, [[15, 1500, 0, true], [30, 2100, 0, true], [45, 25000, 76, false]], 45),
      run('t10k', 10000, [[15, 1400, 0, true], [30, 1600, 0, true], [45, 1700, 0, true], [60, 1800, 0, true]]),
      run('other', 20000, [[15, 1000, 0, true]]),
    ],
    true,
    undefined,
  ]);
});

describe('PoolBenchComparePage', () => {
  it('puts the chosen runs side by side, one row per VM count', () => {
    render(<PoolBenchComparePage />);
    const table = screen.getByRole('grid', { name: 'Runs side by side' });
    expect(table.textContent).toContain('t3000 (3900 GB, 3000 IOPS)');
    expect(table.textContent).toContain('t10k (3900 GB, 10000 IOPS)');
    expect(table.textContent).not.toContain('other');
    const rows = within(table).getAllByRole('row');
    expect(rows).toHaveLength(5);
    expect(rows[3].textContent).toContain('25.0 ms');
    expect(rows[3].textContent).toContain('76%');
    expect(rows[3].textContent).toContain('Knee');
    expect(rows[4].textContent).toContain('-');
  });

  it('sums up each run: knee and recommended count', () => {
    render(<PoolBenchComparePage />);
    expect(screen.getByText(/t3000: knee at 45 VMs, recommended 30/)).toBeTruthy();
    expect(screen.getByText(/t10k: no knee up to 60 VMs/)).toBeTruthy();
  });

  it('draws one line per run, p95 against VMs', () => {
    render(<PoolBenchComparePage />);
    const lines = screen.getAllByTestId('line').map((l) => l.textContent);
    expect(lines).toContain('t3000 (3900 GB, 3000 IOPS):3');
    expect(lines).toContain('t10k (3900 GB, 10000 IOPS):4');
  });
});

describe('PoolBenchComparePage help', () => {
  it('explains the graph and the table', () => {
    render(<PoolBenchComparePage />);
    expect(screen.getByLabelText('About Median VM p95 latency against the VM count')).toBeTruthy();
    expect(screen.getByLabelText('About Step results')).toBeTruthy();
    expect(screen.getByLabelText('About the VMs column')).toBeTruthy();
  });
});

describe('PoolBenchComparePage axes', () => {
  it('draws the numbers of the axes in the text colour of the theme', () => {
    render(<PoolBenchComparePage />);
    const axes = screen.getAllByTestId('axis');
    expect(axes.length).toBe(2);
    for (const a of axes) expect(a.getAttribute('data-tick')).toBe('var(--pf-t--global--text--color--regular)');
  });
});
