import { K8sResourceCommon } from '@openshift-console/dynamic-plugin-sdk';

// The check Job is the same one the Go builder makes (pkg/replication/checkjob.go).
// pkg/replication/checkjob_test.go writes it to a golden file, and receiver-objects.test.ts compares
// this builder with that file, so the two cannot drift apart.

export const RECEIVER_NAMESPACE = 'kube-system';
export const LABEL_RECEIVER_CHECK = 'vpc-file-pool-csi.ibm.io/receiver-check';
export const LABEL_MADE_BY_CONSOLE = 'vpc-file-pool-csi.ibm.io/made-by-console';

export interface CheckJobInput {
  name: string;
  namespace: string;
  image: string;
  endpoint: string;
  authSecret: string;
  /** Empty means the system trust store. */
  caSecret: string;
}

export interface CheckJob {
  apiVersion: string;
  kind: string;
  metadata: { name: string; namespace: string; labels: Record<string, string> };
  spec: {
    backoffLimit: number;
    activeDeadlineSeconds: number;
    ttlSecondsAfterFinished: number;
    template: {
      metadata: { labels: Record<string, string> };
      spec: {
        restartPolicy: string;
        securityContext: { runAsUser: number };
        containers: Array<{
          name: string;
          image: string;
          args: string[];
          volumeMounts: Array<{ name: string; mountPath: string; readOnly: boolean }>;
          resources: { requests: Record<string, string>; limits: Record<string, string> };
        }>;
        volumes: Array<{ name: string; secret: { secretName: string } }>;
      };
    };
  };
}

export interface ReceiverSecret extends K8sResourceCommon {
  type: string;
  stringData: Record<string, string>;
}

/** The names the console gives the two Secrets and the check Job of a policy. */
export function receiverNames(policyName: string): { tokenSecret: string; caSecret: string; job: string } {
  return {
    tokenSecret: `${policyName}-receiver-token`,
    caSecret: `${policyName}-receiver-ca`,
    job: `repl-check-${policyName}`,
  };
}

export function buildCheckJob(input: CheckJobInput): CheckJob {
  const args = [
    '--mode=check-receiver',
    `--receiver-endpoint=${input.endpoint}`,
    '--auth-token-file=/etc/replication/token',
  ];
  const volumeMounts = [{ name: 'auth-token', mountPath: '/etc/replication', readOnly: true }];
  const volumes = [{ name: 'auth-token', secret: { secretName: input.authSecret } }];
  if (input.caSecret) {
    args.push('--ca-cert-file=/etc/replication-ca/ca.crt');
    volumeMounts.push({ name: 'ca-cert', mountPath: '/etc/replication-ca', readOnly: true });
    volumes.push({ name: 'ca-cert', secret: { secretName: input.caSecret } });
  }
  const labels = { [LABEL_RECEIVER_CHECK]: 'true' };
  return {
    apiVersion: 'batch/v1',
    kind: 'Job',
    metadata: { name: input.name, namespace: input.namespace, labels },
    spec: {
      backoffLimit: 0,
      activeDeadlineSeconds: 60,
      ttlSecondsAfterFinished: 600,
      template: {
        metadata: { labels },
        spec: {
          restartPolicy: 'Never',
          securityContext: { runAsUser: 0 },
          containers: [
            {
              name: 'check',
              image: input.image,
              args,
              volumeMounts,
              resources: {
                requests: { cpu: '50m', memory: '64Mi' },
                limits: { cpu: '500m', memory: '128Mi' },
              },
            },
          ],
          volumes,
        },
      },
    },
  };
}

export function buildReceiverSecret(input: { name: string; key: string; value: string }): ReceiverSecret {
  return {
    apiVersion: 'v1',
    kind: 'Secret',
    type: 'Opaque',
    metadata: { name: input.name, namespace: RECEIVER_NAMESPACE, labels: { [LABEL_MADE_BY_CONSOLE]: 'true' } },
    stringData: { [input.key]: input.value },
  };
}

export function validateReceiverAddress(address: string): string | undefined {
  if (!address.trim()) return 'Enter the address of the receiver.';
  if (!address.startsWith('https://')) return 'Use an https:// address. The token is sent to it.';
  try {
    new URL(address);
  } catch {
    return 'This is not a valid address.';
  }
  return undefined;
}

/** An empty box is fine: the receiver's certificate then comes from a public authority. */
export function validateReceiverCA(pem: string): string | undefined {
  if (!pem.trim()) return undefined;
  if (!pem.includes('-----BEGIN CERTIFICATE-----') || !pem.includes('-----END CERTIFICATE-----')) {
    return 'Paste a certificate in PEM form. It starts with -----BEGIN CERTIFICATE-----.';
  }
  return undefined;
}

/** The parts of the check Job's Pod the console reads. */
export interface CheckPodLike extends K8sResourceCommon {
  status?: {
    phase?: string;
    containerStatuses?: Array<{ state?: { terminated?: { exitCode?: number; message?: string } } }>;
  };
}

export type ReceiverCheckResult =
  | { state: 'starting' }
  | { state: 'running' }
  | { state: 'ok' }
  | { state: 'failed'; reason: string };

/** What the form shows, from the check Pod. The result line is in the Pod's termination message. */
export function receiverCheckResult(pod: CheckPodLike | undefined): ReceiverCheckResult {
  if (!pod) return { state: 'starting' };
  if (pod.status?.phase === 'Pending' || pod.status?.phase === 'Running') return { state: 'running' };
  const message = pod.status?.containerStatuses?.[0]?.state?.terminated?.message?.trim();
  if (message === 'ok') return { state: 'ok' };
  return { state: 'failed', reason: message || 'The check ended without a result. It may have run out of time.' };
}
