import { consoleFetchJSON } from '@openshift-console/dynamic-plugin-sdk';
import { useEffect, useState } from 'react';

const PROMETHEUS_BASE = '/api/prometheus/api/v1';

/** Every alert the chart ships has a name that starts with this. */
export const ALERT_NAME_PREFIX = 'VPCFilePool';

export interface FiringAlert {
  name: string;
  severity: string;
  pool?: string;
  share?: string;
}

export interface FiringAlerts {
  /** False when this Prometheus has none of the driver's alert rules, so "none firing" would mean nothing. */
  rulesLoaded: boolean;
  firing: FiringAlert[];
}

interface RulesResponse {
  status: string;
  data: { groups: Array<{ rules: Array<{ name: string; type: string }> }> };
}

interface AlertsResponse {
  status: string;
  data: { result: Array<{ metric: Record<string, string> }> };
}

const SEVERITY_ORDER: Record<string, number> = { critical: 0, warning: 1 };
const severityRank = (s: string) => SEVERITY_ORDER[s] ?? 2;

export async function fetchFiringAlerts(): Promise<FiringAlerts> {
  const query = `ALERTS{alertname=~"${ALERT_NAME_PREFIX}.*",alertstate="firing"}`;
  const [rules, alerts] = (await Promise.all([
    consoleFetchJSON(`${PROMETHEUS_BASE}/rules?type=alert`),
    consoleFetchJSON(`${PROMETHEUS_BASE}/query?query=${encodeURIComponent(query)}`),
  ])) as [RulesResponse, AlertsResponse];

  if (rules.status !== 'success' || alerts.status !== 'success') {
    throw new Error('Prometheus did not return the alerts');
  }

  const rulesLoaded = rules.data.groups.some((g) =>
    g.rules.some((r) => r.type === 'alerting' && r.name.startsWith(ALERT_NAME_PREFIX)),
  );

  const firing = alerts.data.result
    .map(({ metric }) => ({
      name: metric.alertname,
      severity: metric.severity || 'warning',
      pool: metric.pool,
      share: metric.share,
    }))
    .sort(
      (a, b) =>
        severityRank(a.severity) - severityRank(b.severity) || a.name.localeCompare(b.name),
    );

  return { rulesLoaded, firing };
}

interface FiringAlertsState extends FiringAlerts {
  loading: boolean;
  error: string | null;
}

export function useFiringAlerts(refreshInterval = 30000): FiringAlertsState {
  const [state, setState] = useState<FiringAlertsState>({
    rulesLoaded: false,
    firing: [],
    loading: true,
    error: null,
  });

  useEffect(() => {
    let cancelled = false;
    const load = async () => {
      try {
        const result = await fetchFiringAlerts();
        if (!cancelled) setState({ ...result, loading: false, error: null });
      } catch (err) {
        if (!cancelled) {
          setState((prev) => ({
            ...prev,
            loading: false,
            error: err instanceof Error ? err.message : 'Failed to fetch alerts',
          }));
        }
      }
    };
    load();
    const interval = setInterval(load, refreshInterval);
    return () => {
      cancelled = true;
      clearInterval(interval);
    };
  }, [refreshInterval]);

  return state;
}
