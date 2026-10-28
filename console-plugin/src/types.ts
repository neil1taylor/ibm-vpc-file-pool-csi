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
  securityGroupIDs?: string[];
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
  pvcSizeGB?: number;
}

export interface AccessorZone {
  zone: string;
  subnetID?: string;
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
  remainingSnapshots?: number;
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
  /** Instant-snapshot chain of the VM disk, newest file first; the last file is disk.img. */
  diskChain?: string[];
  chainOperation?: string;
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
  destinationNFSServer?: string;
  destinationExportPath?: string;
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
  /** HTTPS URL of the replication receiver on the destination cluster. When set, uses driver-to-driver mode. */
  destinationEndpoint?: string;
  /** Secret name in kube-system containing a "token" key for receiver auth. Required when destinationEndpoint is set. */
  destinationAuthSecretRef?: string;
  /** Secret name in kube-system with a "ca.crt" key. Used in driver-to-driver mode when the receiver has a self-signed certificate. */
  destinationCACertSecretRef?: string;
  /** The highest speed of the copy of a running VM's disk, in MB/s for each disk; none if unset or 0. With vmCopyAdaptive false it is the fixed speed (0 unlimited, unset 20). */
  vmCopySpeedLimitMBps?: number;
  /** The copy starts slow and follows the load on the share. Default true. */
  vmCopyAdaptive?: boolean;
  vmCopyMaxLatencyMs?: number;
  /** How long the destination keeps restore points of a running VM's disks. Default 24. */
  restorePointKeepHours?: number;
}

export interface ReplicationPolicyStatus {
  phase?: string;
  lastSyncTime?: string;
  lastSyncDuration?: string;
  subVolumeStatuses?: SubVolumeReplicationStatus[];
  /** One entry for each running VM: the moments all its disks share. */
  vms?: VMReplicationStatus[];
  consecutiveFailures: number;
  lastError?: string;
  /** `ReceiverReachable` is set by the controller for driver-to-driver policies. */
  conditions?: Condition[];
}

/** A running VM in a policy. commonMoments are the moments ALL its disks can show, newest first. */
export interface VMReplicationStatus {
  /** namespace/name */
  name: string;
  subVolumes: string[];
  copyMoment?: string;
  consistency?: 'FileSystemConsistent' | 'CrashConsistent';
  commonMoments?: string[];
}

export interface SubVolumeReplicationStatus {
  subVolumeName: string;
  lastSyncTime?: string;
  bytesSynced?: number;
  lastError?: string;
  /** Running VM disks only: the moment the last good copy shows. */
  copyMoment?: string;
  consistency?: 'FileSystemConsistent' | 'CrashConsistent';
  /** Why a copy is only crash consistent. */
  consistencyReason?: string;
  /** Empty means the next copy is a full copy. */
  vmChangeList?: string;
  /** How many earlier copies the destination keeps, and the moment of the oldest. */
  restorePoints?: number;
  oldestRestorePoint?: string;
  restorePointMoments?: string[];
  /** namespace/name of the VM that owns the disk. */
  vmName?: string;
  /** A copy that is not finished. */
  vmCopy?: { id: string; phase: string };
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

// ── BackupPolicy ──

export interface BackupPolicy extends K8sResourceCommon {
  spec: BackupPolicySpec;
  status?: BackupPolicyStatus;
}

export interface BackupPolicySpec {
  snapshotPool: string;
  schedule: string;
  selector: LabelSelector;
  namespaces?: string[];
  /** A Go duration or a number of days such as "30d". Default 7d. */
  keepFor?: string;
  /** The highest speed of the copy of a running VM's disk, in MB/s for each disk; none if unset or 0. */
  vmCopySpeedLimitMBps?: number;
  /** The copy starts slow and follows the load on the share. Default true. */
  vmCopyAdaptive?: boolean;
  vmCopyMaxLatencyMs?: number;
  suspend?: boolean;
  export?: BackupExport;
}

export interface BackupExport {
  bucket: string;
  endpoint: string;
  prefix?: string;
  credentialsSecret: string;
  backupKeySecret: string;
  keyProtect: {
    instanceID: string;
    region: string;
    rootKeyID: string;
    apiKeySecret: string;
  };
  emergencyKeySecret?: string;
  keepFor?: string;
  lock?: 'Compliance' | 'None';
  schedule?: string;
}

export interface BackupPolicyStatus {
  phase?: string;
  message?: string;
  volumeCount?: number;
  replicationPolicies?: string[];
  volumes?: BackupVolumeStatus[];
}

export interface BackupVolumeStatus {
  pvcNamespace: string;
  pvcName: string;
  subVolumeName: string;
  vmName?: string;
  subPath?: string;
  shareID?: string;
  requestedGB?: number;
  storageClassName?: string;
  retained?: boolean;
  lastBackupTime?: string;
  oldestBackupTime?: string;
  restorePoints?: number;
  bytesWritten?: number;
  /** FileSystemConsistent or CrashConsistent for a VM disk, FileCopy for any other volume. */
  consistency?: string;
  lastError?: string;
  exportedTime?: string;
  exportedBytes?: number;
  exportError?: string;
}

// ── BackupRestore ──

export interface BackupRestore extends K8sResourceCommon {
  spec: BackupRestoreSpec;
  status?: BackupRestoreStatus;
}

export interface BackupRestoreSpec {
  policy: string;
  sourceNamespace?: string;
  sourceName?: string;
  moment?: string;
  targetNamespace: string;
  targetName: string;
  storageClassName?: string;
  fromCOS?: RestoreFromCOS;
}

export interface RestoreFromCOS {
  bucket: string;
  endpoint: string;
  prefix?: string;
  credentialsSecret: string;
  volume: string;
  sizeGB: number;
  backupKeySecret?: string;
  keyProtect?: { instanceID: string; region: string; rootKeyID: string; apiKeySecret: string };
  emergencyKeySecret?: string;
  emergencyKeyOnly?: boolean;
}

export interface BackupRestoreStatus {
  phase?: 'Pending' | 'Provisioning' | 'Restoring' | 'Done' | 'Failed';
  message?: string;
  restoredMoment?: string;
  latest?: boolean;
  fetched?: boolean;
  copied?: boolean;
}

// ── PoolBench ──

export interface PoolBench extends K8sResourceCommon {
  spec: PoolBenchSpec;
  status?: PoolBenchStatus;
}

export interface PoolBenchVM {
  cpu?: number;
  memoryGiB?: number;
  image?: string;
  imageNamespace?: string;
  preference?: string;
  rootDiskGiB?: number;
}

export interface PoolBenchSpec {
  zone?: string;
  profile?: string;
  shareSizeGB: number;
  iops: number;
  stepVMs?: number;
  maxVMs?: number;
  /** Every Nth VM runs Windows; 0 is Linux only. */
  windowsEvery?: number;
  linux?: PoolBenchVM;
  windows?: PoolBenchVM;
  load?: {
    steadyIOPS?: number;
    burstIOPS?: number;
    burstSeconds?: number;
    periodSeconds?: number;
    readPercent?: number;
    blockMix?: string;
    areaGiB?: number;
  };
  settleMinutes?: number;
  measureMinutes?: number;
  knee?: {
    latencyLimitMs?: number;
    slowVMPercent?: number;
    stallSeconds?: number;
    stopAtKnee?: boolean;
  };
  events?: { type: string; atVMs: number }[];
  /** This run waits until the named run has ended. */
  startAfter?: string;
  keepAfterEnd?: boolean;
  control?: {
    paused?: boolean;
    hold?: boolean;
    stop?: boolean;
    skipStep?: number;
    extendStep?: number;
    extendMinutes?: number;
  };
}

export type PoolBenchPhase =
  | 'Pending'
  | 'Checking'
  | 'Running'
  | 'Paused'
  | 'CleaningUp'
  | 'Complete'
  | 'Stopped'
  | 'Failed';

export interface PoolBenchStepResult {
  vms: number;
  windowsVMs?: number;
  shareIOPS?: number;
  guestIOPS?: number;
  ibmOps?: number;
  shareLatencyMicros?: number;
  p50Micros?: number;
  p95Micros?: number;
  p99Micros?: number;
  throughputKBps?: number;
  slowPercent?: number;
  burstSlowPercent?: number;
  stalls?: number;
  atLimit?: boolean;
  readySeconds?: number;
  pass: boolean;
  cutShort?: boolean;
  started?: string;
  ended?: string;
}

export interface PoolBenchStatus {
  phase?: PoolBenchPhase;
  stage?: 'Preparing' | 'Creating' | 'Settling' | 'Measuring' | 'Event';
  step?: number;
  totalSteps?: number;
  vms?: number;
  windowsVMs?: number;
  readyVMs?: number;
  namespace?: string;
  poolName?: string;
  startTime?: string;
  endTime?: string;
  stageStarted?: string;
  stageEnds?: string;
  pausedAt?: string;
  message?: string;
  steps?: PoolBenchStepResult[];
  kneeVMs?: number;
  recommendedVMs?: number;
  events?: { type: string; atVMs: number; started?: string; ended?: string; seconds?: number; message?: string }[];
  log?: { time: string; kind: 'Info' | 'Warning' | 'Error' | 'Control'; message: string }[];
  cleanedUp?: boolean;
}

// ── Failover ──

/** Asks the driver on the destination (DR) cluster to plan, or carry out, a failover. Cluster scoped. */
export interface Failover extends K8sResourceCommon {
  spec: FailoverSpec;
  status?: FailoverStatus;
}

export interface FailoverSpec {
  /** The destination claim to read. Empty means the claim the Helm chart names for the receiver. */
  source?: string;
  action: 'Plan' | 'Execute';
  /** Limits the moment of each VM to the newest one at or before it. */
  rewindTo?: string;
  /** Only these VMs ("namespace/name"). Empty means every VM on the copy. */
  vms?: string[];
  namespace?: string;
  force?: boolean;
  /** The FileSharePool the new SubVolumes belong to. Required for Execute. */
  drPool?: string;
}

export type FailoverPhase = 'Planning' | 'Planned' | 'Executing' | 'Done' | 'Failed';

export interface FailoverStatus {
  phase?: FailoverPhase;
  message?: string;
  source?: string;
  startedAt?: string;
  finishedAt?: string;
  /** Go duration, for example "1h5m0s". */
  rpo?: string;
  vms?: FailoverVM[];
  volumes?: FailoverVolume[];
  momentsTruncated?: boolean;
  createdPVCs?: FailoverPVC[];
}

export interface FailoverVM {
  /** namespace/name */
  name: string;
  /** The moment the failed-over disks will show. */
  moment: string;
  /** The moments all disks of the VM can show, newest first. */
  moments?: string[];
  disks: FailoverDisk[];
}

export interface FailoverDisk {
  subVolume: string;
  /** True if the disk is rewound to the VM's moment (it copies the whole disk). */
  rewind: boolean;
}

export interface FailoverVolume {
  subVolume: string;
  pvcName: string;
  pvcNamespace: string;
  requestedGB: number;
  subPath: string;
  policy?: string;
  lastSync: string;
}

export interface FailoverPVC {
  namespace: string;
  name: string;
  pv: string;
  subVolume: string;
}
