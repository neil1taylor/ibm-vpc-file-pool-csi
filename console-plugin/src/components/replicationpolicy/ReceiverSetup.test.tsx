import React from 'react';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { k8sCreate, k8sPatch, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import ReceiverSetup from './ReceiverSetup';
import { buildCheckJob } from '../../utils/receiver-objects';

const PEM = '-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----';

type Data = Record<string, unknown[]>;

function watch(data: Data) {
  (useK8sWatchResource as jest.Mock).mockImplementation((req: { groupVersionKind: { kind: string } } | null) => {
    if (!req) return [undefined, true, undefined];
    return [data[req.groupVersionKind.kind] ?? [], true, undefined];
  });
}

const deployment = {
  metadata: { name: 'ibm-vpc-file-pool-csi-controller', namespace: 'kube-system' },
  spec: { template: { spec: { containers: [{ name: 'csi-controller', image: 'icr.io/example/driver:v1' }] } } },
};

const podWith = (phase: string, message?: string) => ({
  metadata: { name: 'repl-check-dr-x-abc' },
  status: {
    phase,
    containerStatuses: message === undefined ? [] : [{ state: { terminated: { exitCode: phase === 'Succeeded' ? 0 : 1, message } } }],
  },
});

const setup = (over: Partial<React.ComponentProps<typeof ReceiverSetup>> = {}) => {
  const props = {
    policyName: 'dr',
    endpoint: 'https://repl.example.com',
    onEndpointChange: jest.fn(),
    authSecretRef: '',
    onAuthSecretRefChange: jest.fn(),
    caCertSecretRef: '',
    onCaCertSecretRefChange: jest.fn(),
    onCheckedChange: jest.fn(),
    ...over,
  };
  render(<ReceiverSetup {...props} />);
  return props;
};

const checkButton = () => screen.getByRole('button', { name: 'Check connection' }) as HTMLButtonElement;
const fillToken = (value = 's3cret') => fireEvent.change(screen.getByLabelText(/^Token( \*)?$/), { target: { value } });
const fillCA = (value: string) => fireEvent.change(screen.getByLabelText(/^CA certificate/), { target: { value } });

beforeEach(() => {
  jest.clearAllMocks();
  (useAccessReview as jest.Mock).mockReturnValue([true, false]);
  (k8sPatch as jest.Mock).mockRejectedValue({ code: 404 });
  (k8sCreate as jest.Mock).mockResolvedValue({});
  watch({ Deployment: [deployment], Pod: [] });
});

describe('ReceiverSetup', () => {
  it('keeps Check connection off until there is a token', () => {
    setup();
    expect(checkButton().disabled).toBe(true);
    fillToken();
    expect(checkButton().disabled).toBe(false);
  });

  it('refuses an http address, with a plain reason', () => {
    setup({ endpoint: 'http://repl.example.com' });
    expect(screen.getByText(/Use an https:\/\/ address/)).toBeTruthy();
    fillToken();
    expect(checkButton().disabled).toBe(true);
  });

  it('refuses a certificate box that does not hold a certificate', () => {
    setup();
    fillToken();
    fillCA('hello');
    expect(screen.getByText(/BEGIN CERTIFICATE/)).toBeTruthy();
    expect(checkButton().disabled).toBe(true);
  });

  it('needs a policy name first, and says so', () => {
    setup({ policyName: '' });
    fillToken();
    expect(checkButton().disabled).toBe(true);
    expect(screen.getByText(/Enter the policy name first/)).toBeTruthy();
  });

  it('makes the two Secrets and the check Job from the driver image of the controller', async () => {
    const props = setup();
    fillToken();
    fillCA(PEM);
    fireEvent.click(checkButton());

    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(3));
    const created = (k8sCreate as jest.Mock).mock.calls.map((c) => c[0].data);
    expect(created[0]).toMatchObject({ kind: 'Secret', metadata: { name: 'dr-receiver-token', namespace: 'kube-system' }, stringData: { token: 's3cret' } });
    expect(created[1]).toMatchObject({ kind: 'Secret', metadata: { name: 'dr-receiver-ca' }, stringData: { 'ca.crt': PEM } });
    expect(created[2].metadata.name).toMatch(/^repl-check-dr-[a-z0-9]+$/);
    expect(created[2]).toEqual(
      buildCheckJob({
        name: created[2].metadata.name,
        namespace: 'kube-system',
        image: 'icr.io/example/driver:v1',
        endpoint: 'https://repl.example.com',
        authSecret: 'dr-receiver-token',
        caSecret: 'dr-receiver-ca',
      }),
    );
    expect(props.onAuthSecretRefChange).toHaveBeenCalledWith('dr-receiver-token');
    expect(props.onCaCertSecretRefChange).toHaveBeenCalledWith('dr-receiver-ca');
  });

  it('makes no CA Secret when the box is empty', async () => {
    const props = setup();
    fillToken();
    fireEvent.click(checkButton());
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(2));
    expect(props.onCaCertSecretRefChange).toHaveBeenCalledWith('');
  });

  it('replaces the token of a Secret the console made earlier, and refuses one it did not make', async () => {
    (k8sPatch as jest.Mock).mockResolvedValue({});
    setup();
    fillToken('new-token');
    fireEvent.click(checkButton());
    await waitFor(() => expect(k8sPatch).toHaveBeenCalled());
    const patch = (k8sPatch as jest.Mock).mock.calls[0][0];
    expect(patch.data).toEqual([
      { op: 'test', path: '/metadata/labels/vpc-file-pool-csi.ibm.io~1made-by-console', value: 'true' },
      { op: 'add', path: '/stringData', value: { token: 'new-token' } },
    ]);
    // Only the Job is created: the Secret already exists.
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    expect((k8sCreate as jest.Mock).mock.calls[0][0].data.kind).toBe('Job');
  });

  it('stops, and says why, when a Secret of that name was not made by the console', async () => {
    (k8sPatch as jest.Mock).mockRejectedValue({ code: 422 });
    setup();
    fillToken();
    fireEvent.click(checkButton());
    expect(await screen.findByText(/was not made by the console/)).toBeTruthy();
    expect(k8sCreate).not.toHaveBeenCalled();
  });

  it('says the receiver is ready when the check Pod ends with ok, and tells the page', async () => {
    watch({ Deployment: [deployment], Pod: [podWith('Succeeded', 'ok')] });
    const props = setup();
    fillToken();
    fireEvent.click(checkButton());
    expect(await screen.findByText(/token is accepted/)).toBeTruthy();
    expect(props.onCheckedChange).toHaveBeenLastCalledWith(true);
  });

  it('shows the reason, and keeps Create off, when the check fails', async () => {
    watch({ Deployment: [deployment], Pod: [podWith('Failed', 'token refused')] });
    const props = setup();
    fillToken();
    fireEvent.click(checkButton());
    expect(await screen.findByText(/token refused/)).toBeTruthy();
    expect(props.onCheckedChange).not.toHaveBeenCalledWith(true);
  });

  it('turns a passed check off again when a field changes', async () => {
    watch({ Deployment: [deployment], Pod: [podWith('Succeeded', 'ok')] });
    const props = setup();
    fillToken();
    fireEvent.click(checkButton());
    await screen.findByText(/token is accepted/);
    fillToken('another');
    expect(props.onCheckedChange).toHaveBeenLastCalledWith(false);
  });

  it('can check existing Secrets by name, and makes no Secret', async () => {
    const props = setup({ authSecretRef: 'my-token', caCertSecretRef: 'my-ca' });
    fireEvent.change(screen.getByLabelText(/^Token and certificate/), { target: { value: 'existing' } });
    fireEvent.click(checkButton());
    await waitFor(() => expect(k8sCreate).toHaveBeenCalledTimes(1));
    const job = (k8sCreate as jest.Mock).mock.calls[0][0].data;
    expect(job.kind).toBe('Job');
    expect(job.spec.template.spec.volumes.map((v: { secret: { secretName: string } }) => v.secret.secretName)).toEqual(['my-token', 'my-ca']);
    expect(props.onAuthSecretRefChange).not.toHaveBeenCalled();
  });

  it('stops, and says why, when the driver controller cannot be found', async () => {
    watch({ Deployment: [], Pod: [] });
    setup();
    fillToken();
    fireEvent.click(checkButton());
    expect(await screen.findByText(/driver controller/i)).toBeTruthy();
    expect(k8sCreate).not.toHaveBeenCalled();
  });

  it('warns, and lets the policy be made without the check, when the user cannot make Jobs', () => {
    (useAccessReview as jest.Mock).mockReturnValue([false, false]);
    const props = setup();
    expect(screen.getByText(/cannot run the check/i)).toBeTruthy();
    expect(props.onCheckedChange).toHaveBeenCalledWith(true);
  });

  it('turns a passed check off again when the policy name changes, as the Secret names come from it', async () => {
    watch({ Deployment: [deployment], Pod: [podWith('Succeeded', 'ok')] });
    const props = {
      policyName: 'dr',
      endpoint: 'https://repl.example.com',
      onEndpointChange: jest.fn(),
      authSecretRef: '',
      onAuthSecretRefChange: jest.fn(),
      caCertSecretRef: '',
      onCaCertSecretRefChange: jest.fn(),
      onCheckedChange: jest.fn(),
    };
    const { rerender } = render(<ReceiverSetup {...props} />);
    fillToken();
    fireEvent.click(checkButton());
    await screen.findByText(/token is accepted/);
    rerender(<ReceiverSetup {...props} policyName="dr2" />);
    expect(props.onCheckedChange).toHaveBeenLastCalledWith(false);
  });
});
