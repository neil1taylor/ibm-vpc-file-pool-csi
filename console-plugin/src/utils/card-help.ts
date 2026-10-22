/**
 * Plain-language help for the number cards on the dashboard and monitoring pages: what the number means,
 * what is normal, and what to do when it is bad. One to three sentences each.
 */
export interface CardHelp {
  title: string;
  text: string;
}

export const CARD_HELP = {
  totalPools: {
    title: 'Total Pools',
    text: 'How many pools exist, and how many of them are not Ready. Normally every pool is Ready. If one is not, open it to see its phase and conditions.',
  },
  totalCapacity: {
    title: 'Total Capacity',
    text: 'Space promised to volumes, out of the total size of all shares in all pools. It counts the size each volume asked for, not the data written. Near 100%, new volumes cannot be placed: raise the share limit of a pool or create a pool.',
  },
  totalSubVolumes: {
    title: 'Total SubVolumes',
    text: 'How many volumes (PVCs) the driver has placed in pools. Each volume is a folder on a VPC file share.',
  },
  activeAlerts: {
    title: 'Active Alerts',
    text: 'Pools that are not Ready, listed by name; normal is 0. Prometheus alerts are in the Firing Alerts card. Open a listed pool to see why it is not Ready.',
  },
  firingAlerts: {
    title: 'Firing Alerts',
    text: 'Alerts from the driver alert rules that are firing now in the cluster Prometheus; normal is none. Each alert names what is wrong, such as a nearly full share or late replication. Observe, Alerting, in the console shows all alerts and their history.',
  },
  allocationRate: {
    title: 'Allocation Rate',
    text: 'New volumes placed per hour, over the last hour, in all pools. There is no right value. A sudden jump can mean a workload is creating volumes in a loop.',
  },
  p95AllocationLatency: {
    title: 'P95 Allocation Latency',
    text: 'The time that 95 out of 100 new-volume requests finish within, over the last 5 minutes, for the slowest pool. Under 100 ms is good, 100 to 500 ms is elevated, and over 500 ms is high. If it stays high, read the controller log.',
  },
  vpcApiHealth: {
    title: 'VPC API Health',
    text: 'Calls from the driver to the IBM Cloud VPC API over the last 5 minutes: calls per minute, the percent that failed, and the time 95 out of 100 calls finish within. Normal is an error rate of 0%, and some calls, such as creating a share, take a minute. If the error rate stays above 5%, check the driver API key and the IBM Cloud status page.',
  },
  aggregateCapacity: {
    title: 'Aggregate Capacity',
    text: 'Space promised to volumes compared with the total size of all shares in all pools. A pool with auto-expand on adds a share when its use passes its expand threshold (80% by default), up to its share limit.',
  },
  poolCapacity: {
    title: 'Pool Capacity',
    text: 'Each pool: space promised to volumes out of its size, and its shares out of its share limit. The bar turns orange at 80% and red at 95%. A red pool at its share limit cannot take new volumes: raise its share limit.',
  },
  replicationLag: {
    title: 'Replication Lag',
    text: 'Time since each replication policy last finished a sync; it grows between syncs, so compare it with the policy schedule. Green is under 5 minutes, orange under 15 minutes, red after that. If it keeps growing past the schedule, open the policy and read its last error.',
  },
  syncRate: {
    title: 'Sync Rate',
    text: 'Sync runs per minute, over the last 5 minutes, for all replication policies. It follows the policy schedules, so a low number is normal for long schedules.',
  },
  consecutiveFailures: {
    title: 'Consecutive Failures',
    text: 'How many syncs in a row have failed, for each replication policy; normal is 0. When a policy reaches its retry limit (3 by default), it pauses and stops syncing. Open the policy, read its last error, fix the cause, then resume it.',
  },
  snapshots: {
    title: 'Snapshots',
    text: 'Snapshots taken since the driver controller last started; the count starts again at 0 when it restarts. The Snapshots tab lists the snapshots that exist now.',
  },
  clones: {
    title: 'Clones',
    text: 'Volume clones finished since the driver controller last started, and the median time a clone took over the last hour. A clone copies the whole volume, so the time grows with its size.',
  },
  groupSnapshots: {
    title: 'Group Snapshots',
    text: 'Group snapshots are not active yet, so this stays at 0. An OpenShift VM snapshot already covers all the disks of a VM at one moment.',
  },
  poolCapacityOverview: {
    title: 'Pool Capacity Overview',
    text: 'Space promised to volumes out of the size of each pool, with its volumes (PVCs) and shares. The bar turns orange at 80% and red at 95%.',
  },
  totalPVCs: {
    title: 'Total PVCs',
    text: 'Volumes (PVCs) in all pools, and how many shares hold them, from the driver metrics.',
  },
  utilization: {
    title: 'Utilization',
    text: 'Space promised to volumes as a percent of all pool space, and the space still free. Healthy is below 75%, Warning from 75%, Critical from 90%. Pools with auto-expand add shares by themselves; when Critical, raise a share limit or create a pool.',
  },
  replicationStatus: {
    title: 'Replication Status',
    text: 'Healthy when no replication policy has failed syncs in a row; otherwise the number of failing policies, with sync runs per minute. Open the Replication tab to see the last error of a failing policy.',
  },
  capacityOverTime: {
    title: 'Capacity Utilization Over Time',
    text: 'Space promised to volumes as a percent of pool space, over the chosen time range. A steady climb shows when the pools will need more shares.',
  },
  pvcCountOverTime: {
    title: 'PVC Count Over Time',
    text: 'Volumes (PVCs) in each pool over the chosen time range.',
  },
  vpcApiCallRate: {
    title: 'VPC API Call Rate',
    text: 'Calls from the driver to the IBM Cloud VPC API per minute, split by result, over the chosen time range. Calls rise while shares are created, grown or deleted, and stay low otherwise. A steady line of failed calls needs a look at the driver API key.',
  },
  vpcApiP95Latency: {
    title: 'VPC API P95 Latency',
    text: 'The time that 95 out of 100 VPC API calls finish within, over the chosen time range. Share changes take up to a minute, so short peaks are normal.',
  },
  allocationRateOverTime: {
    title: 'Allocation Rate Over Time',
    text: 'New volumes placed per hour, over the chosen time range.',
  },
  allocationLatencyOverTime: {
    title: 'P95 Allocation Latency Over Time',
    text: 'The time that 95 out of 100 new-volume requests finish within, over the chosen time range. Under 100 ms is good; if it stays above 500 ms, read the controller log.',
  },
  groupSnapshotWindow: {
    title: 'Group Snapshot Inconsistency Window (P95)',
    text: 'For group snapshots, the time between the first and the last member snapshot. Group snapshots are not active yet, so this chart stays empty.',
  },
  groupSnapshotMembers: {
    title: 'Group Snapshot Members (average)',
    text: 'The average number of volumes in a group snapshot. Group snapshots are not active yet, so this chart stays empty.',
  },
  cdiLimitRaises: {
    title: 'CDI Copy Limit Raises (per hour)',
    text: 'How often the driver raised the memory limit of the OpenShift Virtualization copy pods, so that large disk imports do not run out of memory. After the first raise it should stay at 0.',
  },
  replicationSyncRateOverTime: {
    title: 'Replication Sync Rate',
    text: 'Sync runs per minute for all replication policies, split by result, over the chosen time range. Failed runs that do not stop need a look at the policy.',
  },
  backupPolicy: {
    title: 'Backup Policy',
    text: 'Which volumes are backed up, how often, for how long, and into which snapshot pool. A VM is picked through its disks: if one disk matches, all its disks are backed up.',
  },
  backupVolumes: {
    title: 'Backed-up Volumes',
    text: 'Each volume the policy backs up, with the restore points you can go back to. A volume marked "no longer backed up" keeps its backups until the keep time has passed.',
  },
} satisfies Record<string, CardHelp>;

export type CardHelpKey = keyof typeof CARD_HELP;
