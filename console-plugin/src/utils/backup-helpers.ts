import { API_GROUP_VERSION, ROUTES } from '../constants';
import { BackupPolicy, BackupRestore, BackupVolumeStatus } from '../types';
import { isValidK8sName } from './k8s-objects';
import { formatAge, humanizeBytes } from './resource-helpers';

const HOUR_MS = 3600 * 1000;
const DEFAULT_KEEP_FOR_MS = 7 * 24 * HOUR_MS;

/** A keep time as milliseconds: a number of days such as "7d", or a Go duration such as "1h30m". Empty means the default of 7 days. */
export function parseKeepFor(value: string | undefined): number | undefined {
  if (!value) return DEFAULT_KEEP_FOR_MS;
  const days = value.match(/^(\d+)d$/);
  if (days) return parseInt(days[1], 10) * 24 * HOUR_MS;
  const duration = value.match(/^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+)s)?$/);
  if (!duration || !value) return undefined;
  const [, h, m, s] = duration;
  return (parseInt(h || '0', 10) * 3600 + parseInt(m || '0', 10) * 60 + parseInt(s || '0', 10)) * 1000;
}

/** The date (YYYY-MM-DD) until which the backups of a volume are kept: its newest backup plus the keep time. Empty if unknown. */
export function keptUntil(volume: BackupVolumeStatus, keepFor: string | undefined): string {
  const keep = parseKeepFor(keepFor);
  if (!volume.lastBackupTime || keep === undefined) return '';
  return new Date(new Date(volume.lastBackupTime).getTime() + keep).toISOString().slice(0, 10);
}

export interface BackupVolumeView {
  claim: string;
  vm: string;
  lastBackup: string;
  oldestBackup: string;
  restorePoints: string;
  size: string;
  consistency: string;
  consistencyWarning: boolean;
  retained: boolean;
  retainedNote: string;
  error: string;
}

const CONSISTENCY_TEXT: Record<string, string> = {
  FileSystemConsistent: 'File-system consistent',
  CrashConsistent: 'Crash consistent',
  FileCopy: 'File copy',
};

/** What the policy page shows for one volume. */
export function describeBackupVolume(volume: BackupVolumeStatus, policy: BackupPolicy): BackupVolumeView {
  let retainedNote = '';
  if (volume.retained) {
    const until = keptUntil(volume, policy.spec?.keepFor);
    retainedNote = until ? `No longer backed up, kept until ${until}` : 'No longer backed up';
  }
  return {
    claim: `${volume.pvcNamespace}/${volume.pvcName}`,
    vm: volume.vmName || '-',
    lastBackup: formatAge(volume.lastBackupTime),
    oldestBackup: formatAge(volume.oldestBackupTime),
    restorePoints: volume.restorePoints === undefined ? '-' : String(volume.restorePoints),
    size: volume.bytesWritten ? humanizeBytes(volume.bytesWritten) : '-',
    consistency: CONSISTENCY_TEXT[volume.consistency || ''] || '-',
    consistencyWarning: volume.consistency === 'CrashConsistent',
    retained: !!volume.retained,
    retainedNote,
    error: volume.lastError || '',
  };
}

export interface ExportState {
  text: string;
  severity: 'none' | 'ok' | 'warning' | 'danger';
}

/** The COS export state of one volume. */
export function exportStateText(volume: BackupVolumeStatus, policy: BackupPolicy): ExportState {
  if (!policy.spec?.export) return { text: 'Off', severity: 'none' };
  if (volume.exportError) return { text: `Failed: ${volume.exportError}`, severity: 'danger' };
  if (!volume.exportedTime) return { text: 'Not sent yet', severity: 'warning' };
  const size = volume.exportedBytes ? ` (${humanizeBytes(volume.exportedBytes)})` : '';
  return { text: `Sent ${formatAge(volume.exportedTime)} ago${size}`, severity: 'ok' };
}

/** How old the stalest "last backup" is, over the volumes the policy still backs up. */
export function stalestBackupAge(policy: BackupPolicy): string {
  const live = (policy.status?.volumes || []).filter((v) => !v.retained);
  if (live.length === 0) return '-';
  if (live.some((v) => !v.lastBackupTime)) return 'No backup yet';
  const oldest = live.reduce((a, b) => (new Date(a.lastBackupTime!) < new Date(b.lastBackupTime!) ? a : b));
  return formatAge(oldest.lastBackupTime);
}

export interface PolicyProblem {
  title: string;
  text: string;
  /** The snapshot pool to link to, when the problem is about its space. */
  poolName?: string;
}

// The message of pkg/pool/backup_usage.go snapshotPoolFullMessage; keep in step.
const POOL_FULL = /of the snapshot pool "([^"]+)" is \d+% full/;

/** The banner for a policy in the Error phase, in plain words. */
export function policyProblem(policy: BackupPolicy): PolicyProblem | undefined {
  if (policy.status?.phase !== 'Error') return undefined;
  const message = policy.status.message || '';
  const full = message.match(POOL_FULL);
  if (full) {
    return {
      title: 'New backups are stopped: the snapshot pool is more than 85% full',
      text: `Nothing is deleted to make room. Backups start again by themselves when the use falls to 85% or less: shorten the keep time, delete the backups of a volume you no longer need, or add a share to the pool. ${message}`,
      poolName: full[1],
    };
  }
  return { title: 'The policy has an error', text: message };
}

/** The settings the console reads about the driver. */
export interface DriverSettings {
  /** True once the file-restore feature (issue #92) is built and on. */
  fileRestore?: boolean;
}

/** The "Browse files" action is shown only when the file-restore feature is present. */
export function browseFilesAvailable(settings: DriverSettings | undefined): boolean {
  return settings?.fileRestore === true;
}

// ── Restore ──

/** The name of a restore request: restore-<claim>-<time in base 36>, as `kubectl backup restore` makes it. */
export function restoreRequestName(claim: string, nowMs: number): string {
  const name = `restore-${claim}-${Math.floor(nowMs / 1000).toString(36)}`;
  return name.slice(0, 63).replace(/[-.]+$/, '');
}

export interface BackupRestoreInput {
  policy: string;
  sourceNamespace: string;
  sourceName: string;
  targetNamespace: string;
  targetName: string;
  /** An ISO time. Empty or unset means the latest backup. */
  moment?: string;
  /** Empty or unset means the class of the backed-up claim. */
  storageClass?: string;
}

/** The BackupRestore the console creates: the same object `kubectl backup restore` creates. */
export function buildBackupRestore(input: BackupRestoreInput, nowMs: number): BackupRestore {
  const spec: BackupRestore['spec'] = {
    policy: input.policy,
    sourceNamespace: input.sourceNamespace,
    sourceName: input.sourceName,
    targetNamespace: input.targetNamespace,
    targetName: input.targetName,
  };
  if (input.moment) spec.moment = input.moment.replace(/\.\d+Z$/, 'Z');
  if (input.storageClass) spec.storageClassName = input.storageClass;
  return {
    apiVersion: API_GROUP_VERSION,
    kind: 'BackupRestore',
    metadata: { name: restoreRequestName(input.sourceName, nowMs) },
    spec,
  };
}

export interface BackupRestoreErrors {
  targetNamespace?: string;
  targetName?: string;
  moment?: string;
}

/** Checks the form. The webhook has the final say; this shows the common mistakes at once. */
export function validateBackupRestore(
  form: { targetNamespace: string; targetName: string; moment: string },
  context: { existingNames: string[]; oldestBackupTime?: string },
): BackupRestoreErrors {
  const errors: BackupRestoreErrors = {};
  if (!isValidK8sName(form.targetNamespace)) {
    errors.targetNamespace = 'Use lower-case letters, digits and dashes.';
  }
  if (!isValidK8sName(form.targetName)) {
    errors.targetName = 'Use lower-case letters, digits and dashes.';
  } else if (context.existingNames.includes(form.targetName)) {
    errors.targetName = 'A claim with this name already exists in the namespace. A restore never writes over a volume: choose a new name.';
  }
  if (form.moment) {
    const t = new Date(form.moment).getTime();
    if (Number.isNaN(t)) {
      errors.moment = 'This is not a time.';
    } else if (context.oldestBackupTime && t < new Date(context.oldestBackupTime).getTime()) {
      errors.moment = `This is before the oldest backup (${context.oldestBackupTime}). Choose a later time.`;
    }
  }
  return errors;
}

const pad2 = (n: number) => String(n).padStart(2, '0');

/** An ISO time as the text of a datetime-local input (the browser's time zone). Empty for an empty or bad value. */
export function momentToLocalInput(iso: string): string {
  const d = new Date(iso);
  if (!iso || Number.isNaN(d.getTime())) return '';
  return `${d.getFullYear()}-${pad2(d.getMonth() + 1)}-${pad2(d.getDate())}T${pad2(d.getHours())}:${pad2(d.getMinutes())}`;
}

/** The text of a datetime-local input as an ISO time. Empty for an empty or bad value. */
export function localInputToISO(local: string): string {
  const d = new Date(local);
  return !local || Number.isNaN(d.getTime()) ? '' : d.toISOString();
}

/** The link to the restore page of a policy. The claim ("namespace/name") and the moment are query values, so other pages can link to it. */
export function restorePath(policy: string, claim?: string, moment?: string): string {
  const query = new URLSearchParams();
  if (claim) query.set('claim', claim);
  if (moment) query.set('moment', moment);
  const q = query.toString();
  return `${ROUTES.BACKUP_RESTORE.replace(':name', policy)}${q ? `?${q}` : ''}`;
}

/** The query values of the restore page. */
export function parseRestoreQuery(search: string): { claim: string; moment: string } {
  const q = new URLSearchParams(search);
  return { claim: q.get('claim') || '', moment: q.get('moment') || '' };
}

export interface BackupRestoreView {
  state: 'waiting' | 'working' | 'done' | 'failed';
  title: string;
  text: string;
}

/** What the progress banner shows, from the status of the BackupRestore. */
export function backupRestoreProgress(br: BackupRestore | undefined): BackupRestoreView {
  const status = br?.status;
  switch (status?.phase) {
    case 'Provisioning':
      return { state: 'working', title: 'Restoring: making the new volume', text: status.message || 'The driver is making the new claim.' };
    case 'Restoring':
      return { state: 'working', title: 'Restoring: copying the backup into the new volume', text: status.message || 'The copy takes time. The volume cannot be used until it ends.' };
    case 'Failed':
      return { state: 'failed', title: 'The restore failed', text: status.message || 'The restore failed.' };
    case 'Done': {
      let what = 'The moment of the backup that was restored is not known.';
      if (status.restoredMoment) {
        what = status.latest
          ? `Restored the latest backup, from ${status.restoredMoment}.`
          : `Restored the backup from ${status.restoredMoment}.`;
      } else if (status.latest) {
        what = 'Restored the latest backup.';
      }
      return { state: 'done', title: 'The restored volume is ready', text: what };
    }
    default:
      return { state: 'waiting', title: 'Restoring: the request is starting', text: status?.message || 'The driver has not started the restore yet.' };
  }
}

/** The JSON patch that suspends or resumes a policy. "add" also replaces, so it works with or without the field. */
export function suspendPatch(suspend: boolean): Array<{ op: 'add'; path: string; value: boolean }> {
  return [{ op: 'add', path: '/spec/suspend', value: suspend }];
}
