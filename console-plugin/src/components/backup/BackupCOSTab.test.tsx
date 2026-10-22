import React from 'react';
import { render, screen } from '@testing-library/react';
import BackupCOSTab from './BackupCOSTab';
import { BackupPolicy } from '../../types';

const mockPush = jest.fn();
jest.mock('react-router-dom', () => ({
  useHistory: () => ({ push: mockPush }),
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));
jest.mock('./ExportSetupGuide', () => ({
  __esModule: true,
  default: ({ policy }: { policy: { metadata: { name: string } } }) => <div>Setup guide for {policy.metadata.name}</div>,
}));
let mockProps: { repo?: { bucket: string; prefix: string }; unavailable?: string; onPick?: (v: string, m: string) => void } = {};
jest.mock('./COSContents', () => ({
  __esModule: true,
  default: (props: typeof mockProps) => {
    mockProps = props;
    return <div>COS contents for {props.repo ? `${props.repo.bucket}/${props.repo.prefix}` : 'nothing'}</div>;
  },
}));

const ago = (hours: number) => new Date(Date.now() - hours * 3600 * 1000).toISOString();

const policy = (withExport = true): BackupPolicy =>
  ({
    metadata: { name: 'nightly' },
    spec: {
      snapshotPool: 'snaps',
      schedule: '6h',
      selector: {},
      export: withExport
        ? {
            bucket: 'pool-backups',
            endpoint: 's3.eu-de.cloud-object-storage.appdomain.cloud',
            prefix: 'lab',
            credentialsSecret: 'cos-credentials',
            backupKeySecret: 'cos-backup-key',
            keyProtect: { instanceID: 'inst-1', region: 'eu-de', rootKeyID: 'root-1', apiKeySecret: 'keyprotect-apikey' },
          }
        : undefined,
    },
    status: {
      phase: 'Active',
      volumes: [
        { pvcNamespace: 'apps', pvcName: 'a', subVolumeName: 's1', exportedTime: ago(2), exportedBytes: 2 * 1024 * 1024 },
        { pvcNamespace: 'apps', pvcName: 'b', subVolumeName: 's2', exportError: 'bucket not reachable' },
        { pvcNamespace: 'apps', pvcName: 'c', subVolumeName: 's3' },
      ],
    },
  }) as BackupPolicy;

describe('BackupCOSTab', () => {
  beforeEach(() => {
    jest.clearAllMocks();
    mockProps = {};
  });

  it('shows the export settings in plain words, with the names of the Secrets', () => {
    render(<BackupCOSTab policy={policy()} />);
    expect(screen.getByText('pool-backups')).toBeTruthy();
    expect(screen.getByText('lab/')).toBeTruthy();
    expect(screen.getByText('cos-credentials')).toBeTruthy();
    expect(screen.getByText('keyprotect-apikey')).toBeTruthy();
    expect(screen.getByText(/never shows the contents of a Secret/)).toBeTruthy();
  });

  it('shows the export state of each volume', () => {
    render(<BackupCOSTab policy={policy()} />);
    expect(screen.getByText('apps/a')).toBeTruthy();
    expect(screen.getByText('Sent 2h ago (2.0 MiB)')).toBeTruthy();
    expect(screen.getByText('Failed: bucket not reachable')).toBeTruthy();
    expect(screen.getByText('Not sent yet')).toBeTruthy();
  });

  it('gives the list the repository of the policy', () => {
    render(<BackupCOSTab policy={policy()} />);
    expect(screen.getByText('COS contents for pool-backups/lab/nightly/')).toBeTruthy();
  });

  it('sends a picked moment to the restore-from-COS page', () => {
    render(<BackupCOSTab policy={policy()} />);
    mockProps.onPick!('pvc-1', '2026-10-20T09:00:00Z');
    expect(mockPush).toHaveBeenCalledWith(
      '/vpc-file-pools/backups/~cos-restore?policy=nightly&volume=pvc-1&moment=2026-10-20T09%3A00%3A00Z',
    );
  });

  it('links to the restore-from-COS page for this policy', () => {
    render(<BackupCOSTab policy={policy()} />);
    expect(screen.getByRole('link', { name: 'Restore from COS' }).getAttribute('href')).toBe(
      '/vpc-file-pools/backups/~cos-restore?policy=nightly',
    );
  });

  it('says when the policy does not export, and lists nothing', () => {
    render(<BackupCOSTab policy={policy(false)} />);
    expect(screen.getByText(/does not export to COS/)).toBeTruthy();
    expect(screen.getByText('COS contents for nothing')).toBeTruthy();
    expect(mockProps.unavailable).toMatch(/does not export to COS/);
  });

  it('has the setup guide, whether or not the policy exports', () => {
    const { unmount } = render(<BackupCOSTab policy={policy()} />);
    expect(screen.getByText('Setup guide for nightly')).toBeTruthy();
    unmount();
    render(<BackupCOSTab policy={policy(false)} />);
    expect(screen.getByText('Setup guide for nightly')).toBeTruthy();
  });
});
