import React from 'react';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { k8sPatch, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import BackupListPage from './BackupListPage';

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

const ago = (hours: number) => new Date(Date.now() - hours * 3600 * 1000).toISOString();

const makePolicies = () => [
  {
    metadata: { name: 'nightly' },
    spec: { snapshotPool: 'snaps', schedule: '6h', keepFor: '14d', selector: {}, export: { bucket: 'b' } },
    status: {
      phase: 'Active',
      volumeCount: 2,
      volumes: [
        { pvcNamespace: 'a', pvcName: 'x', subVolumeName: 's1', lastBackupTime: ago(1) },
        { pvcNamespace: 'a', pvcName: 'y', subVolumeName: 's2', lastBackupTime: ago(5) },
      ],
    },
  },
  {
    metadata: { name: 'full-one' },
    spec: { snapshotPool: 'snaps', schedule: '1h', selector: {} },
    status: {
      phase: 'Error',
      message: 'share sh-1 of the snapshot pool "snaps" is 91% full (limit 85%); new backups to it are stopped',
      volumeCount: 0,
    },
  },
];

let policies = makePolicies();

beforeEach(() => {
  jest.clearAllMocks();
  policies = makePolicies();
  (useAccessReview as jest.Mock).mockReturnValue([true, false]);
  (k8sPatch as jest.Mock).mockResolvedValue({});
  (useK8sWatchResource as jest.Mock).mockImplementation((req: unknown) => [req ? policies : undefined, true, undefined]);
});

describe('BackupListPage', () => {
  it('shows one row for each policy with pool, schedule, keep time, volumes, stalest backup and export', () => {
    render(<BackupListPage />);
    const row = screen.getByRole('link', { name: 'nightly' }).closest('tr')!;
    expect(row.textContent).toContain('snaps');
    expect(row.textContent).toContain('6h');
    expect(row.textContent).toContain('14d');
    expect(row.textContent).toContain('Active');
    expect(row.textContent).toContain('2');
    expect(row.textContent).toContain('5h');
    expect(row.textContent).toContain('On');
    expect(screen.getByRole('link', { name: 'nightly' }).getAttribute('href')).toBe('/vpc-file-pools/backups/nightly');
  });

  it('shows the default keep time of 7 days, and the export as off', () => {
    render(<BackupListPage />);
    const row = screen.getByRole('link', { name: 'full-one' }).closest('tr')!;
    expect(row.textContent).toContain('7d');
    expect(row.textContent).toContain('Off');
  });

  it('says in plain words that backups are stopped, with a link to the pool', () => {
    render(<BackupListPage />);
    expect(screen.getByText(/New backups are stopped: the snapshot pool is more than 85% full/)).toBeTruthy();
    expect(screen.getByRole('link', { name: /Open the pool snaps/ }).getAttribute('href')).toBe('/vpc-file-pools/pools/snaps');
  });

  it('has help buttons on the status columns', () => {
    render(<BackupListPage />);
    for (const t of ['Phase', 'Stalest Backup', 'Export']) {
      expect(screen.getByRole('button', { name: `About the ${t} column` })).toBeTruthy();
    }
  });

  it('opens the column help', () => {
    render(<BackupListPage />);
    fireEvent.click(screen.getByRole('button', { name: 'About the Stalest Backup column' }));
    expect(screen.getByText(/oldest/i)).toBeTruthy();
  });

  it('says when there are no policies', () => {
    (useK8sWatchResource as jest.Mock).mockImplementation(() => [[], true, undefined]);
    render(<BackupListPage />);
    expect(screen.getByText('No BackupPolicies found')).toBeTruthy();
  });
});

describe('BackupListPage actions', () => {
  const openMenu = (policyName: string) => {
    const row = screen.getByRole('link', { name: policyName }).closest('tr')!;
    fireEvent.click(within(row).getByRole('button', { name: 'Kebab toggle' }));
  };

  it('has a link to restore from COS', () => {
    render(<BackupListPage />);
    expect(screen.getByRole('link', { name: 'Restore from COS' }).getAttribute('href')).toBe('/vpc-file-pools/backups/~cos-restore');
  });

  it('has a create link', () => {
    render(<BackupListPage />);
    expect(screen.getByText('Create BackupPolicy')).toBeTruthy();
  });

  it('has Edit, Suspend and Delete in the menu of a row', () => {
    render(<BackupListPage />);
    openMenu('nightly');
    expect(screen.getByRole('link', { name: 'Edit' }).getAttribute('href')).toBe('/vpc-file-pools/backups/nightly/edit');
    expect(screen.getByRole('menuitem', { name: 'Suspend' })).toBeTruthy();
    expect(screen.getByRole('menuitem', { name: 'Delete' })).toBeTruthy();
  });

  it('suspends a policy with a patch of spec.suspend', async () => {
    render(<BackupListPage />);
    openMenu('nightly');
    fireEvent.click(screen.getByRole('menuitem', { name: 'Suspend' }));
    await waitFor(() => expect(k8sPatch).toHaveBeenCalledTimes(1));
    const arg = (k8sPatch as jest.Mock).mock.calls[0][0];
    expect(arg.data).toEqual([{ op: 'add', path: '/spec/suspend', value: true }]);
    expect(arg.resource.metadata.name).toBe('nightly');
  });

  it('offers Resume for a suspended policy', () => {
    policies[0].status.phase = 'Suspended';
    (policies[0].spec as { suspend?: boolean }).suspend = true;
    render(<BackupListPage />);
    openMenu('nightly');
    expect(screen.getByRole('menuitem', { name: 'Resume' })).toBeTruthy();
  });

  it('asks before it deletes, and says the backups stay', () => {
    render(<BackupListPage />);
    openMenu('nightly');
    fireEvent.click(screen.getByRole('menuitem', { name: 'Delete' }));
    expect(screen.getByText(/backups already made stay/i)).toBeTruthy();
  });

  it('turns the changing actions off, and names the rights, without the rights', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    render(<BackupListPage />);
    openMenu('nightly');
    expect(screen.getByRole('menuitem', { name: /Suspend.*update BackupPolicy/ })).toBeTruthy();
    expect(screen.getByRole('menuitem', { name: /Delete.*delete BackupPolicy/ })).toBeTruthy();
  });
});
