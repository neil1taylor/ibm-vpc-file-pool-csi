import { destinationSpecFields, replicationDestination, vmCopySettingsText, vmCopySpecFields } from './replication-helpers';
import { ReplicationPolicySpec } from '../types';

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
  it('leaves out settings that equal the defaults', () => {
    expect(vmCopySpecFields({ speedLimitMBps: 20, restorePointKeepHours: 24 })).toEqual({});
  });

  it('sets a changed speed limit, and 0 means unlimited', () => {
    expect(vmCopySpecFields({ speedLimitMBps: 50, restorePointKeepHours: 24 })).toEqual({ vmCopySpeedLimitMBps: 50 });
    expect(vmCopySpecFields({ speedLimitMBps: 0, restorePointKeepHours: 24 })).toEqual({ vmCopySpeedLimitMBps: 0 });
  });

  it('sets a changed keep time', () => {
    expect(vmCopySpecFields({ speedLimitMBps: 20, restorePointKeepHours: 72 })).toEqual({ restorePointKeepHours: 72 });
  });

  it('keeps the keep time at 1 hour or more and the speed at 0 or more', () => {
    expect(vmCopySpecFields({ speedLimitMBps: -5, restorePointKeepHours: 0 })).toEqual({ vmCopySpeedLimitMBps: 0, restorePointKeepHours: 1 });
  });
});

describe('vmCopySettingsText', () => {
  it('shows defaults when the policy sets nothing', () => {
    expect(vmCopySettingsText({} as ReplicationPolicySpec)).toEqual({ speed: '20 MB/s per disk (default)', keep: '24 hours (default)' });
  });

  it('shows what the policy sets', () => {
    expect(vmCopySettingsText({ vmCopySpeedLimitMBps: 0, restorePointKeepHours: 1 } as ReplicationPolicySpec)).toEqual({
      speed: 'Unlimited',
      keep: '1 hour',
    });
    expect(vmCopySettingsText({ vmCopySpeedLimitMBps: 50, restorePointKeepHours: 72 } as ReplicationPolicySpec)).toEqual({
      speed: '50 MB/s per disk',
      keep: '72 hours',
    });
  });
});
