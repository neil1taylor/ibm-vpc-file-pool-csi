import React, { useEffect, useRef, useState } from 'react';
import { useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import {
  Alert,
  Button,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Spinner,
  Stack,
  StackItem,
  TextInput,
} from '@patternfly/react-core';
import { BackupRestoreModel, FileSharePoolModel } from '../../models';
import { BackupPolicy, BackupRestore, BackupVolumeStatus, FileSharePool } from '../../types';
import { DRIVER_NAMESPACE } from '../../constants';
import {
  backupFolders,
  buildMeasureBackupObjects,
  buildRemoveBackupObjects,
  shareJobName,
} from '../../utils/backup-jobs';
import {
  claimConfirmation,
  deleteAccessRights,
  deleteRefusal,
  momentsCount,
  parseMeasureMessage,
  resolveShare,
} from '../../utils/backup-delete';
import { errorMessage } from '../../utils/k8s-objects';
import { cleanUpShareObjects, createShareObjects, useShareJob } from '../../utils/use-share-job';
import { humanizeBytes } from '../../utils/resource-helpers';
import NoAccessNote from '../common/NoAccessNote';

interface Props {
  policy: BackupPolicy;
  volume: BackupVolumeStatus;
  onClose: () => void;
}

type Step = 'start' | 'measuring' | 'confirm' | 'empty' | 'removing' | 'done' | 'error';

const gvk = (model: { apiGroup?: string; apiVersion: string; kind: string }) => ({
  group: model.apiGroup,
  version: model.apiVersion,
  kind: model.kind,
});

/**
 * Removes all backups of one volume, as `kubectl backup delete` does: refuse while the volume is still backed up
 * or restored, measure the backup with a Job, ask the operator to type the claim, then remove it with a Job.
 */
const DeleteBackupsDialog: React.FC<Props> = ({ policy, volume, onClose }) => {
  const policyName = policy.metadata?.name || '';
  const claim = claimConfirmation(volume);

  const [restores] = useK8sWatchResource<BackupRestore[]>({ groupVersionKind: gvk(BackupRestoreModel), isList: true });
  const [pool] = useK8sWatchResource<FileSharePool>({ groupVersionKind: gvk(FileSharePoolModel), name: policy.spec.snapshotPool });

  const rights = deleteAccessRights(DRIVER_NAMESPACE);
  const checks = [
    useAccessReview(rights[0]),
    useAccessReview(rights[1]),
    useAccessReview(rights[2]),
    useAccessReview(rights[3]),
    useAccessReview(rights[4]),
    useAccessReview(rights[5]),
    useAccessReview(rights[6]),
  ];
  const rightsLoading = checks.some(([, loading]) => loading);
  const missing = rights.filter((_, i) => !checks[i][0]).map((r) => `${r.verb} ${r.resource}`);

  const refusal = deleteRefusal(policy, volume, restores || []);
  const { share, error: shareError } = resolveShare(pool, volume.shareID || '');
  const folders = volume.subPath ? safeFolders(policyName, volume.subPath) : undefined;
  const volumeName = (volume.subPath || '').replace('/pvcs/', '');

  const measure = folders && share
    ? buildMeasureBackupObjects({ key: `bkmeasure-${volumeName}`, namespace: DRIVER_NAMESPACE, share, backupPath: folders.backup, momentsPath: folders.moments })
    : undefined;
  const remove = folders && share
    ? buildRemoveBackupObjects({ key: `bkdelete-${volumeName}`, namespace: DRIVER_NAMESPACE, share, backupPath: folders.backup, momentsPath: folders.moments })
    : undefined;

  const blocked = !!refusal || !!shareError || !folders || missing.length > 0;
  const [step, setStep] = useState<Step>('start');
  const [size, setSize] = useState(0);
  const [error, setError] = useState('');
  const [typed, setTyped] = useState('');
  const handled = useRef<Step | ''>('');

  const measureOutcome = useShareJob(step === 'measuring' && measure ? shareJobName(`bkmeasure-${volumeName}`) : '');
  const removeOutcome = useShareJob(step === 'removing' && remove ? shareJobName(`bkdelete-${volumeName}`) : '');

  const fail = (message: string) => {
    setError(message);
    setStep('error');
  };

  // Start the measure Job as soon as the checks pass.
  useEffect(() => {
    if (step !== 'start' || blocked || rightsLoading || !measure) return;
    setStep('measuring');
    createShareObjects(measure).catch((err) => fail(errorMessage(err, 'Could not start the measure Job')));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, blocked, rightsLoading]);

  // The measure Job ended: read the size, and remove the Job.
  useEffect(() => {
    if (step !== 'measuring' || !measure || measureOutcome.state === 'running' || handled.current === 'measuring') return;
    handled.current = 'measuring';
    cleanUpShareObjects(measure);
    if (measureOutcome.state === 'failed') {
      fail('Measuring the backups failed. See the log of the Job in the driver namespace.');
      return;
    }
    const bytes = parseMeasureMessage(measureOutcome.message);
    if (bytes === undefined) {
      fail(`The measure Job did not report a size (got "${measureOutcome.message}").`);
      return;
    }
    setSize(bytes);
    setStep(bytes === 0 ? 'empty' : 'confirm');
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, measureOutcome.state]);

  // The delete Job ended: remove the Job, and say how it went.
  useEffect(() => {
    if (step !== 'removing' || !remove || removeOutcome.state === 'running' || handled.current === 'removing') return;
    handled.current = 'removing';
    cleanUpShareObjects(remove);
    if (removeOutcome.state === 'failed') {
      fail('Removing the backups failed. See the log of the Job in the driver namespace. Some files may be removed already; you can try again.');
      return;
    }
    setStep('done');
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, removeOutcome.state]);

  const startRemove = () => {
    if (!remove) return;
    setStep('removing');
    createShareObjects(remove).catch((err) => fail(errorMessage(err, 'Could not start the delete Job')));
  };

  const moments = momentsCount(volume);
  // A refused dialog starts no Job, so it has ended as much as a finished one.
  const ended = blocked || step === 'done' || step === 'empty' || step === 'error';
  const working = step === 'start' || step === 'measuring' || step === 'removing';
  const adminCommand = {
    text: `kubectl backup delete --policy ${policy.metadata?.name} --pvc ${claim}`,
    note: 'It measures first, shows what would go, and asks them to type the claim name before anything is removed.',
  };

  let body: React.ReactNode;
  if (refusal || shareError || !folders) {
    body = (
      <Alert variant="warning" isInline title="The backups of this volume cannot be removed now">
        {refusal || shareError || 'The backup folder of the volume is not known.'}
      </Alert>
    );
  } else if (missing.length > 0 && !rightsLoading) {
    body = (
      <NoAccessNote
        right={`make and remove Jobs, claims and volumes in ${DRIVER_NAMESPACE} (missing: ${missing.join(', ')})`}
        command={adminCommand}
      />
    );
  } else if (step === 'error') {
    body = (
      <Alert variant="danger" isInline title="The delete stopped">
        {error}
      </Alert>
    );
  } else if (step === 'done') {
    body = (
      <Alert variant="success" isInline title="The backups are removed">
        Removed the backups of {claim} ({humanizeBytes(size)}).
      </Alert>
    );
  } else if (step === 'empty') {
    body = (
      <Alert variant="info" isInline title="No backup data found">
        Nothing to remove: {folders.backup} and {folders.moments} are empty or do not exist on the share of pool{' '}
        {policy.spec.snapshotPool}.
      </Alert>
    );
  } else if (step === 'confirm') {
    body = (
      <Stack hasGutter>
        <StackItem>
          <Alert variant="danger" isInline title={`This removes all backups of ${claim} from policy ${policyName}, for good`}>
            <div>
              <code>{folders.backup}</code>
            </div>
            <div>
              <code>{folders.moments}</code>
            </div>
            <div>
              {humanizeBytes(size)}, {moments} {moments === 1 ? 'moment' : 'moments'}, on the share of pool {policy.spec.snapshotPool}.
              The backups cannot be recovered. The volume itself is not touched.
            </div>
          </Alert>
        </StackItem>
        <StackItem>
          <TextInput
            aria-label="Type the claim to confirm"
            value={typed}
            onChange={(_e, v) => setTyped(v)}
            placeholder={`Type ${claim} to confirm`}
          />
        </StackItem>
      </Stack>
    );
  } else {
    body = (
      <Stack hasGutter>
        <StackItem>
          <Spinner size="md" />{' '}
          {step === 'removing' ? 'Removing the backups on the snapshot pool...' : 'Measuring the backups on the snapshot pool...'}
        </StackItem>
        <StackItem>This runs a short Job in {DRIVER_NAMESPACE}. Nothing is removed until you confirm.</StackItem>
      </Stack>
    );
  }

  return (
    <Modal variant={ModalVariant.medium} isOpen onClose={onClose}>
      <ModalHeader title={`Delete backups of ${claim}`} />
      <ModalBody>{body}</ModalBody>
      <ModalFooter>
        {step === 'confirm' && (
          <Button variant="danger" onClick={startRemove} isDisabled={typed !== claim}>
            Delete backups
          </Button>
        )}
        <Button variant={ended ? 'primary' : 'link'} onClick={onClose}>
          {ended ? 'Close' : working ? 'Close (the Job goes on)' : 'Cancel'}
        </Button>
      </ModalFooter>
    </Modal>
  );
};

function safeFolders(policy: string, subPath: string) {
  try {
    return backupFolders(policy, subPath);
  } catch {
    return undefined;
  }
}

export default DeleteBackupsDialog;
