import React, { useState } from 'react';
import {
  Alert,
  Button,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Stack,
  StackItem,
} from '@patternfly/react-core';
import { k8sPatch } from '@openshift-console/dynamic-plugin-sdk';
import { FileSharePoolModel } from '../../models';
import { FileSharePool, PoolShareStatus } from '../../types';
import { DrainAction, drainConsequences, drainListAfter } from '../../utils/pool-helpers';
import { errorMessage } from '../../utils/k8s-objects';

interface Props {
  isOpen: boolean;
  pool: FileSharePool;
  share: PoolShareStatus;
  action: DrainAction;
  onClose: () => void;
  onDone: () => void;
}

/** Confirms draining a share, or returning it to service, and updates the pool's drain list. */
const ShareDrainModal: React.FC<Props> = ({ isOpen, pool, share, action, onClose, onDone }) => {
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const drain = action === 'drain';

  const confirm = async () => {
    setBusy(true);
    setError('');
    try {
      await k8sPatch({
        model: FileSharePoolModel,
        resource: pool,
        data: [
          {
            op: 'add',
            path: '/spec/drainShares',
            value: drainListAfter(pool.spec?.drainShares, share.shareID, action),
          },
        ],
      });
      onDone();
    } catch (err) {
      setError(errorMessage(err, 'The update failed'));
    } finally {
      setBusy(false);
    }
  };

  return (
    <Modal variant={ModalVariant.small} isOpen={isOpen} onClose={onClose}>
      <ModalHeader title={drain ? `Drain share ${share.shareName}?` : `Return share ${share.shareName} to service?`} />
      <ModalBody>
        <Stack hasGutter>
          {drainConsequences(share, action).map((line) => (
            <StackItem key={line}>{line}</StackItem>
          ))}
          {error && (
            <StackItem>
              <Alert variant="danger" isInline title="Could not update the pool">
                {error}
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        <Button variant={drain ? 'danger' : 'primary'} onClick={confirm} isLoading={busy} isDisabled={busy}>
          {drain ? 'Drain share' : 'Return to service'}
        </Button>
        <Button variant="link" onClick={onClose} isDisabled={busy}>
          Cancel
        </Button>
      </ModalFooter>
    </Modal>
  );
};

export default ShareDrainModal;
