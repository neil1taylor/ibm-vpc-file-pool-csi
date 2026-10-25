import { consoleFetchText } from '@openshift-console/dynamic-plugin-sdk';
import { PLUGIN_NAME } from '../constants';

/**
 * The PoolBench service's raw results, through the console's plugin proxy
 * (alias "poolbench"). The proxy passes the person's own token; the service
 * checks that they may get the run.
 */
const base = (run: string): string => `/api/proxy/plugin/${PLUGIN_NAME}/poolbench/v1/runs/${encodeURIComponent(run)}`;

/** Every result line of every VM of a run, as CSV. */
export const fetchRawCSV = (run: string): Promise<string> => consoleFetchText(`${base(run)}/raw.csv`);

/** The raw lines one VM of a run sent. */
export const fetchVMLines = (run: string, vm: string): Promise<string> =>
  consoleFetchText(`${base(run)}/vms/${encodeURIComponent(vm)}/log`);

export const RAW_UNAVAILABLE =
  'The raw results need the PoolBench service and the console proxy to it (the chart adds it when the console plugin is on).';
