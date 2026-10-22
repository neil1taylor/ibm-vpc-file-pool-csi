import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sCreate, k8sPatch, useAccessReview } from '@openshift-console/dynamic-plugin-sdk';
import ExportSetupGuide from './ExportSetupGuide';
import { BackupPolicy } from '../../types';

const ago = new Date(Date.now() - 3600 * 1000).toISOString();

const policy = (over: Partial<BackupPolicy> = {}): BackupPolicy =>
  ({
    metadata: { name: 'nightly' },
    spec: { snapshotPool: 'snaps', schedule: '6h', selector: {} },
    status: { phase: 'Active', volumes: [] },
    ...over,
  }) as BackupPolicy;

const manifests = `apiVersion: v1
kind: Secret
metadata:
  name: cos-credentials
  namespace: kube-system
type: Opaque
stringData:
  access_key_id: "AKIA123"
  secret_access_key: "sekret"
---
apiVersion: v1
kind: Secret
metadata:
  name: cos-backup-key
  namespace: kube-system
type: Opaque
stringData:
  wrappedKey: "eyJ3cmFwIjoxfQ=="
`;

const block = `spec:
  export:
    backupKeySecret: cos-backup-key
    bucket: pool-backups
    credentialsSecret: cos-credentials
    endpoint: s3.eu-de.cloud-object-storage.appdomain.cloud
    keepFor: 30d
    keyProtect:
      apiKeySecret: keyprotect-apikey
      instanceID: inst-1
      region: eu-de
      rootKeyID: root-1
    lock: Compliance
`;

beforeEach(() => {
  jest.clearAllMocks();
  (useAccessReview as jest.Mock).mockReturnValue([true, false]);
  (k8sCreate as jest.Mock).mockResolvedValue({});
  (k8sPatch as jest.Mock).mockResolvedValue({});
});

const paste = (label: string, value: string) => fireEvent.change(screen.getByLabelText(label), { target: { value } });

describe('ExportSetupGuide: the command', () => {
  it('says that the console does not run cos-setup, and why', () => {
    render(<ExportSetupGuide policy={policy()} />);
    expect(screen.getByText(/does not run cos-setup/)).toBeTruthy();
    expect(screen.getByText(/IBM Cloud login/)).toBeTruthy();
  });

  it('builds the command from the names typed in, and explains the flags', () => {
    render(<ExportSetupGuide policy={policy()} />);
    paste('Region', 'eu-de');
    paste('Resource group', 'default');
    paste('Bucket', 'pool-backups');
    paste('Key Protect root key CRN', 'crn:v1:bluemix:public:kms:eu-de:a/acc:inst:key:k');
    const code = document.getElementById('yaml-content')!.textContent!;
    expect(code).toContain('kubectl backup cos-setup');
    expect(code).toContain('--bucket pool-backups');
    expect(code).toContain("--root-key 'crn:v1:bluemix:public:kms:eu-de:a/acc:inst:key:k'");
    expect(screen.getByText('--dry-run')).toBeTruthy();
  });
});

describe('ExportSetupGuide: the Secrets', () => {
  it('makes the Secrets from the pasted manifests, and never reads one back', async () => {
    render(<ExportSetupGuide policy={policy()} />);
    paste('Secret manifests from cos-setup', manifests);
    fireEvent.click(screen.getByRole('button', { name: 'Make the Secrets' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(2));
    const created = (k8sCreate as jest.Mock).mock.calls.map((c) => c[0].data);
    expect(created.map((s) => s.metadata.name)).toEqual(['cos-credentials', 'cos-backup-key']);
    expect(created[0].stringData).toEqual({ access_key_id: 'AKIA123', secret_access_key: 'sekret' });
    expect(await screen.findByText('Secret cos-credentials: made')).toBeTruthy();
  });

  it('leaves a Secret that exists as it is', async () => {
    (k8sCreate as jest.Mock).mockRejectedValue({ code: 409 });
    render(<ExportSetupGuide policy={policy()} />);
    paste('Secret manifests from cos-setup', manifests);
    fireEvent.click(screen.getByRole('button', { name: 'Make the Secrets' }));
    expect(await screen.findByText('Secret cos-credentials: exists already, left as it is')).toBeTruthy();
  });

  it('makes the Key Protect API key Secret from the key typed in', async () => {
    render(<ExportSetupGuide policy={policy()} />);
    paste('Key Protect API key', 'kp-key');
    fireEvent.click(screen.getByRole('button', { name: 'Make the Secrets' }));
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    const data = (k8sCreate as jest.Mock).mock.calls[0][0].data;
    expect(data.metadata.name).toBe('keyprotect-apikey');
    expect(data.stringData).toEqual({ apikey: 'kp-key' });
  });

  it('shows what is wrong with the manifests and makes nothing', () => {
    render(<ExportSetupGuide policy={policy()} />);
    paste('Secret manifests from cos-setup', manifests.replace('kube-system', 'default'));
    expect(screen.getByText(/kube-system only/)).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Make the Secrets' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('refuses the readable backup key', () => {
    render(<ExportSetupGuide policy={policy()} />);
    paste('Secret manifests from cos-setup', manifests.replace('wrappedKey', 'backupKey'));
    expect(screen.getByText(/readable backup key/)).toBeTruthy();
  });

  it('shows the error when a Secret cannot be made', async () => {
    (k8sCreate as jest.Mock).mockRejectedValue(new Error('secrets is forbidden'));
    render(<ExportSetupGuide policy={policy()} />);
    paste('Secret manifests from cos-setup', manifests);
    fireEvent.click(screen.getByRole('button', { name: 'Make the Secrets' }));
    expect(await screen.findByText(/secrets is forbidden/)).toBeTruthy();
  });

  it('turns the button off and names the missing right without it', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    render(<ExportSetupGuide policy={policy()} />);
    paste('Secret manifests from cos-setup', manifests);
    expect((screen.getByRole('button', { name: 'Make the Secrets' }) as HTMLButtonElement).disabled).toBe(true);
    expect(screen.getByText(/You are not allowed to create Secrets in kube-system/)).toBeTruthy();
  });
});

describe('ExportSetupGuide: the policy', () => {
  it('writes spec.export into the policy with a patch', async () => {
    render(<ExportSetupGuide policy={policy()} />);
    paste('spec.export block from cos-setup', block);
    fireEvent.click(screen.getByRole('button', { name: 'Save the export to the policy' }));
    await waitFor(() => expect(k8sPatch).toHaveBeenCalledTimes(1));
    const arg = (k8sPatch as jest.Mock).mock.calls[0][0];
    expect(arg.data).toEqual([{ op: 'add', path: '/spec/export', value: expect.objectContaining({ bucket: 'pool-backups' }) }]);
    expect(arg.resource.metadata.name).toBe('nightly');
    expect(await screen.findByText(/The export is saved/)).toBeTruthy();
  });

  it('shows what is wrong with the block and does not patch', () => {
    render(<ExportSetupGuide policy={policy()} />);
    paste('spec.export block from cos-setup', block.replace('keepFor: 30d', 'keepFor: 24h'));
    expect(screen.getByText(/at least 25h/)).toBeTruthy();
    expect((screen.getByRole('button', { name: 'Save the export to the policy' }) as HTMLButtonElement).disabled).toBe(true);
  });

  it('shows the message of the admission webhook when the patch is refused', async () => {
    (k8sPatch as jest.Mock).mockRejectedValue(new Error('admission webhook denied the request: bucket is not valid'));
    render(<ExportSetupGuide policy={policy()} />);
    paste('spec.export block from cos-setup', block);
    fireEvent.click(screen.getByRole('button', { name: 'Save the export to the policy' }));
    expect(await screen.findByText(/bucket is not valid/)).toBeTruthy();
  });

  it('turns the button off and names the missing right without the right to update the policy', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    render(<ExportSetupGuide policy={policy()} />);
    paste('spec.export block from cos-setup', block);
    expect(screen.getByText(/You are not allowed to update BackupPolicy objects/)).toBeTruthy();
    expect(screen.getByText(/bucket: pool-backups/, { selector: 'code, pre, code *' })).toBeTruthy();
  });
});

describe('ExportSetupGuide: does it work', () => {
  it('says the export is off for a policy with none', () => {
    render(<ExportSetupGuide policy={policy()} />);
    expect(screen.getByText('The policy does not export to COS.')).toBeTruthy();
  });

  it('says whether the export works, from the export state of the volumes', () => {
    const p = policy();
    p.spec.export = { bucket: 'b' } as never;
    p.status!.volumes = [{ pvcNamespace: 'a', pvcName: 'x', subVolumeName: 's', exportedTime: ago }];
    render(<ExportSetupGuide policy={p} />);
    expect(screen.getByText('The export works: 1 of 1 volumes have been sent to COS.')).toBeTruthy();
  });
});
