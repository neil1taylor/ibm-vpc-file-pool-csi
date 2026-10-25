import { renderHook, waitFor } from '@testing-library/react';
import { consoleFetchJSON } from '@openshift-console/dynamic-plugin-sdk';
import { useRunMetrics, useVMNumbers } from './use-run-metrics';
import { ChartSpec } from './poolbench-queries';

const specs: ChartSpec[] = [
  { title: 'IOPS', yLabel: 'IOPS', series: [{ label: 'Share', query: 'q_share' }, { label: 'Guests', query: 'q_guests' }], lines: [] },
];

const matrix = (values: [number, string][]) => ({ status: 'success', data: { resultType: 'matrix', result: [{ metric: {}, values }] } });

beforeEach(() => jest.clearAllMocks());

describe('useRunMetrics', () => {
  it('reads each series over the window of the run', async () => {
    (consoleFetchJSON as unknown as jest.Mock).mockImplementation((url: string) =>
      Promise.resolve(url.includes('q_share') ? matrix([[100, '5'], [130, 'NaN'], [160, '7']]) : matrix([])),
    );
    const { result } = renderHook(() => useRunMetrics(specs, { start: 100, end: 160, step: 30 }, false));
    await waitFor(() => expect(result.current.loading).toBe(false));
    const urls = (consoleFetchJSON as unknown as jest.Mock).mock.calls.map((c) => c[0] as string);
    expect(urls).toHaveLength(2);
    expect(urls[0]).toContain('/api/prometheus/api/v1/query_range?query=q_share&start=100&end=160&step=30');
    const iops = result.current.charts.IOPS;
    expect(iops[0]).toEqual({ label: 'Share', points: [{ x: 100, y: 5 }, { x: 160, y: 7 }] });
    expect(iops[1]).toEqual({ label: 'Guests', points: [] });
  });

  it('says when Prometheus cannot be read', async () => {
    (consoleFetchJSON as unknown as jest.Mock).mockRejectedValue(new Error('403 Forbidden'));
    const { result } = renderHook(() => useRunMetrics(specs, { start: 100, end: 160, step: 30 }, false));
    await waitFor(() => expect(result.current.loading).toBe(false));
    expect(result.current.error).toContain('403 Forbidden');
  });

  it('reads nothing without a window', () => {
    renderHook(() => useRunMetrics(specs, undefined, false));
    expect(consoleFetchJSON).not.toHaveBeenCalled();
  });
});

describe('useVMNumbers', () => {
  it('gives each VM its last steady IOPS and p95 in ms', async () => {
    (consoleFetchJSON as unknown as jest.Mock).mockImplementation((url: string) => {
      const vector = (rows: [string, string][]) => ({
        status: 'success',
        data: { resultType: 'vector', result: rows.map(([vm, v]) => ({ metric: { vm }, value: [1, v] })) },
      });
      return Promise.resolve(
        decodeURIComponent(url).includes('poolbench_vm_iops') ? vector([['vm-001', '29']]) : vector([['vm-001', '0.0042']]),
      );
    });
    const { result } = renderHook(() => useVMNumbers('t10k', true));
    await waitFor(() => expect(result.current['vm-001']).toBeDefined());
    expect(result.current['vm-001']).toEqual({ iops: 29, p95Ms: 4.2 });
  });
});
