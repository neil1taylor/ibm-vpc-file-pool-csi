import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import BackupDetailsPage from './BackupDetailsPage';
import BackupVolumesTable from './BackupVolumesTable';
import { BackupPolicy } from '../../types';

jest.mock('react-router-dom', () => ({
  useParams: () => ({ name: 'nightly' }),
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

const ago = (hours: number) => new Date(Date.now() - hours * 3600 * 1000).toISOString();

const policy = (over: Partial<BackupPolicy> = {}): BackupPolicy =>
  ({
    metadata: { name: 'nightly' },
    spec: {
      snapshotPool: 'snaps',
      schedule: '6h',
      keepFor: '7d',
      selector: { matchLabels: { backup: 'yes' } },
      namespaces: ['apps'],
      vmCopySpeedLimitMBps: 10,
    },
    status: {
      phase: 'Active',
      volumeCount: 3,
      volumes: [
        {
          pvcNamespace: 'apps',
          pvcName: 'disk-1',
          subVolumeName: 'pvc-1',
          vmName: 'web',
          lastBackupTime: ago(2),
          oldestBackupTime: ago(50),
          restorePoints: 8,
          bytesWritten: 1024 * 1024 * 1024,
          consistency: 'CrashConsistent',
        },
        {
          pvcNamespace: 'apps',
          pvcName: 'data',
          subVolumeName: 'pvc-2',
          lastBackupTime: ago(3),
          consistency: 'FileCopy',
          lastError: 'copy failed',
        },
        {
          pvcNamespace: 'apps',
          pvcName: 'old',
          subVolumeName: 'pvc-3',
          retained: true,
          lastBackupTime: '2026-10-01T00:00:00Z',
        },
      ],
    },
    ...over,
  }) as BackupPolicy;

const watch = (p: BackupPolicy | undefined) =>
  (useK8sWatchResource as jest.Mock).mockImplementation((req: unknown) => [req ? p : undefined, true, undefined]);

describe('BackupDetailsPage', () => {
  it('shows the spec in plain words', () => {
    watch(policy());
    render(<BackupDetailsPage />);
    expect(screen.getByRole('heading', { name: 'nightly' })).toBeTruthy();
    expect(screen.getByText('backup=yes')).toBeTruthy();
    expect(screen.getByText('Namespaces').closest('div')!.textContent).toContain('apps');
    expect(screen.getByText(/10 MB\/s per disk/)).toBeTruthy();
    expect(screen.getByRole('link', { name: 'snaps' }).getAttribute('href')).toBe('/vpc-file-pools/pools/snaps');
  });

  it('shows the default VM copy speed when none is set, and "all namespaces"', () => {
    const p = policy();
    delete p.spec.vmCopySpeedLimitMBps;
    delete p.spec.namespaces;
    watch(p);
    render(<BackupDetailsPage />);
    expect(screen.getByText(/20 MB\/s per disk \(default\)/)).toBeTruthy();
    expect(screen.getByText('All namespaces')).toBeTruthy();
  });

  it('has card help on the Overview and Volumes cards', () => {
    watch(policy());
    render(<BackupDetailsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'About Backed-up Volumes' }));
    expect(screen.getByText(/restore points you can go back to/)).toBeTruthy();
    expect(screen.getByRole('button', { name: 'About Backup Policy' })).toBeTruthy();
  });

  it('shows the stop banner with a link to the pool', () => {
    watch(
      policy({
        status: {
          phase: 'Error',
          message: 'share sh-1 of the snapshot pool "snaps" is 91% full (limit 85%); stopped',
          volumes: [],
        },
      }),
    );
    render(<BackupDetailsPage />);
    expect(screen.getByText(/New backups are stopped/)).toBeTruthy();
    expect(screen.getByRole('link', { name: /Open the pool snaps/ })).toBeTruthy();
  });

  it('says when the policy is not found', () => {
    watch(undefined);
    render(<BackupDetailsPage />);
    expect(screen.getByText(/was not found/)).toBeTruthy();
  });
});

describe('BackupVolumesTable', () => {
  it('shows one row for each volume with the columns of the spec', () => {
    render(<BackupVolumesTable policy={policy()} />);
    const row = screen.getByText('apps/disk-1').closest('tr')!;
    expect(row.textContent).toContain('web');
    expect(row.textContent).toContain('2h');
    expect(row.textContent).toContain('2d');
    expect(row.textContent).toContain('8');
    expect(row.textContent).toContain('1.0 GiB');
  });

  it('warns for a crash-consistent copy', () => {
    render(<BackupVolumesTable policy={policy()} />);
    expect(screen.getByText('Crash consistent')).toBeTruthy();
    expect(screen.getByText('File copy')).toBeTruthy();
  });

  it('shows the error of a volume', () => {
    render(<BackupVolumesTable policy={policy()} />);
    expect(screen.getByText('copy failed')).toBeTruthy();
  });

  it('marks a retained volume with the date it is kept until', () => {
    render(<BackupVolumesTable policy={policy()} />);
    expect(screen.getByText('No longer backed up, kept until 2026-10-08')).toBeTruthy();
  });

  it('shows the export state of each volume', () => {
    const p = policy();
    p.spec.export = { bucket: 'b' } as never;
    p.status!.volumes![0].exportedTime = ago(4);
    render(<BackupVolumesTable policy={p} />);
    expect(screen.getByText('Sent 4h ago')).toBeTruthy();
  });

  it('has help buttons on the status columns', () => {
    render(<BackupVolumesTable policy={policy()} />);
    for (const t of ['Last Backup', 'Oldest Backup', 'Restore Points', 'Consistency', 'Export']) {
      expect(screen.getByRole('button', { name: `About the ${t} column` })).toBeTruthy();
    }
  });

  it('says when no volume is backed up yet', () => {
    render(<BackupVolumesTable policy={policy({ status: { phase: 'Active', volumes: [] } })} />);
    expect(screen.getByText(/No volumes are backed up yet/)).toBeTruthy();
  });

  it('hides the Browse files action until the file-restore feature is present', () => {
    render(<BackupVolumesTable policy={policy()} />);
    expect(screen.queryByRole('button', { name: 'Kebab toggle' })).toBeNull();
    expect(screen.queryByText('Browse files')).toBeNull();
  });

  it('shows the Browse files action on each volume when the feature is present', () => {
    render(<BackupVolumesTable policy={policy()} settings={{ fileRestore: true }} />);
    fireEvent.click(screen.getAllByRole('button', { name: 'Kebab toggle' })[0]);
    expect(screen.getByText('Browse files')).toBeTruthy();
  });
});
