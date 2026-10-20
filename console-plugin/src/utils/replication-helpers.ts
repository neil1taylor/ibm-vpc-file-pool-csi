import { ReplicationPolicySpec } from '../types';

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

export const DEFAULT_VM_COPY_SPEED_MBPS = 20;
export const DEFAULT_RESTORE_POINT_KEEP_HOURS = 24;

/**
 * The spec fields for the VM copy settings. A setting equal to its default is
 * left out, so the controller's default applies. A speed below 0 becomes 0 and
 * a keep time below 1 hour becomes 1, the lowest the CRD accepts.
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
  const speed =
    speedValue === undefined
      ? `${DEFAULT_VM_COPY_SPEED_MBPS} MB/s per disk (default)`
      : speedValue === 0
        ? 'Unlimited'
        : `${speedValue} MB/s per disk`;
  const hours = (n: number) => `${n} ${n === 1 ? 'hour' : 'hours'}`;
  const keep =
    keepValue === undefined ? `${hours(DEFAULT_RESTORE_POINT_KEEP_HOURS)} (default)` : hours(keepValue);
  return { speed, keep };
}
