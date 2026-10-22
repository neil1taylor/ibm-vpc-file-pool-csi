import React from 'react';
import { act, fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { k8sCreate, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import BackupCOSRestorePage from './BackupCOSRestorePage';

let mockSearch = '';
const mockPush = jest.fn();
jest.mock('react-router-dom', () => ({
  useLocation: () => ({ search: mockSearch }),
  useHistory: () => ({ push: mockPush }),
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

let cosProps: { repo?: { bucket: string; prefix: string; keyProtect: { rootKeyID: string } }; onPick?: (v: string, m: string) => void } = {};
jest.mock('./COSContents', () => ({
  __esModule: true,
  default: (props: typeof cosProps) => {
    cosProps = props;
    return <div>COS list for {props.repo ? `${props.repo.bucket}/${props.repo.prefix}` : 'no settings'}</div>;
  },
}));
jest.mock('./BackupRestoreBanner', () => ({
  __esModule: true,
  default: ({ restoreName, targetName }: { restoreName: string; targetName: string }) => (
    <div>Banner for {restoreName} to {targetName}</div>
  ),
}));

const VOL = 'pvc-11111111-2222-3333-4444-555555555555';

const policyWithExport = {
  metadata: { name: 'nightly' },
  spec: {
    snapshotPool: 'snaps',
    schedule: '6h',
    selector: {},
    export: {
      bucket: 'pool-backups',
      endpoint: 's3.eu-de.cloud-object-storage.appdomain.cloud',
      prefix: 'lab',
      credentialsSecret: 'cos-credentials',
      backupKeySecret: 'cos-backup-key',
      keyProtect: { instanceID: 'inst-1', region: 'eu-de', rootKeyID: 'root-1', apiKeySecret: 'keyprotect-apikey' },
    },
  },
};
const policyWithoutExport = { metadata: { name: 'plain' }, spec: { snapshotPool: 'snaps', schedule: '6h', selector: {} } };

const classes = [
  { metadata: { name: 'pool-a' }, provisioner: 'vpc-file-pool.csi.ibm.io' },
  { metadata: { name: 'other' }, provisioner: 'example.com/other' },
];
let claims: unknown[];

beforeEach(() => {
  jest.clearAllMocks();
  mockSearch = '';
  cosProps = {};
  claims = [];
  (useAccessReview as jest.Mock).mockReturnValue([true, false]);
  (k8sCreate as jest.Mock).mockResolvedValue({});
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind: { kind: string } } | null) => {
    if (!req) return [undefined, true, undefined];
    switch (req.groupVersionKind.kind) {
      case 'BackupPolicy':
        return [[policyWithExport, policyWithoutExport], true, undefined];
      case 'StorageClass':
        return [classes, true, undefined];
      case 'PersistentVolumeClaim':
        return [claims, true, undefined];
      default:
        return [[], true, undefined];
    }
  });
});

const field = (label: RegExp | string) => screen.getByLabelText(label) as HTMLInputElement;
const restoreButton = () => screen.getByRole('button', { name: 'Restore' }) as HTMLButtonElement;

const fillRest = () => {
  fireEvent.change(field(/^Volume( \*)?$/), { target: { value: VOL } });
  fireEvent.change(field(/^Namespace( \*)?$/), { target: { value: 'apps' } });
  fireEvent.change(field(/^New claim name( \*)?$/), { target: { value: 'data-restored' } });
  fireEvent.change(field(/^Storage class( \*)?$/), { target: { value: 'pool-a' } });
  fireEvent.change(field(/^Size \(GB\)( \*)?$/), { target: { value: '20' } });
};

describe('BackupCOSRestorePage', () => {
  it('says the console cannot clean up delete markers, and where to find the command that can', () => {
    render(<BackupCOSRestorePage />);
    expect(screen.getByText(/does not remove delete markers/)).toBeTruthy();
    expect(screen.getByText(/kubectl backup restore --from-cos/)).toBeTruthy();
  });

  it('fills the settings from the export of the policy in the link', () => {
    mockSearch = '?policy=nightly&volume=' + VOL + '&moment=2026-10-20T09%3A00%3A00Z';
    render(<BackupCOSRestorePage />);
    expect(field(/^Policy( \*)?$/).value).toBe('nightly');
    expect(field(/^Bucket( \*)?$/).value).toBe('pool-backups');
    expect(field(/^Endpoint( \*)?$/).value).toBe('s3.eu-de.cloud-object-storage.appdomain.cloud');
    expect(field(/^Folder/).value).toBe('lab');
    expect(field(/^Volume( \*)?$/).value).toBe(VOL);
    expect(field(/^Moment/).value).not.toBe('');
  });

  it('fills the settings from a policy chosen in the list', () => {
    render(<BackupCOSRestorePage />);
    expect(field(/^Bucket( \*)?$/).value).toBe('');
    fireEvent.change(screen.getByLabelText('Fill from a policy of this cluster'), { target: { value: 'nightly' } });
    expect(field(/^Bucket( \*)?$/).value).toBe('pool-backups');
    expect(field(/^Policy( \*)?$/).value).toBe('nightly');
  });

  it('lists only policies that export', () => {
    render(<BackupCOSRestorePage />);
    const select = screen.getByLabelText('Fill from a policy of this cluster') as HTMLSelectElement;
    expect(within(select).getByRole('option', { name: 'nightly' })).toBeTruthy();
    expect(within(select).queryByRole('option', { name: 'plain' })).toBeNull();
  });

  it('gives the list the repository from the settings, and nothing while the settings are not good', () => {
    render(<BackupCOSRestorePage />);
    expect(screen.getByText('COS list for no settings')).toBeTruthy();
    fireEvent.change(screen.getByLabelText('Fill from a policy of this cluster'), { target: { value: 'nightly' } });
    expect(screen.getByText('COS list for pool-backups/lab/nightly/')).toBeTruthy();
  });

  it('takes the volume and the moment from a pick in the list', () => {
    render(<BackupCOSRestorePage />);
    fireEvent.change(screen.getByLabelText('Fill from a policy of this cluster'), { target: { value: 'nightly' } });
    act(() => cosProps.onPick!(VOL, '2026-10-20T09:00:00Z'));
    expect(field(/^Volume( \*)?$/).value).toBe(VOL);
    expect(field(/^Moment/).value).not.toBe('');
  });

  it('keeps Restore off until COS settings, a volume, a size and a storage class are given', () => {
    render(<BackupCOSRestorePage />);
    fireEvent.change(screen.getByLabelText('Fill from a policy of this cluster'), { target: { value: 'nightly' } });
    expect(restoreButton().disabled).toBe(true);
    fillRest();
    expect(restoreButton().disabled).toBe(false);
  });

  it('offers only storage classes of the driver, and says COS does not keep the class or the size', () => {
    render(<BackupCOSRestorePage />);
    const select = field(/^Storage class( \*)?$/) as unknown as HTMLSelectElement;
    expect(within(select).getByRole('option', { name: 'pool-a' })).toBeTruthy();
    expect(within(select).queryByRole('option', { name: 'other' })).toBeNull();
    expect(screen.getByText(/COS does not keep the storage class or the size/)).toBeTruthy();
  });

  it('refuses a claim name that exists', () => {
    claims = [{ metadata: { name: 'data-restored' } }];
    render(<BackupCOSRestorePage />);
    fireEvent.change(screen.getByLabelText('Fill from a policy of this cluster'), { target: { value: 'nightly' } });
    fillRest();
    expect(screen.getByText(/already exists in the namespace/)).toBeTruthy();
    expect(restoreButton().disabled).toBe(true);
  });

  it('asks for confirmation, then makes one BackupRestore with fromCOS and follows it', async () => {
    render(<BackupCOSRestorePage />);
    fireEvent.change(screen.getByLabelText('Fill from a policy of this cluster'), { target: { value: 'nightly' } });
    fillRest();
    fireEvent.click(restoreButton());
    expect(k8sCreate).not.toHaveBeenCalled();
    const dialog = screen.getByRole('dialog');
    expect(within(dialog).getByText(/A new claim apps\/data-restored is made/)).toBeTruthy();
    expect(within(dialog).getByText(/Nothing is overwritten/)).toBeTruthy();
    fireEvent.click(within(dialog).getByRole('button', { name: 'Restore' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    const { data, model } = (k8sCreate as jest.Mock).mock.calls[0][0];
    expect(model.kind).toBe('BackupRestore');
    expect(data.spec).toEqual({
      policy: 'nightly',
      targetNamespace: 'apps',
      targetName: 'data-restored',
      storageClassName: 'pool-a',
      fromCOS: {
        bucket: 'pool-backups',
        endpoint: 's3.eu-de.cloud-object-storage.appdomain.cloud',
        prefix: 'lab',
        credentialsSecret: 'cos-credentials',
        volume: VOL,
        sizeGB: 20,
        backupKeySecret: 'cos-backup-key',
        keyProtect: { instanceID: 'inst-1', region: 'eu-de', rootKeyID: 'root-1', apiKeySecret: 'keyprotect-apikey' },
      },
    });
    expect(await screen.findByText(/Banner for restore-pvc-/)).toBeTruthy();
  });

  it('shows the message when the request is refused', async () => {
    (k8sCreate as jest.Mock).mockRejectedValue(new Error('admission webhook denied the request: spec.fromCOS.volume is not valid'));
    render(<BackupCOSRestorePage />);
    fireEvent.change(screen.getByLabelText('Fill from a policy of this cluster'), { target: { value: 'nightly' } });
    fillRest();
    fireEvent.click(restoreButton());
    fireEvent.click(within(screen.getByRole('dialog')).getByRole('button', { name: 'Restore' }));
    expect(await screen.findByText(/spec.fromCOS.volume is not valid/)).toBeTruthy();
  });

  it('turns Restore off, names the missing right and shows the YAML without the right to make BackupRestore objects', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    render(<BackupCOSRestorePage />);
    fireEvent.change(screen.getByLabelText('Fill from a policy of this cluster'), { target: { value: 'nightly' } });
    fillRest();
    expect(restoreButton().disabled).toBe(true);
    expect(screen.getByText(/You are not allowed to create BackupRestore objects/)).toBeTruthy();
    expect(screen.getByText(/fromCOS:/)).toBeTruthy();
  });

  it('never asks for or shows a Secret value, only Secret names', () => {
    render(<BackupCOSRestorePage />);
    expect(screen.queryByLabelText(/secret_access_key|access key id/i)).toBeNull();
    expect(screen.getByText(/The Secrets must already exist in kube-system/)).toBeTruthy();
  });
});
