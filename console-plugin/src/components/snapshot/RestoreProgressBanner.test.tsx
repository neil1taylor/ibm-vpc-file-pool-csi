import React from 'react';
import { render, screen } from '@testing-library/react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import RestoreProgressBanner from './RestoreProgressBanner';

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

function watch(data: Record<string, unknown>) {
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind: { kind: string } } | null) => [
    req ? data[req.groupVersionKind.kind] : undefined,
    true,
    undefined,
  ]);
}

const pvc = (phase: string, volumeName?: string) => ({
  metadata: { name: 'data-restored', namespace: 'apps' },
  spec: { accessModes: ['ReadWriteMany'], volumeName, resources: { requests: { storage: '10Gi' } } },
  status: { phase },
});

describe('RestoreProgressBanner', () => {
  it('says the restore is starting while the claim has no copy yet', () => {
    watch({ PersistentVolumeClaim: pvc('Pending') });
    render(<RestoreProgressBanner namespace="apps" name="data-restored" />);
    expect(screen.getByText(/Restoring/)).toBeTruthy();
  });

  it('shows the percent copied while the copy runs', () => {
    watch({
      PersistentVolumeClaim: pvc('Pending', 'pvc-x'),
      SubVolume: { metadata: { name: 'pvc-x' }, status: { cloneStatus: 'InProgress', cloneProgress: { bytesCopied: 5, totalBytes: 20 } } },
    });
    render(<RestoreProgressBanner namespace="apps" name="data-restored" />);
    expect(screen.getByText(/25%/)).toBeTruthy();
  });

  it('says it is ready, with a link to the claim, when the claim is Bound', () => {
    watch({ PersistentVolumeClaim: pvc('Bound', 'pvc-x') });
    render(<RestoreProgressBanner namespace="apps" name="data-restored" />);
    expect(screen.getByText(/ready/i)).toBeTruthy();
    expect(screen.getByRole('link', { name: 'data-restored' }).getAttribute('href')).toBe(
      '/k8s/ns/apps/persistentvolumeclaims/data-restored',
    );
  });

  it('shows the copy error and does not delete anything when the copy failed', () => {
    watch({
      PersistentVolumeClaim: pvc('Pending', 'pvc-x'),
      SubVolume: { metadata: { name: 'pvc-x' }, status: { cloneStatus: 'Failed', cloneProgress: { bytesCopied: 0, totalBytes: 20, error: 'rsync failed' } } },
    });
    render(<RestoreProgressBanner namespace="apps" name="data-restored" />);
    expect(screen.getByText(/rsync failed/)).toBeTruthy();
  });
});
