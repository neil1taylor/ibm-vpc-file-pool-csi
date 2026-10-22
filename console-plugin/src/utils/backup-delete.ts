import { K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';
import { BackupPolicy, BackupRestore, BackupVolumeStatus, FileSharePool } from '../types';
import { ShareMount } from './backup-jobs';

// The steps of `kubectl backup delete` (cmd/backup/delete.go), as the console does them: refuse, measure with a
// Job, ask for a typed confirmation, then remove with a Job.

/** Why the backups of this volume cannot be removed now, in the words of the command line; undefined if they can. */
export function deleteRefusal(policy: BackupPolicy, volume: BackupVolumeStatus, restores: BackupRestore[]): string | undefined {
  const policyName = policy.metadata?.name || '';
  const claim = `${volume.pvcNamespace}/${volume.pvcName}`;
  if (!volume.subPath) return `Policy ${policyName} has no backup of ${claim} to remove.`;
  if (!volume.retained) {
    return `Policy ${policyName} still backs up ${claim}: remove the label that selects it, or delete the policy, wait for the next backup cycle, then try again.`;
  }
  for (const r of restores) {
    const sp = r.spec;
    if (sp?.policy !== policyName || sp.sourceNamespace !== volume.pvcNamespace || sp.sourceName !== volume.pvcName) continue;
    if (r.status?.phase !== 'Done' && r.status?.phase !== 'Failed') {
      return `Restore ${r.metadata?.name} of ${claim} has not ended; wait for it, then try again.`;
    }
  }
  return undefined;
}

/**
 * The share of the snapshot pool that holds a backup. An entry with no share ID was made when the pool had one
 * share: it belongs to the first share.
 */
export function resolveShare(pool: FileSharePool | undefined, shareID: string): { share?: ShareMount; error?: string } {
  const name = pool?.metadata?.name || 'of the policy';
  if (!pool) return { error: `The snapshot pool ${name} was not found.` };
  const shares = pool.status?.shares || [];
  if (shares.length === 0) return { error: `The snapshot pool ${name} has no share.` };
  const share = shareID ? shares.find((s) => s.shareID === shareID) : shares[0];
  if (!share) return { error: `Share ${shareID} is not a share of the snapshot pool ${name}.` };
  if (!share.mountTargetIP) {
    return { error: `Share ${share.shareID} of the snapshot pool ${name} has no mount target yet.` };
  }
  return { share: { mountTargetIP: share.mountTargetIP, exportPath: share.exportPath || '' } };
}

/** The size the measure Job writes as its termination message: whole kilobytes. Bytes, or undefined if it is not a size. */
export function parseMeasureMessage(message: string | undefined): number | undefined {
  const text = (message || '').trim();
  if (!/^\d+$/.test(text)) return undefined;
  return parseInt(text, 10) * 1024;
}

interface JobLike extends K8sResourceCommon {
  status?: {
    active?: number;
    succeeded?: number;
    failed?: number;
    conditions?: Array<{ type: string; status: string }>;
  };
}

interface PodLike {
  status?: { containerStatuses?: Array<{ state?: { terminated?: { message?: string } } }> };
}

// The Job is retried this many times (pkg/pool shareJobBackoffLimit).
const BACKOFF_LIMIT = 3;

export type JobOutcome = { state: 'running' } | { state: 'succeeded'; message: string } | { state: 'failed' };

/** Where a share Job is, and what its pod wrote as its termination message when it succeeded. */
export function jobOutcome(job: JobLike | undefined, pods: PodLike[]): JobOutcome {
  const status = job?.status;
  if (!status) return { state: 'running' };
  if ((status.succeeded || 0) > 0) {
    const message = pods
      .flatMap((p) => p.status?.containerStatuses || [])
      .map((cs) => cs.state?.terminated?.message)
      .find((m) => !!m);
    return { state: 'succeeded', message: message || '' };
  }
  const failedCondition = (status.conditions || []).some((c) => c.type === 'Failed' && c.status === 'True');
  if ((status.failed || 0) > BACKOFF_LIMIT || failedCondition) return { state: 'failed' };
  return { state: 'running' };
}

/** How many moments the delete would remove: the older restore points and the newest backup. */
export function momentsCount(volume: BackupVolumeStatus): number {
  return (volume.restorePoints || 0) + (volume.lastBackupTime ? 1 : 0);
}

/** What the operator types to start the delete: namespace/name of the claim, as the command line asks. */
export function claimConfirmation(volume: BackupVolumeStatus): string {
  return `${volume.pvcNamespace}/${volume.pvcName}`;
}

export interface AccessRight {
  group: string;
  resource: string;
  verb: 'create' | 'delete' | 'list';
  /** Unset for a cluster-wide right. */
  namespace?: string;
}

/** What the measure and delete Jobs need, as `kubectl backup delete` needs it (BACKUP-RECOVERY.md). */
export function deleteAccessRights(driverNamespace: string): AccessRight[] {
  return [
    { group: 'batch', resource: 'jobs', verb: 'create', namespace: driverNamespace },
    { group: 'batch', resource: 'jobs', verb: 'delete', namespace: driverNamespace },
    { group: '', resource: 'persistentvolumeclaims', verb: 'create', namespace: driverNamespace },
    { group: '', resource: 'persistentvolumeclaims', verb: 'delete', namespace: driverNamespace },
    { group: '', resource: 'persistentvolumes', verb: 'create' },
    { group: '', resource: 'persistentvolumes', verb: 'delete' },
    { group: '', resource: 'pods', verb: 'list', namespace: driverNamespace },
  ];
}
