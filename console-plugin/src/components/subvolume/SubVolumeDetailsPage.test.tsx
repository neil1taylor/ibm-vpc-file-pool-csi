import React from 'react';
import { render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import SubVolumeDetailsPage from './SubVolumeDetailsPage';

jest.mock('react-router-dom', () => ({
  useHistory: () => ({ push: jest.fn() }),
  useParams: () => ({ name: 'pvc-x' }),
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

const subvolume = (phase: string) => ({
  metadata: { name: 'pvc-x' },
  spec: {
    poolName: 'pool-a',
    shareID: 's',
    shareMountTargetIP: '1.2.3.4',
    subPath: '/pvcs/x',
    requestedGB: 10,
    pvName: 'pv-x',
    pvcName: 'data',
    pvcNamespace: 'apps',
    reclaimPolicy: 'Delete',
  },
  status: { phase },
});

describe('SubVolumeDetailsPage clone action', () => {
  it('links to the clone page when the volume is Bound', () => {
    (useK8sWatchResource as jest.Mock).mockReturnValue([subvolume('Bound'), true, undefined]);
    render(<SubVolumeDetailsPage />);
    expect(screen.getByRole('link', { name: 'Clone to new PVC' }).getAttribute('href')).toBe(
      '/vpc-file-pools/subvolumes/pvc-x/clone',
    );
  });

  it('turns the action off and says why when the volume is not Bound', () => {
    (useK8sWatchResource as jest.Mock).mockReturnValue([subvolume('Creating'), true, undefined]);
    render(<SubVolumeDetailsPage />);
    expect(screen.queryByRole('link', { name: 'Clone to new PVC' })).toBeNull();
    expect((screen.getByRole('button', { name: 'Clone to new PVC' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/not Bound/)).toBeTruthy();
  });
});
