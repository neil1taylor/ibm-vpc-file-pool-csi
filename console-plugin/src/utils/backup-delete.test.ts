import { BackupPolicy, BackupRestore, BackupVolumeStatus, FileSharePool } from '../types';
import {
  claimConfirmation,
  deleteAccessRights,
  deleteRefusal,
  jobOutcome,
  momentsCount,
  parseMeasureMessage,
  resolveShare,
} from './backup-delete';

const volume = (over: Partial<BackupVolumeStatus> = {}): BackupVolumeStatus => ({
  pvcNamespace: 'apps',
  pvcName: 'old',
  subVolumeName: 'pvc-3',
  subPath: '/pvcs/pvc-11111111-2222-3333-4444-555555555555',
  retained: true,
  restorePoints: 8,
  lastBackupTime: '2026-10-20T00:00:00Z',
  ...over,
});

const policy = {
  metadata: { name: 'nightly' },
  spec: { snapshotPool: 'snaps', schedule: '6h', selector: {} },
} as BackupPolicy;

const restore = (phase: string, over = {}): BackupRestore =>
  ({
    metadata: { name: 'restore-old-abc' },
    spec: { policy: 'nightly', sourceNamespace: 'apps', sourceName: 'old', targetNamespace: 'apps', targetName: 'x', ...over },
    status: { phase },
  }) as BackupRestore;

describe('deleteRefusal', () => {
  it('allows a volume that is no longer backed up and has no running restore', () => {
    expect(deleteRefusal(policy, volume(), [restore('Done'), restore('Failed')])).toBeUndefined();
  });

  it('refuses while the policy still backs up the volume, with the wording of the command line', () => {
    const msg = deleteRefusal(policy, volume({ retained: false }), []);
    expect(msg).toBe(
      'Policy nightly still backs up apps/old: remove the label that selects it, or delete the policy, wait for the next backup cycle, then try again.',
    );
  });

  it('refuses while a restore of the volume has not ended', () => {
    for (const phase of ['Pending', 'Provisioning', 'Restoring', '']) {
      expect(deleteRefusal(policy, volume(), [restore(phase)])).toBe(
        'Restore restore-old-abc of apps/old has not ended; wait for it, then try again.',
      );
    }
  });

  it('ignores a restore of another volume or another policy', () => {
    expect(deleteRefusal(policy, volume(), [restore('Restoring', { sourceName: 'other' })])).toBeUndefined();
    expect(deleteRefusal(policy, volume(), [restore('Restoring', { policy: 'weekly' })])).toBeUndefined();
  });

  it('refuses a volume with no backup folder in the status', () => {
    expect(deleteRefusal(policy, volume({ subPath: '' }), [])).toBe(
      'Policy nightly has no backup of apps/old to remove.',
    );
  });
});

describe('resolveShare', () => {
  const pool = (shares: unknown[]) => ({ metadata: { name: 'snaps' }, status: { shares } }) as unknown as FileSharePool;
  const share = (shareID: string, ip = '10.0.0.1', exportPath = `/${shareID}`) => ({ shareID, mountTargetIP: ip, exportPath });

  it('uses the share named in the status', () => {
    expect(resolveShare(pool([share('a'), share('b', '10.0.0.2')]), 'b')).toEqual({
      share: { mountTargetIP: '10.0.0.2', exportPath: '/b' },
    });
  });

  it('uses the first share for an entry with no share ID', () => {
    expect(resolveShare(pool([share('a'), share('b')]), '')).toEqual({ share: { mountTargetIP: '10.0.0.1', exportPath: '/a' } });
  });

  it('says why it cannot find the share', () => {
    expect(resolveShare(undefined, '').error).toMatch(/snapshot pool .* not found/i);
    expect(resolveShare(pool([]), '').error).toMatch(/has no share/);
    expect(resolveShare(pool([share('a')]), 'zzz').error).toMatch(/Share zzz is not a share of the snapshot pool/);
    expect(resolveShare(pool([share('a', '')]), '').error).toMatch(/no mount target yet/);
  });
});

describe('parseMeasureMessage', () => {
  it('reads kilobytes as bytes', () => {
    expect(parseMeasureMessage('12582912\n')).toBe(12582912 * 1024);
    expect(parseMeasureMessage('0')).toBe(0);
  });

  it('gives undefined for anything else', () => {
    for (const bad of ['', 'abc', '-5', '1.5', undefined]) {
      expect(parseMeasureMessage(bad)).toBeUndefined();
    }
  });
});

describe('jobOutcome', () => {
  const pod = (message?: string) => ({
    status: { containerStatuses: [{ state: { terminated: message === undefined ? undefined : { message } } }] },
  });

  it('is running until the Job has ended', () => {
    expect(jobOutcome(undefined, []).state).toBe('running');
    expect(jobOutcome({ metadata: { name: 'j' }, status: { active: 1 } }, []).state).toBe('running');
  });

  it('succeeds with the termination message of the pod', () => {
    expect(jobOutcome({ metadata: { name: 'j' }, status: { succeeded: 1 } }, [pod('42')])).toEqual({
      state: 'succeeded',
      message: '42',
    });
  });

  it('succeeds with an empty message when the pod is not there to read', () => {
    expect(jobOutcome({ metadata: { name: 'j' }, status: { succeeded: 1 } }, [])).toEqual({ state: 'succeeded', message: '' });
  });

  it('fails when the Job used up its retries, or has the Failed condition', () => {
    expect(jobOutcome({ metadata: { name: 'j' }, status: { failed: 4 } }, []).state).toBe('failed');
    expect(
      jobOutcome({ metadata: { name: 'j' }, status: { conditions: [{ type: 'Failed', status: 'True' }] } }, []).state,
    ).toBe('failed');
    expect(jobOutcome({ metadata: { name: 'j' }, status: { failed: 2 } }, []).state).toBe('running');
  });
});

describe('momentsCount', () => {
  it('is the older restore points plus the newest backup', () => {
    expect(momentsCount(volume({ restorePoints: 8 }))).toBe(9);
    expect(momentsCount(volume({ restorePoints: undefined }))).toBe(1);
    expect(momentsCount(volume({ restorePoints: 3, lastBackupTime: undefined }))).toBe(3);
  });
});

describe('claimConfirmation', () => {
  it('is namespace/name, as the command line asks', () => {
    expect(claimConfirmation(volume())).toBe('apps/old');
  });
});

describe('deleteAccessRights', () => {
  it('lists what the Jobs need in the driver namespace and for PVs', () => {
    const rights = deleteAccessRights('kube-system');
    expect(rights.map((r) => `${r.verb} ${r.resource}`)).toEqual([
      'create jobs',
      'delete jobs',
      'create persistentvolumeclaims',
      'delete persistentvolumeclaims',
      'create persistentvolumes',
      'delete persistentvolumes',
      'list pods',
    ]);
    expect(rights.find((r) => r.resource === 'persistentvolumes')!.namespace).toBeUndefined();
    expect(rights.find((r) => r.resource === 'jobs')!.namespace).toBe('kube-system');
  });
});
