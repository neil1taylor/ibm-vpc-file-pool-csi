import React, { useState, useEffect } from 'react';
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
  NumberInput,
  FormSelect,
  FormSelectOption,
  ActionGroup,
  Button,
  Stack,
  StackItem,
  Card,
  CardBody,
  Split,
  SplitItem,
  Label,
} from '@patternfly/react-core';
import { SnapshotModel, SubVolumeModel } from '../../models';
import { Snapshot, SubVolume } from '../../types';
import { ROUTES } from '../../constants';
import FieldPopover from '../common/FieldPopover';

const SnapshotCreatePage: React.FC = () => {
  const history = useHistory();
  const location = useLocation();

  const queryParams = new URLSearchParams(location.search);
  const sourceParam = queryParams.get('source') || '';

  const [name, setName] = useState('');
  const [sourceSubVolume, setSourceSubVolume] = useState(sourceParam);
  const [poolName, setPoolName] = useState('');
  const [sizeGB, setSizeGB] = useState(0);
  const [poolAutoFilled, setPoolAutoFilled] = useState(false);

  const [error, setError] = useState('');
  const [creating, setCreating] = useState(false);

  const [subVolumes, svLoaded] = useK8sWatchResource<SubVolume[]>({
    groupVersionKind: {
      group: SubVolumeModel.apiGroup,
      version: SubVolumeModel.apiVersion,
      kind: SubVolumeModel.kind,
    },
    isList: true,
  });

  const boundSubVolumes = (subVolumes || []).filter(
    (sv) => sv.status?.phase === 'Bound',
  );

  // Auto-fill fields when the selected subvolume changes
  useEffect(() => {
    if (!sourceSubVolume || !svLoaded) return;

    const sv = boundSubVolumes.find(
      (s) => s.metadata?.name === sourceSubVolume,
    );
    if (sv) {
      setPoolName(sv.spec.poolName);
      setPoolAutoFilled(true);
      setSizeGB(sv.spec.requestedGB);
    } else {
      setPoolName('');
      setPoolAutoFilled(false);
      setSizeGB(0);
    }
  }, [sourceSubVolume, svLoaded, subVolumes]);

  const handleCreate = async () => {
    setError('');
    setCreating(true);

    const selectedSV = boundSubVolumes.find(
      (sv) => sv.metadata?.name === sourceSubVolume,
    );

    if (!selectedSV) {
      setError('Selected SubVolume not found or not in Bound phase.');
      setCreating(false);
      return;
    }

    const snapshotPath = `/pvcs/.snapshots/${name}`;

    const resource: Snapshot = {
      apiVersion: 'storage.ibmcloud.io/v1alpha1',
      kind: 'Snapshot',
      metadata: { name },
      spec: {
        sourceSubVolume,
        poolName: selectedSV.spec.poolName,
        shareID: selectedSV.spec.shareID,
        shareMountTargetIP: selectedSV.spec.shareMountTargetIP,
        snapshotPath,
        sourceSubPath: selectedSV.spec.subPath,
        sizeGB,
      },
    };

    try {
      await k8sCreate({ model: SnapshotModel, data: resource });
      history.push(ROUTES.SNAPSHOT_DETAIL.replace(':name', name));
    } catch (err: unknown) {
      const message =
        err instanceof Error
          ? err.message
          : typeof err === 'object' && err !== null && 'message' in err
            ? String((err as { message: unknown }).message)
            : 'Failed to create Snapshot';
      setError(message);
    } finally {
      setCreating(false);
    }
  };

  const isValid =
    name.length > 0 &&
    sourceSubVolume.length > 0 &&
    poolName.length > 0 &&
    sizeGB > 0;

  // Find the selected SubVolume for the info card
  const selectedSV = boundSubVolumes.find(
    (sv) => sv.metadata?.name === sourceSubVolume,
  );

  return (
    <PageSection variant="default">
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Create Snapshot
      </Title>
      <Stack hasGutter>
        {error && (
          <StackItem>
            <Alert variant="danger" title="Creation failed" isInline>
              {error}
            </Alert>
          </StackItem>
        )}

        <StackItem>
          <FieldPopover
            fieldId="snap-name"
            label="Name"
            description="Unique name for this snapshot. Used as the directory name under /pvcs/.snapshots/ on the NFS share."
            isRequired
          >
            <TextInput
              id="snap-name"
              value={name}
              onChange={(_event, value) => setName(value)}
              isRequired
              placeholder="my-snapshot"
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>Unique name for this snapshot</HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="snap-source"
            label="Source SubVolume"
            description="The SubVolume to snapshot. Only Bound SubVolumes are available for snapshotting. Selecting a SubVolume auto-fills the pool name and size estimate."
            isRequired
          >
            <FormSelect
              id="snap-source"
              value={sourceSubVolume}
              onChange={(_event, value) => setSourceSubVolume(value)}
              isDisabled={!svLoaded}
            >
              <FormSelectOption value="" label="-- Select a SubVolume --" isDisabled />
              {boundSubVolumes.map((sv) => {
                const svName = sv.metadata?.name || '';
                const phase = sv.status?.phase || '';
                return (
                  <FormSelectOption
                    key={svName}
                    value={svName}
                    label={`${svName} (${sv.spec.poolName}, ${sv.spec.requestedGB} GB, ${phase})`}
                  />
                );
              })}
            </FormSelect>
            <FormHelperText>
              <HelperText>
                <HelperTextItem>SubVolume to snapshot (only Bound subvolumes are shown)</HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        {/* Source SubVolume info card */}
        {selectedSV && (
          <StackItem>
            <Card isCompact variant="secondary">
              <CardBody>
                <Split hasGutter>
                  <SplitItem>
                    <Label isCompact>Pool: {selectedSV.spec.poolName}</Label>
                  </SplitItem>
                  <SplitItem>
                    <Label isCompact>Size: {selectedSV.spec.requestedGB} GB</Label>
                  </SplitItem>
                  <SplitItem>
                    <Label isCompact>
                      PVC: {selectedSV.spec.pvcNamespace}/{selectedSV.spec.pvcName}
                    </Label>
                  </SplitItem>
                  {selectedSV.spec.shareID && (
                    <SplitItem>
                      <Label isCompact color="blue">
                        Share: {selectedSV.spec.shareID.slice(0, 12)}...
                      </Label>
                    </SplitItem>
                  )}
                </Split>
              </CardBody>
            </Card>
          </StackItem>
        )}

        <StackItem>
          <FieldPopover
            fieldId="snap-pool"
            label="Pool Name"
            description="Pool that owns the source SubVolume. Auto-filled when you select a SubVolume."
            isRequired
          >
            <TextInput
              id="snap-pool"
              value={poolName}
              onChange={(_event, value) => setPoolName(value)}
              isDisabled={poolAutoFilled}
              isRequired
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  {poolAutoFilled ? 'Auto-filled from selected SubVolume' : 'Pool that owns the source SubVolume'}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="snap-size"
            label="Size (GB)"
            description="Snapshot size estimate in GB. Auto-filled from the source SubVolume's requested size. Actual snapshot size depends on data written."
            isRequired
          >
            <NumberInput
              id="snap-size"
              value={sizeGB}
              min={1}
              onMinus={() => setSizeGB(Math.max(1, sizeGB - 1))}
              onPlus={() => setSizeGB(sizeGB + 1)}
              onChange={(event) => {
                const val = parseInt((event.target as HTMLInputElement).value, 10);
                if (!isNaN(val)) setSizeGB(val);
              }}
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  {selectedSV
                    ? `Auto-filled from source (${selectedSV.spec.requestedGB} GB)`
                    : 'Snapshot size in GB'}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <ActionGroup>
            <Button
              variant="primary"
              onClick={handleCreate}
              isDisabled={creating || !isValid}
              isLoading={creating}
            >
              Create
            </Button>
            <Button variant="link" onClick={() => history.goBack()} isDisabled={creating}>
              Cancel
            </Button>
          </ActionGroup>
        </StackItem>
      </Stack>
    </PageSection>
  );
};

export default SnapshotCreatePage;
