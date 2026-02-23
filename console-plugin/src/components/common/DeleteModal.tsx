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
}

const DeleteModal: React.FC<DeleteModalProps> = ({
  isOpen,
  resourceName,
  resourceKind,
  model,
  onClose,
  onDeleted,
  requireConfirmation = false,
}) => {
  const [confirmation, setConfirmation] = useState('');
  const [error, setError] = useState<string | null>(null);
  const [deleting, setDeleting] = useState(false);

  const canDelete = !requireConfirmation || confirmation === resourceName;

  const handleDelete = async () => {
    setDeleting(true);
    setError(null);
    try {
      await k8sDelete({ model, resource: { metadata: { name: resourceName } } });
      onDeleted();
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Delete failed');
    } finally {
      setDeleting(false);
    }
  };

  return (
    <Modal
      variant={ModalVariant.small}
      isOpen={isOpen}
      onClose={onClose}
    >
      <ModalHeader title={`Delete ${resourceKind}?`} />
      <ModalBody>
        <Stack hasGutter>
          <StackItem>
            Are you sure you want to delete <strong>{resourceName}</strong>? This action cannot be
            undone.
          </StackItem>
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
          {error && (
            <StackItem>
              <Alert variant="danger" title="Delete failed" isInline>
                {error}
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
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
      </ModalFooter>
    </Modal>
  );
};

export default DeleteModal;
