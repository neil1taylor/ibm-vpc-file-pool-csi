import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sCreate, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import RestoreSnapshotPage from './RestoreSnapshotPage';

const push = jest.fn();
jest.mock('react-router-dom', () => ({
  useHistory: () => ({ push, goBack: jest.fn() }),
  useParams: () => ({ name: 'snapshot-abc' }),
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

const DRIVER = 'vpc-file-pool.csi.ibm.io';

type Data = Record<string, unknown[]>;

/** Answers each watch from `data` by kind; a watch with a name picks the item with that name. */
function watch(data: Data) {
  (useK8sWatchResource as jest.Mock).mockImplementation(
    (req: { groupVersionKind: { kind: string }; name?: string } | null) => {
      if (!req) return [undefined, true, undefined];
      const items = data[req.groupVersionKind.kind] ?? [];
      if (req.name) {
        const found = (items as Array<{ metadata?: { name?: string } }>).find((i) => i.metadata?.name === req.name);
        return [found, true, undefined];
      }
      return [items, true, undefined];
    },
  );
}

const snapshot = (over: Record<string, unknown> = {}) => ({
  metadata: { name: 'snapshot-abc' },
  spec: { poolName: 'pool-a', sizeGB: 10, sourceSubVolume: 'pvc-1' },
  status: { readyToUse: true, phase: 'Ready' },
  ...over,
});

const content = {
  metadata: { name: 'snapcontent-1' },
  spec: { volumeSnapshotRef: { name: 'daily', namespace: 'apps' } },
  status: { snapshotHandle: 'pool-a/share-9/snapshot-abc' },
};

const volumeSnapshot = {
  metadata: { name: 'daily', namespace: 'apps' },
  spec: { volumeSnapshotClassName: 'c', source: { persistentVolumeClaimName: 'data' } },
};

const sourcePVC = {
  metadata: { name: 'data', namespace: 'apps' },
  spec: { accessModes: ['ReadWriteMany'], storageClassName: 'pool-a', resources: { requests: { storage: '10Gi' } } },
};

const storageClasses = [
  { metadata: { name: 'pool-a' }, provisioner: DRIVER },
  { metadata: { name: 'gp' }, provisioner: 'other' },
];

const pool = { metadata: { name: 'pool-a' }, status: { totalCapacityGB: 200, totalAllocatedGB: 150 } };

const base = (): Data => ({
  Snapshot: [snapshot()],
  VolumeSnapshotContent: [content],
  VolumeSnapshot: [volumeSnapshot],
  PersistentVolumeClaim: [sourcePVC],
  StorageClass: storageClasses,
  FileSharePool: [pool],
  VirtualMachine: [],
});

beforeEach(() => jest.clearAllMocks());

const createButton = () => screen.getByRole('button', { name: 'Restore' }) as HTMLButtonElement;

describe('RestoreSnapshotPage', () => {
  it('opens with defaults: name <source>-restored, the snapshot size, the source class, and the namespace fixed', () => {
    watch(base());
    render(<RestoreSnapshotPage />);
    expect((screen.getByLabelText(/^Name( \*)?$/) as HTMLInputElement).value).toBe('data-restored');
    expect((screen.getByLabelText(/^Size \(GiB\)( \*)?$/) as HTMLInputElement).value).toBe('10');
    expect((screen.getByLabelText(/^Storage class( \*)?$/) as HTMLSelectElement).value).toBe('pool-a');
    expect(screen.getByText('apps')).toBeTruthy();
    expect(createButton().disabled).toBe(false);
  });

  it('offers only storage classes of this driver', () => {
    watch(base());
    render(<RestoreSnapshotPage />);
    expect(screen.getByRole('option', { name: 'pool-a' })).toBeTruthy();
    expect(screen.queryByRole('option', { name: 'gp' })).toBeNull();
  });

  it('refuses a size under the snapshot size', () => {
    watch(base());
    render(<RestoreSnapshotPage />);
    fireEvent.change(screen.getByLabelText(/^Size \(GiB\)( \*)?$/), { target: { value: '5' } });
    expect(screen.getByText(/at least 10 GiB/)).toBeTruthy();
    expect(createButton().disabled).toBe(true);
  });

  it('refuses a name that a claim already has', () => {
    watch({
      ...base(),
      PersistentVolumeClaim: [sourcePVC, { ...sourcePVC, metadata: { name: 'data-restored', namespace: 'apps' } }],
    });
    render(<RestoreSnapshotPage />);
    expect(screen.getByText(/already exists/)).toBeTruthy();
    expect(createButton().disabled).toBe(true);
  });

  it('warns, but does not block, when a VM uses the source claim', () => {
    watch({
      ...base(),
      VirtualMachine: [
        {
          metadata: { name: 'vm1', namespace: 'apps' },
          spec: { template: { spec: { volumes: [{ persistentVolumeClaim: { claimName: 'data' } }] } } },
        },
      ],
    });
    render(<RestoreSnapshotPage />);
    expect(screen.getByText(/taken while a VM was running/i)).toBeTruthy();
    expect(createButton().disabled).toBe(false);
  });

  it('shows no VM warning when no VM uses the claim', () => {
    watch(base());
    render(<RestoreSnapshotPage />);
    expect(screen.queryByText(/taken while a VM was running/i)).toBeNull();
  });

  it('shows the pool free space and warns when the size is more than that', () => {
    watch(base());
    render(<RestoreSnapshotPage />);
    expect(screen.getByText(/50 GB free/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText(/^Size \(GiB\)( \*)?$/), { target: { value: '80' } });
    expect(screen.getByText(/more than the free space/i)).toBeTruthy();
    expect(createButton().disabled).toBe(false);
  });

  it('shows the reason and no form when no VolumeSnapshot points to the snapshot', () => {
    watch({ ...base(), VolumeSnapshotContent: [] });
    render(<RestoreSnapshotPage />);
    expect(screen.getByText(/create a VolumeSnapshot first/i)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Restore' })).toBeNull();
  });

  it('shows the reason and no form when the snapshot is not ready', () => {
    watch({ ...base(), Snapshot: [snapshot({ status: { readyToUse: false, phase: 'Creating' } })] });
    render(<RestoreSnapshotPage />);
    expect(screen.getByText(/not ready/i)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Restore' })).toBeNull();
  });

  it('creates a claim with the VolumeSnapshot as its data source and then shows the copy banner', async () => {
    watch(base());
    (k8sCreate as jest.Mock).mockResolvedValue({});
    render(<RestoreSnapshotPage />);
    fireEvent.click(createButton());

    await waitFor(() => expect(k8sCreate).toHaveBeenCalled());
    const { data } = (k8sCreate as jest.Mock).mock.calls[0][0];
    expect(data.metadata).toEqual({ name: 'data-restored', namespace: 'apps' });
    expect(data.spec.dataSource).toEqual({ apiGroup: 'snapshot.storage.k8s.io', kind: 'VolumeSnapshot', name: 'daily' });
    expect(data.spec.resources.requests.storage).toBe('10Gi');
    expect(await screen.findByText(/Restoring/)).toBeTruthy();
  });

  it('shows the error when the claim cannot be created', async () => {
    watch(base());
    (k8sCreate as jest.Mock).mockRejectedValue(new Error('quota exceeded'));
    render(<RestoreSnapshotPage />);
    fireEvent.click(createButton());
    expect(await screen.findByText(/quota exceeded/)).toBeTruthy();
  });
});
