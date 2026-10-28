import React from 'react';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { k8sCreate, k8sUpdate, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import BackupPolicyFormPage from './BackupPolicyFormPage';

let mockParams: { name?: string } = {};
const mockPush = jest.fn();
jest.mock('react-router-dom', () => ({
  useParams: () => mockParams,
  useHistory: () => ({ push: mockPush }),
  Link: ({ to, children }: { to: string; children: React.ReactNode }) => <a href={to}>{children}</a>,
}));

const pool = (name: string, ready = true) => ({
  metadata: { name },
  status: { shares: [{ state: ready ? 'stable' : 'creating', mountTargetIP: ready ? '10.0.0.1' : '' }] },
});

const existingPolicy = {
  apiVersion: 'storage.ibmcloud.io/v1alpha1',
  kind: 'BackupPolicy',
  metadata: { name: 'nightly', resourceVersion: '5' },
  spec: {
    snapshotPool: 'snaps',
    schedule: '6h',
    keepFor: '14d',
    selector: { matchLabels: { backup: 'yes' }, matchExpressions: [{ key: 'tier', operator: 'In', values: ['gold'] }] },
    export: { bucket: 'b', endpoint: 'e' },
  },
};

let policies: unknown[];
let matchingClaims: unknown[];
let lastClaimRequest: { selector?: unknown } | null;

beforeEach(() => {
  jest.clearAllMocks();
  mockParams = {};
  policies = [{ metadata: { name: 'taken' }, spec: {} }];
  matchingClaims = [];
  lastClaimRequest = null;
  (useAccessReview as jest.Mock).mockReturnValue([true, false]);
  (k8sCreate as jest.Mock).mockResolvedValue({});
  (k8sUpdate as jest.Mock).mockResolvedValue({});
  (useK8sWatchResource as jest.Mock).mockImplementation(
    (req: { groupVersionKind: { kind: string }; isList?: boolean; name?: string; selector?: unknown } | null) => {
      if (!req) return [undefined, true, undefined];
      switch (req.groupVersionKind.kind) {
        case 'BackupPolicy':
          return req.isList ? [policies, true, undefined] : [existingPolicy, true, undefined];
        case 'FileSharePool':
          return [[pool('snaps'), pool('data'), pool('fresh', false)], true, undefined];
        case 'SubVolume':
          return [[{ spec: { poolName: 'data' } }], true, undefined];
        case 'PersistentVolumeClaim':
          lastClaimRequest = req;
          return [matchingClaims, true, undefined];
        default:
          return [[], true, undefined];
      }
    },
  );
});

const createButton = () => screen.getByRole('button', { name: 'Create' }) as HTMLButtonElement;
const saveButton = () => screen.getByRole('button', { name: 'Save' }) as HTMLButtonElement;

const fillValid = () => {
  fireEvent.change(screen.getByLabelText(/^Name( \*)?$/), { target: { value: 'nightly' } });
  fireEvent.change(screen.getByLabelText(/^Snapshot pool/), { target: { value: 'snaps' } });
  fireEvent.change(screen.getByLabelText('Label key'), { target: { value: 'backup' } });
  fireEvent.change(screen.getByLabelText('Label value'), { target: { value: 'yes' } });
  fireEvent.click(screen.getByRole('button', { name: 'Add label' }));
};

describe('BackupPolicyFormPage (create)', () => {
  it('explains the 85% stop', () => {
    render(<BackupPolicyFormPage />);
    expect(screen.getByText('Backups stop when the snapshot pool is more than 85% full')).toBeTruthy();
    expect(screen.getByText(/never deleted to make room/i)).toBeTruthy();
  });

  it('offers only pools that can hold backups, and says why the others cannot', () => {
    render(<BackupPolicyFormPage />);
    const select = screen.getByLabelText(/^Snapshot pool/) as HTMLSelectElement;
    expect((within(select).getByRole('option', { name: 'snaps' }) as HTMLOptionElement).disabled).toBe(false);
    const data = within(select).getByRole('option', { name: /data.*holds 1 workload claim/ }) as HTMLOptionElement;
    expect(data.disabled).toBe(true);
    expect((within(select).getByRole('option', { name: /fresh.*no ready share/ }) as HTMLOptionElement).disabled).toBe(true);
  });

  it('keeps Create off until the form is valid', () => {
    render(<BackupPolicyFormPage />);
    expect(createButton().disabled).toBe(true);
    fillValid();
    expect(createButton().disabled).toBe(false);
  });

  it('refuses a name that is taken', () => {
    render(<BackupPolicyFormPage />);
    fillValid();
    fireEvent.change(screen.getByLabelText(/^Name( \*)?$/), { target: { value: 'taken' } });
    expect(screen.getByText(/already exists/)).toBeTruthy();
    expect(createButton().disabled).toBe(true);
  });

  it('refuses a schedule that is too short', () => {
    render(<BackupPolicyFormPage />);
    fillValid();
    fireEvent.click(screen.getByRole('button', { name: 'Custom' }));
    fireEvent.change(screen.getByPlaceholderText(/e\.g\. 30m/), { target: { value: '30s' } });
    expect(screen.getByText(/at least 1 minute/)).toBeTruthy();
    expect(createButton().disabled).toBe(true);
  });

  it('says that a policy with no labels would back up every volume', () => {
    render(<BackupPolicyFormPage />);
    expect(screen.getByText(/would back up every volume/)).toBeTruthy();
  });

  it('counts the claims the labels match, live, and does not ask the cluster before there is a label', () => {
    render(<BackupPolicyFormPage />);
    expect(lastClaimRequest).toBeNull();
    matchingClaims = [
      { metadata: { namespace: 'apps', name: 'a' } },
      { metadata: { namespace: 'apps', name: 'b' } },
    ];
    fillValid();
    expect(lastClaimRequest?.selector).toEqual({ matchLabels: { backup: 'yes' } });
    expect(screen.getByText('2 claims match.')).toBeTruthy();
  });

  it('removes a label with its close button', () => {
    render(<BackupPolicyFormPage />);
    fillValid();
    fireEvent.click(screen.getByRole('button', { name: 'Remove backup' }));
    expect(screen.queryByText('backup=yes')).toBeNull();
  });

  it('explains the VM copy speed and that Red Hat does not support copying a running VM', () => {
    render(<BackupPolicyFormPage />);
    expect(screen.getByText(/Not supported by Red Hat/)).toBeTruthy();
    expect(screen.getByLabelText('More info for VM copy highest speed (MB/s per disk)')).toBeTruthy();
  });

  it('creates the policy and opens its page', async () => {
    render(<BackupPolicyFormPage />);
    fillValid();
    fireEvent.change(screen.getByLabelText(/^Namespaces/), { target: { value: 'apps, web' } });
    fireEvent.click(createButton());
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    const { data, model } = (k8sCreate as jest.Mock).mock.calls[0][0];
    expect(model.kind).toBe('BackupPolicy');
    expect(data.metadata.name).toBe('nightly');
    expect(data.spec).toEqual({
      snapshotPool: 'snaps',
      schedule: '6h',
      keepFor: '7d',
      selector: { matchLabels: { backup: 'yes' } },
      namespaces: ['apps', 'web'],
    });
    await waitFor(() => expect(mockPush).toHaveBeenCalledWith('/vpc-file-pools/backups/nightly'));
  });

  it('shows the message of the admission webhook as it is', async () => {
    (k8sCreate as jest.Mock).mockRejectedValue(new Error('admission webhook denied the request: spec.keepFor must be at least 1h'));
    render(<BackupPolicyFormPage />);
    fillValid();
    fireEvent.click(createButton());
    expect(await screen.findByText(/spec.keepFor must be at least 1h/)).toBeTruthy();
    expect(mockPush).not.toHaveBeenCalled();
  });

  it('turns Create off, names the missing right and shows the YAML for an administrator without the right', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    render(<BackupPolicyFormPage />);
    fillValid();
    expect(createButton().disabled).toBe(true);
    expect(screen.getByText(/You are not allowed to create BackupPolicy objects/)).toBeTruthy();
    expect(screen.getByText(/kind: BackupPolicy/)).toBeTruthy();
  });
});

describe('BackupPolicyFormPage (edit)', () => {
  beforeEach(() => {
    mockParams = { name: 'nightly' };
  });

  it('fills the form from the policy, and the name cannot change', () => {
    render(<BackupPolicyFormPage />);
    expect((screen.getByLabelText(/^Name( \*)?$/) as HTMLInputElement).value).toBe('nightly');
    expect((screen.getByLabelText(/^Name( \*)?$/) as HTMLInputElement).disabled).toBe(true);
    expect((screen.getByLabelText(/^Keep for/) as HTMLInputElement).value).toBe('14d');
    expect(screen.getByText('backup=yes')).toBeTruthy();
  });

  it('says that match expressions and the export are kept and are not edited here', () => {
    render(<BackupPolicyFormPage />);
    expect(screen.getByText(/match expressions/i)).toBeTruthy();
  });

  it('saves with an update that keeps the export and the expressions', async () => {
    render(<BackupPolicyFormPage />);
    fireEvent.change(screen.getByLabelText(/^Keep for/), { target: { value: '30d' } });
    fireEvent.click(saveButton());
    await waitFor(() => expect(k8sUpdate).toHaveBeenCalledTimes(1));
    const { data } = (k8sUpdate as jest.Mock).mock.calls[0][0];
    expect(data.metadata.resourceVersion).toBe('5');
    expect(data.spec.keepFor).toBe('30d');
    expect(data.spec.export).toEqual({ bucket: 'b', endpoint: 'e' });
    expect(data.spec.selector.matchExpressions).toHaveLength(1);
    await waitFor(() => expect(mockPush).toHaveBeenCalledWith('/vpc-file-pools/backups/nightly'));
  });

  it('can suspend the policy from the form', async () => {
    render(<BackupPolicyFormPage />);
    fireEvent.click(screen.getByRole('checkbox', { name: /Suspend/ }));
    fireEvent.click(saveButton());
    await waitFor(() => expect(k8sUpdate).toHaveBeenCalled());
    expect((k8sUpdate as jest.Mock).mock.calls[0][0].data.spec.suspend).toBe(true);
  });

  it('names the missing right to update', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    render(<BackupPolicyFormPage />);
    expect(saveButton().disabled).toBe(true);
    expect(screen.getByText(/You are not allowed to update BackupPolicy objects/)).toBeTruthy();
  });
});
