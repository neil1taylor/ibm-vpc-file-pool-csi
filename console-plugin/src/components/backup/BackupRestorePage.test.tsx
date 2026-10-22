import React from 'react';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { k8sCreate, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import BackupRestorePage from './BackupRestorePage';

let mockSearch = '';
const mockPush = jest.fn();
jest.mock('react-router-dom', () => ({
  useParams: () => ({ name: 'nightly' }),
  useLocation: () => ({ search: mockSearch }),
  useHistory: () => ({ push: mockPush }),
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

const policy = {
  metadata: { name: 'nightly' },
  spec: { snapshotPool: 'snaps', schedule: '6h', selector: {} },
  status: {
    phase: 'Active',
    volumes: [
      {
        pvcNamespace: 'apps',
        pvcName: 'data',
        subVolumeName: 'pvc-1',
        storageClassName: 'pool-a',
        lastBackupTime: '2026-10-22T06:00:00Z',
        oldestBackupTime: '2026-10-15T06:00:00Z',
        restorePoints: 20,
        consistency: 'FileCopy',
      },
      {
        pvcNamespace: 'apps',
        pvcName: 'disk-1',
        subVolumeName: 'pvc-2',
        vmName: 'web',
        storageClassName: 'pool-a',
        lastBackupTime: '2026-10-22T06:00:00Z',
        consistency: 'CrashConsistent',
      },
      {
        pvcNamespace: 'apps',
        pvcName: 'gone',
        subVolumeName: 'pvc-3',
        retained: true,
        lastBackupTime: '2026-10-20T06:00:00Z',
      },
    ],
  },
};

const classes = [
  { metadata: { name: 'pool-a' }, provisioner: 'vpc-file-pool.csi.ibm.io' },
  { metadata: { name: 'pool-b' }, provisioner: 'vpc-file-pool.csi.ibm.io' },
  { metadata: { name: 'other' }, provisioner: 'example.com/other' },
];

let claims: Array<{ metadata: { name: string } }> = [];
let restore: unknown;

beforeEach(() => {
  jest.clearAllMocks();
  mockSearch = '';
  claims = [];
  restore = undefined;
  (useAccessReview as jest.Mock).mockReturnValue([true, false]);
  (k8sCreate as jest.Mock).mockResolvedValue({});
  (useK8sWatchResource as jest.Mock).mockImplementation(
    (req: { groupVersionKind: { kind: string }; isList?: boolean; name?: string } | null) => {
      if (!req) return [undefined, true, undefined];
      switch (req.groupVersionKind.kind) {
        case 'BackupPolicy':
          return [policy, true, undefined];
        case 'StorageClass':
          return [classes, true, undefined];
        case 'PersistentVolumeClaim':
          return [claims, true, undefined];
        case 'BackupRestore':
          return [restore, true, undefined];
        default:
          return [[], true, undefined];
      }
    },
  );
});

const restoreButton = () => screen.getByRole('button', { name: 'Restore' }) as HTMLButtonElement;

describe('BackupRestorePage', () => {
  it('starts with the first volume, the latest backup, a new name and the same namespace', () => {
    render(<BackupRestorePage />);
    expect((screen.getByLabelText(/^Volume/) as HTMLSelectElement).value).toBe('apps/data');
    expect((screen.getByLabelText('Latest backup') as HTMLInputElement).checked).toBe(true);
    expect((screen.getByLabelText(/^New claim name/) as HTMLInputElement).value).toBe('data-restored');
    expect((screen.getByLabelText(/^Namespace/) as HTMLInputElement).value).toBe('apps');
  });

  it('lists a volume that is gone, marked as kept', () => {
    render(<BackupRestorePage />);
    expect(screen.getByRole('option', { name: /apps\/gone.*no longer backed up/ })).toBeTruthy();
  });

  it('takes the claim and the moment from the link', () => {
    mockSearch = '?claim=apps%2Fdisk-1&moment=2026-10-20T09%3A00%3A00Z';
    render(<BackupRestorePage />);
    expect((screen.getByLabelText(/^Volume/) as HTMLSelectElement).value).toBe('apps/disk-1');
    expect((screen.getByLabelText('At a moment') as HTMLInputElement).checked).toBe(true);
    expect((screen.getByLabelText(/^Moment/) as HTMLInputElement).value).not.toBe('');
    expect((screen.getByLabelText(/^New claim name/) as HTMLInputElement).value).toBe('disk-1-restored');
  });

  it('shows the range of moments of the chosen volume', () => {
    render(<BackupRestorePage />);
    expect(screen.getByText(/Backups from 2026-10-15T06:00:00Z to 2026-10-22T06:00:00Z/)).toBeTruthy();
  });

  it('warns that a VM disk copy may be crash consistent', () => {
    mockSearch = '?claim=apps%2Fdisk-1';
    render(<BackupRestorePage />);
    expect(screen.getByText(/crash consistent/i)).toBeTruthy();
  });

  it('offers only storage classes of the driver, with the class of the backed-up claim as the default', () => {
    render(<BackupRestorePage />);
    const select = screen.getByLabelText(/^Storage class/) as HTMLSelectElement;
    expect(select.value).toBe('');
    expect(within(select).getByRole('option', { name: /Same as the backed-up claim \(pool-a\)/ })).toBeTruthy();
    expect(within(select).getByRole('option', { name: 'pool-b' })).toBeTruthy();
    expect(within(select).queryByRole('option', { name: 'other' })).toBeNull();
  });

  it('refuses a name that exists, checked live', () => {
    claims = [{ metadata: { name: 'data-restored' } }];
    render(<BackupRestorePage />);
    expect(screen.getByText(/already exists in the namespace/)).toBeTruthy();
    expect(restoreButton().disabled).toBe(true);
  });

  it('refuses a moment before the oldest backup', () => {
    mockSearch = '?moment=2026-09-01T00%3A00%3A00Z';
    render(<BackupRestorePage />);
    expect(screen.getByText(/before the oldest backup/)).toBeTruthy();
    expect(restoreButton().disabled).toBe(true);
  });

  it('asks for confirmation first, and says what will and will not happen', () => {
    render(<BackupRestorePage />);
    fireEvent.click(restoreButton());
    expect(k8sCreate).not.toHaveBeenCalled();
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText(/A new claim apps\/data-restored is made/)).toBeTruthy();
    expect(within(dialog).getByText(/Nothing is overwritten/)).toBeTruthy();
    expect(within(dialog).getByText(/takes time/)).toBeTruthy();
  });

  it('creates one BackupRestore for the latest backup, and follows it', async () => {
    render(<BackupRestorePage />);
    fireEvent.click(restoreButton());
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Restore' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    const { data, model } = (k8sCreate as jest.Mock).mock.calls[0][0];
    expect(model.kind).toBe('BackupRestore');
    expect(data.kind).toBe('BackupRestore');
    expect(data.spec).toEqual({
      policy: 'nightly',
      sourceNamespace: 'apps',
      sourceName: 'data',
      targetNamespace: 'apps',
      targetName: 'data-restored',
    });
    expect(await screen.findByText(/the request is starting/)).toBeTruthy();
  });

  it('sends the moment and a chosen storage class', async () => {
    mockSearch = '?claim=apps%2Fdata&moment=2026-10-20T09%3A00%3A00Z';
    render(<BackupRestorePage />);
    fireEvent.change(screen.getByLabelText(/^Storage class/), { target: { value: 'pool-b' } });
    fireEvent.click(restoreButton());
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Restore' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalled());
    const { data } = (k8sCreate as jest.Mock).mock.calls[0][0];
    expect(data.spec.moment).toBe('2026-10-20T09:00:00Z');
    expect(data.spec.storageClassName).toBe('pool-b');
  });

  it('shows the error when the request is refused, and makes no banner', async () => {
    (k8sCreate as jest.Mock).mockRejectedValue(new Error('admission webhook denied the request: spec.moment is in the future'));
    render(<BackupRestorePage />);
    fireEvent.click(restoreButton());
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Restore' }));
    expect(await screen.findByText(/spec.moment is in the future/)).toBeTruthy();
    expect(screen.queryByText(/the request is starting/)).toBeNull();
  });

  it('turns Restore off, names the missing right and shows the YAML for an administrator without the right', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    render(<BackupRestorePage />);
    expect(restoreButton().disabled).toBe(true);
    expect(screen.getByText(/You are not allowed to create BackupRestore objects/)).toBeTruthy();
    expect(screen.getByText(/kind: BackupRestore/)).toBeTruthy();
    expect(screen.getByText(/sourceName: data/)).toBeTruthy();
  });

  it('says when the policy has no volumes to restore', () => {
    (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind: { kind: string } } | null) =>
      req?.groupVersionKind.kind === 'BackupPolicy'
        ? [{ ...policy, status: { phase: 'Active', volumes: [] } }, true, undefined]
        : [[], true, undefined],
    );
    render(<BackupRestorePage />);
    expect(screen.getByText(/has no backups to restore yet/)).toBeTruthy();
  });

  it('says when the policy is not found', () => {
    (useK8sWatchResource as jest.Mock).mockImplementation(() => [undefined, true, undefined]);
    render(<BackupRestorePage />);
    expect(screen.getByText(/was not found/)).toBeTruthy();
  });
});
