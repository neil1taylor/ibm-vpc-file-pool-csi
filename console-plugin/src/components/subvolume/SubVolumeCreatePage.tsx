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
  NumberInput,
  FormSelect,
  FormSelectOption,
  ActionGroup,
  Button,
  Form,
  Stack,
  StackItem,
  Split,
  SplitItem,
  Label,
  Card,
  CardBody,
} from '@patternfly/react-core';
import { PersistentVolumeClaimModel, FileSharePoolModel } from '../../models';
import { FileSharePool } from '../../types';
import { ROUTES } from '../../constants';
import { hasUsableShare } from '../../utils/pool-helpers';
import { buildPVC, errorMessage, isValidK8sName, pvcPath } from '../../utils/k8s-objects';
import FieldPopover from '../common/FieldPopover';
import { useBackupPolicies } from '../../utils/use-backup-policies';
import { isSnapshotPool } from '../../utils/snapshot-pools';

const SubVolumeCreatePage: React.FC = () => {
  const history = useHistory();
  const [namespace, setNamespace] = useState('');
  const [name, setName] = useState('');
  const [poolName, setPoolName] = useState('');
  const [tier, setTier] = useState('');
  const [sizeGiB, setSizeGiB] = useState(10);
  const [error, setError] = useState('');
  const [creating, setCreating] = useState(false);

  const policies = useBackupPolicies();
  const [pools, poolsLoaded] = useK8sWatchResource<FileSharePool[]>({
    groupVersionKind: {
      group: FileSharePoolModel.apiGroup,
      version: FileSharePoolModel.apiVersion,
      kind: FileSharePoolModel.kind,
    },
    isList: true,
  });

  const usablePools = (pools || []).filter((p) => hasUsableShare(p) && !isSnapshotPool(p.metadata?.name || '', policies));
  const selectedPool = usablePools.find((p) => p.metadata?.name === poolName);
  const tiers = selectedPool?.spec?.tiers ?? [];

  const currentAllocated = selectedPool?.status?.totalAllocatedGB ?? 0;
  const totalCapacity = selectedPool?.status?.totalCapacityGB ?? 0;
  const afterAllocated = currentAllocated + sizeGiB;
  const afterPct = totalCapacity > 0 ? Math.round((afterAllocated / totalCapacity) * 100) : 0;

  const isValid =
    isValidK8sName(namespace) &&
    isValidK8sName(name) &&
    poolName.length > 0 &&
    (tiers.length === 0 || tier.length > 0) &&
    sizeGiB >= 1;

  const handlePoolChange = (value: string) => {
    setPoolName(value);
    setTier('');
  };

  const handleCreate = async () => {
    setError('');
    setCreating(true);
    try {
      await k8sCreate({
        model: PersistentVolumeClaimModel,
        data: buildPVC({ namespace, name, poolName, tier: tier || undefined, sizeGiB }),
      });
      history.push(pvcPath(namespace, name));
    } catch (err: unknown) {
      setError(errorMessage(err, 'Failed to create volume'));
    } finally {
      setCreating(false);
    }
  };

  return (
    <PageSection variant="default">
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Create volume (PVC)
      </Title>

      <Alert variant="info" title="This creates a PersistentVolumeClaim" isInline style={{ marginBottom: '1rem' }}>
        The driver allocates space in the pool when the claim is created. The SubVolume record appears
        in the SubVolumes list afterwards.
      </Alert>

      {error && (
        <Alert variant="danger" title="Creation failed" isInline style={{ marginBottom: '1rem' }}>
          {error}
        </Alert>
      )}

      <Form>
        <Stack hasGutter>
          <StackItem>
            <FieldPopover
              fieldId="pvc-namespace"
              label="Namespace"
              description="The namespace where the PersistentVolumeClaim is created. It must already exist."
              isRequired
            >
              <TextInput
                id="pvc-namespace"
                value={namespace}
                onChange={(_event, value) => setNamespace(value)}
                isRequired
                placeholder="default"
              />
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="pvc-name"
              label="Name"
              description="The name of the PersistentVolumeClaim."
              isRequired
            >
              <TextInput
                id="pvc-name"
                value={name}
                onChange={(_event, value) => setName(value)}
                isRequired
                validated={name.length === 0 || isValidK8sName(name) ? 'default' : 'error'}
                placeholder="my-volume"
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
              fieldId="pvc-pool"
              label="Pool"
              description="The pool that will hold the volume. Only pools with at least one usable share are shown: the share must be stable and have a mount target."
              isRequired
            >
              <FormSelect
                id="pvc-pool"
                value={poolName}
                onChange={(_event, value) => handlePoolChange(value)}
                isRequired
              >
                <FormSelectOption value="" label="-- Select a pool --" isDisabled />
                {usablePools.map((p) => {
                  const pName = p.metadata?.name || '';
                  const alloc = p.status?.totalAllocatedGB ?? 0;
                  const cap = p.status?.totalCapacityGB ?? 0;
                  return (
                    <FormSelectOption key={pName} value={pName} label={`${pName} (${alloc}/${cap} GB used)`} />
                  );
                })}
              </FormSelect>
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>
                    {poolsLoaded ? `${usablePools.length} pool(s) can serve volumes` : 'Loading pools...'}
                  </HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>

          {tiers.length > 0 && (
            <StackItem>
              <FieldPopover
                fieldId="pvc-tier"
                label="Tier"
                description="This pool has tiers. Each tier has its own storage class and its own shares."
                isRequired
              >
                <FormSelect id="pvc-tier" value={tier} onChange={(_event, value) => setTier(value)} isRequired>
                  <FormSelectOption value="" label="-- Select a tier --" isDisabled />
                  {tiers.map((t) => (
                    <FormSelectOption key={t.name} value={t.name} label={t.name} />
                  ))}
                </FormSelect>
              </FieldPopover>
            </StackItem>
          )}

          <StackItem>
            <FieldPopover
              fieldId="pvc-size"
              label="Size (GiB)"
              description="The requested size of the volume. The pool counts this against its capacity. NFS does not enforce a limit per volume."
              isRequired
            >
              <NumberInput
                id="pvc-size"
                value={sizeGiB}
                min={1}
                onMinus={() => setSizeGiB(Math.max(1, sizeGiB - 1))}
                onPlus={() => setSizeGiB(sizeGiB + 1)}
                onChange={(event) => {
                  const val = parseInt((event.target as HTMLInputElement).value, 10);
                  if (!isNaN(val)) setSizeGiB(val);
                }}
              />
            </FieldPopover>
          </StackItem>

          {selectedPool && (
            <StackItem>
              <Card isCompact isPlain>
                <CardBody>
                  <Split hasGutter>
                    <SplitItem>After creation:</SplitItem>
                    <SplitItem>
                      <strong>{afterAllocated}</strong> / {totalCapacity} GB
                    </SplitItem>
                    <SplitItem>
                      <Label color={afterPct >= 90 ? 'red' : afterPct >= 75 ? 'orange' : 'blue'}>
                        {afterPct}% used
                      </Label>
                    </SplitItem>
                    {afterAllocated > totalCapacity && (
                      <SplitItem>
                        <Label color="red">Exceeds capacity</Label>
                      </SplitItem>
                    )}
                  </Split>
                </CardBody>
              </Card>
            </StackItem>
          )}

          <StackItem>
            <ActionGroup>
              <Button variant="primary" onClick={handleCreate} isDisabled={creating || !isValid} isLoading={creating}>
                Create
              </Button>
              <Button variant="link" onClick={() => history.push(ROUTES.SUBVOLUMES)} isDisabled={creating}>
                Cancel
              </Button>
            </ActionGroup>
          </StackItem>
        </Stack>
      </Form>
    </PageSection>
  );
};

export default SubVolumeCreatePage;
