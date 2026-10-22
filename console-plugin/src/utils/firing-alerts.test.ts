import { consoleFetchJSON } from '@openshift-console/dynamic-plugin-sdk';
import { fetchFiringAlerts, ALERT_NAME_PREFIX } from './firing-alerts';

const mockFetch = consoleFetchJSON as jest.MockedFunction<typeof consoleFetchJSON>;

beforeEach(() => {
  jest.clearAllMocks();
});

const rulesResponse = (names: string[]) => ({
  status: 'success',
  data: {
    groups: [
      { name: 'vpc-file-pool-csi.capacity', rules: names.map((name) => ({ name, type: 'alerting' })) },
      { name: 'other', rules: [{ name: 'NodeDown', type: 'alerting' }] },
    ],
  },
});

const alertsResponse = (series: Array<Record<string, string>>) => ({
  status: 'success',
  data: { resultType: 'vector', result: series.map((metric) => ({ metric, value: [0, '1'] })) },
});

describe('fetchFiringAlerts', () => {
  it('returns the firing alerts of the driver, with severity, pool and share', async () => {
    mockFetch
      .mockResolvedValueOnce(rulesResponse(['VPCFilePoolShareNearlyFull']))
      .mockResolvedValueOnce(
        alertsResponse([
          { alertname: 'VPCFilePoolShareNearlyFull', severity: 'warning', pool: 'gp', share: 's1' },
          { alertname: 'VPCFilePoolControllerDown', severity: 'critical' },
        ]),
      );

    const result = await fetchFiringAlerts();

    expect(result.rulesLoaded).toBe(true);
    expect(result.firing).toEqual([
      { name: 'VPCFilePoolControllerDown', severity: 'critical', pool: undefined, share: undefined },
      { name: 'VPCFilePoolShareNearlyFull', severity: 'warning', pool: 'gp', share: 's1' },
    ]);
  });

  it('asks Prometheus only for firing alerts whose name starts with the driver prefix', async () => {
    mockFetch.mockResolvedValueOnce(rulesResponse([])).mockResolvedValueOnce(alertsResponse([]));

    await fetchFiringAlerts();

    const urls = mockFetch.mock.calls.map((c) => String(c[0]));
    expect(urls[0]).toBe('/api/prometheus/api/v1/rules?type=alert');
    expect(decodeURIComponent(urls[1])).toContain(
      `ALERTS{alertname=~"${ALERT_NAME_PREFIX}.*",alertstate="firing"}`,
    );
  });

  it('says the rules are not loaded when this Prometheus has none of the driver alerts', async () => {
    mockFetch.mockResolvedValueOnce(rulesResponse([])).mockResolvedValueOnce(alertsResponse([]));

    const result = await fetchFiringAlerts();

    expect(result).toEqual({ rulesLoaded: false, firing: [] });
  });

  it('puts critical alerts before warnings', async () => {
    mockFetch
      .mockResolvedValueOnce(rulesResponse(['VPCFilePoolBackupLate']))
      .mockResolvedValueOnce(
        alertsResponse([
          { alertname: 'VPCFilePoolBackupLate', severity: 'warning' },
          { alertname: 'VPCFilePoolOverCommitted', severity: 'critical', pool: 'p' },
        ]),
      );

    const result = await fetchFiringAlerts();

    expect(result.firing.map((a) => a.severity)).toEqual(['critical', 'warning']);
  });

  it('throws when Prometheus does not answer success', async () => {
    mockFetch.mockResolvedValueOnce({ status: 'error' }).mockResolvedValueOnce(alertsResponse([]));

    await expect(fetchFiringAlerts()).rejects.toThrow();
  });
});
