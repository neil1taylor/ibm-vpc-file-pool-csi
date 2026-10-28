import { ReplicationPolicy, ReplicationPolicySpec, SubVolumeReplicationStatus, VMReplicationStatus } from '../types';
import { parseGoDuration } from './backup-policy-form';
import { formatAge } from './resource-helpers';

interface DestinationInput {
  isDriverToDriver: boolean;
  nfsServer: string;
  endpoint: string;
  authSecretRef: string;
  caCertSecretRef: string;
}

/** The destination fields of a policy; only those that apply to the chosen mode. */
export function destinationSpecFields(input: DestinationInput): Partial<ReplicationPolicySpec> {
  if (!input.isDriverToDriver) {
    return { destinationNFSServer: input.nfsServer };
  }
  const fields: Partial<ReplicationPolicySpec> = {
    destinationEndpoint: input.endpoint,
    destinationAuthSecretRef: input.authSecretRef,
  };
  const ca = input.caCertSecretRef.trim();
  if (ca) fields.destinationCACertSecretRef = ca;
  return fields;
}

export function replicationDestination(spec: ReplicationPolicySpec | undefined): string {
  if (!spec) return '-';
  if (spec.destinationEndpoint) return spec.destinationEndpoint;
  if (spec.destinationNFSServer) {
    return `${spec.destinationNFSServer}:${spec.destinationBasePath || '/pvcs'}`;
  }
  return '-';
}

/** The highest copy speed a policy has when it sets none: there is none, the copy adapts to the share. */
export const DEFAULT_VM_COPY_SPEED_MBPS = 0;
/** The speed of a copy that does not adapt, when the policy sets none. */
const FIXED_VM_COPY_SPEED_MBPS = 20;
export const DEFAULT_RESTORE_POINT_KEEP_HOURS = 24;

/**
 * The spec fields for the VM copy settings. A setting equal to its default is
 * left out, so the controller's default applies. A highest speed of 0 means
 * there is none. A speed below 0 becomes 0 and a keep time below 1 hour
 * becomes 1, the lowest the CRD accepts.
 */
export function vmCopySpecFields(input: {
  speedLimitMBps: number;
  restorePointKeepHours: number;
}): Partial<ReplicationPolicySpec> {
  const fields: Partial<ReplicationPolicySpec> = {};
  const speed = Math.max(0, input.speedLimitMBps);
  const keep = Math.max(1, input.restorePointKeepHours);
  if (speed !== DEFAULT_VM_COPY_SPEED_MBPS) fields.vmCopySpeedLimitMBps = speed;
  if (keep !== DEFAULT_RESTORE_POINT_KEEP_HOURS) fields.restorePointKeepHours = keep;
  return fields;
}

/** The VM copy settings of a policy as text, with the defaults named. */
export function vmCopySettingsText(spec: ReplicationPolicySpec): { speed: string; keep: string } {
  const speedValue = spec.vmCopySpeedLimitMBps;
  const keepValue = spec.restorePointKeepHours;
  let speed: string;
  if (spec.vmCopyAdaptive === false) {
    speed =
      speedValue === undefined
        ? `${FIXED_VM_COPY_SPEED_MBPS} MB/s per disk (fixed, default)`
        : speedValue === 0
          ? 'Unlimited (fixed)'
          : `${speedValue} MB/s per disk (fixed)`;
  } else {
    speed = speedValue
      ? `Adapts to the load on the share, up to ${speedValue} MB/s per disk`
      : 'Adapts to the load on the share (no highest speed)';
  }
  const hours = (n: number) => `${n} ${n === 1 ? 'hour' : 'hours'}`;
  const keep =
    keepValue === undefined ? `${hours(DEFAULT_RESTORE_POINT_KEEP_HOURS)} (default)` : hours(keepValue);
  return { speed, keep };
}

// Messages the controller sets when a volume waits. They are not failures.
// Keep in step with pkg/pool/replication_controller.go and replication_vmcopy.go.
const WAITING_MESSAGE_PREFIXES = ['skipped: in use by running VM', 'copy waits:', 'copy lost:'];

export interface VolumeStatusView {
  /** True for the disk of a running VM (it has a copy moment or a copy in progress). */
  isVMDisk: boolean;
  /** The moment the last good copy shows, or '-'. */
  copyMoment: string;
  consistency: string;
  /** True when the copy is only crash consistent. */
  consistencyWarning: boolean;
  consistencyReason: string;
  restorePoints: string;
  copyPhase: string;
  /** What the next scheduled copy will hold, for a VM disk. */
  nextCopy: string;
  severity: 'ok' | 'warning' | 'danger';
  message: string;
}

/** What the replication page shows for one volume of a policy. */
export function describeVolumeStatus(s: SubVolumeReplicationStatus): VolumeStatusView {
  const isVMDisk = !!(s.copyMoment || s.vmCopy);
  const message = s.lastError || '';
  let severity: VolumeStatusView['severity'] = 'ok';
  if (message) {
    severity = WAITING_MESSAGE_PREFIXES.some((p) => message.startsWith(p)) ? 'warning' : 'danger';
  }
  let consistency = '-';
  if (s.consistency === 'FileSystemConsistent') consistency = 'File-system consistent';
  if (s.consistency === 'CrashConsistent') consistency = 'Crash consistent';
  let restorePoints = '-';
  if (isVMDisk) {
    restorePoints =
      s.restorePoints && s.restorePoints > 0
        ? `${s.restorePoints} (oldest ${formatAge(s.oldestRestorePoint)})`
        : 'none yet';
  }
  return {
    isVMDisk,
    copyMoment: formatAge(s.copyMoment),
    consistency,
    consistencyWarning: s.consistency === 'CrashConsistent',
    consistencyReason: s.consistencyReason || '',
    restorePoints,
    copyPhase: s.vmCopy?.phase || '',
    nextCopy: isVMDisk && s.copyMoment ? (s.vmChangeList ? 'changes only' : 'full') : '',
    severity,
    message,
  };
}

export interface ReceiverReachableView {
  label: string;
  color: 'grey' | 'green' | 'red';
  /** The reason in plain words when the receiver is not reachable. */
  detail: string;
}

/**
 * What the pages show for the controller's `ReceiverReachable` check, or null for a policy that
 * does not use a receiver.
 */
export function describeReceiverReachable(policy: ReplicationPolicy): ReceiverReachableView | null {
  if (!policy.spec?.destinationEndpoint) return null;
  const cond = policy.status?.conditions?.find((c) => c.type === 'ReceiverReachable');
  if (!cond) return { label: 'Checking...', color: 'grey', detail: '' };
  if (cond.status === 'True') return { label: 'Reachable', color: 'green', detail: '' };
  return { label: 'Not reachable', color: 'red', detail: cond.message || cond.reason || '' };
}

export interface VMRecoveryView {
  name: string;
  disks: number;
  /** How long ago the newest copy was made, or '-'. */
  copyMoment: string;
  consistency: string;
  /** True when the copy is only crash consistent. */
  consistencyWarning: boolean;
  /** How many moments all disks of the VM share. */
  momentCount: number;
  /** The newest of those moments, as ages. */
  moments: string[];
  /** True when the disks share no moment, so the VM cannot be recovered to a matching state. */
  noCommonMoment: boolean;
  /** True when the newest copy is older than twice the schedule. */
  stale: boolean;
  staleText: string;
}

const MAX_MOMENTS_SHOWN = 5;

/**
 * What the policy page shows for each running VM: the copy moment, whether it is consistent, the moments a
 * recovery can use, and a warning if the data is stale. Stale means the newest copy is older than twice the
 * schedule, or there is no copy. A schedule that is not a duration never makes a copy stale.
 */
export function describeVMRecovery(
  vms: VMReplicationStatus[] | undefined,
  schedule: string | undefined,
  now: Date = new Date(),
): VMRecoveryView[] {
  const scheduleMs = schedule ? parseGoDuration(schedule) : undefined;
  return (vms || []).map((vm) => {
    const moments = vm.commonMoments || [];
    let stale = false;
    let staleText = '';
    if (!vm.copyMoment) {
      stale = true;
      staleText = 'No copy yet';
    } else if (scheduleMs && now.getTime() - new Date(vm.copyMoment).getTime() > 2 * scheduleMs) {
      stale = true;
      staleText = `Newest copy is older than twice the ${schedule} schedule`;
    }
    let consistency = '-';
    if (vm.consistency === 'FileSystemConsistent') consistency = 'File-system consistent';
    if (vm.consistency === 'CrashConsistent') consistency = 'Crash consistent';
    return {
      name: vm.name,
      disks: vm.subVolumes.length,
      copyMoment: formatAge(vm.copyMoment),
      consistency,
      consistencyWarning: vm.consistency === 'CrashConsistent',
      momentCount: moments.length,
      moments: moments.slice(0, MAX_MOMENTS_SHOWN).map((m) => formatAge(m)),
      noCommonMoment: moments.length === 0,
      stale,
      staleText,
    };
  });
}
