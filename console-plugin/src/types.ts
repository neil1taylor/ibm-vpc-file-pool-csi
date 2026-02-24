import { K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';

// ── FileSharePool ──

export interface FileSharePool extends K8sResourceCommon {
  spec: FileSharePoolSpec;
  status?: FileSharePoolStatus;
}

export interface FileSharePoolSpec {
  zone: string;
  profile: string;
  shareSizeGB: number;
  iops?: number;
  maxShares: number;
  initialShares: number;
  autoExpand: boolean;
  expandThresholdPercent: number;
  allocationStrategy: 'spread' | 'binpack';
  encryptionInTransit: boolean;
  defaultPermissions: string;
  defaultUID?: number;
  defaultGID?: number;
  resourceGroup?: string;
  tags?: string[];
  mountOptions?: string[];
  accessorZones?: AccessorZone[];
  tiers?: ShareTier[];
  drainShares?: string[];
  goldenImages?: GoldenImageConfig;
}

export interface GoldenImageConfig {
  enabled: boolean;
  targetNamespaces: string[];
  imageFilter?: string[];
  refreshInterval?: string;
  converterImage?: string;
  pvcSizeGB?: number;
}

export interface AccessorZone {
  zone: string;
  subnetID: string;
}

export interface ShareTier {
  name: string;
  profile: string;
  shareSizeGB: number;
  iops?: number;
  maxShares: number;
  initialShares: number;
}

export interface FileSharePoolStatus {
  phase?: string;
  shares?: PoolShareStatus[];
  shareCount: number;
  totalCapacityGB: number;
  totalAllocatedGB: number;
  totalPVCCount: number;
  goldenImages?: GoldenImageStatus[];
  drainStatus?: ShareDrainStatus[];
  conditions?: Condition[];
  lastReconcileTime?: string;
}

export interface PoolShareStatus {
  shareID: string;
  shareName: string;
  mountTargetIP: string;
  mountTargetID: string;
  exportPath?: string;
  totalGB: number;
  allocatedGB: number;
  pvcCount: number;
  state: string;
  tier?: string;
  zone: string;
  mountTargets?: ZoneMountTarget[];
  createdAt?: string;
}

export interface ZoneMountTarget {
  zone: string;
  mountTargetID: string;
  mountTargetIP: string;
  exportPath?: string;
}

export interface GoldenImageStatus {
  name: string;
  sourceURL?: string;
  namespaces?: GoldenImageNamespaceStatus[];
}

export interface GoldenImageNamespaceStatus {
  namespace: string;
  phase: string;
  pvcName?: string;
  templateName?: string;
  dataSourceName?: string;
  lastSyncTime?: string;
  message?: string;
}

export interface ShareDrainStatus {
  shareID: string;
  remainingSubVolumes: number;
  drained: boolean;
  drainStartedAt?: string;
}

// ── SubVolume ──

export interface SubVolume extends K8sResourceCommon {
  spec: SubVolumeSpec;
  status?: SubVolumeStatus;
}

export interface SubVolumeSpec {
  poolName: string;
  shareID: string;
  shareMountTargetIP: string;
  shareExportPath?: string;
  subPath: string;
  requestedGB: number;
  pvName: string;
  pvcName: string;
  pvcNamespace: string;
  uid?: number;
  gid?: number;
  permissions?: string;
  reclaimPolicy: 'Delete' | 'Retain' | 'Archive';
  sourceVolume?: string;
  sourceShareID?: string;
}

export interface SubVolumeStatus {
  phase?: string;
  actualUsageBytes?: number;
  lastUsageCheck?: string;
  conditions?: Condition[];
  createdAt?: string;
  cloneStatus?: string;
  cloneProgress?: CloneProgress;
}

export interface CloneProgress {
  bytesCopied: number;
  totalBytes: number;
  startedAt?: string;
  completedAt?: string;
  error?: string;
}

// ── Snapshot ──

export interface Snapshot extends K8sResourceCommon {
  spec: SnapshotSpec;
  status?: SnapshotStatus;
}

export interface SnapshotSpec {
  sourceSubVolume: string;
  poolName: string;
  shareID: string;
  shareMountTargetIP: string;
  snapshotPath: string;
  sourceSubPath: string;
  sizeGB: number;
}

export interface SnapshotStatus {
  phase?: string;
  readyToUse: boolean;
  sizeBytes?: number;
  creationTime?: string;
}

// ── VolumeGroupSnapshot ──

export interface VolumeGroupSnapshot extends K8sResourceCommon {
  spec: VolumeGroupSnapshotSpec;
  status?: VolumeGroupSnapshotStatus;
}

export interface VolumeGroupSnapshotSpec {
  poolName: string;
  sourcePVCs?: string[];
  copyOrder?: string[];
  failurePolicy: 'Abort' | 'Continue';
  preSnapshotHooks?: Hook[];
  postSnapshotHooks?: Hook[];
}

export interface VolumeGroupSnapshotStatus {
  phase?: string;
  members?: GroupSnapshotMember[];
  memberCount: number;
  readyCount: number;
  failedCount: number;
  consistencyLevel?: string;
  inconsistencyWindowMs?: number;
  startedAt?: string;
  completedAt?: string;
  creationTime?: string;
  hookResults?: HookResult[];
}

export interface GroupSnapshotMember {
  subVolumeName: string;
  snapshotName: string;
  phase: string;
  error?: string;
}

// ── ReplicationPolicy ──

export interface ReplicationPolicy extends K8sResourceCommon {
  spec: ReplicationPolicySpec;
  status?: ReplicationPolicyStatus;
}

export interface ReplicationPolicySpec {
  sourcePoolName: string;
  destinationNFSServer: string;
  destinationBasePath: string;
  schedule: string;
  subVolumeSelector?: LabelSelector;
  maxRetries: number;
  incrementalSync?: boolean;
  bandwidthLimitMbps?: number;
  maxParallelSyncs?: number;
  rsyncOptions?: string[];
  preSyncHooks?: Hook[];
  postSyncHooks?: Hook[];
}

export interface ReplicationPolicyStatus {
  phase?: string;
  lastSyncTime?: string;
  lastSyncDuration?: string;
  subVolumeStatuses?: SubVolumeReplicationStatus[];
  consecutiveFailures: number;
  lastError?: string;
}

export interface SubVolumeReplicationStatus {
  subVolumeName: string;
  lastSyncTime?: string;
  bytesSynced?: number;
  lastError?: string;
}

// ── Hooks ──

export interface Hook {
  name: string;
  type: 'exec' | 'http';
  exec?: ExecHookSpec;
  http?: HTTPHookSpec;
  timeoutSeconds?: number;
  onError?: 'Continue' | 'Abort';
}

export interface ExecHookSpec {
  podSelector: Record<string, string>;
  namespace: string;
  container?: string;
  command: string[];
}

export interface HTTPHookSpec {
  url: string;
  method?: 'GET' | 'POST' | 'PUT';
  headers?: Record<string, string>;
}

export interface HookResult {
  name: string;
  phase: string;
  success: boolean;
  message?: string;
  startedAt: string;
  duration: string;
}

// ── Common ──

export interface Condition {
  type: string;
  status: string;
  lastTransitionTime?: string;
  reason?: string;
  message?: string;
  observedGeneration?: number;
}

export interface LabelSelector {
  matchLabels?: Record<string, string>;
  matchExpressions?: LabelSelectorRequirement[];
}

export interface LabelSelectorRequirement {
  key: string;
  operator: 'In' | 'NotIn' | 'Exists' | 'DoesNotExist';
  values?: string[];
}
