import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sCreate, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import CloneVolumePage from './CloneVolumePage';

const push = jest.fn();
jest.mock('react-router-dom', () => ({
  useHistory: () => ({ push, goBack: jest.fn() }),
  useParams: () => ({ name: 'pvc-x' }),
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

const subvolume = (over: Record<string, unknown> = {}) => ({
  metadata: { name: 'pvc-x' },
  spec: { poolName: 'pool-a', pvcName: 'data', pvcNamespace: 'apps', requestedGB: 10 },
  status: { phase: 'Bound' },
  ...over,
});

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
  SubVolume: [subvolume()],
  PersistentVolumeClaim: [sourcePVC],
  StorageClass: storageClasses,
  FileSharePool: [pool],
  VirtualMachine: [],
});

beforeEach(() => jest.clearAllMocks());

const cloneButton = () => screen.getByRole('button', { name: 'Clone' }) as HTMLButtonElement;

describe('CloneVolumePage', () => {
  it('opens with defaults: name <claim>-clone, the volume size, the source class, and the namespace fixed', () => {
    watch(base());
    render(<CloneVolumePage />);
    expect((screen.getByLabelText(/^Name( \*)?$/) as HTMLInputElement).value).toBe('data-clone');
    expect((screen.getByLabelText(/^Size \(GiB\)( \*)?$/) as HTMLInputElement).value).toBe('10');
    expect((screen.getByLabelText(/^Storage class( \*)?$/) as HTMLSelectElement).value).toBe('pool-a');
    expect(screen.getByText('apps')).toBeTruthy();
    expect(cloneButton().disabled).toBe(false);
  });

  it('offers only storage classes of this driver', () => {
    watch(base());
    render(<CloneVolumePage />);
    expect(screen.getByRole('option', { name: 'pool-a' })).toBeTruthy();
    expect(screen.queryByRole('option', { name: 'gp' })).toBeNull();
  });

  it('does not offer the storage class of a snapshot pool', () => {
    watch({ ...base(), StorageClass: [...storageClasses, { metadata: { name: 'backups' }, provisioner: DRIVER, parameters: { pool: 'backups' } }], BackupPolicy: [{ metadata: { name: 'nightly' }, spec: { snapshotPool: 'backups' } }] });
    render(<CloneVolumePage />);
    expect(screen.getByRole('option', { name: 'pool-a' })).toBeTruthy();
    expect(screen.queryByRole('option', { name: 'backups' })).toBeNull();
  });

  it('refuses a size under the volume size', () => {
    watch(base());
    render(<CloneVolumePage />);
    fireEvent.change(screen.getByLabelText(/^Size \(GiB\)( \*)?$/), { target: { value: '5' } });
    expect(screen.getByText(/at least 10 GiB \(the volume's size\)/)).toBeTruthy();
    expect(cloneButton().disabled).toBe(true);
  });

  it('does not call its own new claim a name that is taken, after the clone starts', async () => {
    const data = base();
    watch(data);
    (k8sCreate as jest.Mock).mockImplementation(async () => {
      data.PersistentVolumeClaim.push({ ...sourcePVC, metadata: { name: 'data-clone', namespace: 'apps' } });
      return {};
    });
    render(<CloneVolumePage />);
    fireEvent.click(cloneButton());
    expect(await screen.findByText(/Cloning/)).toBeTruthy();
    expect(screen.queryByText(/already exists/)).toBeNull();
  });

  it('refuses a name that a claim already has', () => {
    watch({
      ...base(),
      PersistentVolumeClaim: [sourcePVC, { ...sourcePVC, metadata: { name: 'data-clone', namespace: 'apps' } }],
    });
    render(<CloneVolumePage />);
    expect(screen.getByText(/already exists/)).toBeTruthy();
    expect(cloneButton().disabled).toBe(true);
  });

  it('warns, but does not block, when a VM uses the claim', () => {
    watch({
      ...base(),
      VirtualMachine: [
        {
          metadata: { name: 'vm1', namespace: 'apps' },
          spec: { template: { spec: { volumes: [{ persistentVolumeClaim: { claimName: 'data' } }] } } },
        },
      ],
    });
    render(<CloneVolumePage />);
    expect(screen.getByText(/while the VM is running/i)).toBeTruthy();
    expect(cloneButton().disabled).toBe(false);
  });

  it('shows no VM warning when no VM uses the claim', () => {
    watch(base());
    render(<CloneVolumePage />);
    expect(screen.queryByText(/while the VM is running/i)).toBeNull();
  });

  it('suggests a VM snapshot for a safe copy and points to no future step', () => {
    watch({
      ...base(),
      VirtualMachine: [
        {
          metadata: { name: 'vm1', namespace: 'apps' },
          spec: { template: { spec: { volumes: [{ persistentVolumeClaim: { claimName: 'data' } }] } } },
        },
      ],
    });
    render(<CloneVolumePage />);
    expect(screen.getByText(/take a snapshot of the VM and restore it/i)).toBeTruthy();
    expect(screen.queryByText(/step 7/i)).toBeNull();
  });

  it('shows the pool free space and warns when the size is more than that', () => {
    watch(base());
    render(<CloneVolumePage />);
    expect(screen.getByText(/50 GB free/)).toBeTruthy();
    fireEvent.change(screen.getByLabelText(/^Size \(GiB\)( \*)?$/), { target: { value: '80' } });
    expect(screen.getByText(/more than the free space/i)).toBeTruthy();
    expect(cloneButton().disabled).toBe(false);
  });

  it('shows the reason and no form when the volume is not Bound', () => {
    watch({ ...base(), SubVolume: [subvolume({ status: { phase: 'Creating' } })] });
    render(<CloneVolumePage />);
    expect(screen.getByText(/not Bound/i)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Clone' })).toBeNull();
  });

  it('shows the reason and no form when the claim of the volume no longer exists', () => {
    watch({ ...base(), PersistentVolumeClaim: [] });
    render(<CloneVolumePage />);
    expect(screen.getByText(/claim .* no longer exists/i)).toBeTruthy();
    expect(screen.queryByRole('button', { name: 'Clone' })).toBeNull();
  });

  it('creates a claim with the source claim as its data source and then shows the copy banner', async () => {
    watch(base());
    (k8sCreate as jest.Mock).mockResolvedValue({});
    render(<CloneVolumePage />);
    fireEvent.click(cloneButton());

    await waitFor(() => expect(k8sCreate).toHaveBeenCalled());
    const { data } = (k8sCreate as jest.Mock).mock.calls[0][0];
    expect(data.metadata).toEqual({ name: 'data-clone', namespace: 'apps' });
    expect(data.spec.dataSource).toEqual({ kind: 'PersistentVolumeClaim', name: 'data' });
    expect(data.spec.resources.requests.storage).toBe('10Gi');
    expect(await screen.findByText(/Cloning/)).toBeTruthy();
  });

  it('shows the error when the claim cannot be created', async () => {
    watch(base());
    (k8sCreate as jest.Mock).mockRejectedValue(new Error('quota exceeded'));
    render(<CloneVolumePage />);
    fireEvent.click(cloneButton());
    expect(await screen.findByText(/quota exceeded/)).toBeTruthy();
  });
});
