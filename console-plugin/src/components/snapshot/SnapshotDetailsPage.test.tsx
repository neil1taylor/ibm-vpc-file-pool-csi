import React from 'react';
import { render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import SnapshotDetailsPage from './SnapshotDetailsPage';

jest.mock('react-router-dom', () => ({
  useHistory: () => ({ push: jest.fn() }),
  useParams: () => ({ name: 'snapshot-abc' }),
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

function watch(snapshot: unknown, contents: unknown[]) {
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind: { kind: string } } | null) => {
    if (!req) return [undefined, true, undefined];
    return req.groupVersionKind.kind === 'Snapshot' ? [snapshot, true, undefined] : [contents, true, undefined];
  });
}

const snapshot = (ready: boolean) => ({
  metadata: { name: 'snapshot-abc' },
  spec: { poolName: 'pool-a', sizeGB: 10, sourceSubVolume: 'pvc-1', shareID: 's', shareMountTargetIP: '1.2.3.4', snapshotPath: '/p', sourceSubPath: '/q' },
  status: { readyToUse: ready, phase: ready ? 'Ready' : 'Creating' },
});

const content = {
  metadata: { name: 'c' },
  spec: { volumeSnapshotRef: { name: 'daily', namespace: 'apps' } },
  status: { snapshotHandle: 'pool-a/share-9/snapshot-abc' },
};

describe('SnapshotDetailsPage restore action', () => {
  it('links to the restore page when the snapshot is ready and a VolumeSnapshot points to it', () => {
    watch(snapshot(true), [content]);
    render(<SnapshotDetailsPage />);
    expect(screen.getByRole('link', { name: 'Restore to new PVC' }).getAttribute('href')).toBe(
      '/vpc-file-pools/snapshots/snapshot-abc/restore',
    );
  });

  it('turns the action off and says why when no VolumeSnapshot points to the snapshot', () => {
    watch(snapshot(true), []);
    render(<SnapshotDetailsPage />);
    expect(screen.queryByRole('link', { name: 'Restore to new PVC' })).toBeNull();
    expect((screen.getByRole('button', { name: 'Restore to new PVC' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/Create a VolumeSnapshot first/)).toBeTruthy();
  });

  it('turns the action off and says why when the snapshot is not ready', () => {
    watch(snapshot(false), [content]);
    render(<SnapshotDetailsPage />);
    expect((screen.getByRole('button', { name: 'Restore to new PVC' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/not ready/i)).toBeTruthy();
  });
});
