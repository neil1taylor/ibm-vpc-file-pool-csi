import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sCreate, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import SnapshotCreatePage from './SnapshotCreatePage';

const push = jest.fn();
jest.mock('react-router-dom', () => ({
  useHistory: () => ({ push, goBack: jest.fn() }),
  useLocation: () => ({ search: '' }),
}));

const DRIVER = 'vpc-file-pool.csi.ibm.io';

function watch(data: Record<string, unknown[]>) {
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind: { kind: string } }) => [
    data[req.groupVersionKind.kind] ?? [],
    true,
    undefined,
  ]);
}

const pvc = (name: string, sc: string) => ({
  metadata: { name, namespace: 'apps' },
  spec: { storageClassName: sc },
});

beforeEach(() => jest.clearAllMocks());

describe('SnapshotCreatePage', () => {
  it('lists only claims from storage classes this driver provisions', () => {
    watch({
      PersistentVolumeClaim: [pvc('ours', 'pool-a'), pvc('theirs', 'gp')],
      StorageClass: [
        { metadata: { name: 'pool-a' }, provisioner: DRIVER },
        { metadata: { name: 'gp' }, provisioner: 'other' },
      ],
      VolumeSnapshotClass: [{ metadata: { name: 'any-name' }, driver: DRIVER }],
    });
    render(<SnapshotCreatePage />);
    expect(screen.getByRole('option', { name: /apps\/ours/ })).toBeTruthy();
    expect(screen.queryByRole('option', { name: /apps\/theirs/ })).toBeNull();
  });

  it('says so and disables Create when there is no snapshot class for the driver', () => {
    watch({
      PersistentVolumeClaim: [pvc('ours', 'pool-a')],
      StorageClass: [{ metadata: { name: 'pool-a' }, provisioner: DRIVER }],
      VolumeSnapshotClass: [{ metadata: { name: 'other' }, driver: 'other.csi' }],
    });
    render(<SnapshotCreatePage />);
    expect(screen.getByText(/No VolumeSnapshotClass uses the driver/)).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Create' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('creates a VolumeSnapshot with the class found by driver and opens it', async () => {
    watch({
      PersistentVolumeClaim: [pvc('ours', 'pool-a')],
      StorageClass: [{ metadata: { name: 'pool-a' }, provisioner: DRIVER }],
      VolumeSnapshotClass: [{ metadata: { name: 'any-name' }, driver: DRIVER }],
    });
    (k8sCreate as jest.Mock).mockResolvedValue({});
    render(<SnapshotCreatePage />);
    fireEvent.change(screen.getByLabelText(/^Name( \*)?$/), { target: { value: 'snap1' } });
    fireEvent.change(screen.getByLabelText(/^Source volume( \*)?$/), { target: { value: 'apps/ours' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));

    await waitFor(() => expect(k8sCreate).toHaveBeenCalled());
    const { data } = (k8sCreate as jest.Mock).mock.calls[0][0];
    expect(data.kind).toBe('VolumeSnapshot');
    expect(data.metadata).toEqual({ name: 'snap1', namespace: 'apps' });
    expect(data.spec).toEqual({
      volumeSnapshotClassName: 'any-name',
      source: { persistentVolumeClaimName: 'ours' },
    });
    await waitFor(() =>
      expect(push).toHaveBeenCalledWith('/k8s/ns/apps/snapshot.storage.k8s.io~v1~VolumeSnapshot/snap1'),
    );
  });
});
