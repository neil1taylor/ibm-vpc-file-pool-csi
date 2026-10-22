import React from 'react';
import { fireEvent, render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import SnapshotListPage from './snapshot/SnapshotListPage';
import SubVolumeListPage from './subvolume/SubVolumeListPage';
import FileSharePoolListPage from './filesharepool/FileSharePoolListPage';
import ReplicationPolicyListPage from './replicationpolicy/ReplicationPolicyListPage';
import VolumeStatusTable from './replicationpolicy/VolumeStatusTable';
import { COLUMN_HELP } from '../utils/column-help';

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
  useHistory: () => ({ push: jest.fn() }),
  useLocation: () => ({ pathname: '/vpc-file-pools' }),
}));
jest.mock('./common/InstantSnapshotStatus', () => ({ __esModule: true, default: () => null }));

const items: Record<string, unknown[]> = {
  Snapshot: [{ metadata: { name: 's1' }, spec: { poolName: 'p', sourceSubVolume: 'pvc-1', sizeGB: 1 }, status: { phase: 'Ready', readyToUse: true } }],
  SubVolume: [{ metadata: { name: 'pvc-1' }, spec: { poolName: 'p', pvcName: 'c', pvcNamespace: 'n', requestedGB: 1 }, status: { phase: 'Bound' } }],
  FileSharePool: [{ metadata: { name: 'p' }, spec: { zone: 'z', profile: 'dp2', maxShares: 2 }, status: { phase: 'Ready' } }],
  ReplicationPolicy: [{ metadata: { name: 'rp' }, spec: { sourcePoolName: 'p', schedule: '1h', destinationEndpoint: 'https://r' }, status: { phase: 'Active' } }],
};

beforeEach(() => {
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind: { kind: string } } | null) => [
    req ? items[req.groupVersionKind.kind] ?? [] : undefined,
    true,
    undefined,
  ]);
});

const expectColumnHelp = (...titles: string[]) => {
  for (const t of titles) {
    expect(screen.getByRole('button', { name: `About the ${t} column` })).toBeTruthy();
  }
};

describe('status column help', () => {
  it('explains the snapshot Phase and Ready columns', () => {
    render(<SnapshotListPage />);
    expectColumnHelp('Phase', 'Ready');
  });

  it('explains the SubVolume Phase and Clone Status columns', () => {
    render(<SubVolumeListPage />);
    expectColumnHelp('Phase', 'Clone Status');
  });

  it('explains the pool Phase column', () => {
    render(<FileSharePoolListPage />);
    expectColumnHelp('Phase');
  });

  it('explains the replication Phase, Last Sync and Failures columns', () => {
    render(<ReplicationPolicyListPage />);
    expectColumnHelp('Phase', 'Last Sync', 'Failures', 'Receiver');
  });

  it('explains the per-volume replication status columns', () => {
    render(<VolumeStatusTable statuses={[{ subVolumeName: 'pvc-1' } as never]} />);
    expectColumnHelp('Last Sync / Copy Moment', 'Consistency', 'Restore Points', 'Next Copy', 'Status');
  });

  it('opens the text when the button is clicked', () => {
    render(<ReplicationPolicyListPage />);
    fireEvent.click(screen.getByRole('button', { name: 'About the Failures column' }));
    expect(screen.getByText(/pauses/)).toBeTruthy();
  });
});

describe('COLUMN_HELP', () => {
  it('has one to three sentences for every column', () => {
    for (const [key, help] of Object.entries(COLUMN_HELP)) {
      const sentences = help.text.split(/(?<=[.!?])\s+/).filter(Boolean);
      expect({ key, ok: sentences.length >= 1 && sentences.length <= 3 }).toEqual({ key, ok: true });
    }
  });
});
