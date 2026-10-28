import React from 'react';
import { render, screen } from '@testing-library/react';
import VPCFilePoolsPage from './VPCFilePoolsPage';
import { GUIDE_BASE } from '../utils/help-links';

// react-router 5 brings its own React 17, so the router is replaced by a stub (as in the other page tests).
let mockPath = '/';
jest.mock('react-router-dom', () => ({
  Route: () => null,
  Switch: () => null,
  useHistory: () => ({ push: jest.fn() }),
  useLocation: () => ({ pathname: mockPath }),
}));

// Every sub-page is replaced by a stub: this test is about the Help link of the layout.
jest.mock('./dashboard/DashboardPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./filesharepool/FileSharePoolListPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./filesharepool/FileSharePoolDetailsPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./filesharepool/FileSharePoolCreatePage', () => ({ __esModule: true, default: () => null }));
jest.mock('./subvolume/SubVolumeListPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./subvolume/SubVolumeDetailsPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./subvolume/SubVolumeCreatePage', () => ({ __esModule: true, default: () => null }));
jest.mock('./subvolume/CloneVolumePage', () => ({ __esModule: true, default: () => null }));
jest.mock('./snapshot/SnapshotListPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./snapshot/SnapshotDetailsPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./snapshot/SnapshotCreatePage', () => ({ __esModule: true, default: () => null }));
jest.mock('./snapshot/RestoreSnapshotPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./migrate/MigrateVolumesPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./replicationpolicy/ReplicationPolicyListPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./replicationpolicy/ReplicationPolicyDetailsPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./replicationpolicy/ReplicationPolicyCreatePage', () => ({ __esModule: true, default: () => null }));
jest.mock('./backup/BackupListPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./backup/BackupRestorePage', () => ({ __esModule: true, default: () => null }));
jest.mock('./backup/BackupPolicyFormPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./backup/BackupCOSRestorePage', () => ({ __esModule: true, default: () => null }));
jest.mock('./backup/BackupDetailsPage', () => ({ __esModule: true, default: () => null }));
jest.mock('./monitoring/MonitoringPage', () => ({ __esModule: true, default: () => null }));

const cases: Array<[string, string]> = [
  ['/vpc-file-pools', 'SUPPORT.md'],
  ['/vpc-file-pools/pools', 'TUTORIAL-01-POOL-CREATION.md'],
  ['/vpc-file-pools/subvolumes', 'USER-GUIDE.md'],
  ['/vpc-file-pools/snapshots', 'TUTORIAL-02-SNAPSHOTS-AND-CLONES.md'],
  ['/vpc-file-pools/replication', 'TUTORIAL-06-REPLICATION-AND-FAILOVER.md'],
  ['/vpc-file-pools/backups', 'TUTORIAL-10-BACKUP-AND-RESTORE.md'],
  ['/vpc-file-pools/backups/nightly', 'TUTORIAL-10-BACKUP-AND-RESTORE.md'],
  ['/vpc-file-pools/backups/nightly/restore', 'TUTORIAL-10-BACKUP-AND-RESTORE.md'],
  ['/vpc-file-pools/backups/~new', 'TUTORIAL-10-BACKUP-AND-RESTORE.md'],
  ['/vpc-file-pools/backups/~cos-restore', 'TUTORIAL-10-BACKUP-AND-RESTORE.md'],
  ['/vpc-file-pools/backups/nightly/edit', 'TUTORIAL-10-BACKUP-AND-RESTORE.md'],
  ['/vpc-file-pools/monitoring', 'MONITORING.md'],
  ['/vpc-file-pools/pools/~new', 'TUTORIAL-01-POOL-CREATION.md'],
  ['/vpc-file-pools/pools/general-purpose', 'TUTORIAL-01-POOL-CREATION.md'],
  ['/vpc-file-pools/snapshots/snap-1', 'TUTORIAL-02-SNAPSHOTS-AND-CLONES.md'],
  ['/vpc-file-pools/replication/dr', 'TUTORIAL-06-REPLICATION-AND-FAILOVER.md'],
  ['/vpc-file-pools/replication/~recovery', 'TUTORIAL-06-REPLICATION-AND-FAILOVER.md'],
  ['/vpc-file-pools/migrate', 'TUTORIAL-08-MIGRATION-AND-CONSOLE.md'],
];

describe('every page has a Help link with the right guide', () => {
  it.each(cases)('%s', async (path, file) => {
    mockPath = path;
    render(<VPCFilePoolsPage />);
    const a = (await screen.findByTestId('help-link')) as HTMLAnchorElement;
    expect(a.getAttribute('href')).toBe(`${GUIDE_BASE}${file}`);
    expect(a.getAttribute('target')).toBe('_blank');
  });
});

describe('the Backups tab', () => {
  it('is one of the tabs under Pools', () => {
    mockPath = '/vpc-file-pools/backups';
    render(<VPCFilePoolsPage />);
    expect(screen.getByRole('tab', { name: 'Backups' })).toBeTruthy();
  });
});
