import { BackupPolicy, BackupVolumeStatus } from '../types';
import {
  backupRestoreProgress,
  browseFilesAvailable,
  buildBackupRestore,
  describeBackupVolume,
  exportStateText,
  keptUntil,
  localInputToISO,
  momentToLocalInput,
  parseKeepFor,
  parseRestoreQuery,
  policyProblem,
  restorePath,
  restoreRequestName,
  suspendPatch,
  stalestBackupAge,
  validateBackupRestore,
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
  it('is off until the chart says the backup gateway runs', () => {
    expect(browseFilesAvailable(undefined)).toBe(false);
    expect(browseFilesAvailable({})).toBe(false);
    expect(browseFilesAvailable({ data: { vmSnapshotMethod: 'auto' } })).toBe(false);
    expect(browseFilesAvailable({ data: { fileBrowse: 'false' } })).toBe(false);
    expect(browseFilesAvailable({ data: { fileBrowse: 'true' } })).toBe(true);
  });
});

describe('restoreRequestName', () => {
  it('is restore-<claim>-<seconds in base 36>, like the command line', () => {
    expect(restoreRequestName('data', 1761000000000)).toBe(`restore-data-${(1761000000).toString(36)}`);
  });

  it('is cut to 63 characters with no dash or dot at the end', () => {
    const name = restoreRequestName('a'.repeat(80), 1761000000000);
    expect(name.length).toBeLessThanOrEqual(63);
    expect(name).toMatch(/[a-z0-9]$/);
    expect(restoreRequestName('a'.repeat(51) + '-', 1761000000000).endsWith('-')).toBe(false);
  });
});

describe('buildBackupRestore', () => {
  const input = {
    policy: 'nightly',
    sourceNamespace: 'apps',
    sourceName: 'data',
    targetNamespace: 'apps',
    targetName: 'data-restored',
  };

  it('asks for the latest backup with no moment and no class', () => {
    const br = buildBackupRestore(input, 1761000000000);
    expect(br).toEqual({
      apiVersion: 'storage.ibmcloud.io/v1alpha1',
      kind: 'BackupRestore',
      metadata: { name: restoreRequestName('data', 1761000000000) },
      spec: {
        policy: 'nightly',
        sourceNamespace: 'apps',
        sourceName: 'data',
        targetNamespace: 'apps',
        targetName: 'data-restored',
      },
    });
  });

  it('adds the moment as an RFC 3339 time without fractions, and the class when one is chosen', () => {
    const br = buildBackupRestore(
      { ...input, moment: '2026-10-20T09:00:00.000Z', storageClass: 'fast' },
      1761000000000,
    );
    expect(br.spec.moment).toBe('2026-10-20T09:00:00Z');
    expect(br.spec.storageClassName).toBe('fast');
  });
});

describe('validateBackupRestore', () => {
  const ok = { targetNamespace: 'apps', targetName: 'data-restored', moment: '' };
  const ctx = {
    existingNames: ['data'],
    oldestBackupTime: '2026-10-01T00:00:00Z',
  };

  it('has no errors for a good request', () => {
    expect(validateBackupRestore(ok, ctx)).toEqual({});
  });

  it('refuses a bad namespace or claim name', () => {
    expect(validateBackupRestore({ ...ok, targetNamespace: 'Apps' }, ctx).targetNamespace).toBeTruthy();
    expect(validateBackupRestore({ ...ok, targetName: 'Data_1' }, ctx).targetName).toBeTruthy();
    expect(validateBackupRestore({ ...ok, targetName: '' }, ctx).targetName).toBeTruthy();
  });

  it('refuses a claim that exists, as a restore never writes over a volume', () => {
    expect(validateBackupRestore({ ...ok, targetName: 'data' }, ctx).targetName).toMatch(/already exists/);
  });

  it('refuses a moment before the oldest backup, and a moment that is not a time', () => {
    expect(validateBackupRestore({ ...ok, moment: '2026-09-01T00:00:00Z' }, ctx).moment).toMatch(/oldest backup/);
    expect(validateBackupRestore({ ...ok, moment: 'soon' }, ctx).moment).toBeTruthy();
    expect(validateBackupRestore({ ...ok, moment: '2026-10-05T00:00:00Z' }, ctx).moment).toBeUndefined();
  });

  it('does not check the moment when the oldest backup is not known', () => {
    expect(validateBackupRestore({ ...ok, moment: '2020-01-01T00:00:00Z' }, { existingNames: [] }).moment).toBeUndefined();
  });
});

describe('moments in the form', () => {
  it('round-trips a time through the local input', () => {
    const iso = '2026-10-20T09:30:00.000Z';
    expect(localInputToISO(momentToLocalInput(iso))).toBe(iso);
  });

  it('gives empty text for an empty or bad value', () => {
    expect(momentToLocalInput('')).toBe('');
    expect(momentToLocalInput('nonsense')).toBe('');
    expect(localInputToISO('')).toBe('');
  });
});

describe('restorePath and parseRestoreQuery', () => {
  it('builds the restore link with the claim and the moment as query values', () => {
    expect(restorePath('nightly', 'apps/data')).toBe('/vpc-file-pools/backups/nightly/restore?claim=apps%2Fdata');
    expect(restorePath('nightly', 'apps/data', '2026-10-20T09:00:00Z')).toBe(
      '/vpc-file-pools/backups/nightly/restore?claim=apps%2Fdata&moment=2026-10-20T09%3A00%3A00Z',
    );
    expect(restorePath('nightly')).toBe('/vpc-file-pools/backups/nightly/restore');
  });

  it('reads them back', () => {
    expect(parseRestoreQuery('?claim=apps%2Fdata&moment=2026-10-20T09%3A00%3A00Z')).toEqual({
      claim: 'apps/data',
      moment: '2026-10-20T09:00:00Z',
    });
    expect(parseRestoreQuery('')).toEqual({ claim: '', moment: '' });
  });
});

describe('backupRestoreProgress', () => {
  it('waits while there is no status', () => {
    expect(backupRestoreProgress(undefined).state).toBe('waiting');
    expect(backupRestoreProgress({ metadata: { name: 'r' }, spec: {} as never }).state).toBe('waiting');
  });

  it('follows the phases', () => {
    const at = (phase: string, extra = {}) =>
      backupRestoreProgress({ metadata: { name: 'r' }, spec: {} as never, status: { phase: phase as never, ...extra } });
    expect(at('Pending', { message: 'waiting for the policy' })).toMatchObject({ state: 'waiting', text: 'waiting for the policy' });
    expect(at('Provisioning').state).toBe('working');
    expect(at('Restoring').state).toBe('working');
    expect(at('Failed', { message: 'no backup' })).toMatchObject({ state: 'failed', text: 'no backup' });
  });

  it('says which moment was restored when it is done', () => {
    const done = backupRestoreProgress({
      metadata: { name: 'r' },
      spec: {} as never,
      status: { phase: 'Done', restoredMoment: '2026-10-20T09:00:00Z', latest: false },
    });
    expect(done.state).toBe('done');
    expect(done.text).toContain('2026-10-20T09:00:00Z');
    const latest = backupRestoreProgress({
      metadata: { name: 'r' },
      spec: {} as never,
      status: { phase: 'Done', latest: true, restoredMoment: '2026-10-21T09:00:00Z' },
    });
    expect(latest.text).toContain('latest');
  });

  it('says so when the moment is not known', () => {
    const done = backupRestoreProgress({ metadata: { name: 'r' }, spec: {} as never, status: { phase: 'Done' } });
    expect(done.text).toMatch(/not known/);
  });
});

describe('suspendPatch', () => {
  it('sets spec.suspend with a JSON patch that works whether or not the field is there', () => {
    expect(suspendPatch(true)).toEqual([{ op: 'add', path: '/spec/suspend', value: true }]);
    expect(suspendPatch(false)).toEqual([{ op: 'add', path: '/spec/suspend', value: false }]);
  });
});
