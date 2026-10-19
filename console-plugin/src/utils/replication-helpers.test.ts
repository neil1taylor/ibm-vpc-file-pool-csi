import { destinationSpecFields, replicationDestination } from './replication-helpers';

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
