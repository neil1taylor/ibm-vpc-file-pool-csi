import { readFileSync } from 'fs';
import { join } from 'path';
import {
  buildCheckJob,
  buildReceiverSecret,
  receiverCheckResult,
  receiverNames,
  validateReceiverAddress,
  validateReceiverCA,
  CheckPodLike,
} from './receiver-objects';

// The Go side (pkg/replication/checkjob_test.go) writes this file from its own builder.
// If either side changes alone, this test or the Go test fails.
const golden = JSON.parse(
  readFileSync(join(__dirname, '../../../pkg/replication/testdata/check-job.golden.json'), 'utf8'),
);

describe('receiver objects', () => {
  it('builds the same check Job as the Go builder', () => {
    expect(buildCheckJob(golden.input)).toEqual(golden.job);
  });

  it('leaves out the CA argument, volume and mount when there is no CA Secret', () => {
    const job = buildCheckJob({ ...golden.input, caSecret: '' });
    const spec = job.spec.template.spec;
    expect(spec.containers[0].args).not.toContain('--ca-cert-file=/etc/replication-ca/ca.crt');
    expect(spec.volumes).toHaveLength(1);
    expect(spec.containers[0].volumeMounts).toHaveLength(1);
  });

  it('names the Secrets and the Job from the policy name', () => {
    expect(receiverNames('dr')).toEqual({ tokenSecret: 'dr-receiver-token', caSecret: 'dr-receiver-ca', job: 'repl-check-dr' });
  });

  it('builds a Secret in kube-system that says the console made it', () => {
    expect(buildReceiverSecret({ name: 'dr-receiver-token', key: 'token', value: 's3cret' })).toEqual({
      apiVersion: 'v1',
      kind: 'Secret',
      type: 'Opaque',
      metadata: {
        name: 'dr-receiver-token',
        namespace: 'kube-system',
        labels: { 'vpc-file-pool-csi.ibm.io/made-by-console': 'true' },
      },
      stringData: { token: 's3cret' },
    });
  });
});

describe('validateReceiverAddress', () => {
  it('accepts an https address', () => {
    expect(validateReceiverAddress('https://repl.apps.example.com')).toBeUndefined();
    expect(validateReceiverAddress('https://repl.example.com:8443/')).toBeUndefined();
  });

  it('refuses http, a bare host, and an empty address, with a plain reason', () => {
    expect(validateReceiverAddress('http://repl.example.com')).toMatch(/https/);
    expect(validateReceiverAddress('repl.example.com')).toMatch(/https/);
    expect(validateReceiverAddress('')).toMatch(/address/i);
  });
});

describe('validateReceiverCA', () => {
  const pem = '-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n';

  it('accepts an empty box (the system trust store) and a certificate', () => {
    expect(validateReceiverCA('')).toBeUndefined();
    expect(validateReceiverCA(pem)).toBeUndefined();
  });

  it('refuses text that is not a certificate, and a private key', () => {
    expect(validateReceiverCA('hello')).toMatch(/BEGIN CERTIFICATE/);
    expect(validateReceiverCA('-----BEGIN PRIVATE KEY-----\nx\n-----END PRIVATE KEY-----')).toMatch(/BEGIN CERTIFICATE/);
  });
});

describe('receiverCheckResult', () => {
  const pod = (phase: string, message?: string, exitCode = 0): CheckPodLike => ({
    metadata: { name: 'repl-check-dr-x' },
    status: {
      phase,
      containerStatuses: message === undefined && phase !== 'Failed' ? [] : [{ state: { terminated: { exitCode, message } } }],
    },
  });

  it('is starting while there is no Pod', () => {
    expect(receiverCheckResult(undefined)).toEqual({ state: 'starting' });
  });

  it('is running while the Pod is Pending or Running', () => {
    expect(receiverCheckResult(pod('Pending'))).toEqual({ state: 'running' });
    expect(receiverCheckResult(pod('Running'))).toEqual({ state: 'running' });
  });

  it('is ok when the result line is ok', () => {
    expect(receiverCheckResult(pod('Succeeded', 'ok'))).toEqual({ state: 'ok' });
  });

  it('shows the reason when the check failed', () => {
    expect(receiverCheckResult(pod('Failed', 'token refused', 1))).toEqual({ state: 'failed', reason: 'token refused' });
  });

  it('says the check ended without a result when the Pod failed with no message', () => {
    expect(receiverCheckResult(pod('Failed', '', 1))).toEqual({
      state: 'failed',
      reason: 'The check ended without a result. It may have run out of time.',
    });
  });
});
