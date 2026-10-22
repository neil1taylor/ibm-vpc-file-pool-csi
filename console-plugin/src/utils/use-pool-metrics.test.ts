import { renderHook, waitFor } from '@testing-library/react';
import { consoleFetchJSON } from '@openshift-console/dynamic-plugin-sdk';
import { checkPrometheusConnection, usePoolMetrics, resultToNumber, resultSum } from './use-pool-metrics';

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

describe('usePoolMetrics replication lag', () => {
  it('matches replication lag to a pool by the source_pool label', async () => {
    mockFetch.mockImplementation(async (url: string) => {
      const result = url.includes('replication_lag_seconds')
        ? [{ metric: { policy: 'p1', source_pool: 'pool-a' }, value: [0, '42'] as [number, string] }]
        : [];
      return { status: 'success', data: { resultType: 'vector', result } };
    });

    const { result } = renderHook(() => usePoolMetrics(['pool-a', 'pool-b'], 60000));
    await waitFor(() => expect(result.current.loading).toBe(false));

    expect(result.current.metrics['pool-a'].replicationLagSeconds).toBe(42);
    expect(result.current.metrics['pool-b'].replicationLagSeconds).toBe(0);
  });

  it('queries latency from _bucket series', async () => {
    mockFetch.mockImplementation(async () => ({
      status: 'success',
      data: { resultType: 'vector', result: [] },
    }));
    renderHook(() => usePoolMetrics(['pool-a'], 60000));
    await waitFor(() => expect(mockFetch).toHaveBeenCalled());
    const urls = mockFetch.mock.calls.map((c) => decodeURIComponent(c[0] as string));
    expect(urls.some((u) => u.includes('allocation_duration_seconds_bucket'))).toBe(true);
    expect(urls.some((u) => /_seconds\[/.test(u))).toBe(false);
  });
});

const r = (pool: string, value: string) => ({ metric: { pool }, value: [0, value] as [number, string] });

describe('resultToNumber', () => {
  it('reads the value of the pool', () => {
    expect(resultToNumber([r('a', '0.25')], 'a')).toBe(0.25);
  });

  it('returns 0 when Prometheus has no data for the pool (NaN)', () => {
    expect(resultToNumber([r('a', 'NaN')], 'a')).toBe(0);
  });

  it('returns 0 for an infinite value', () => {
    expect(resultToNumber([r('a', '+Inf')], 'a')).toBe(0);
  });

  it('returns 0 when the pool is missing', () => {
    expect(resultToNumber([r('a', '1')], 'b')).toBe(0);
  });
});

describe('resultSum', () => {
  it('ignores NaN values', () => {
    expect(resultSum([r('a', '2'), r('b', 'NaN')])).toBe(2);
  });
});
