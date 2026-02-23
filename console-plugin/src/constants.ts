export const API_GROUP = 'storage.ibmcloud.io';
export const API_VERSION = 'v1alpha1';
export const API_GROUP_VERSION = `${API_GROUP}/${API_VERSION}`;

export const PLUGIN_NAME = 'ibm-vpc-file-pool-csi';

// Routes
export const ROUTES = {
  DASHBOARD: '/vpc-file-pools',
  POOLS: '/vpc-file-pools/pools',
  POOL_CREATE: '/vpc-file-pools/pools/~new',
  POOL_DETAIL: '/vpc-file-pools/pools/:name',
  SUBVOLUMES: '/vpc-file-pools/subvolumes',
  SUBVOLUME_CREATE: '/vpc-file-pools/subvolumes/~new',
  SUBVOLUME_DETAIL: '/vpc-file-pools/subvolumes/:name',
  SNAPSHOTS: '/vpc-file-pools/snapshots',
  SNAPSHOT_CREATE: '/vpc-file-pools/snapshots/~new',
  SNAPSHOT_DETAIL: '/vpc-file-pools/snapshots/:name',
  GROUP_SNAPSHOTS: '/vpc-file-pools/group-snapshots',
  GROUP_SNAPSHOT_CREATE: '/vpc-file-pools/group-snapshots/~new',
  GROUP_SNAPSHOT_DETAIL: '/vpc-file-pools/group-snapshots/:name',
  REPLICATION: '/vpc-file-pools/replication',
  REPLICATION_CREATE: '/vpc-file-pools/replication/~new',
  REPLICATION_DETAIL: '/vpc-file-pools/replication/:name',
  MONITORING: '/vpc-file-pools/monitoring',
} as const;

// Prometheus metric names
export const METRICS = {
  ALLOCATIONS_TOTAL: 'vpc_file_pool_allocations_total',
  ALLOCATION_DURATION: 'vpc_file_pool_allocation_duration_seconds',
  POOL_CAPACITY_GB: 'vpc_file_pool_capacity_gb',
  POOL_ALLOCATED_GB: 'vpc_file_pool_allocated_gb',
  POOL_SHARE_COUNT: 'vpc_file_pool_share_count',
  POOL_PVC_COUNT: 'vpc_file_pool_pvc_count',
  VPC_API_CALLS_TOTAL: 'vpc_file_pool_api_calls_total',
  VPC_API_CALL_DURATION: 'vpc_file_pool_api_call_duration_seconds',
  SNAPSHOTS_TOTAL: 'vpc_file_pool_snapshots_total',
  SNAPSHOT_DURATION: 'vpc_file_pool_snapshot_duration_seconds',
  CLONES_TOTAL: 'vpc_file_pool_clones_total',
  CLONE_DURATION: 'vpc_file_pool_clone_duration_seconds',
  GROUP_SNAPSHOTS_TOTAL: 'vpc_file_pool_group_snapshots_total',
  GROUP_SNAPSHOT_DURATION: 'vpc_file_pool_group_snapshot_duration_seconds',
  REPLICATION_SYNC_TOTAL: 'pool_csi_replication_sync_total',
  REPLICATION_SYNC_DURATION: 'pool_csi_replication_sync_duration_seconds',
  REPLICATION_LAG: 'pool_csi_replication_lag_seconds',
  REPLICATION_CONSECUTIVE_FAILURES: 'pool_csi_replication_consecutive_failures',
  REPLICATION_SUBVOLUME_COUNT: 'pool_csi_replication_subvolume_count',
} as const;

// Pool phases
export const POOL_PHASES = ['Initializing', 'Ready', 'Expanding', 'Degraded', 'Full'] as const;
export type PoolPhase = (typeof POOL_PHASES)[number];

// Share states
export const SHARE_STATES = ['creating', 'stable', 'draining', 'degraded', 'deleting'] as const;
export type ShareState = (typeof SHARE_STATES)[number];

// SubVolume phases
export const SUBVOLUME_PHASES = [
  'Creating', 'Cloning', 'Bound', 'Expanding', 'Deleting', 'Retained', 'Archived', 'Failed',
] as const;
export type SubVolumePhase = (typeof SUBVOLUME_PHASES)[number];

// Snapshot phases
export const SNAPSHOT_PHASES = ['Creating', 'Ready', 'Deleting', 'Failed'] as const;
export type SnapshotPhase = (typeof SNAPSHOT_PHASES)[number];

// Group snapshot phases
export const GROUP_SNAPSHOT_PHASES = [
  'Pending', 'InProgress', 'Complete', 'PartialFailure', 'Failed',
] as const;
export type GroupSnapshotPhase = (typeof GROUP_SNAPSHOT_PHASES)[number];

// Replication phases
export const REPLICATION_PHASES = ['Active', 'Paused', 'Failed'] as const;
export type ReplicationPhase = (typeof REPLICATION_PHASES)[number];

// Allocation strategies
export const ALLOCATION_STRATEGIES = ['spread', 'binpack'] as const;

// Failure policies
export const FAILURE_POLICIES = ['Abort', 'Continue'] as const;

// Hook types
export const HOOK_TYPES = ['exec', 'http'] as const;

// Hook on error
export const HOOK_ON_ERROR = ['Continue', 'Abort'] as const;

// Reclaim policies
export const RECLAIM_POLICIES = ['Delete', 'Retain', 'Archive'] as const;
