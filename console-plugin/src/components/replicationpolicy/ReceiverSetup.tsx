import React, { useEffect, useRef, useState } from 'react';
import {
  K8sResourceCommon,
  k8sCreate,
  k8sPatch,
  useAccessReview,
  useK8sWatchResource,
} from '@openshift-console/dynamic-plugin-sdk';
import {
  Alert,
  Button,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  Stack,
  StackItem,
  TextArea,
  TextInput,
} from '@patternfly/react-core';
import { DeploymentModel, JobModel, PodModel, SecretModel } from '../../models';
import { errorMessage } from '../../utils/k8s-objects';
import {
  CheckPodLike,
  LABEL_MADE_BY_CONSOLE,
  RECEIVER_NAMESPACE,
  buildCheckJob,
  buildReceiverSecret,
  receiverCheckResult,
  receiverNames,
  validateReceiverAddress,
  validateReceiverCA,
} from '../../utils/receiver-objects';
import FieldPopover from '../common/FieldPopover';

interface DeploymentLike extends K8sResourceCommon {
  spec?: { template?: { spec?: { containers?: Array<{ name?: string; image?: string }> } } };
}

interface Props {
  policyName: string;
  endpoint: string;
  onEndpointChange: (value: string) => void;
  authSecretRef: string;
  onAuthSecretRefChange: (value: string) => void;
  caCertSecretRef: string;
  onCaCertSecretRefChange: (value: string) => void;
  /** True when the receiver passed the check (or the check cannot run for this user), false otherwise. */
  onCheckedChange: (checked: boolean) => void;
}

const gvk = (model: { apiGroup?: string; apiVersion: string; kind: string }) => ({
  group: model.apiGroup,
  version: model.apiVersion,
  kind: model.kind,
});

const errorCode = (err: unknown): number | undefined => {
  const e = err as { code?: number; response?: { status?: number }; json?: { code?: number } };
  return e?.code ?? e?.response?.status ?? e?.json?.code;
};

/**
 * Makes a Secret, or replaces the value of one this form made earlier. It never reads a Secret back, and it
 * refuses to change a Secret that the console did not make.
 */
async function putSecret(name: string, key: string, value: string): Promise<void> {
  try {
    await k8sPatch({
      model: SecretModel,
      resource: { metadata: { name, namespace: RECEIVER_NAMESPACE } },
      data: [
        { op: 'test', path: `/metadata/labels/${LABEL_MADE_BY_CONSOLE.replace('/', '~1')}`, value: 'true' },
        { op: 'add', path: '/stringData', value: { [key]: value } },
      ],
    });
  } catch (err) {
    const code = errorCode(err);
    if (code === 404) {
      await k8sCreate({ model: SecretModel, data: buildReceiverSecret({ name, key, value }) });
    } else if (code === 422) {
      throw new Error(
        `A Secret named ${name} already exists in ${RECEIVER_NAMESPACE} and was not made by the console. Rename the policy, or choose "Use Secrets that already exist".`,
      );
    } else {
      throw err;
    }
  }
}

/** The receiver part of the replication form: the address, the token and the certificate, and a check of all three. */
const ReceiverSetup: React.FC<Props> = ({
  policyName,
  endpoint,
  onEndpointChange,
  authSecretRef,
  onAuthSecretRefChange,
  caCertSecretRef,
  onCaCertSecretRefChange,
  onCheckedChange,
}) => {
  const [mode, setMode] = useState<'new' | 'existing'>('new');
  const [token, setToken] = useState('');
  const [caPEM, setCaPEM] = useState('');
  const [jobName, setJobName] = useState('');
  const [starting, setStarting] = useState(false);
  const [error, setError] = useState('');

  const [canMakeSecrets, secretsLoading] = useAccessReview({
    group: '',
    resource: 'secrets',
    verb: 'create',
    namespace: RECEIVER_NAMESPACE,
  });
  const [canMakeJobs, jobsLoading] = useAccessReview({
    group: 'batch',
    resource: 'jobs',
    verb: 'create',
    namespace: RECEIVER_NAMESPACE,
  });
  const cannotCheck = !jobsLoading && !canMakeJobs;
  const cannotMakeSecrets = mode === 'new' && !secretsLoading && !canMakeSecrets;

  const [deployments] = useK8sWatchResource<DeploymentLike[]>({
    groupVersionKind: gvk(DeploymentModel),
    namespace: RECEIVER_NAMESPACE,
    selector: { matchLabels: { 'app.kubernetes.io/component': 'controller' } },
    isList: true,
  });
  const [pods] = useK8sWatchResource<CheckPodLike[]>(
    jobName
      ? {
          groupVersionKind: gvk(PodModel),
          namespace: RECEIVER_NAMESPACE,
          selector: { matchLabels: { 'job-name': jobName } },
          isList: true,
        }
      : null,
  );
  const result = jobName ? receiverCheckResult((pods || [])[0]) : undefined;

  useEffect(() => {
    if (cannotCheck) onCheckedChange(true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [cannotCheck]);

  useEffect(() => {
    if (result?.state === 'ok') onCheckedChange(true);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [result?.state]);

  // Any change to what was checked turns the check off.
  const changed = () => {
    setJobName('');
    setError('');
    onCheckedChange(cannotCheck);
  };

  // The Secret and Job names come from the policy name, so a new name turns a passed check off.
  const firstRender = useRef(true);
  useEffect(() => {
    if (firstRender.current) {
      firstRender.current = false;
      return;
    }
    changed();
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [policyName]);

  const addressError = endpoint ? validateReceiverAddress(endpoint) : undefined;
  const caError = mode === 'new' ? validateReceiverCA(caPEM) : undefined;
  const names = receiverNames(policyName);
  const inputsReady =
    !!policyName &&
    !!endpoint &&
    !addressError &&
    (mode === 'new' ? !!token && !caError : !!authSecretRef);
  const checking = starting || result?.state === 'starting' || result?.state === 'running';

  const runCheck = async () => {
    setError('');
    setJobName('');
    setStarting(true);
    try {
      const containers = deployments?.[0]?.spec?.template?.spec?.containers || [];
      const image = (containers.find((c) => c.name === 'csi-controller') || containers[0])?.image;
      if (!image) {
        throw new Error(`Cannot find the driver controller in ${RECEIVER_NAMESPACE}, so the check cannot start.`);
      }

      let authSecret = authSecretRef;
      let caSecret = caCertSecretRef;
      if (mode === 'new') {
        authSecret = names.tokenSecret;
        await putSecret(authSecret, 'token', token);
        caSecret = caPEM.trim() ? names.caSecret : '';
        if (caSecret) await putSecret(caSecret, 'ca.crt', caPEM.trim());
        onAuthSecretRefChange(authSecret);
        onCaCertSecretRefChange(caSecret);
      }

      const name = `${names.job.slice(0, 50)}-${Math.random().toString(36).slice(2, 7)}`;
      await k8sCreate({
        model: JobModel,
        data: buildCheckJob({ name, namespace: RECEIVER_NAMESPACE, image, endpoint, authSecret, caSecret }),
      });
      setJobName(name);
    } catch (err: unknown) {
      setError(errorMessage(err, 'The check could not start'));
    } finally {
      setStarting(false);
    }
  };

  return (
    <Stack hasGutter>
      <StackItem>
        <FieldPopover
          fieldId="rp-dest-endpoint"
          label="Receiver Endpoint"
          description="HTTPS URL of the replication receiver service on the destination cluster. This is typically an OpenShift Route URL."
          isRequired
        >
          <TextInput
            id="rp-dest-endpoint"
            value={endpoint}
            onChange={(_event, value) => {
              onEndpointChange(value);
              changed();
            }}
            isRequired
            validated={addressError ? 'error' : 'default'}
            placeholder="https://repl-receiver.apps.cluster.example.com"
          />
          <FormHelperText>
            <HelperText>
              <HelperTextItem variant={addressError ? 'error' : 'default'}>
                {addressError || 'HTTPS URL of the receiver on the destination cluster'}
              </HelperTextItem>
            </HelperText>
          </FormHelperText>
        </FieldPopover>
      </StackItem>

      <StackItem>
        <FieldPopover
          fieldId="rp-receiver-secrets"
          label="Token and certificate"
          description="Make new Secrets from what you paste, or use Secrets that are already in kube-system. The Secrets must hold the keys token and ca.crt."
        >
          <FormSelect
            id="rp-receiver-secrets"
            value={mode}
            onChange={(_event, value) => {
              setMode(value as 'new' | 'existing');
              changed();
            }}
          >
            <FormSelectOption value="new" label="Make new Secrets from what I paste" />
            <FormSelectOption value="existing" label="Use Secrets that already exist" />
          </FormSelect>
        </FieldPopover>
      </StackItem>

      {mode === 'new' ? (
        <>
          <StackItem>
            <FieldPopover
              fieldId="rp-receiver-token"
              label="Token"
              description="The bearer token of the receiver. It is in the receiver's Secret on the destination cluster. The console sends it once, to this cluster's API server, and keeps no copy."
              isRequired
            >
              <TextInput
                id="rp-receiver-token"
                type="password"
                value={token}
                onChange={(_event, value) => {
                  setToken(value);
                  changed();
                }}
                isRequired
                autoComplete="off"
              />
            </FieldPopover>
          </StackItem>
          <StackItem>
            <FieldPopover
              fieldId="rp-receiver-ca"
              label="CA certificate (optional)"
              description="The certificate of the authority that signed the receiver's certificate, in PEM form. Leave it empty when the certificate comes from a public authority."
            >
              <TextArea
                id="rp-receiver-ca"
                value={caPEM}
                onChange={(_event, value) => {
                  setCaPEM(value);
                  changed();
                }}
                validated={caError ? 'error' : 'default'}
                rows={4}
                placeholder="-----BEGIN CERTIFICATE-----"
              />
              {caError && (
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem variant="error">{caError}</HelperTextItem>
                  </HelperText>
                </FormHelperText>
              )}
            </FieldPopover>
          </StackItem>
        </>
      ) : (
        <>
          <StackItem>
            <FieldPopover
              fieldId="rp-dest-auth-secret"
              label="Auth Secret Name"
              description="Name of a Secret in kube-system containing a 'token' key with the bearer token for authenticating to the receiver."
              isRequired
            >
              <TextInput
                id="rp-dest-auth-secret"
                value={authSecretRef}
                onChange={(_event, value) => {
                  onAuthSecretRefChange(value);
                  changed();
                }}
                isRequired
                placeholder="replication-auth-dr"
              />
            </FieldPopover>
          </StackItem>
          <StackItem>
            <FieldPopover
              fieldId="rp-dest-ca-secret"
              label="CA Cert Secret Name (optional)"
              description="Name of a Secret in kube-system containing a 'ca.crt' key with the CA certificate that verifies the receiver's TLS certificate."
            >
              <TextInput
                id="rp-dest-ca-secret"
                value={caCertSecretRef}
                onChange={(_event, value) => {
                  onCaCertSecretRefChange(value);
                  changed();
                }}
                placeholder="repl-receiver-ca"
              />
            </FieldPopover>
          </StackItem>
        </>
      )}

      {cannotMakeSecrets && (
        <StackItem>
          <Alert variant="warning" title="You cannot make Secrets in kube-system" isInline>
            Ask an administrator to make them, then choose &quot;Use Secrets that already exist&quot;.
          </Alert>
        </StackItem>
      )}

      {cannotCheck ? (
        <StackItem>
          <Alert variant="warning" title="You cannot run the check" isInline>
            You may not make Jobs in kube-system. The policy can be made without the check, so a wrong address, token or
            certificate shows up only when the first copy runs.
          </Alert>
        </StackItem>
      ) : (
        <>
          <StackItem>
            <Button variant="secondary" onClick={runCheck} isDisabled={!inputsReady || checking || cannotMakeSecrets} isLoading={checking}>
              Check connection
            </Button>
            {!policyName && <HelperText><HelperTextItem>Enter the policy name first.</HelperTextItem></HelperText>}
          </StackItem>
          {error && (
            <StackItem>
              <Alert variant="danger" title="The check could not start" isInline>
                {error}
              </Alert>
            </StackItem>
          )}
          {result?.state === 'ok' && (
            <StackItem>
              <Alert variant="success" title="The receiver is ready" isInline>
                The receiver answered, its certificate is trusted, and the token is accepted.
              </Alert>
            </StackItem>
          )}
          {result?.state === 'failed' && (
            <StackItem>
              <Alert variant="danger" title="The check failed" isInline>
                {result.reason}
              </Alert>
            </StackItem>
          )}
          {(result?.state === 'starting' || result?.state === 'running') && (
            <StackItem>
              <Alert variant="info" title="Checking the receiver" isInline>
                The check runs in this cluster, the same way a copy reaches the receiver.
              </Alert>
            </StackItem>
          )}
        </>
      )}
    </Stack>
  );
};

export default ReceiverSetup;
