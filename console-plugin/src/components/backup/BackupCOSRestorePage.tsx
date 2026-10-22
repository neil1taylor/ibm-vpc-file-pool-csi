import React, { useEffect, useMemo, useState } from 'react';
import { k8sCreate, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Link, useHistory, useLocation } from 'react-router-dom';
import {
  ActionGroup,
  Alert,
  Button,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  PageSection,
  Stack,
  StackItem,
  TextInput,
  Title,
} from '@patternfly/react-core';
import { BackupPolicyModel, BackupRestoreModel, PersistentVolumeClaimModel, StorageClassModel } from '../../models';
import { API_GROUP, DRIVER_NAMESPACE, ROUTES } from '../../constants';
import { BackupPolicy } from '../../types';
import {
  COSRestoreErrors,
  COSRestoreForm,
  buildCOSRestore,
  parseCOSRestoreQuery,
  restoreFormFromPolicy,
  validateCOSRestore,
} from '../../utils/backup-cos';
import { localInputToISO, momentToLocalInput } from '../../utils/backup-helpers';
import { COSRepo, cosAreaPrefix } from '../../utils/backup-jobs';
import { CSI_DRIVER_NAME, PersistentVolumeClaim, StorageClass, errorMessage } from '../../utils/k8s-objects';
import { toYAML } from '../../utils/yaml-lite';
import FieldPopover from '../common/FieldPopover';
import NoAccessNote from '../common/NoAccessNote';
import BackupRestoreBanner from './BackupRestoreBanner';
import COSContents from './COSContents';

const gvk = (model: { apiGroup?: string; apiVersion: string; kind: string }) => ({
  group: model.apiGroup,
  version: model.apiVersion,
  kind: model.kind,
});

const EMPTY: COSRestoreForm = restoreFormFromPolicy({ metadata: { name: '' }, spec: {} } as BackupPolicy);

// The fields the list Job needs; the others belong to the new claim.
const SOURCE_FIELDS: Array<keyof COSRestoreForm> = [
  'policy',
  'bucket',
  'endpoint',
  'prefix',
  'credentialsSecret',
  'backupKeySecret',
  'kpInstance',
  'kpRegion',
  'kpRootKey',
  'kpApiKeySecret',
  'emergencyKeySecret',
];

/**
 * Restores a volume from a COS bucket to a new claim, in this or any other cluster that runs the driver. It makes
 * one BackupRestore with spec.fromCOS, as `kubectl backup restore --from-cos` does. COS keeps neither the claim
 * name, the storage class nor the size, so the page asks for them.
 */
const BackupCOSRestorePage: React.FC = () => {
  const history = useHistory();
  const location = useLocation();
  const query = useMemo(() => parseCOSRestoreQuery(location.search), [location.search]);

  const [policies] = useK8sWatchResource<BackupPolicy[]>({ groupVersionKind: gvk(BackupPolicyModel), isList: true });
  const [storageClasses] = useK8sWatchResource<StorageClass[]>({ groupVersionKind: gvk(StorageClassModel), isList: true });
  const [canCreate] = useAccessReview({ group: API_GROUP, resource: 'backuprestores', verb: 'create' });

  const [form, setForm] = useState<COSRestoreForm>({
    ...EMPTY,
    policy: query.policy,
    volume: query.volume,
    moment: query.moment,
  });
  const [momentText, setMomentText] = useState(momentToLocalInput(query.moment));
  const [filled, setFilled] = useState(false);
  const [confirming, setConfirming] = useState(false);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState('');
  const [created, setCreated] = useState<{ name: string; namespace: string; target: string } | undefined>();

  const exporting = (policies || []).filter((p) => !!p.spec?.export);

  const fillFrom = (p: BackupPolicy) =>
    setForm((f) => ({
      ...restoreFormFromPolicy(p),
      volume: f.volume,
      moment: f.moment,
      targetNamespace: f.targetNamespace,
      targetName: f.targetName,
      storageClass: f.storageClass,
      sizeGB: f.sizeGB,
    }));

  // A link that names a policy of this cluster fills the settings from its export.
  useEffect(() => {
    if (filled || !policies || !query.policy) return;
    const p = policies.find((x) => x.metadata?.name === query.policy);
    if (p?.spec?.export) fillFrom(p);
    setFilled(true);
  }, [filled, policies, query.policy]);

  const namespaceOk = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(form.targetNamespace);
  const [claims] = useK8sWatchResource<PersistentVolumeClaim[]>(
    namespaceOk ? { groupVersionKind: gvk(PersistentVolumeClaimModel), namespace: form.targetNamespace, isList: true } : null,
  );

  const errors: COSRestoreErrors = validateCOSRestore(
    { ...form, moment: form.moment },
    (claims || []).map((c) => c.metadata?.name || ''),
  );
  const valid = Object.keys(errors).length === 0;
  const sourceOk = SOURCE_FIELDS.every((f) => f === 'emergencyKeySecret' || !errors[f]);
  const repo: COSRepo | undefined = sourceOk
    ? {
        endpoint: form.endpoint,
        bucket: form.bucket,
        prefix: cosAreaPrefix(form.prefix, form.policy),
        credentialsSecret: form.credentialsSecret,
        backupKeySecret: form.backupKeySecret,
        keyProtect: { instanceID: form.kpInstance, region: form.kpRegion, rootKeyID: form.kpRootKey, apiKeySecret: form.kpApiKeySecret },
        emergencyKeySecret: form.emergencyKeySecret,
      }
    : undefined;
  const driverClasses = (storageClasses || []).filter((sc) => sc.provisioner === CSI_DRIVER_NAME);
  const set = <K extends keyof COSRestoreForm>(key: K, value: COSRestoreForm[K]) => setForm((f) => ({ ...f, [key]: value }));
  const locked = !!created;
  const request = buildCOSRestore(form, Date.now());

  const handleRestore = async () => {
    setError('');
    setCreating(true);
    try {
      await k8sCreate({ model: BackupRestoreModel, data: request });
      setCreated({ name: request.metadata!.name!, namespace: form.targetNamespace, target: form.targetName });
    } catch (err: unknown) {
      setError(errorMessage(err, 'Failed to make the restore request'));
    } finally {
      setCreating(false);
      setConfirming(false);
    }
  };

  const text = (id: string, label: string, key: keyof COSRestoreForm, description: string, placeholder?: string) => (
    <StackItem>
      <FieldPopover fieldId={id} label={label} description={description} isRequired={key !== 'prefix' && key !== 'emergencyKeySecret'}>
        <TextInput
          id={id}
          value={String(form[key])}
          onChange={(_e, v) => set(key, v as never)}
          isDisabled={locked}
          placeholder={placeholder}
          validated={errors[key] && form[key] !== '' ? 'error' : 'default'}
        />
        {errors[key] && form[key] !== '' && (
          <FormHelperText>
            <HelperText>
              <HelperTextItem variant="error">{errors[key]}</HelperTextItem>
            </HelperText>
          </FormHelperText>
        )}
      </FieldPopover>
    </StackItem>
  );

  return (
    <PageSection variant="default">
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Restore from COS
      </Title>
      <Stack hasGutter>
        {created && (
          <StackItem>
            <BackupRestoreBanner restoreName={created.name} targetNamespace={created.namespace} targetName={created.target} />
          </StackItem>
        )}
        {error && (
          <StackItem>
            <Alert variant="danger" title="The restore request was refused" isInline>
              {error}
            </Alert>
          </StackItem>
        )}
        <StackItem>
          <Alert variant="info" isInline title="Before you restore">
            The Secrets must already exist in {DRIVER_NAMESPACE}: the COS key, the wrapped backup key and the Key Protect API key.
            The console shows only their names. The console does not remove delete markers in the bucket, because it has no IBM
            Cloud login. If the restore cannot find a backup that you can see in the list, run kubectl backup restore --from-cos,
            which cleans them up first.
          </Alert>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="cos-fill"
            label="Fill from a policy of this cluster"
            description="Copies the bucket, the endpoint, the folder and the Secret names from the export of a policy. For a bucket of another cluster, type them in."
          >
            <FormSelect
              id="cos-fill"
              aria-label="Fill from a policy of this cluster"
              value=""
              onChange={(_e, v) => {
                const p = exporting.find((x) => x.metadata?.name === v);
                if (p) fillFrom(p);
              }}
              isDisabled={locked}
            >
              <FormSelectOption value="" label="Choose a policy" isPlaceholder />
              {exporting.map((p) => (
                <FormSelectOption key={p.metadata?.name} value={p.metadata?.name} label={p.metadata?.name || ''} />
              ))}
            </FormSelect>
          </FieldPopover>
        </StackItem>

        {text('cos-policy', 'Policy', 'policy', 'The name of the policy whose area in the bucket holds the backups. It need not exist in this cluster.')}
        {text('cos-bucket', 'Bucket', 'bucket', 'The COS bucket.')}
        {text('cos-endpoint', 'Endpoint', 'endpoint', 'The S3 host of the bucket, such as s3.eu-de.cloud-object-storage.appdomain.cloud.')}
        {text('cos-prefix', 'Folder (optional)', 'prefix', 'The folder set on the policy export. The area of the policy is below it.')}
        {text('cos-creds', 'COS key Secret', 'credentialsSecret', `The Secret in ${DRIVER_NAMESPACE} with the COS key (access_key_id and secret_access_key).`)}
        {text('cos-keysecret', 'Wrapped backup key Secret', 'backupKeySecret', `The Secret in ${DRIVER_NAMESPACE} with the backup key, wrapped by Key Protect.`)}
        {text('cos-kp-instance', 'Key Protect instance ID', 'kpInstance', 'The ID of the Key Protect instance.')}
        {text('cos-kp-region', 'Key Protect region', 'kpRegion', 'The region of the Key Protect instance, such as eu-de.')}
        {text('cos-kp-root', 'Key Protect root key ID', 'kpRootKey', 'The ID of the root key that wrapped the backup key.')}
        {text('cos-kp-apikey', 'Key Protect API key Secret', 'kpApiKeySecret', `The Secret in ${DRIVER_NAMESPACE} with an API key that may use the root key.`)}
        {text('cos-emergency', 'Emergency key Secret (optional)', 'emergencyKeySecret', 'A Secret with the readable backup key, used only when Key Protect cannot be reached.')}

        <StackItem>
          <COSContents
            repo={repo}
            unavailable={repo ? undefined : 'Fill in the COS settings above first.'}
            onPick={(volume, moment) => {
              setForm((f) => ({ ...f, volume, moment }));
              setMomentText(momentToLocalInput(moment));
            }}
          />
        </StackItem>

        {text('cos-volume', 'Volume', 'volume', 'The volume to restore, as the list shows it: pvc- and a UUID. COS does not keep the claim name.')}

        <StackItem>
          <FieldPopover
            fieldId="cos-moment"
            label="Moment (optional)"
            description="Your time zone. The restore uses the oldest export made at or after this time, or the newest one. Empty means the newest."
          >
            <TextInput
              id="cos-moment"
              type="datetime-local"
              value={momentText}
              onChange={(_e, v) => {
                setMomentText(v);
                set('moment', localInputToISO(v));
              }}
              isDisabled={locked}
            />
          </FieldPopover>
        </StackItem>

        {text('cos-ns', 'Namespace', 'targetNamespace', 'The namespace of the new claim.')}
        {text('cos-name', 'New claim name', 'targetName', 'The name of the new claim. It must not exist: a restore never writes over a volume.')}

        <StackItem>
          <FieldPopover
            fieldId="cos-class"
            label="Storage class"
            description="COS does not keep the storage class or the size, so choose them. Only storage classes of this driver are offered."
            isRequired
          >
            <FormSelect id="cos-class" value={form.storageClass} onChange={(_e, v) => set('storageClass', v)} isDisabled={locked}>
              <FormSelectOption value="" label="Choose a storage class" isPlaceholder />
              {driverClasses.map((sc) => (
                <FormSelectOption key={sc.metadata?.name} value={sc.metadata?.name} label={sc.metadata?.name || ''} />
              ))}
            </FormSelect>
          </FieldPopover>
        </StackItem>
        <StackItem>
          <FieldPopover
            fieldId="cos-size"
            label="Size (GB)"
            description="The size of the new claim. COS does not keep the storage class or the size: make it as big as the original, or bigger."
            isRequired
          >
            <TextInput
              id="cos-size"
              type="number"
              value={String(form.sizeGB)}
              onChange={(_e, v) => set('sizeGB', Number(v))}
              isDisabled={locked}
              validated={errors.sizeGB ? 'error' : 'default'}
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem variant={errors.sizeGB ? 'error' : 'default'}>
                  {errors.sizeGB || 'COS does not keep the storage class or the size.'}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        {canCreate === false && (
          <StackItem>
            <NoAccessNote right="create BackupRestore objects" yaml={toYAML(request)} />
          </StackItem>
        )}

        <StackItem>
          <ActionGroup>
            {!created && (
              <Button variant="primary" onClick={() => setConfirming(true)} isDisabled={!valid || creating || !canCreate}>
                Restore
              </Button>
            )}
            <Button variant="link" onClick={() => history.push(ROUTES.BACKUPS)} isDisabled={creating}>
              {created ? 'Back to Backups' : 'Cancel'}
            </Button>
          </ActionGroup>
          <Link to={ROUTES.BACKUPS}>All backup policies</Link>
        </StackItem>
      </Stack>

      <Modal variant={ModalVariant.small} isOpen={confirming} onClose={() => setConfirming(false)}>
        <ModalHeader title="Restore from COS?" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>{`A new claim ${form.targetNamespace}/${form.targetName} is made from ${form.volume} in bucket ${form.bucket}, ${form.moment ? `the export at or after ${form.moment}` : 'the newest export'}.`}</StackItem>
            <StackItem>Nothing is overwritten: no existing claim is changed.</StackItem>
            <StackItem>The copy takes time, and the new claim cannot be used until it ends.</StackItem>
          </Stack>
        </ModalBody>
        <ModalFooter>
          <Button variant="primary" onClick={handleRestore} isLoading={creating} isDisabled={creating}>
            Restore
          </Button>
          <Button variant="link" onClick={() => setConfirming(false)} isDisabled={creating}>
            Cancel
          </Button>
        </ModalFooter>
      </Modal>
    </PageSection>
  );
};

export default BackupCOSRestorePage;
