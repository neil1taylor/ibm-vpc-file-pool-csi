import { BackupPolicy, BackupVolumeStatus } from '../types';
import {
  browseFilesAvailable,
  describeBackupVolume,
  exportStateText,
  keptUntil,
  parseKeepFor,
  policyProblem,
  stalestBackupAge,
} from './backup-helpers';

const HOUR = 3600 * 1000;
const ago = (hours: number) => new Date(Date.now() - hours * HOUR).toISOString();

const volume = (over: Partial<BackupVolumeStatus> = {}): BackupVolumeStatus => ({
  pvcNamespace: 'apps',
  pvcName: 'data',
  subVolumeName: 'pvc-1',
  ...over,
});

const policy = (over: Partial<BackupPolicy> = {}): BackupPolicy =>
  ({
    metadata: { name: 'nightly' },
    spec: { snapshotPool: 'snaps', schedule: '6h', selector: { matchLabels: { backup: 'yes' } }, keepFor: '7d' },
    status: { phase: 'Active', volumeCount: 1, volumes: [] },
    ...over,
  }) as BackupPolicy;

describe('parseKeepFor', () => {
  it('reads days, and Go durations', () => {
    expect(parseKeepFor('7d')).toBe(7 * 24 * HOUR);
    expect(parseKeepFor('36h')).toBe(36 * HOUR);
    expect(parseKeepFor('1h30m')).toBe(90 * 60 * 1000);
  });

  it('gives the default of 7 days for an empty value, and undefined for nonsense', () => {
    expect(parseKeepFor(undefined)).toBe(7 * 24 * HOUR);
    expect(parseKeepFor('')).toBe(7 * 24 * HOUR);
    expect(parseKeepFor('soon')).toBeUndefined();
  });
});

describe('keptUntil', () => {
  it('is the newest backup plus the keep time', () => {
    const last = '2026-10-01T00:00:00Z';
    expect(keptUntil(volume({ lastBackupTime: last }), '2d')).toBe('2026-10-03');
  });

  it('is empty when the time cannot be worked out', () => {
    expect(keptUntil(volume(), '2d')).toBe('');
    expect(keptUntil(volume({ lastBackupTime: '2026-10-01T00:00:00Z' }), 'soon')).toBe('');
  });
});

describe('describeBackupVolume', () => {
  it('shows claim, VM, ages, restore points and size', () => {
    const v = describeBackupVolume(
      volume({
        vmName: 'web',
        lastBackupTime: ago(2),
        oldestBackupTime: ago(50),
        restorePoints: 8,
        bytesWritten: 3 * 1024 * 1024 * 1024,
        consistency: 'FileSystemConsistent',
      }),
      policy(),
    );
    expect(v).toMatchObject({
      claim: 'apps/data',
      vm: 'web',
      lastBackup: '2h',
      oldestBackup: '2d',
      restorePoints: '8',
      size: '3.0 GiB',
      consistency: 'File-system consistent',
      consistencyWarning: false,
      retained: false,
      error: '',
    });
  });

  it('warns for a crash-consistent copy', () => {
    const v = describeBackupVolume(volume({ consistency: 'CrashConsistent' }), policy());
    expect(v.consistency).toBe('Crash consistent');
    expect(v.consistencyWarning).toBe(true);
  });

  it('calls a plain volume a file copy', () => {
    expect(describeBackupVolume(volume({ consistency: 'FileCopy' }), policy()).consistency).toBe('File copy');
  });

  it('shows dashes for a volume with no backup yet', () => {
    const v = describeBackupVolume(volume(), policy());
    expect(v).toMatchObject({ lastBackup: '-', oldestBackup: '-', restorePoints: '-', size: '-', consistency: '-', vm: '-' });
  });

  it('marks a retained volume with the date its backups are kept until', () => {
    const v = describeBackupVolume(volume({ retained: true, lastBackupTime: '2026-10-01T00:00:00Z' }), policy());
    expect(v.retained).toBe(true);
    expect(v.retainedNote).toBe('No longer backed up, kept until 2026-10-08');
  });

  it('says only "no longer backed up" when the date is unknown', () => {
    const v = describeBackupVolume(volume({ retained: true }), policy());
    expect(v.retainedNote).toBe('No longer backed up');
  });

  it('carries the last error', () => {
    expect(describeBackupVolume(volume({ lastError: 'boom' }), policy()).error).toBe('boom');
  });
});

describe('exportStateText', () => {
  it('is "Off" when the policy has no export', () => {
    expect(exportStateText(volume(), policy())).toEqual({ text: 'Off', severity: 'none' });
  });

  const exporting = () =>
    policy({
      spec: { ...policy().spec, export: { bucket: 'b', endpoint: 'e' } as never },
    });

  it('shows the age of the newest copy in COS', () => {
    const s = exportStateText(volume({ exportedTime: ago(3), exportedBytes: 1024 * 1024 }), exporting());
    expect(s).toEqual({ text: 'Sent 3h ago (1.0 MiB)', severity: 'ok' });
  });

  it('says nothing was sent yet', () => {
    expect(exportStateText(volume(), exporting())).toEqual({ text: 'Not sent yet', severity: 'warning' });
  });

  it('shows an export error before anything else', () => {
    const s = exportStateText(volume({ exportedTime: ago(3), exportError: 'bucket gone' }), exporting());
    expect(s).toEqual({ text: 'Failed: bucket gone', severity: 'danger' });
  });
});

describe('stalestBackupAge', () => {
  it('is the age of the oldest "last backup" among volumes still backed up', () => {
    const p = policy({
      status: {
        volumes: [
          volume({ lastBackupTime: ago(1) }),
          volume({ pvcName: 'b', lastBackupTime: ago(30) }),
          volume({ pvcName: 'c', retained: true, lastBackupTime: ago(900) }),
        ],
      },
    });
    expect(stalestBackupAge(p)).toBe('1d');
  });

  it('is "No backup yet" when a volume has none, and "-" with no volumes', () => {
    expect(stalestBackupAge(policy({ status: { volumes: [volume()] } }))).toBe('No backup yet');
    expect(stalestBackupAge(policy({ status: { volumes: [] } }))).toBe('-');
    expect(stalestBackupAge(policy({ status: undefined }))).toBe('-');
  });
});

describe('policyProblem', () => {
  it('is nothing for an Active or Suspended policy', () => {
    expect(policyProblem(policy())).toBeUndefined();
    expect(policyProblem(policy({ status: { phase: 'Suspended', message: 'suspended' } }))).toBeUndefined();
  });

  it('explains a full snapshot pool in plain words and links the pool', () => {
    const message =
      'share share-9 of the snapshot pool "snaps" is 91% full (limit 85%); new backups to it are stopped until usage falls; backups are not deleted to make room';
    const p = policyProblem(policy({ status: { phase: 'Error', message } }));
    expect(p?.title).toBe('New backups are stopped: the snapshot pool is more than 85% full');
    expect(p?.text).toContain('Nothing is deleted');
    expect(p?.text).toContain(message);
    expect(p?.poolName).toBe('snaps');
  });

  it('shows any other error message as it is', () => {
    const p = policyProblem(policy({ status: { phase: 'Error', message: 'pool not found' } }));
    expect(p?.title).toBe('The policy has an error');
    expect(p?.text).toBe('pool not found');
    expect(p?.poolName).toBeUndefined();
  });
});

describe('browseFilesAvailable', () => {
  it('is off until the file-restore feature says it is present', () => {
    expect(browseFilesAvailable(undefined)).toBe(false);
    expect(browseFilesAvailable({})).toBe(false);
    expect(browseFilesAvailable({ fileRestore: true })).toBe(true);
  });
});
