import { renderHook, waitFor } from '@testing-library/react';
import { consoleFetchJSON } from '@openshift-console/dynamic-plugin-sdk';
import { checkPrometheusConnection, usePoolMetrics } from './use-pool-metrics';

const mockFetch = consoleFetchJSON as jest.MockedFunction<typeof consoleFetchJSON>;

beforeEach(() => {
  jest.clearAllMocks();
});

describe('checkPrometheusConnection', () => {
  it('returns connected with metrics when both succeed', async () => {
    mockFetch
      .mockResolvedValueOnce({
        status: 'success',
        data: { resultType: 'vector', result: [{ metric: {}, value: [0, '1'] }] },
      })
      .mockResolvedValueOnce({
        status: 'success',
        data: { resultType: 'vector', result: [{ metric: { pool: 'test' }, value: [0, '100'] }] },
      });

    const result = await checkPrometheusConnection();
    expect(result).toEqual({ connected: true, hasMetrics: true });
  });

  it('returns connected without metrics when capacity query has no results', async () => {
    mockFetch
      .mockResolvedValueOnce({
        status: 'success',
        data: { resultType: 'vector', result: [{ metric: {}, value: [0, '1'] }] },
      })
      .mockResolvedValueOnce({
        status: 'success',
        data: { resultType: 'vector', result: [] },
      });

    const result = await checkPrometheusConnection();
    expect(result).toEqual({ connected: true, hasMetrics: false });
  });

  it('returns disconnected when up query fails', async () => {
    mockFetch.mockResolvedValueOnce({
      status: 'error',
      data: { resultType: 'vector', result: [] },
    });

    const result = await checkPrometheusConnection();
    expect(result).toEqual({
      connected: false,
      hasMetrics: false,
      error: 'Prometheus returned non-success status',
    });
  });

  it('returns disconnected with error on fetch error', async () => {
    mockFetch.mockRejectedValueOnce(new Error('Network error'));

    const result = await checkPrometheusConnection();
    expect(result).toEqual({
      connected: false,
      hasMetrics: false,
      error: 'Network error',
    });
  });
});

describe('usePoolMetrics', () => {
  function makePrometheusResponse(results: Array<{ metric: Record<string, string>; value: [number, string] }>) {
    return { status: 'success', data: { resultType: 'vector', result: results } };
  }

  it('starts with loading=true', () => {
    mockFetch.mockImplementation(() => new Promise(() => {})); // never resolves
    const { result } = renderHook(() => usePoolMetrics(['pool-a']));
    expect(result.current.loading).toBe(true);
  });

  it('returns metrics after fetch resolves', async () => {
    mockFetch.mockImplementation(async (url: string) => {
      if (url.includes('capacity_gb')) {
        return makePrometheusResponse([{ metric: { pool: 'pool-a' }, value: [0, '100'] }]);
      }
      return makePrometheusResponse([{ metric: { pool: 'pool-a' }, value: [0, '0'] }]);
    });

    const { result } = renderHook(() => usePoolMetrics(['pool-a'], 60000));

    await waitFor(() => {
      expect(result.current.loading).toBe(false);
    });

    expect(result.current.error).toBeNull();
    expect(result.current.metrics['pool-a']).toBeDefined();
    expect(result.current.metrics['pool-a'].capacityGB).toBe(100);
  });

  it('returns error state when fetch rejects', async () => {
    mockFetch.mockRejectedValue(new Error('Prometheus unavailable'));

    const { result } = renderHook(() => usePoolMetrics(['pool-a'], 60000));

    await waitFor(() => {
      expect(result.current.loading).toBe(false);
    });

    expect(result.current.error).toBe('Prometheus unavailable');
  });

  it('aggregates per-pool metrics for multiple pools', async () => {
    mockFetch.mockImplementation(async () =>
      makePrometheusResponse([
        { metric: { pool: 'pool-a' }, value: [0, '50'] },
        { metric: { pool: 'pool-b' }, value: [0, '75'] },
      ]),
    );

    const { result } = renderHook(() => usePoolMetrics(['pool-a', 'pool-b'], 60000));

    await waitFor(() => {
      expect(result.current.loading).toBe(false);
    });

    expect(result.current.metrics['pool-a']).toBeDefined();
    expect(result.current.metrics['pool-b']).toBeDefined();
    expect(result.current.metrics['pool-a'].capacityGB).toBe(50);
    expect(result.current.metrics['pool-b'].capacityGB).toBe(75);
  });

  it('clears interval on unmount', async () => {
    const clearIntervalSpy = jest.spyOn(global, 'clearInterval');

    mockFetch.mockImplementation(async () =>
      makePrometheusResponse([{ metric: { pool: 'pool-a' }, value: [0, '10'] }]),
    );

    const { result, unmount } = renderHook(() => usePoolMetrics(['pool-a'], 60000));

    await waitFor(() => {
      expect(result.current.loading).toBe(false);
    });

    unmount();
    expect(clearIntervalSpy).toHaveBeenCalled();
    clearIntervalSpy.mockRestore();
  });
});
