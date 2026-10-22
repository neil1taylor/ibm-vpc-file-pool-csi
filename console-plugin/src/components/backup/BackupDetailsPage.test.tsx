import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sPatch, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import BackupDetailsPage from './BackupDetailsPage';
import BackupVolumesTable from './BackupVolumesTable';
import { BackupPolicy } from '../../types';

let mockSearch = '';
const mockReplace = jest.fn();
jest.mock('./DeleteBackupsDialog', () => ({
  __esModule: true,
  default: ({ volume, onClose }: { volume: { pvcNamespace: string; pvcName: string }; onClose: () => void }) => (
    <div role="dialog">
      Delete backups of {volume.pvcNamespace}/{volume.pvcName}
      <button onClick={onClose}>Close dialog</button>
    </div>
  ),
}));

jest.mock('react-router-dom', () => ({
  useParams: () => ({ name: 'nightly' }),
  useLocation: () => ({ search: mockSearch }),
  useHistory: () => ({ push: jest.fn(), replace: mockReplace }),
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

beforeEach(() => {
  jest.clearAllMocks();
  mockSearch = '';
  (useAccessReview as jest.Mock).mockReturnValue([true, false]);
  (k8sPatch as jest.Mock).mockResolvedValue({});
});

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

describe('BackupDetailsPage actions', () => {
  it('has Edit, Suspend and Delete', () => {
    watch(policy());
    render(<BackupDetailsPage />);
    expect(screen.getByRole('link', { name: 'Edit' }).getAttribute('href')).toBe('/vpc-file-pools/backups/nightly/edit');
    expect(screen.getByRole('button', { name: 'Suspend' })).toBeTruthy();
    expect(screen.getByRole('button', { name: 'Delete' })).toBeTruthy();
  });

  it('suspends and resumes with a patch', async () => {
    watch(policy());
    const { unmount } = render(<BackupDetailsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Suspend' }));
    await waitFor(() => expect(k8sPatch).toHaveBeenCalledTimes(1));
    expect((k8sPatch as jest.Mock).mock.calls[0][0].data).toEqual([{ op: 'add', path: '/spec/suspend', value: true }]);
    unmount();

    const p = policy();
    p.spec.suspend = true;
    p.status!.phase = 'Suspended';
    watch(p);
    render(<BackupDetailsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Resume' }));
    await waitFor(() => expect(k8sPatch).toHaveBeenCalledTimes(2));
    expect((k8sPatch as jest.Mock).mock.calls[1][0].data).toEqual([{ op: 'add', path: '/spec/suspend', value: false }]);
  });

  it('shows the error when the patch is refused', async () => {
    (k8sPatch as jest.Mock).mockRejectedValue(new Error('forbidden'));
    watch(policy());
    render(<BackupDetailsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Suspend' }));
    expect(await screen.findByText(/forbidden/)).toBeTruthy();
  });

  it('asks before it deletes, and says the backups stay on the snapshot pool', () => {
    watch(policy());
    render(<BackupDetailsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Delete' }));
    expect(screen.getByText(/backups already made stay/i)).toBeTruthy();
  });

  it('turns the actions off and names the rights without them', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    watch(policy());
    render(<BackupDetailsPage />);
    expect((screen.getByRole('button', { name: 'Suspend' }) as HTMLButtonElement).disabled).toBe(true);
    expect((screen.getByRole('button', { name: 'Delete' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/You are not allowed to update BackupPolicy objects/)).toBeTruthy();
    expect(screen.getByText(/You are not allowed to delete BackupPolicy objects/)).toBeTruthy();
  });
});

describe('Delete backups', () => {
  it('has a Delete backups action on each volume that opens the dialog for that volume', () => {
    watch(policy());
    render(<BackupDetailsPage />);
    fireEvent.click(screen.getAllByRole('button', { name: 'Kebab toggle' })[2]);
    fireEvent.click(screen.getByRole('menuitem', { name: 'Delete backups' }));
    expect(screen.getByRole('dialog').textContent).toContain('Delete backups of apps/old');
  });

  it('opens the dialog at once when the link has ?claim=', () => {
    mockSearch = '?claim=apps%2Fold';
    watch(policy());
    render(<BackupDetailsPage />);
    expect(screen.getByRole('dialog').textContent).toContain('Delete backups of apps/old');
  });

  it('opens no dialog for a claim that is not in the policy', () => {
    mockSearch = '?claim=apps%2Funknown';
    watch(policy());
    render(<BackupDetailsPage />);
    expect(screen.queryByRole('dialog')).toBeNull();
  });

  it('closes the dialog, and drops ?claim= from the link', () => {
    mockSearch = '?claim=apps%2Fold';
    watch(policy());
    render(<BackupDetailsPage />);
    fireEvent.click(screen.getByRole('button', { name: 'Close dialog' }));
    expect(screen.queryByRole('dialog')).toBeNull();
    expect(mockReplace).toHaveBeenCalledWith('/vpc-file-pools/backups/nightly');
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
    fireEvent.click(screen.getAllByRole('button', { name: 'Kebab toggle' })[0]);
    expect(screen.queryByText('Browse files')).toBeNull();
  });

  it('has a Restore action on each volume that links to the restore page with the claim', () => {
    render(<BackupVolumesTable policy={policy()} />);
    fireEvent.click(screen.getAllByRole('button', { name: 'Kebab toggle' })[0]);
    expect(screen.getByRole('link', { name: 'Restore' }).getAttribute('href')).toBe(
      '/vpc-file-pools/backups/nightly/restore?claim=apps%2Fdisk-1',
    );
  });

  it('turns Restore off and names the right when the user may not create BackupRestore objects', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    render(<BackupVolumesTable policy={policy()} />);
    fireEvent.click(screen.getAllByRole('button', { name: 'Kebab toggle' })[0]);
    expect(screen.getByRole('menuitem', { name: /Restore.*create BackupRestore/ }).hasAttribute('disabled') ||
      screen.getByRole('menuitem', { name: /Restore.*create BackupRestore/ }).getAttribute('aria-disabled') === 'true').toBe(true);
  });

  it('shows the Browse files action on each volume when the feature is present', () => {
    render(<BackupVolumesTable policy={policy()} settings={{ fileRestore: true }} />);
    fireEvent.click(screen.getAllByRole('button', { name: 'Kebab toggle' })[0]);
    expect(screen.getByText('Browse files')).toBeTruthy();
  });
});
