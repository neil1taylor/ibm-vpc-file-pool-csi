import React, { useState } from 'react';
import { k8sCreate, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { useHistory } from 'react-router-dom';
import {
  PageSection,
  Title,
  Alert,
  FormHelperText,
  HelperText,
  HelperTextItem,
  TextInput,
  TextArea,
  ActionGroup,
  Button,
  Stack,
  StackItem,
  Bullseye,
  Spinner,
} from '@patternfly/react-core';
import { KubeVolumeGroupSnapshotModel, VolumeGroupSnapshotClassModel } from '../../models';
import { ROUTES } from '../../constants';
import {
  DriverClass,
  buildVolumeGroupSnapshot,
  errorMessage,
  findClassByDriver,
  isNotFoundError,
  isValidK8sName,
  parseMatchLabels,
  volumeGroupSnapshotPath,
} from '../../utils/k8s-objects';
import FieldPopover from '../common/FieldPopover';

const VolumeGroupSnapshotCreatePage: React.FC = () => {
  const history = useHistory();
  const [namespace, setNamespace] = useState('');
  const [name, setName] = useState('');
  const [labels, setLabels] = useState('');
  const [error, setError] = useState('');
  const [creating, setCreating] = useState(false);

  const [classes, loaded, loadError] = useK8sWatchResource<DriverClass[]>({
    groupVersionKind: {
      group: VolumeGroupSnapshotClassModel.apiGroup,
      version: VolumeGroupSnapshotClassModel.apiVersion,
      kind: VolumeGroupSnapshotClassModel.kind,
    },
    isList: true,
  });

  if (isNotFoundError(loadError)) {
    return (
      <PageSection variant="default">
        <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
          Create group snapshot
        </Title>
        <Alert variant="warning" title="Group snapshots are not available" isInline>
          This cluster does not have the Kubernetes group snapshot API, so group snapshots cannot be created.
        </Alert>
      </PageSection>
    );
  }

  if (loadError) {
    return (
      <PageSection variant="default">
        <Alert variant="danger" title="Error loading group snapshot classes" isInline>
          {errorMessage(loadError, 'Unknown error')}
        </Alert>
      </PageSection>
    );
  }

  if (!loaded) {
    return (
      <PageSection variant="default">
        <Bullseye>
          <Spinner aria-label="Loading" />
        </Bullseye>
      </PageSection>
    );
  }

  const groupClass = findClassByDriver(classes || []);
  const matchLabels = parseMatchLabels(labels);
  const isValid =
    isValidK8sName(namespace) &&
    isValidK8sName(name) &&
    Object.keys(matchLabels).length > 0 &&
    !!groupClass;

  const handleCreate = async () => {
    if (!groupClass) return;
    setError('');
    setCreating(true);
    try {
      await k8sCreate({
        model: KubeVolumeGroupSnapshotModel,
        data: buildVolumeGroupSnapshot({
          namespace,
          name,
          matchLabels,
          className: groupClass.metadata?.name || '',
        }),
      });
      history.push(volumeGroupSnapshotPath(namespace, name));
    } catch (err: unknown) {
      setError(errorMessage(err, 'Failed to create group snapshot'));
    } finally {
      setCreating(false);
    }
  };

  return (
    <PageSection variant="default">
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Create group snapshot
      </Title>
      <Stack hasGutter>
        {!groupClass && (
          <StackItem>
            <Alert variant="warning" title="No group snapshot class for this driver" isInline>
              No VolumeGroupSnapshotClass uses the driver vpc-file-pool.csi.ibm.io. Create one before taking group
              snapshots.
            </Alert>
          </StackItem>
        )}

        {error && (
          <StackItem>
            <Alert variant="danger" title="Creation failed" isInline>
              {error}
            </Alert>
          </StackItem>
        )}

        <StackItem>
          <FieldPopover
            fieldId="vgs-namespace"
            label="Namespace"
            description="The namespace of the volumes to snapshot. The group snapshot is created in the same namespace."
            isRequired
          >
            <TextInput
              id="vgs-namespace"
              value={namespace}
              onChange={(_event, value) => setNamespace(value)}
              isRequired
              placeholder="default"
            />
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="vgs-name"
            label="Name"
            description="The name of the VolumeGroupSnapshot."
            isRequired
          >
            <TextInput
              id="vgs-name"
              value={name}
              onChange={(_event, value) => setName(value)}
              isRequired
              validated={name.length === 0 || isValidK8sName(name) ? 'default' : 'error'}
              placeholder="my-group-snapshot"
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>Lower-case letters, digits and dashes</HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="vgs-labels"
            label="PVC labels"
            description="Every PersistentVolumeClaim in the namespace that has all of these labels is included in the group snapshot."
            isRequired
          >
            <TextArea
              id="vgs-labels"
              value={labels}
              onChange={(_event, value) => setLabels(value)}
              rows={3}
              placeholder={'app=database'}
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>One key=value pair per line</HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <ActionGroup>
            <Button variant="primary" onClick={handleCreate} isDisabled={creating || !isValid} isLoading={creating}>
              Create
            </Button>
            <Button variant="link" onClick={() => history.push(ROUTES.GROUP_SNAPSHOTS)} isDisabled={creating}>
              Cancel
            </Button>
          </ActionGroup>
        </StackItem>
      </Stack>
    </PageSection>
  );
};

export default VolumeGroupSnapshotCreatePage;
