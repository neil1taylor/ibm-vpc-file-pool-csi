import React from 'react';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { k8sCreate, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import MigrateVolumesPage from './MigrateVolumesPage';
import { buildMigrationPod, buildTargetPVC } from '../../utils/migrate-objects';

jest.mock('react-router-dom', () => ({
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

const DRIVER = 'vpc-file-pool.csi.ibm.io';
const OLD = 'ibmc-vpc-file-3000-iops';

type Data = Record<string, unknown[]>;

function watch(data: Data) {
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind: { kind: string } } | null) => {
    if (!req) return [undefined, true, undefined];
    return [data[req.groupVersionKind.kind] ?? [], true, undefined];
  });
}

const pvc = (name: string, over: Record<string, unknown> = {}, phase = 'Bound', storageClassName = OLD) => ({
  metadata: { name, namespace: 'apps' },
  spec: { accessModes: ['ReadWriteMany'], storageClassName, resources: { requests: { storage: '200Gi' } }, ...over },
  status: { phase },
});

const base = (): Data => ({
  PersistentVolumeClaim: [pvc('data'), pvc('logs')],
  Pod: [],
  StorageClass: [
    { metadata: { name: OLD }, provisioner: 'vpc.file.csi.ibm.io' },
    { metadata: { name: 'pool-a' }, provisioner: DRIVER },
  ],
  FileSharePool: [{ metadata: { name: 'pool-a' }, spec: {}, status: { totalCapacityGB: 1000, totalAllocatedGB: 100 } }],
});

beforeEach(() => jest.clearAllMocks());

const migrateButton = (claim: string) =>
  within(screen.getByRole('row', { name: new RegExp(claim) })).getByRole('button', { name: 'Migrate' }) as HTMLButtonElement;

describe('MigrateVolumesPage', () => {
  it('lists the claims of the first namespace and source class, with size and phase', () => {
    watch(base());
    render(<MigrateVolumesPage />);
    expect(screen.getByRole('row', { name: /data.*200 GiB.*Bound/ })).toBeTruthy();
    expect(screen.getByRole('row', { name: /logs/ })).toBeTruthy();
  });

  it('says so when no claim sits outside the driver classes', () => {
    watch({ ...base(), PersistentVolumeClaim: [pvc('mine', {}, 'Bound', 'pool-a')] });
    render(<MigrateVolumesPage />);
    expect(screen.getByText(/No claims of other storage classes/)).toBeTruthy();
  });

  it('turns Migrate off for a claim a pod uses, and names the pod', () => {
    watch({
      ...base(),
      Pod: [{ metadata: { name: 'web', namespace: 'apps' }, spec: { volumes: [{ persistentVolumeClaim: { claimName: 'data' } }] }, status: { phase: 'Running' } }],
    });
    render(<MigrateVolumesPage />);
    expect(migrateButton('data').disabled).toBe(true);
    expect(screen.getByText(/In use by: web/)).toBeTruthy();
    expect(migrateButton('logs').disabled).toBe(false);
  });

  it('asks to confirm first, and says what happens', () => {
    watch(base());
    render(<MigrateVolumesPage />);
    fireEvent.click(migrateButton('data'));
    expect(screen.getByText(/new claim .*data-pooled.* is created/i)).toBeTruthy();
    expect(screen.getByText(/old claim stays/i)).toBeTruthy();
    expect(k8sCreate).not.toHaveBeenCalled();
  });

  it('refuses a move that does not fit in the pool', () => {
    watch({
      ...base(),
      FileSharePool: [{ metadata: { name: 'pool-a' }, spec: {}, status: { totalCapacityGB: 1000, totalAllocatedGB: 900 } }],
    });
    render(<MigrateVolumesPage />);
    fireEvent.click(migrateButton('data'));
    expect(screen.getByText(/100 GB free/)).toBeTruthy();
    expect(screen.getByText(/does not fit/i)).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Start migration' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('creates the new claim, then the pod, with the same objects as kubectl migrate', async () => {
    watch(base());
    (k8sCreate as jest.Mock).mockResolvedValue({});
    render(<MigrateVolumesPage />);
    fireEvent.click(migrateButton('data'));
    fireEvent.click(screen.getByRole('button', { name: 'Start migration' }));

    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(2));
    const calls = (k8sCreate as jest.Mock).mock.calls.map((c) => c[0].data);
    expect(calls[0]).toEqual(buildTargetPVC({ namespace: 'apps', sourcePVC: 'data', pool: 'pool-a', storageClass: 'pool-a', size: '200Gi' }));
    expect(calls[1]).toEqual(buildMigrationPod({ namespace: 'apps', sourcePVC: 'data' }));
  });

  it('shows the error when a claim cannot be created, and makes no pod', async () => {
    watch(base());
    (k8sCreate as jest.Mock).mockRejectedValue(new Error('quota exceeded'));
    render(<MigrateVolumesPage />);
    fireEvent.click(migrateButton('data'));
    fireEvent.click(screen.getByRole('button', { name: 'Start migration' }));
    expect(await screen.findByText(/quota exceeded/)).toBeTruthy();
    expect(k8sCreate).toHaveBeenCalledTimes(1);
  });

  it('shows the copy while the migration pod runs', () => {
    watch({
      ...base(),
      PersistentVolumeClaim: [pvc('data'), pvc('data-pooled', {}, 'Bound', 'pool-a')],
      Pod: [{ metadata: { name: 'migrate-data', namespace: 'apps' }, spec: {}, status: { phase: 'Running' } }],
    });
    render(<MigrateVolumesPage />);
    expect(within(screen.getByRole('row', { name: /data/ })).getByText(/Copying/)).toBeTruthy();
  });

  it('says to switch the application and delete the old claim when the copy is done', () => {
    watch({
      ...base(),
      PersistentVolumeClaim: [pvc('data'), pvc('data-pooled', {}, 'Bound', 'pool-a')],
      Pod: [{ metadata: { name: 'migrate-data', namespace: 'apps' }, spec: {}, status: { phase: 'Succeeded' } }],
    });
    render(<MigrateVolumesPage />);
    expect(screen.getByText(/Switch your application to data-pooled/)).toBeTruthy();
  });

  it('links to the pod log when the copy failed', () => {
    watch({
      ...base(),
      PersistentVolumeClaim: [pvc('data'), pvc('data-pooled', {}, 'Bound', 'pool-a')],
      Pod: [{ metadata: { name: 'migrate-data', namespace: 'apps' }, spec: {}, status: { phase: 'Failed', containerStatuses: [{ state: { terminated: { exitCode: 1 } } }] } }],
    });
    render(<MigrateVolumesPage />);
    expect(screen.getByText(/Failed/)).toBeTruthy();
    expect(screen.getByRole('link', { name: 'Pod log' }).getAttribute('href')).toBe('/k8s/ns/apps/pods/migrate-data/logs');
  });
});
