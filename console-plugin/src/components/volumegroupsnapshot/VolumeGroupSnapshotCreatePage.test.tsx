import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sCreate, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import VolumeGroupSnapshotCreatePage from './VolumeGroupSnapshotCreatePage';

const push = jest.fn();
jest.mock('react-router-dom', () => ({ useHistory: () => ({ push, goBack: jest.fn() }) }));

const DRIVER = 'vpc-file-pool.csi.ibm.io';

beforeEach(() => jest.clearAllMocks());

describe('VolumeGroupSnapshotCreatePage', () => {
  it('says the API is missing and shows no form when the cluster lacks it', () => {
    (useK8sWatchResource as jest.Mock).mockReturnValue([[], false, { code: 404 }]);
    render(<VolumeGroupSnapshotCreatePage />);
    expect(
      screen.getByText(
        'This cluster does not have the Kubernetes group snapshot API, so group snapshots cannot be created.',
      ),
    ).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Create' })).toBeNull();
  });

  it('creates a group snapshot from a label selector', async () => {
    (useK8sWatchResource as jest.Mock).mockReturnValue([
      [{ metadata: { name: 'grp-class' }, driver: DRIVER }],
      true,
      undefined,
    ]);
    (k8sCreate as jest.Mock).mockResolvedValue({});
    render(<VolumeGroupSnapshotCreatePage />);
    fireEvent.change(screen.getByLabelText(/^Namespace( \*)?$/), { target: { value: 'apps' } });
    fireEvent.change(screen.getByLabelText(/^Name( \*)?$/), { target: { value: 'grp' } });
    fireEvent.change(screen.getByLabelText(/^PVC labels( \*)?$/), { target: { value: 'app=db' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));

    await waitFor(() => expect(k8sCreate).toHaveBeenCalled());
    const { data } = (k8sCreate as jest.Mock).mock.calls[0][0];
    expect(data.apiVersion).toBe('groupsnapshot.storage.k8s.io/v1beta1');
    expect(data.spec).toEqual({
      volumeGroupSnapshotClassName: 'grp-class',
      source: { selector: { matchLabels: { app: 'db' } } },
    });
    await waitFor(() =>
      expect(push).toHaveBeenCalledWith(
        '/k8s/ns/apps/groupsnapshot.storage.k8s.io~v1beta1~VolumeGroupSnapshot/grp',
      ),
    );
  });

  it('tells the user when no group snapshot class uses the driver', () => {
    (useK8sWatchResource as jest.Mock).mockReturnValue([
      [{ metadata: { name: 'x' }, driver: 'other' }],
      true,
      undefined,
    ]);
    render(<VolumeGroupSnapshotCreatePage />);
    expect(screen.getByText(/No VolumeGroupSnapshotClass uses the driver/)).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Create' }) as HTMLButtonElement).disabled).toBe(true);
  });
});
