import React, { useEffect, useMemo, useState } from 'react';
import { k8sCreate, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Link, useHistory, useLocation, useParams } from 'react-router-dom';
import {
  ActionGroup,
  Alert,
  Bullseye,
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
  Radio,
  Spinner,
  Stack,
  StackItem,
  TextInput,
  Title,
} from '@patternfly/react-core';
import { BackupPolicyModel, BackupRestoreModel, PersistentVolumeClaimModel, StorageClassModel } from '../../models';
import { API_GROUP, ROUTES } from '../../constants';
import { BackupPolicy, BackupVolumeStatus } from '../../types';
import {
  buildBackupRestore,
  localInputToISO,
  momentToLocalInput,
  parseRestoreQuery,
  validateBackupRestore,
} from '../../utils/backup-helpers';
import { CSI_DRIVER_NAME, PersistentVolumeClaim, StorageClass, errorMessage } from '../../utils/k8s-objects';
import { toYAML } from '../../utils/yaml-lite';
import FieldPopover from '../common/FieldPopover';
import NoAccessNote from '../common/NoAccessNote';
import BackupRestoreBanner from './BackupRestoreBanner';

const gvk = (model: { apiGroup?: string; apiVersion: string; kind: string }) => ({
  group: model.apiGroup,
  version: model.apiVersion,
  kind: model.kind,
});

const claimOf = (v: BackupVolumeStatus) => `${v.pvcNamespace}/${v.pvcName}`;

/**
 * Restores a backup of a volume to a new claim. The console makes one BackupRestore object, as
 * `kubectl backup restore` does; the driver makes the claim and copies the backup into it.
 */
const BackupRestorePage: React.FC = () => {
  const { name } = useParams<{ name: string }>();
  const history = useHistory();
  const location = useLocation();
  const query = useMemo(() => parseRestoreQuery(location.search), [location.search]);

  const [policy, loaded, loadError] = useK8sWatchResource<BackupPolicy>({ groupVersionKind: gvk(BackupPolicyModel), name });
  const [storageClasses] = useK8sWatchResource<StorageClass[]>({ groupVersionKind: gvk(StorageClassModel), isList: true });
  const [canCreate] = useAccessReview({ group: API_GROUP, resource: 'backuprestores', verb: 'create' });

  const volumes = useMemo(() => policy?.status?.volumes || [], [policy]);
  const [claim, setClaim] = useState('');
  const [useMoment, setUseMoment] = useState(!!query.moment);
  const [momentText, setMomentText] = useState(momentToLocalInput(query.moment));
  const [targetNamespace, setTargetNamespace] = useState('');
  const [targetName, setTargetName] = useState('');
  const [nameTouched, setNameTouched] = useState(false);
  const [storageClass, setStorageClass] = useState('');
  const [confirming, setConfirming] = useState(false);
  const [creating, setCreating] = useState(false);
  const [error, setError] = useState('');
  const [created, setCreated] = useState<{ name: string; namespace: string; target: string } | undefined>();

  const volume = volumes.find((v) => claimOf(v) === claim);

  // Choose the volume of the link, or the first one, once the policy is loaded.
  useEffect(() => {
    if (claim || volumes.length === 0) return;
    const wanted = volumes.find((v) => claimOf(v) === query.claim) || volumes[0];
    setClaim(claimOf(wanted));
  }, [claim, volumes, query.claim]);

  // The target namespace and name follow the volume until the user types a name.
  useEffect(() => {
    if (!volume) return;
    setTargetNamespace(volume.pvcNamespace);
    if (!nameTouched) setTargetName(`${volume.pvcName}-restored`);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [claim]);

  const namespaceOk = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/.test(targetNamespace);
  const [claims] = useK8sWatchResource<PersistentVolumeClaim[]>(
    namespaceOk ? { groupVersionKind: gvk(PersistentVolumeClaimModel), namespace: targetNamespace, isList: true } : null,
  );

  if (loadError) {
    return (
      <PageSection>
        <Alert variant="danger" title="Error loading BackupPolicy" isInline>
          {loadError.message || 'An unexpected error occurred.'}
        </Alert>
      </PageSection>
    );
  }
  if (!loaded) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }
  if (!policy) {
    return (
      <PageSection>
        <Alert variant="warning" title="Not found" isInline>
          BackupPolicy &quot;{name}&quot; was not found.
        </Alert>
      </PageSection>
    );
  }

  const backPath = ROUTES.BACKUP_DETAIL.replace(':name', name);
  const title = (
    <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
      Restore from a backup
    </Title>
  );

  if (volumes.length === 0) {
    return (
      <PageSection variant="default">
        {title}
        <Alert variant="info" title="Nothing to restore" isInline>
          The policy {name} has no backups to restore yet. <Link to={backPath}>Back to the policy</Link>
        </Alert>
      </PageSection>
    );
  }

  const moment = useMoment ? localInputToISO(momentText) : '';
  const errors = validateBackupRestore(
    { targetNamespace, targetName, moment: useMoment ? moment || 'invalid' : '' },
    {
      // The claim this page made is not a name taken by someone else.
      existingNames: (claims || []).map((c) => c.metadata?.name || '').filter((n) => n !== created?.target),
      oldestBackupTime: volume?.oldestBackupTime,
    },
  );
  if (useMoment && !momentText) errors.moment = 'Choose a time, or restore the latest backup.';
  const valid = !!volume && !errors.targetNamespace && !errors.targetName && !errors.moment;
  const driverClasses = (storageClasses || []).filter((sc) => sc.provisioner === CSI_DRIVER_NAME);

  const request = volume
    ? buildBackupRestore(
        {
          policy: name,
          sourceNamespace: volume.pvcNamespace,
          sourceName: volume.pvcName,
          targetNamespace,
          targetName,
          moment,
          storageClass,
        },
        Date.now(),
      )
    : undefined;

  const handleRestore = async () => {
    if (!request) return;
    setError('');
    setCreating(true);
    try {
      await k8sCreate({ model: BackupRestoreModel, data: request });
      setCreated({ name: request.metadata!.name!, namespace: targetNamespace, target: targetName });
    } catch (err: unknown) {
      setError(errorMessage(err, 'Failed to make the restore request'));
    } finally {
      setCreating(false);
      setConfirming(false);
    }
  };

  const range = volume?.oldestBackupTime && volume.lastBackupTime
    ? `Backups from ${volume.oldestBackupTime} to ${volume.lastBackupTime}. The restore uses the newest backup at or before the time you choose.`
    : volume?.lastBackupTime
      ? `The newest backup is from ${volume.lastBackupTime}.`
      : '';
  const defaultClass = volume?.storageClassName ? `Same as the backed-up claim (${volume.storageClassName})` : 'Same as the backed-up claim';
  const locked = !!created;

  return (
    <PageSection variant="default">
      {title}
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
        {volume?.consistency === 'CrashConsistent' && (
          <StackItem>
            <Alert variant="warning" title="This VM disk copy is crash consistent" isInline>
              {volume.vmName ? `The disk belongs to the VM ${volume.vmName}. ` : ''}
              The guest file systems were not frozen when the copy was made, as after a sudden power cut. Most
              applications recover from that, and the guest may need to repair its file system when it starts.
            </Alert>
          </StackItem>
        )}

        <StackItem>
          <FieldPopover
            fieldId="restore-volume"
            label="Volume"
            description="The claim whose backup you want. A claim that is gone is listed too, until its backups pass the keep time."
            isRequired
          >
            <FormSelect id="restore-volume" value={claim} onChange={(_e, v) => setClaim(v)} isDisabled={locked}>
              {volumes.map((v) => (
                <FormSelectOption
                  key={claimOf(v)}
                  value={claimOf(v)}
                  label={`${claimOf(v)}${v.retained ? ' (no longer backed up)' : ''}`}
                />
              ))}
            </FormSelect>
            {range && (
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>{range}</HelperTextItem>
                </HelperText>
              </FormHelperText>
            )}
          </FieldPopover>
        </StackItem>

        <StackItem>
          <Radio
            id="restore-latest"
            name="restore-when"
            label="Latest backup"
            isChecked={!useMoment}
            isDisabled={locked}
            onChange={() => setUseMoment(false)}
          />
          <Radio
            id="restore-at-moment"
            name="restore-when"
            label="At a moment"
            isChecked={useMoment}
            isDisabled={locked}
            onChange={() => setUseMoment(true)}
          />
        </StackItem>

        {useMoment && (
          <StackItem>
            <FieldPopover
              fieldId="restore-moment"
              label="Moment"
              description="Your time zone. The restore uses the newest backup at or before this time, and the page tells you which one it used."
              isRequired
            >
              <TextInput
                id="restore-moment"
                type="datetime-local"
                value={momentText}
                onChange={(_e, v) => setMomentText(v)}
                isDisabled={locked}
                validated={errors.moment ? 'error' : 'default'}
              />
              <FormHelperText>
                <HelperText>
                  {errors.moment && <HelperTextItem variant="error">{errors.moment}</HelperTextItem>}
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>
        )}

        <StackItem>
          <FieldPopover
            fieldId="restore-namespace"
            label="Namespace"
            description="The namespace of the new claim. It is the namespace of the backed-up claim unless you change it."
            isRequired
          >
            <TextInput
              id="restore-namespace"
              value={targetNamespace}
              onChange={(_e, v) => setTargetNamespace(v)}
              isDisabled={locked}
              validated={errors.targetNamespace ? 'error' : 'default'}
            />
            {errors.targetNamespace && (
              <FormHelperText>
                <HelperText>
                  <HelperTextItem variant="error">{errors.targetNamespace}</HelperTextItem>
                </HelperText>
              </FormHelperText>
            )}
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="restore-name"
            label="New claim name"
            description="The name of the new claim. It must not exist: a restore never writes over a volume."
            isRequired
          >
            <TextInput
              id="restore-name"
              value={targetName}
              onChange={(_e, v) => {
                setNameTouched(true);
                setTargetName(v);
              }}
              isDisabled={locked}
              validated={errors.targetName ? 'error' : 'default'}
            />
            {errors.targetName && (
              <FormHelperText>
                <HelperText>
                  <HelperTextItem variant="error">{errors.targetName}</HelperTextItem>
                </HelperText>
              </FormHelperText>
            )}
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="restore-class"
            label="Storage class"
            description="Only storage classes of this driver are offered. The default is the class of the backed-up claim."
          >
            <FormSelect id="restore-class" value={storageClass} onChange={(_e, v) => setStorageClass(v)} isDisabled={locked}>
              <FormSelectOption value="" label={defaultClass} />
              {driverClasses.map((sc) => (
                <FormSelectOption key={sc.metadata?.name} value={sc.metadata?.name} label={sc.metadata?.name || ''} />
              ))}
            </FormSelect>
          </FieldPopover>
        </StackItem>

        {canCreate === false && request && (
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
            <Button variant="link" onClick={() => history.push(backPath)} isDisabled={creating}>
              {created ? 'Back to the policy' : 'Cancel'}
            </Button>
          </ActionGroup>
        </StackItem>
      </Stack>

      <Modal variant={ModalVariant.small} isOpen={confirming} onClose={() => setConfirming(false)}>
        <ModalHeader title="Restore from this backup?" />
        <ModalBody>
          <Stack hasGutter>
            <StackItem>{`A new claim ${targetNamespace}/${targetName} is made from the ${moment ? `backup at or before ${moment}` : 'latest backup'} of ${volume ? claimOf(volume) : ''}.`}</StackItem>
            <StackItem>Nothing is overwritten: the backed-up claim and its data are not changed.</StackItem>
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

export default BackupRestorePage;
