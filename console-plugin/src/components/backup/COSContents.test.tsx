import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sCreate, k8sDelete, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import COSContents from './COSContents';
import { COSRepo, buildCOSListJob } from '../../utils/backup-jobs';

const repo: COSRepo = {
  endpoint: 's3.eu-de.cloud-object-storage.appdomain.cloud',
  bucket: 'pool-backups',
  prefix: 'lab/nightly/',
  credentialsSecret: 'cos-credentials',
  backupKeySecret: 'cos-backup-key',
  keyProtect: { instanceID: 'inst-1', region: 'eu-de', rootKeyID: 'root-1', apiKeySecret: 'keyprotect-apikey' },
  emergencyKeySecret: '',
};

const VOL = 'pvc-11111111-2222-3333-4444-555555555555';
const deployments = [
  { spec: { template: { spec: { containers: [{ name: 'csi-controller', image: 'icr.io/x/driver:v1' }] } } } },
];
const pod = (message: string) => ({ status: { containerStatuses: [{ state: { terminated: { message } } }] } });

let job: unknown;
let pods: unknown[];
let controllers: unknown[];
let lastJobName = '';

beforeEach(() => {
  jest.clearAllMocks();
  job = { metadata: { name: 'j' }, status: { succeeded: 1 } };
  pods = [pod(`{"volumes":[{"volume":"${VOL}","moments":["2026-10-20T09:00:00Z","2026-10-21T09:00:00Z"]}],"truncated":false}`)];
  controllers = deployments;
  lastJobName = '';
  (useAccessReview as jest.Mock).mockReturnValue([true, false]);
  (k8sCreate as jest.Mock).mockResolvedValue({});
  (k8sDelete as jest.Mock).mockResolvedValue({});
  (useK8sWatchResource as jest.Mock).mockImplementation(
    (req: { groupVersionKind: { kind: string }; name?: string } | null) => {
      if (!req) return [undefined, true, undefined];
      switch (req.groupVersionKind.kind) {
        case 'Deployment':
          return [controllers, true, undefined];
        case 'Job':
          lastJobName = req.name || '';
          return [job, true, undefined];
        case 'Pod':
          return [pods, true, undefined];
        default:
          return [[], true, undefined];
      }
    },
  );
});

const listButton = () => screen.getByRole('button', { name: 'List COS contents' }) as HTMLButtonElement;

describe('COSContents', () => {
  it('starts the list Job with the same spec as the driver builds, and no PV or claim', async () => {
    render(<COSContents repo={repo} />);
    fireEvent.click(listButton());
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    const call = (k8sCreate as jest.Mock).mock.calls[0][0];
    expect(call.model.kind).toBe('Job');
    const expected = buildCOSListJob({
      key: call.data.metadata.name.replace('share-data-', ''),
      namespace: 'kube-system',
      driverImage: 'icr.io/x/driver:v1',
      repo,
      volume: '',
    });
    expect(call.data).toEqual(expected);
  });

  it('lists the volumes and the moments, and removes the Job', async () => {
    render(<COSContents repo={repo} />);
    fireEvent.click(listButton());
    expect(await screen.findByText(VOL)).toBeTruthy();
    expect(screen.getByText('2026-10-20T09:00:00Z')).toBeTruthy();
    expect(screen.getByText('2026-10-21T09:00:00Z')).toBeTruthy();
    await waitFor(() => expect(k8sDelete).toHaveBeenCalledTimes(1));
    expect((k8sDelete as jest.Mock).mock.calls[0][0].model.kind).toBe('Job');
    expect(lastJobName).toMatch(/^share-data-coslist-/);
  });

  it('limits the list to one volume when asked', async () => {
    render(<COSContents repo={repo} />);
    fireEvent.change(screen.getByLabelText('Limit to one volume (optional)'), { target: { value: VOL } });
    fireEvent.click(listButton());
    await waitFor(() => expect(k8sCreate).toHaveBeenCalled());
    const script = (k8sCreate as jest.Mock).mock.calls[0][0].data.spec.template.spec.containers[0].command[2];
    expect(script).toContain(`-v vol='${VOL}'`);
  });

  it('refuses a volume name that is not one, before it starts a Job', () => {
    render(<COSContents repo={repo} />);
    fireEvent.change(screen.getByLabelText('Limit to one volume (optional)'), { target: { value: 'data' } });
    expect(listButton().disabled).toBe(true);
    expect(screen.getByText(/pvc- and a UUID/)).toBeTruthy();
  });

  it('says when the list was cut', async () => {
    pods = [pod(`{"volumes":[{"volume":"${VOL}","moments":["2026-10-20T09:00:00Z"]}],"truncated":true}`)];
    render(<COSContents repo={repo} />);
    fireEvent.click(listButton());
    expect(await screen.findByText(/The list is cut/)).toBeTruthy();
  });

  it('says when the bucket holds no backups of the policy', async () => {
    pods = [pod('{"volumes":[],"truncated":false}')];
    render(<COSContents repo={repo} />);
    fireEvent.click(listButton());
    expect(await screen.findByText(/No backups found in bucket pool-backups/)).toBeTruthy();
  });

  it('shows an error when the Job fails', async () => {
    job = { metadata: { name: 'j' }, status: { failed: 4 } };
    render(<COSContents repo={repo} />);
    fireEvent.click(listButton());
    expect(await screen.findByText(/Listing the bucket failed/)).toBeTruthy();
  });

  it('shows an error when the Job reports no listing', async () => {
    pods = [pod('garbage')];
    render(<COSContents repo={repo} />);
    fireEvent.click(listButton());
    expect(await screen.findByText(/did not report a listing/)).toBeTruthy();
  });

  it('says when it cannot find the driver controller', async () => {
    controllers = [];
    render(<COSContents repo={repo} />);
    fireEvent.click(listButton());
    expect(await screen.findByText(/Cannot find the driver controller in kube-system/)).toBeTruthy();
    expect(k8sCreate).not.toHaveBeenCalled();
  });

  it('shows the error when the Job cannot be started', async () => {
    (k8sCreate as jest.Mock).mockRejectedValue(new Error('jobs.batch is forbidden'));
    render(<COSContents repo={repo} />);
    fireEvent.click(listButton());
    expect(await screen.findByText(/jobs.batch is forbidden/)).toBeTruthy();
  });

  it('offers a Restore button for each moment when it is given a picker', async () => {
    const onPick = jest.fn();
    render(<COSContents repo={repo} onPick={onPick} />);
    fireEvent.click(listButton());
    await screen.findByText(VOL);
    fireEvent.click(screen.getAllByRole('button', { name: 'Restore this moment' })[1]);
    expect(onPick).toHaveBeenCalledWith(VOL, '2026-10-21T09:00:00Z');
  });

  it('turns the button off, names the missing rights and shows the YAML for an administrator without the rights', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    render(<COSContents repo={repo} />);
    expect(listButton().disabled).toBe(true);
    expect(screen.getByText(/You are not allowed to make and remove a Job in kube-system/)).toBeTruthy();
    expect(screen.getByText(/kind: Job/)).toBeTruthy();
  });

  it('turns the button off when there are no settings to list with', () => {
    render(<COSContents repo={undefined} unavailable="This policy does not export to COS." />);
    expect(listButton().disabled).toBe(true);
    expect(screen.getByText('This policy does not export to COS.')).toBeTruthy();
  });

  it('never shows a Secret value, only names', async () => {
    render(<COSContents repo={repo} />);
    fireEvent.click(listButton());
    await screen.findByText(VOL);
    expect(document.body.textContent).not.toMatch(/access_key_id|secret_access_key/);
  });
});
