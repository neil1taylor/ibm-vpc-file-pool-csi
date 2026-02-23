/** Routes used by the console plugin */
export const ROUTES = {
  dashboard: '/vpc-file-pools',
  pools: '/vpc-file-pools/pools',
  subvolumes: '/vpc-file-pools/subvolumes',
  snapshots: '/vpc-file-pools/snapshots',
  groupSnapshots: '/vpc-file-pools/group-snapshots',
  replication: '/vpc-file-pools/replication',
  monitoring: '/vpc-file-pools/monitoring',
} as const;

/** Tab names in the main page layout */
export const TAB_NAMES = [
  'Overview',
  'Pools',
  'SubVolumes',
  'Snapshots',
  'Group Snapshots',
  'Replication',
  'Monitoring',
] as const;

/** Dashboard stat card titles (row 1) */
export const DASHBOARD_STAT_CARDS = [
  'Total Pools',
  'Total Capacity',
  'Total SubVolumes',
  'Active Alerts',
] as const;

/** Dashboard performance card titles (row 2) */
export const DASHBOARD_PERF_CARDS = [
  'Allocation Rate',
  'P95 Allocation Latency',
  'VPC API Health',
] as const;

/** Dashboard capacity card titles (row 3) */
export const DASHBOARD_CAPACITY_CARDS = [
  'Aggregate Capacity',
  'Pool Capacity',
] as const;

/** Monitoring stat card titles */
export const MONITORING_STAT_CARDS = [
  'Total PVCs',
  'Utilization',
  'VPC API Health',
  'Replication Status',
] as const;

/** Monitoring chart card titles */
export const MONITORING_CHART_CARDS = [
  'Capacity Utilization Over Time',
  'PVC Count Over Time',
  'VPC API Call Rate',
  'VPC API P95 Latency',
] as const;

/** Time range labels in the monitoring page */
export const TIME_RANGE_LABELS = ['1h', '6h', '24h', '7d'] as const;

/** Pools table column headers */
export const POOLS_COLUMNS = [
  'Name', 'Zone', 'Profile', 'IOPS', 'Shares', 'Capacity', 'PVCs', 'Phase', 'Age',
] as const;

/** SubVolumes table column headers */
export const SUBVOLUMES_COLUMNS = [
  'Name', 'Pool', 'PVC', 'Size', 'Phase', 'Clone Status', 'Age',
] as const;

/** Snapshots table column headers */
export const SNAPSHOTS_COLUMNS = [
  'Name', 'Pool', 'Source SubVolume', 'Size', 'Phase', 'Ready', 'Age',
] as const;

/** Group Snapshots table column headers */
export const GROUP_SNAPSHOTS_COLUMNS = [
  'Name', 'Pool', 'Members', 'Ready', 'Phase', 'Age',
] as const;

/** Replication table column headers */
export const REPLICATION_COLUMNS = [
  'Name', 'Source Pool', 'Destination', 'Schedule', 'Phase', 'Last Sync', 'Failures',
] as const;

/** Recent activity table column headers (dashboard) */
export const RECENT_ACTIVITY_COLUMNS = [
  'Name', 'Pool', 'PVC', 'Size', 'Phase', 'Age',
] as const;

/** Selectors for key UI elements */
export const SELECTORS = {
  pageTitle: 'h1',
  cardTitle: '.pf-v6-c-card__title',
  phaseLabel: '.pf-v6-c-label',
  timeRangeGroup: '[aria-label="Time range selector"]',
  recentActivityTable: 'table[aria-label="Recent SubVolume activity"]',
  expandableToggle: 'button',
} as const;

/** Golden image DataSource names to verify */
export const GOLDEN_IMAGE_DATASOURCES = [
  'rhel8-nfs-pool',
  'rhel9-nfs-pool',
  'rhel10-nfs-pool',
] as const;

/** DataSource detail route template */
export function dataSourceRoute(name: string): string {
  return `/k8s/ns/openshift-virtualization-os-images/datasources.cdi.kubevirt.io~v1beta1~DataSource/${name}`;
}
