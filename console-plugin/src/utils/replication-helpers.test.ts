import {
  describeReceiverReachable,
  describeVMRecovery,
  describeVolumeStatus,
  destinationSpecFields,
  replicationDestination,
  vmCopySettingsText,
  vmCopySpecFields,
} from './replication-helpers';
import { ReplicationPolicySpec, SubVolumeReplicationStatus } from '../types';

describe('destinationSpecFields', () => {
  it('direct mode sets only the NFS server', () => {
    expect(
      destinationSpecFields({
        isDriverToDriver: false,
        nfsServer: '10.0.0.5',
        endpoint: 'https://x',
        authSecretRef: 's',
        caCertSecretRef: 'ca',
      }),
    ).toEqual({ destinationNFSServer: '10.0.0.5' });
  });

  it('driver-to-driver mode omits the NFS server and an empty CA secret', () => {
    expect(
      destinationSpecFields({
        isDriverToDriver: true,
        nfsServer: '10.0.0.5',
        endpoint: 'https://x',
        authSecretRef: 's',
        caCertSecretRef: '  ',
      }),
    ).toEqual({ destinationEndpoint: 'https://x', destinationAuthSecretRef: 's' });
  });

  it('driver-to-driver mode includes a trimmed CA secret', () => {
    expect(
      destinationSpecFields({
        isDriverToDriver: true,
        nfsServer: '',
        endpoint: 'https://x',
        authSecretRef: 's',
        caCertSecretRef: ' ca ',
      }),
    ).toEqual({
      destinationEndpoint: 'https://x',
      destinationAuthSecretRef: 's',
      destinationCACertSecretRef: 'ca',
    });
  });
});

describe('replicationDestination', () => {
  it('shows the endpoint for driver-to-driver policies', () => {
    expect(
      replicationDestination({
        destinationEndpoint: 'https://r.example.com',
        destinationBasePath: '/pvcs',
      } as never),
    ).toBe('https://r.example.com');
  });

  it('shows server and base path for direct NFS policies', () => {
    expect(
      replicationDestination({ destinationNFSServer: '10.0.0.5', destinationBasePath: '/dr' } as never),
    ).toBe('10.0.0.5:/dr');
  });

  it('falls back to a dash', () => {
    expect(replicationDestination(undefined)).toBe('-');
  });
});

describe('vmCopySpecFields', () => {
  it('leaves out settings that equal the defaults: no highest speed, 24 hours', () => {
    expect(vmCopySpecFields({ speedLimitMBps: 0, restorePointKeepHours: 24 })).toEqual({});
  });

  it('sets a highest speed when there is one', () => {
    expect(vmCopySpecFields({ speedLimitMBps: 50, restorePointKeepHours: 24 })).toEqual({ vmCopySpeedLimitMBps: 50 });
    expect(vmCopySpecFields({ speedLimitMBps: 20, restorePointKeepHours: 24 })).toEqual({ vmCopySpeedLimitMBps: 20 });
  });

  it('sets a changed keep time', () => {
    expect(vmCopySpecFields({ speedLimitMBps: 0, restorePointKeepHours: 72 })).toEqual({ restorePointKeepHours: 72 });
  });

  it('keeps the keep time at 1 hour or more and the speed at 0 or more', () => {
    expect(vmCopySpecFields({ speedLimitMBps: -5, restorePointKeepHours: 0 })).toEqual({ restorePointKeepHours: 1 });
  });
});

describe('vmCopySettingsText', () => {
  it('says the copy adapts when the policy sets nothing', () => {
    expect(vmCopySettingsText({} as ReplicationPolicySpec)).toEqual({
      speed: 'Adapts to the load on the share (no highest speed)',
      keep: '24 hours (default)',
    });
  });

  it('shows the highest speed an adaptive copy has', () => {
    expect(vmCopySettingsText({ vmCopySpeedLimitMBps: 50, restorePointKeepHours: 72 } as ReplicationPolicySpec)).toEqual({
      speed: 'Adapts to the load on the share, up to 50 MB/s per disk',
      keep: '72 hours',
    });
    expect(vmCopySettingsText({ vmCopySpeedLimitMBps: 0, restorePointKeepHours: 1 } as ReplicationPolicySpec).speed).toBe(
      'Adapts to the load on the share (no highest speed)',
    );
  });

  it('shows the fixed speed of a copy that does not adapt', () => {
    expect(vmCopySettingsText({ vmCopyAdaptive: false } as ReplicationPolicySpec).speed).toBe('20 MB/s per disk (fixed, default)');
    expect(vmCopySettingsText({ vmCopyAdaptive: false, vmCopySpeedLimitMBps: 0 } as ReplicationPolicySpec).speed).toBe('Unlimited (fixed)');
    expect(vmCopySettingsText({ vmCopyAdaptive: false, vmCopySpeedLimitMBps: 50 } as ReplicationPolicySpec).speed).toBe('50 MB/s per disk (fixed)');
  });
});

describe('describeVolumeStatus', () => {
  const base: SubVolumeReplicationStatus = { subVolumeName: 'pvc-a' };

  it('a plain volume with no error is ok and has no VM fields', () => {
    const d = describeVolumeStatus({ ...base, lastSyncTime: '2026-10-20T10:00:00Z', bytesSynced: 100 });
    expect(d.severity).toBe('ok');
    expect(d.isVMDisk).toBe(false);
    expect(d.consistency).toBe('-');
    expect(d.restorePoints).toBe('-');
  });

  it('a VM disk shows its copy moment, consistency and restore points, with times as ages like the rest of the page', () => {
    jest.useFakeTimers().setSystemTime(new Date('2026-10-20T12:00:00Z'));
    const d = describeVolumeStatus({
      ...base,
      copyMoment: '2026-10-20T10:00:00Z',
      consistency: 'FileSystemConsistent',
      restorePoints: 3,
      oldestRestorePoint: '2026-10-19T10:30:00Z',
    });
    expect(d.isVMDisk).toBe(true);
    jest.useRealTimers();
    expect(d.copyMoment).toBe('2h');
    expect(d.consistency).toBe('File-system consistent');
    expect(d.consistencyWarning).toBe(false);
    expect(d.restorePoints).toBe('3 (oldest 1d)');
  });

  it('crash consistent copies are flagged, with the reason', () => {
    const d = describeVolumeStatus({
      ...base,
      copyMoment: '2026-10-20T10:00:00Z',
      consistency: 'CrashConsistent',
      consistencyReason: 'no guest agent',
    });
    expect(d.consistency).toBe('Crash consistent');
    expect(d.consistencyWarning).toBe(true);
    expect(d.consistencyReason).toBe('no guest agent');
  });

  it('a first copy with no restore points says none', () => {
    const d = describeVolumeStatus({ ...base, copyMoment: '2026-10-20T10:00:00Z', consistency: 'FileSystemConsistent' });
    expect(d.restorePoints).toBe('none yet');
  });

  it('waiting messages are warnings, not errors', () => {
    for (const msg of [
      'skipped: in use by running VM ns/pod; hot-plugged disks cannot be copied yet',
      'copy waits: the VM is moving to another worker',
      'copy lost: the VM restarted or moved during the copy; the next cycle starts a new copy',
    ]) {
      const d = describeVolumeStatus({ ...base, lastError: msg });
      expect(d.severity).toBe('warning');
      expect(d.message).toBe(msg);
    }
  });

  it('other errors are errors', () => {
    const d = describeVolumeStatus({ ...base, lastError: 'apply Job failed for pvc-a' });
    expect(d.severity).toBe('danger');
  });

  it('a copy in progress shows its phase', () => {
    const d = describeVolumeStatus({ ...base, vmCopy: { id: 'x', phase: 'Sending' } });
    expect(d.isVMDisk).toBe(true);
    expect(d.copyPhase).toBe('Sending');
  });

  it('a next copy that will be full is noted', () => {
    // vmChangeList is empty after a VM move, restart or a refused change copy
    const d = describeVolumeStatus({ ...base, copyMoment: '2026-10-20T10:00:00Z', consistency: 'FileSystemConsistent', vmChangeList: '' });
    expect(d.nextCopy).toBe('full');
    const e = describeVolumeStatus({ ...base, copyMoment: '2026-10-20T10:00:00Z', consistency: 'FileSystemConsistent', vmChangeList: 'repl-abc' });
    expect(e.nextCopy).toBe('changes only');
  });
});

describe('describeReceiverReachable', () => {
  const rp = (spec: object, conditions?: object[]) =>
    ({ spec, status: { conditions } }) as never;

  it('says nothing for a direct NFS policy', () => {
    expect(describeReceiverReachable(rp({ destinationNFSServer: '10.0.0.5' }))).toBeNull();
  });

  it('says Checking until the controller has a result', () => {
    expect(describeReceiverReachable(rp({ destinationEndpoint: 'https://r' }))).toEqual({
      label: 'Checking...',
      color: 'grey',
      detail: '',
    });
  });

  it('says Reachable when the condition is True', () => {
    const view = describeReceiverReachable(
      rp({ destinationEndpoint: 'https://r' }, [{ type: 'ReceiverReachable', status: 'True', message: 'ok' }]),
    );
    expect(view).toEqual({ label: 'Reachable', color: 'green', detail: '' });
  });

  it('says Not reachable with the reason in plain words when it is False', () => {
    const view = describeReceiverReachable(
      rp({ destinationEndpoint: 'https://r' }, [
        { type: 'ReceiverReachable', status: 'False', reason: 'TokenRefused', message: 'token refused' },
      ]),
    );
    expect(view).toEqual({ label: 'Not reachable', color: 'red', detail: 'token refused' });
  });
});

describe('describeVMRecovery', () => {
  const now = new Date('2026-10-20T12:00:00Z');
  beforeEach(() => jest.useFakeTimers().setSystemTime(now));
  afterEach(() => jest.useRealTimers());

  it('shows the copy moment, consistency and the moments all disks share', () => {
    const [v] = describeVMRecovery(
      [
        {
          name: 'default/web',
          subVolumes: ['pvc-a', 'pvc-b'],
          copyMoment: '2026-10-20T11:00:00Z',
          consistency: 'FileSystemConsistent',
          commonMoments: ['2026-10-20T11:00:00Z', '2026-10-20T10:00:00Z', '2026-10-20T09:00:00Z'],
        },
      ],
      '2h',
      now,
    );
    expect(v.name).toBe('default/web');
    expect(v.disks).toBe(2);
    expect(v.copyMoment).toBe('1h');
    expect(v.consistency).toBe('File-system consistent');
    expect(v.consistencyWarning).toBe(false);
    expect(v.momentCount).toBe(3);
    expect(v.moments).toEqual(['1h', '2h', '3h']);
    expect(v.stale).toBe(false);
  });

  it('warns when the newest copy is older than twice the schedule', () => {
    const vm = (copyMoment: string) => ({ name: 'default/web', subVolumes: ['a'], copyMoment, commonMoments: [copyMoment] });
    expect(describeVMRecovery([vm('2026-10-20T07:59:00Z')], '2h', now)[0].stale).toBe(true);
    expect(describeVMRecovery([vm('2026-10-20T08:01:00Z')], '2h', now)[0].stale).toBe(false);
    expect(describeVMRecovery([vm('2026-10-20T07:59:00Z')], '2h', now)[0].staleText).toMatch(/older than twice the 2h schedule/);
  });

  it('warns when the VM has no copy at all, or the schedule cannot be read', () => {
    const none = describeVMRecovery([{ name: 'default/web', subVolumes: ['a'] }], '2h', now)[0];
    expect(none.copyMoment).toBe('-');
    expect(none.momentCount).toBe(0);
    expect(none.stale).toBe(true);
    const odd = describeVMRecovery([{ name: 'x', subVolumes: ['a'], copyMoment: '2020-01-01T00:00:00Z' }], 'weekly', now)[0];
    expect(odd.stale).toBe(false);
  });

  it('flags a crash consistent copy and caps the list of moments', () => {
    const moments = Array.from({ length: 8 }, (_, i) => new Date(now.getTime() - (i + 1) * 60000).toISOString());
    const [v] = describeVMRecovery(
      [{ name: 'a/b', subVolumes: ['x'], copyMoment: moments[0], consistency: 'CrashConsistent', commonMoments: moments }],
      '1h',
      now,
    );
    expect(v.consistencyWarning).toBe(true);
    expect(v.momentCount).toBe(8);
    expect(v.moments).toHaveLength(5);
  });

  it('says there is nothing to recover to when the disks share no moment', () => {
    const [v] = describeVMRecovery(
      [{ name: 'a/b', subVolumes: ['x', 'y'], copyMoment: '2026-10-20T11:00:00Z', commonMoments: [] }],
      '2h',
      now,
    );
    expect(v.momentCount).toBe(0);
    expect(v.noCommonMoment).toBe(true);
  });
});
