import React, { useState } from 'react';
import {
  Modal,
  ModalVariant,
  ModalHeader,
  ModalBody,
  ModalFooter,
  Button,
  TextInput,
  Alert,
  Checkbox,
  Stack,
  StackItem,
} from '@patternfly/react-core';
import { k8sDelete, K8sModel } from '@openshift-console/dynamic-plugin-sdk';

interface DeleteModalProps {
  isOpen: boolean;
  resourceName: string;
  resourceKind: string;
  model: K8sModel;
  onClose: () => void;
  onDeleted: () => void;
  requireConfirmation?: boolean;
  /** An extra step the user may tick. It runs after the delete, and it is off by default. */
  extraOption?: { label: string; run: () => Promise<void> };
  /** A note under the question, for what the delete does not remove. */
  note?: React.ReactNode;
}

const DeleteModal: React.FC<DeleteModalProps> = ({
  isOpen,
  resourceName,
  resourceKind,
  model,
  onClose,
  onDeleted,
  requireConfirmation = false,
  extraOption,
  note,
}) => {
  const [confirmation, setConfirmation] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [extraTicked, setExtraTicked] = useState(false);
  // The delete worked but the extra step did not: the dialog stays to say so, and Continue carries on.
  const [deletedWithProblem, setDeletedWithProblem] = useState(false);

  const canDelete = !requireConfirmation || confirmation === resourceName;

  const handleDelete = async () => {
    setDeleting(true);
    setError(null);
    try {
      await k8sDelete({ model, resource: { metadata: { name: resourceName } } });
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Delete failed');
      setDeleting(false);
      return;
    }
    try {
      if (extraOption && extraTicked) await extraOption.run();
      onDeleted();
    } catch (err) {
      setDeletedWithProblem(true);
      setError(err instanceof Error ? err.message : 'The extra step failed');
    } finally {
      setDeleting(false);
    }
  };

  return (
    <Modal
      variant={ModalVariant.small}
      isOpen={isOpen}
      onClose={deletedWithProblem ? onDeleted : onClose}
    >
      <ModalHeader title={`Delete ${resourceKind}?`} />
      <ModalBody>
        <Stack hasGutter>
          <StackItem>
            Are you sure you want to delete <strong>{resourceName}</strong>? This action cannot be
            undone.
          </StackItem>
          {note && <StackItem>{note}</StackItem>}
          {requireConfirmation && (
            <StackItem>
              <TextInput
                value={confirmation}
                onChange={(_event, value) => setConfirmation(value)}
                placeholder={`Type "${resourceName}" to confirm`}
                aria-label="Confirmation input"
              />
            </StackItem>
          )}
          {extraOption && !deletedWithProblem && (
            <StackItem>
              <Checkbox
                id="delete-extra-option"
                label={extraOption.label}
                isChecked={extraTicked}
                onChange={(_event, checked) => setExtraTicked(checked)}
              />
            </StackItem>
          )}
          {error && (
            <StackItem>
              <Alert
                variant="danger"
                title={deletedWithProblem ? `The ${resourceKind} was deleted, but` : 'Delete failed'}
                isInline
              >
                {error}
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        {deletedWithProblem ? (
          <Button key="continue" variant="primary" onClick={onDeleted}>
            Continue
          </Button>
        ) : (
          <>
            <Button
              key="delete"
              variant="danger"
              onClick={handleDelete}
              isDisabled={!canDelete || deleting}
              isLoading={deleting}
            >
              Delete
            </Button>
            <Button key="cancel" variant="link" onClick={onClose}>
              Cancel
            </Button>
          </>
        )}
      </ModalFooter>
    </Modal>
  );
};

export default DeleteModal;
