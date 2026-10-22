import React, { useMemo, useState } from 'react';
import { k8sCreate, k8sPatch, useAccessReview } from '@openshift-console/dynamic-plugin-sdk';
import {
  Alert,
  Button,
  Card,
  CardBody,
  CardTitle,
  List,
  ListItem,
  Stack,
  StackItem,
  TextArea,
  TextInput,
} from '@patternfly/react-core';
import { BackupPolicyModel, SecretModel } from '../../models';
import { API_GROUP, DRIVER_NAMESPACE } from '../../constants';
import { BackupPolicy } from '../../types';
import {
  COS_SETUP_FLAGS,
  ParsedSecret,
  apiKeySecret,
  buildCosSetupCommand,
  buildSecret,
  exportHealth,
  exportPatch,
  parseExportBlock,
  parseSecretManifests,
  validateExport,
} from '../../utils/backup-setup';
import { errorMessage, isAlreadyExistsError } from '../../utils/k8s-objects';
import { toYAML } from '../../utils/yaml-lite';
import FieldPopover from '../common/FieldPopover';
import NoAccessNote from '../common/NoAccessNote';
import YAMLEditorFallback from '../common/YAMLEditorFallback';

const HEALTH_VARIANT = { off: 'info', waiting: 'info', working: 'success', failing: 'danger' } as const;

/**
 * Sets up the COS export as far as a browser can. The console does not run `cos-setup` (it needs the person's own
 * IBM Cloud login, and the console must not hold an IBM Cloud API key). It shows the command with the person's names
 * filled in, then makes the Secrets from the manifests the command wrote, writes the printed spec.export block into
 * the policy, and shows whether the export works. It never reads a Secret back.
 */
const ExportSetupGuide: React.FC<{ policy: BackupPolicy }> = ({ policy }) => {
  const policyName = policy.metadata?.name || '';
  const [names, setNames] = useState({
    region: '',
    resourceGroup: '',
    bucket: '',
    rootKey: '',
    vpcCRN: '',
    adminAddresses: '',
    prefix: '',
  });
  const [manifests, setManifests] = useState('');
  const [kpKey, setKpKey] = useState('');
  const [secretResults, setSecretResults] = useState<string[]>([]);
  const [secretError, setSecretError] = useState('');
  const [makingSecrets, setMakingSecrets] = useState(false);
  const [block, setBlock] = useState('');
  const [saving, setSaving] = useState(false);
  const [saved, setSaved] = useState(false);
  const [saveError, setSaveError] = useState('');

  const [canMakeSecrets] = useAccessReview({ group: '', resource: 'secrets', verb: 'create', namespace: DRIVER_NAMESPACE });
  const [canUpdate] = useAccessReview({ group: API_GROUP, resource: 'backuppolicies', verb: 'update' });

  const command = buildCosSetupCommand({ ...names, keepFor: '30d', lock: 'Compliance' });
  const parsed = useMemo(() => parseSecretManifests(manifests), [manifests]);
  const secretsToMake: ParsedSecret[] = [
    ...parsed.secrets,
    ...(kpKey.trim() ? [{ name: 'keyprotect-apikey', namespace: DRIVER_NAMESPACE, data: { apikey: kpKey.trim() } }] : []),
  ];

  const parsedBlock = useMemo(() => (block.trim() ? parseExportBlock(block) : undefined), [block]);
  const blockErrors = parsedBlock ? (parsedBlock.export ? validateExport(parsedBlock.export) : parsedBlock.errors) : [];
  const health = exportHealth(policy);

  const makeSecrets = async () => {
    setSecretError('');
    setSecretResults([]);
    setMakingSecrets(true);
    const results: string[] = [];
    try {
      for (const s of secretsToMake) {
        const data = s.name === 'keyprotect-apikey' && s.data.apikey === kpKey.trim() ? apiKeySecret(s.name, kpKey) : buildSecret(s);
        try {
          await k8sCreate({ model: SecretModel, data });
          results.push(`Secret ${s.name}: made`);
        } catch (err) {
          if (!isAlreadyExistsError(err)) throw err;
          results.push(`Secret ${s.name}: exists already, left as it is`);
        }
      }
      setManifests('');
      setKpKey('');
    } catch (err) {
      setSecretError(errorMessage(err, 'Could not make a Secret'));
    } finally {
      setSecretResults(results);
      setMakingSecrets(false);
    }
  };

  const saveExport = async () => {
    if (!parsedBlock?.export) return;
    setSaveError('');
    setSaved(false);
    setSaving(true);
    try {
      await k8sPatch({ model: BackupPolicyModel, resource: policy, data: exportPatch(parsedBlock.export) });
      setSaved(true);
    } catch (err) {
      setSaveError(errorMessage(err, 'Could not save the export'));
    } finally {
      setSaving(false);
    }
  };

  const nameField = (id: string, label: string, key: keyof typeof names, description: string, placeholder?: string) => (
    <StackItem key={id}>
      <FieldPopover fieldId={id} label={label} description={description}>
        <TextInput id={id} value={names[key]} placeholder={placeholder} onChange={(_e, v) => setNames({ ...names, [key]: v })} />
      </FieldPopover>
    </StackItem>
  );

  return (
    <Card>
      <CardTitle>Set up the export</CardTitle>
      <CardBody>
        <Stack hasGutter>
          <StackItem>
            <Alert variant="info" isInline title="The console does not run cos-setup">
              Making the bucket, the keys, the Key Protect wrap and the access rule needs your own IBM Cloud login. A browser has
              none, and the console must not hold an IBM Cloud API key. So you run one command on your machine, then paste what it
              wrote here, and the console does the rest.
            </Alert>
          </StackItem>

          <StackItem>
            <strong>Step 1. Run the command.</strong> Type your names; the command updates as you type. Run it with --dry-run
            first.
          </StackItem>
          {nameField('cos-region', 'Region', 'region', 'The region of the bucket and of Key Protect, such as eu-de.', 'eu-de')}
          {nameField('cos-rg', 'Resource group', 'resourceGroup', 'The IBM Cloud resource group for the COS instance.')}
          {nameField('cos-bucket', 'Bucket', 'bucket', 'The name of the bucket to make.')}
          {nameField('cos-rootkey', 'Key Protect root key CRN', 'rootKey', 'The CRN of an existing Key Protect root key. It wraps the backup key.')}
          {nameField('cos-vpc', 'VPC CRN', 'vpcCRN', 'The CRN of the cluster VPC, for the access rule.')}
          {nameField('cos-admins', 'Admin addresses', 'adminAddresses', 'Your public address and other admin addresses, separated by commas. Yours must be in the list.')}
          {nameField('cos-prefix', 'Folder in the bucket (optional)', 'prefix', 'A folder. The area of each policy is below it.')}
          <StackItem>
            <YAMLEditorFallback yaml={command} />
          </StackItem>
          <StackItem>
            <List isPlain>
              {COS_SETUP_FLAGS.map((f) => (
                <ListItem key={f.flag}>
                  <code>{f.flag}</code>: {f.text}
                </ListItem>
              ))}
            </List>
          </StackItem>

          <StackItem>
            <strong>Step 2. Make the Secrets.</strong> Paste the contents of the two files the command wrote (the COS key and the
            wrapped backup key), and the Key Protect API key. The Secrets are made in {DRIVER_NAMESPACE}. The console sends them once and
            never reads a Secret back. Delete the files afterwards.
          </StackItem>
          <StackItem>
            <FieldPopover
              fieldId="cos-manifests"
              label="Secret manifests from cos-setup"
              description={`The contents of cos-key.yaml and cos-wrapped-key.yaml. Only Secrets in ${DRIVER_NAMESPACE} with the keys the export uses are accepted. The readable backup key is refused.`}
            >
              <TextArea id="cos-manifests" value={manifests} onChange={(_e, v) => setManifests(v)} rows={8} autoComplete="off" />
            </FieldPopover>
            {parsed.errors.map((e) => (
              <Alert key={e} variant="danger" isInline title={e} style={{ marginTop: 8 }} />
            ))}
          </StackItem>
          <StackItem>
            <FieldPopover
              fieldId="cos-kpkey"
              label="Key Protect API key"
              description="An IBM Cloud API key with the Reader role on the root key. cos-setup does not make it. Leave it empty if the Secret keyprotect-apikey exists already."
            >
              <TextInput id="cos-kpkey" type="password" value={kpKey} onChange={(_e, v) => setKpKey(v)} autoComplete="off" />
            </FieldPopover>
          </StackItem>
          {canMakeSecrets === false && (
            <StackItem>
              <NoAccessNote right={`create Secrets in ${DRIVER_NAMESPACE}`} />
            </StackItem>
          )}
          <StackItem>
            <Button
              variant="secondary"
              onClick={makeSecrets}
              isDisabled={!canMakeSecrets || makingSecrets || secretsToMake.length === 0 || parsed.errors.length > 0}
              isLoading={makingSecrets}
            >
              Make the Secrets
            </Button>
            {secretResults.map((r) => (
              <div key={r}>{r}</div>
            ))}
            {secretError && (
              <Alert variant="danger" isInline title="A Secret could not be made" style={{ marginTop: 8 }}>
                {secretError}
              </Alert>
            )}
          </StackItem>

          <StackItem>
            <strong>Step 3. Save the export to the policy.</strong> Paste the spec.export block that the command printed.
          </StackItem>
          <StackItem>
            <FieldPopover
              fieldId="cos-block"
              label="spec.export block from cos-setup"
              description="The block that starts with spec: and export:. The console checks it with the same rules as the admission webhook, then writes it into this policy."
            >
              <TextArea id="cos-block" value={block} onChange={(_e, v) => setBlock(v)} rows={10} />
            </FieldPopover>
            {blockErrors.map((e) => (
              <Alert key={e} variant="danger" isInline title={e} style={{ marginTop: 8 }} />
            ))}
          </StackItem>
          {canUpdate === false && (
            <StackItem>
              <NoAccessNote
                right="update BackupPolicy objects"
                yaml={parsedBlock?.export ? toYAML({ spec: { export: parsedBlock.export } }) : undefined}
              />
            </StackItem>
          )}
          <StackItem>
            <Button
              variant="primary"
              onClick={saveExport}
              isDisabled={!canUpdate || saving || !parsedBlock?.export || blockErrors.length > 0}
              isLoading={saving}
            >
              Save the export to the policy
            </Button>
            {saved && (
              <Alert variant="success" isInline title="The export is saved" style={{ marginTop: 8 }}>
                Policy {policyName} now exports to COS. The first export runs after the next local backup.
              </Alert>
            )}
            {saveError && (
              <Alert variant="danger" isInline title="The policy was not changed" style={{ marginTop: 8 }}>
                {saveError}
              </Alert>
            )}
          </StackItem>

          <StackItem>
            <strong>Step 4. Check that it works.</strong>
            <Alert variant={HEALTH_VARIANT[health.state]} isInline title={health.text} style={{ marginTop: 8 }} />
          </StackItem>
        </Stack>
      </CardBody>
    </Card>
  );
};

export default ExportSetupGuide;
