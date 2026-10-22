import { BackupPolicy, BackupVolumeStatus } from '../types';
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
