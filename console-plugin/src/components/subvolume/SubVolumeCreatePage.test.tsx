import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sCreate, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import SubVolumeCreatePage from './SubVolumeCreatePage';

const push = jest.fn();
jest.mock('react-router-dom', () => ({ useHistory: () => ({ push }) }));

const share = (state: string, mountTargetIP: string) => ({
  shareID: 's',
  shareName: 's',
  mountTargetIP,
  mountTargetID: '',
  totalGB: 100,
  allocatedGB: 0,
  pvcCount: 0,
  state,
  zone: 'z',
});

const pools = [
  { metadata: { name: 'usable' }, spec: {}, status: { phase: 'Degraded', shares: [share('stable', '10.0.0.4')] } },
  { metadata: { name: 'no-target' }, spec: {}, status: { phase: 'Ready', shares: [share('stable', '')] } },
  {
    metadata: { name: 'tiered' },
    spec: { tiers: [{ name: 'fast' }, { name: 'slow' }] },
    status: { phase: 'Ready', shares: [share('stable', '10.0.0.5')] },
  },
];

beforeEach(() => {
  jest.clearAllMocks();
  (useK8sWatchResource as jest.Mock).mockReturnValue([pools, true, undefined]);
});

describe('SubVolumeCreatePage', () => {
  it('offers only pools that have a usable share', () => {
    render(<SubVolumeCreatePage />);
    expect(screen.getByRole('option', { name: /usable/ })).toBeTruthy();
    expect(screen.queryByRole('option', { name: /no-target/ })).toBeNull();
  });

  it('does not offer a snapshot pool', () => {
    (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind: { kind: string } }) =>
      req.groupVersionKind.kind === 'BackupPolicy'
        ? [[{ metadata: { name: 'nightly' }, spec: { snapshotPool: 'tiered' } }], true, undefined]
        : [pools, true, undefined],
    );
    render(<SubVolumeCreatePage />);
    expect(screen.getByRole('option', { name: /usable/ })).toBeTruthy();
    expect(screen.queryByRole('option', { name: /tiered/ })).toBeNull();
  });

  it('creates a PVC and opens its console page', async () => {
    (k8sCreate as jest.Mock).mockResolvedValue({});
    render(<SubVolumeCreatePage />);
    fireEvent.change(screen.getByLabelText(/^Namespace( \*)?$/), { target: { value: 'apps' } });
    fireEvent.change(screen.getByLabelText(/^Name( \*)?$/), { target: { value: 'data' } });
    fireEvent.change(screen.getByLabelText(/^Pool/), { target: { value: 'usable' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));

    await waitFor(() => expect(k8sCreate).toHaveBeenCalled());
    const { data } = (k8sCreate as jest.Mock).mock.calls[0][0];
    expect(data.kind).toBe('PersistentVolumeClaim');
    expect(data.metadata).toEqual({ name: 'data', namespace: 'apps' });
    expect(data.spec.storageClassName).toBe('usable');
    await waitFor(() => expect(push).toHaveBeenCalledWith('/k8s/ns/apps/persistentvolumeclaims/data'));
  });

  it('requires a tier for a tiered pool and uses the tier storage class', async () => {
    (k8sCreate as jest.Mock).mockResolvedValue({});
    render(<SubVolumeCreatePage />);
    fireEvent.change(screen.getByLabelText(/^Namespace( \*)?$/), { target: { value: 'apps' } });
    fireEvent.change(screen.getByLabelText(/^Name( \*)?$/), { target: { value: 'data' } });
    fireEvent.change(screen.getByLabelText(/^Pool/), { target: { value: 'tiered' } });
    expect((screen.getByRole('button', { name: 'Create' }) as HTMLButtonElement).disabled).toBe(true);
    fireEvent.change(screen.getByLabelText(/^Tier/), { target: { value: 'fast' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalled());
    expect((k8sCreate as jest.Mock).mock.calls[0][0].data.spec.storageClassName).toBe('tiered-fast');
  });

  it('shows the error when creation fails', async () => {
    (k8sCreate as jest.Mock).mockRejectedValue(new Error('quota exceeded'));
    render(<SubVolumeCreatePage />);
    fireEvent.change(screen.getByLabelText(/^Namespace( \*)?$/), { target: { value: 'apps' } });
    fireEvent.change(screen.getByLabelText(/^Name( \*)?$/), { target: { value: 'data' } });
    fireEvent.change(screen.getByLabelText(/^Pool/), { target: { value: 'usable' } });
    fireEvent.click(screen.getByRole('button', { name: 'Create' }));
    expect(await screen.findByText('quota exceeded')).toBeTruthy();
    expect(push).not.toHaveBeenCalled();
  });
});
