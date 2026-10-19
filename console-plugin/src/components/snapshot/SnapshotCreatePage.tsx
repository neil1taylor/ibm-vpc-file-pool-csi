import React, { useState } from 'react';
import { k8sCreate, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { useHistory, useLocation } from 'react-router-dom';
import {
  PageSection,
  Title,
  Alert,
  FormHelperText,
  HelperText,
  HelperTextItem,
  TextInput,
  FormSelect,
  FormSelectOption,
  ActionGroup,
  Button,
  Stack,
  StackItem,
} from '@patternfly/react-core';
import {
  PersistentVolumeClaimModel,
  StorageClassModel,
  VolumeSnapshotClassModel,
  VolumeSnapshotModel,
} from '../../models';
import { ROUTES } from '../../constants';
import {
  DriverClass,
  PersistentVolumeClaim,
  StorageClass,
  buildVolumeSnapshot,
  errorMessage,
  findClassByDriver,
  isValidK8sName,
  poolPVCs,
  volumeSnapshotPath,
} from '../../utils/k8s-objects';
import FieldPopover from '../common/FieldPopover';

const watch = (model: { apiGroup?: string; apiVersion: string; kind: string }) => ({
  groupVersionKind: { group: model.apiGroup, version: model.apiVersion, kind: model.kind },
  isList: true,
});

const SnapshotCreatePage: React.FC = () => {
  const history = useHistory();
  const location = useLocation();

  const params = new URLSearchParams(location.search);
  const presetSource =
    params.get('namespace') && params.get('pvc') ? `${params.get('namespace')}/${params.get('pvc')}` : '';

  const [name, setName] = useState('');
  const [source, setSource] = useState(presetSource);
  const [error, setError] = useState('');
  const [creating, setCreating] = useState(false);

  const [pvcs, pvcsLoaded] = useK8sWatchResource<PersistentVolumeClaim[]>(watch(PersistentVolumeClaimModel));
  const [storageClasses] = useK8sWatchResource<StorageClass[]>(watch(StorageClassModel));
  const [snapshotClasses, classesLoaded] = useK8sWatchResource<DriverClass[]>(watch(VolumeSnapshotClassModel));

  const sources = poolPVCs(pvcs || [], storageClasses || []);
  const snapshotClass = findClassByDriver(snapshotClasses || []);
  const noClass = classesLoaded && !snapshotClass;
  const [namespace, pvcName] = source.split('/');

  const isValid = isValidK8sName(name) && source.length > 0 && !!snapshotClass;

  const handleCreate = async () => {
    if (!snapshotClass) return;
    setError('');
    setCreating(true);
    try {
      await k8sCreate({
        model: VolumeSnapshotModel,
        data: buildVolumeSnapshot({
          namespace,
          name,
          pvcName,
          className: snapshotClass.metadata?.name || '',
        }),
      });
      history.push(volumeSnapshotPath(namespace, name));
    } catch (err: unknown) {
      setError(errorMessage(err, 'Failed to create snapshot'));
    } finally {
      setCreating(false);
    }
  };

  return (
    <PageSection variant="default">
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Create snapshot
      </Title>
      <Stack hasGutter>
        {noClass && (
          <StackItem>
            <Alert variant="warning" title="No snapshot class for this driver" isInline>
              No VolumeSnapshotClass uses the driver vpc-file-pool.csi.ibm.io. Create one before taking snapshots.
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
            fieldId="snap-source"
            label="Source volume"
            description="The PersistentVolumeClaim to snapshot. Only claims whose storage class uses this driver are shown. The snapshot is created in the same namespace."
            isRequired
          >
            <FormSelect
              id="snap-source"
              value={source}
              onChange={(_event, value) => setSource(value)}
              isDisabled={!pvcsLoaded}
            >
              <FormSelectOption value="" label="-- Select a volume --" isDisabled />
              {sources.map((p) => {
                const key = `${p.metadata?.namespace}/${p.metadata?.name}`;
                return <FormSelectOption key={key} value={key} label={key} />;
              })}
            </FormSelect>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="snap-name"
            label="Name"
            description="The name of the VolumeSnapshot."
            isRequired
          >
            <TextInput
              id="snap-name"
              value={name}
              onChange={(_event, value) => setName(value)}
              isRequired
              validated={name.length === 0 || isValidK8sName(name) ? 'default' : 'error'}
              placeholder="my-snapshot"
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>Lower-case letters, digits and dashes</HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <ActionGroup>
            <Button variant="primary" onClick={handleCreate} isDisabled={creating || !isValid} isLoading={creating}>
              Create
            </Button>
            <Button variant="link" onClick={() => history.push(ROUTES.SNAPSHOTS)} isDisabled={creating}>
              Cancel
            </Button>
          </ActionGroup>
        </StackItem>
      </Stack>
    </PageSection>
  );
};

export default SnapshotCreatePage;
