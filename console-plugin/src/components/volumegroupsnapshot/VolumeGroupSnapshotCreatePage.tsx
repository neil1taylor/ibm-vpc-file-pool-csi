import React, { useState } from 'react';
import {
  useK8sWatchResource,
  k8sCreate,
} from '@openshift-console/dynamic-plugin-sdk';
import { useHistory } from 'react-router-dom';
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
  Checkbox,
  Card,
  CardTitle,
  CardBody,
  Grid,
  GridItem,
  Label,
  Split,
  SplitItem,
} from '@patternfly/react-core';
import { FileSharePoolModel, SubVolumeModel, VolumeGroupSnapshotModel } from '../../models';
import {
  FileSharePool,
  SubVolume,
  VolumeGroupSnapshot,
  VolumeGroupSnapshotSpec,
  Hook,
} from '../../types';
import { ROUTES } from '../../constants';
import FieldPopover from '../common/FieldPopover';
import HookEditor from '../common/HookEditor';

const VolumeGroupSnapshotCreatePage: React.FC = () => {
  const history = useHistory();

  const [name, setName] = useState('');
  const [poolName, setPoolName] = useState('');
  const [selectedPVCs, setSelectedPVCs] = useState<string[]>([]);
  const [copyOrder, setCopyOrder] = useState('');
  const [failurePolicy, setFailurePolicy] = useState<'Abort' | 'Continue'>('Abort');
  const [preSnapshotHooks, setPreSnapshotHooks] = useState<Hook[]>([]);
  const [postSnapshotHooks, setPostSnapshotHooks] = useState<Hook[]>([]);
  const [error, setError] = useState('');
  const [creating, setCreating] = useState(false);

  const [pools, poolsLoaded] = useK8sWatchResource<FileSharePool[]>({
    groupVersionKind: {
      group: FileSharePoolModel.apiGroup,
      version: FileSharePoolModel.apiVersion,
      kind: FileSharePoolModel.kind,
    },
    isList: true,
  });

  const [subVolumes, subVolumesLoaded] = useK8sWatchResource<SubVolume[]>({
    groupVersionKind: {
      group: SubVolumeModel.apiGroup,
      version: SubVolumeModel.apiVersion,
      kind: SubVolumeModel.kind,
    },
    isList: true,
  });

  const filteredSubVolumes = (subVolumes || []).filter(
    (sv) => sv.spec?.poolName === poolName,
  );

  const handlePVCToggle = (pvcName: string, checked: boolean) => {
    if (checked) {
      setSelectedPVCs((prev) => [...prev, pvcName]);
    } else {
      setSelectedPVCs((prev) => prev.filter((p) => p !== pvcName));
    }
  };

  const handleSelectAll = () => {
    const allNames = filteredSubVolumes.map((sv) => sv.metadata?.name || '').filter(Boolean);
    setSelectedPVCs(allNames);
  };

  const handleDeselectAll = () => {
    setSelectedPVCs([]);
  };

  const handlePoolChange = (newPool: string) => {
    setPoolName(newPool);
    setSelectedPVCs([]);
  };

  const isValid = name.length > 0 && poolName.length > 0 && selectedPVCs.length > 0;

  const handleCreate = async () => {
    setError('');
    setCreating(true);
    try {
      const spec: VolumeGroupSnapshotSpec = {
        poolName,
        sourcePVCs: selectedPVCs,
        failurePolicy,
      };

      const orderEntries = copyOrder
        .split(',')
        .map((s) => s.trim())
        .filter((s) => s.length > 0);
      if (orderEntries.length > 0) spec.copyOrder = orderEntries;
      if (preSnapshotHooks.length > 0) spec.preSnapshotHooks = preSnapshotHooks;
      if (postSnapshotHooks.length > 0) spec.postSnapshotHooks = postSnapshotHooks;

      const resource: VolumeGroupSnapshot = {
        apiVersion: 'storage.ibmcloud.io/v1alpha1',
        kind: 'VolumeGroupSnapshot',
        metadata: { name },
        spec,
      };

      await k8sCreate({ model: VolumeGroupSnapshotModel, data: resource });
      history.push(ROUTES.GROUP_SNAPSHOT_DETAIL.replace(':name', name));
    } catch (err: unknown) {
      const message =
        err instanceof Error
          ? err.message
          : typeof err === 'object' && err !== null && 'message' in err
            ? String((err as { message: unknown }).message)
            : 'Failed to create VolumeGroupSnapshot';
      setError(message);
    } finally {
      setCreating(false);
    }
  };

  return (
    <PageSection variant="default">
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Create VolumeGroupSnapshot
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
            fieldId="vgs-name"
            label="Name"
            description="Unique name for this group snapshot. The snapshot controller will create individual Snapshot CRs for each selected SubVolume, named {name}-{subvolume}."
            isRequired
          >
            <TextInput
              id="vgs-name"
              value={name}
              onChange={(_event, value) => setName(value)}
              isRequired
              placeholder="my-group-snapshot"
            />
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="vgs-pool"
            label="Pool Name"
            description="The pool containing the SubVolumes to snapshot. All selected SubVolumes must belong to the same pool."
            isRequired
          >
            <FormSelect
              id="vgs-pool"
              value={poolName}
              onChange={(_event, value) => handlePoolChange(value)}
              isDisabled={!poolsLoaded}
            >
              <FormSelectOption value="" label="-- Select a pool --" isDisabled />
              {(pools || []).map((pool) => {
                const pName = pool.metadata?.name || '';
                const svCount = (subVolumes || []).filter(
                  (sv) => sv.spec?.poolName === pName,
                ).length;
                return (
                  <FormSelectOption
                    key={pName}
                    value={pName}
                    label={`${pName} (${svCount} SubVolumes)`}
                  />
                );
              })}
            </FormSelect>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="vgs-source-pvcs"
            label="Source PVCs"
            description="Select the SubVolumes to include in this group snapshot. All selected volumes will be snapshotted in a coordinated operation with minimal inconsistency window."
            isRequired
          >
            {poolName && subVolumesLoaded ? (
              filteredSubVolumes.length > 0 ? (
                <Card isPlain isCompact>
                  <CardBody>
                    <Stack hasGutter>
                      <StackItem>
                        <Split hasGutter>
                          <SplitItem>
                            <Button variant="link" isInline onClick={handleSelectAll}>
                              Select all
                            </Button>
                          </SplitItem>
                          <SplitItem>
                            <Button variant="link" isInline onClick={handleDeselectAll}>
                              Deselect all
                            </Button>
                          </SplitItem>
                          <SplitItem isFilled />
                          <SplitItem>
                            <Label isCompact>
                              {selectedPVCs.length}/{filteredSubVolumes.length} selected
                            </Label>
                          </SplitItem>
                        </Split>
                      </StackItem>
                      {filteredSubVolumes.map((sv) => {
                        const svName = sv.metadata?.name || '';
                        const phase = sv.status?.phase || 'Unknown';
                        return (
                          <StackItem key={svName}>
                            <Checkbox
                              id={`pvc-${svName}`}
                              label={
                                <Split hasGutter>
                                  <SplitItem>{svName}</SplitItem>
                                  <SplitItem>
                                    <Label isCompact color={phase === 'Bound' ? 'green' : 'grey'}>
                                      {phase}
                                    </Label>
                                  </SplitItem>
                                  <SplitItem>
                                    <Label isCompact>{sv.spec?.requestedGB ?? 0} GB</Label>
                                  </SplitItem>
                                  {sv.spec?.pvcName && (
                                    <SplitItem>
                                      <span style={{ color: 'var(--pf-v6-global--Color--200)' }}>
                                        {sv.spec.pvcNamespace}/{sv.spec.pvcName}
                                      </span>
                                    </SplitItem>
                                  )}
                                </Split>
                              }
                              isChecked={selectedPVCs.includes(svName)}
                              onChange={(_event, checked) =>
                                handlePVCToggle(svName, checked)
                              }
                            />
                          </StackItem>
                        );
                      })}
                    </Stack>
                  </CardBody>
                </Card>
              ) : (
                <Alert variant="info" title="No SubVolumes found" isInline>
                  No SubVolumes exist in pool &quot;{poolName}&quot;.
                </Alert>
              )
            ) : (
              <TextInput
                id="vgs-source-pvcs-placeholder"
                isDisabled
                placeholder="Select a pool first"
                value=""
                readOnlyVariant="default"
              />
            )}
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  {poolName
                    ? `Select SubVolumes from pool "${poolName}"`
                    : 'Select a pool first'}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="vgs-copy-order"
            label="Copy Order"
            description="Define the order in which SubVolumes are snapshotted. For database consistency, snapshot the DB volume first, then application volumes. Comma-separated list of SubVolume names."
          >
            <TextInput
              id="vgs-copy-order"
              value={copyOrder}
              onChange={(_event, value) => setCopyOrder(value)}
              placeholder="sv-db,sv-app,sv-logs"
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>Comma-separated list of SubVolume names defining snapshot order (optional)</HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="vgs-failure-policy"
            label="Failure Policy"
            description="Controls behavior when a snapshot fails for one of the selected SubVolumes."
            isRequired
          >
            <Grid hasGutter>
              <GridItem sm={12} md={6}>
                <Card
                  isSelectable
                  isSelected={failurePolicy === 'Abort'}
                  isClickable
                  isCompact
                  onClick={() => setFailurePolicy('Abort')}
                >
                  <CardTitle>Abort</CardTitle>
                  <CardBody>
                    Stop on first failure. No further SubVolumes will be snapshotted. Use when all-or-nothing consistency is required (e.g., database + WAL).
                  </CardBody>
                </Card>
              </GridItem>
              <GridItem sm={12} md={6}>
                <Card
                  isSelectable
                  isSelected={failurePolicy === 'Continue'}
                  isClickable
                  isCompact
                  onClick={() => setFailurePolicy('Continue')}
                >
                  <CardTitle>Continue</CardTitle>
                  <CardBody>
                    Snapshot remaining SubVolumes even if one fails. Results in a PartialFailure status. Use when partial snapshots are still useful.
                  </CardBody>
                </Card>
              </GridItem>
            </Grid>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <Card isPlain>
            <CardTitle>
              <FieldPopover
                fieldId="vgs-hooks"
                label="Hooks"
                description="Execute commands or HTTP webhooks before and after the snapshot operation. Pre-snapshot hooks can quiesce databases (e.g., FLUSH TABLES WITH READ LOCK). Post-snapshot hooks can release locks. Exec hooks run a command in a pod. HTTP hooks call a webhook URL."
              >
                <span />
              </FieldPopover>
            </CardTitle>
            <CardBody>
              <Stack hasGutter>
                <StackItem>
                  <HookEditor
                    hooks={preSnapshotHooks}
                    onChange={setPreSnapshotHooks}
                    label="Pre-Snapshot Hooks"
                  />
                </StackItem>
                <StackItem>
                  <HookEditor
                    hooks={postSnapshotHooks}
                    onChange={setPostSnapshotHooks}
                    label="Post-Snapshot Hooks"
                  />
                </StackItem>
              </Stack>
            </CardBody>
          </Card>
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

export default VolumeGroupSnapshotCreatePage;
