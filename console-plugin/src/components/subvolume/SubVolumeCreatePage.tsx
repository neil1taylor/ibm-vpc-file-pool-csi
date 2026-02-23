import React, { useState, useMemo } from 'react';
import {
  k8sCreate,
  useK8sWatchResource,
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
import { SubVolumeModel, FileSharePoolModel } from '../../models';
import { SubVolume, FileSharePool } from '../../types';
import { ROUTES, RECLAIM_POLICIES } from '../../constants';
import FieldPopover from '../common/FieldPopover';

const NAME_PATTERN = /^pvc-[a-f0-9-]{36}$/;

/** Generate a UUID v4 */
const generateUUID = (): string => {
  const hex = '0123456789abcdef';
  const sections = [8, 4, 4, 4, 12];
  return sections
    .map((len) =>
      Array.from({ length: len }, () => hex[Math.floor(Math.random() * 16)]).join(''),
    )
    .join('-');
};

interface FormState {
  name: string;
  poolName: string;
  requestedGB: number;
  pvcName: string;
  pvcNamespace: string;
  uid: string;
  gid: string;
  permissions: string;
  reclaimPolicy: 'Delete' | 'Retain' | 'Archive';
  sourceVolume: string;
}

const initialFormState: FormState = {
  name: '',
  poolName: '',
  requestedGB: 10,
  pvcName: '',
  pvcNamespace: '',
  uid: '',
  gid: '',
  permissions: '0755',
  reclaimPolicy: 'Delete',
  sourceVolume: '',
};

const SubVolumeCreatePage: React.FC = () => {
  const history = useHistory();
  const [form, setForm] = useState<FormState>(initialFormState);
  const [error, setError] = useState<string>('');
  const [creating, setCreating] = useState(false);

  const [pools, poolsLoaded] = useK8sWatchResource<FileSharePool[]>({
    groupVersionKind: {
      group: FileSharePoolModel.apiGroup,
      version: FileSharePoolModel.apiVersion,
      kind: FileSharePoolModel.kind,
    },
    isList: true,
  });

  // Watch existing SubVolumes for the source volume dropdown
  const [subVolumes, svLoaded] = useK8sWatchResource<SubVolume[]>({
    groupVersionKind: {
      group: SubVolumeModel.apiGroup,
      version: SubVolumeModel.apiVersion,
      kind: SubVolumeModel.kind,
    },
    isList: true,
  });

  const readyPools = (pools || []).filter((p) => p.status?.phase === 'Ready');

  // SubVolumes in the selected pool (for source volume selector)
  const poolSubVolumes = useMemo(
    () =>
      (subVolumes || []).filter(
        (sv) => sv.spec?.poolName === form.poolName && sv.status?.phase === 'Bound',
      ),
    [subVolumes, form.poolName],
  );

  const set = <K extends keyof FormState>(key: K, value: FormState[K]) =>
    setForm((prev) => ({ ...prev, [key]: value }));

  const generateName = () => {
    set('name', `pvc-${generateUUID()}`);
  };

  // Validation
  const isNameValid = form.name.length > 0 && NAME_PATTERN.test(form.name);
  const isFormValid =
    isNameValid &&
    form.poolName.length > 0 &&
    form.requestedGB >= 1 &&
    form.pvcName.length > 0 &&
    form.pvcNamespace.length > 0;

  // Capacity impact preview
  const selectedPool = readyPools.find((p) => p.metadata?.name === form.poolName);
  const currentAllocated = selectedPool?.status?.totalAllocatedGB ?? 0;
  const totalCapacity = selectedPool?.status?.totalCapacityGB ?? 0;
  const afterAllocated = currentAllocated + form.requestedGB;
  const afterPct = totalCapacity > 0 ? Math.round((afterAllocated / totalCapacity) * 100) : 0;

  const handleCreate = async () => {
    setError('');
    setCreating(true);
    try {
      const resource: SubVolume = {
        apiVersion: 'storage.ibmcloud.io/v1alpha1',
        kind: 'SubVolume',
        metadata: { name: form.name },
        spec: {
          poolName: form.poolName,
          shareID: '',
          shareMountTargetIP: '',
          subPath: `/pvcs/${form.name}`,
          requestedGB: form.requestedGB,
          pvName: form.name,
          pvcName: form.pvcName,
          pvcNamespace: form.pvcNamespace,
          reclaimPolicy: form.reclaimPolicy,
        },
      };

      if (form.uid !== '') {
        const uid = parseInt(form.uid, 10);
        if (!isNaN(uid)) resource.spec.uid = uid;
      }
      if (form.gid !== '') {
        const gid = parseInt(form.gid, 10);
        if (!isNaN(gid)) resource.spec.gid = gid;
      }
      if (form.permissions.trim()) {
        resource.spec.permissions = form.permissions.trim();
      }
      if (form.sourceVolume.trim()) {
        resource.spec.sourceVolume = form.sourceVolume.trim();
      }

      await k8sCreate({ model: SubVolumeModel, data: resource });
      history.push(ROUTES.SUBVOLUME_DETAIL.replace(':name', form.name));
    } catch (err: unknown) {
      const message =
        err instanceof Error
          ? err.message
          : typeof err === 'object' && err !== null && 'message' in err
            ? String((err as { message: unknown }).message)
            : 'Failed to create SubVolume';
      setError(message);
    } finally {
      setCreating(false);
    }
  };

  return (
    <PageSection variant="default">
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Create SubVolume
      </Title>

      <Alert
        variant="info"
        title="Advanced operation"
        isInline
        style={{ marginBottom: '1rem' }}
      >
        This form creates a SubVolume directly. For normal PVC provisioning, use a
        StorageClass that references a pool.
      </Alert>

      {error && (
        <Alert
          variant="danger"
          title="Creation failed"
          isInline
          style={{ marginBottom: '1rem' }}
        >
          {error}
        </Alert>
      )}

      <Form>
        <Stack hasGutter>
          <StackItem>
            <FieldPopover
              fieldId="sv-name"
              label="Name"
              description="Must follow the pvc-<UUID> naming convention (e.g. pvc-550e8400-e29b-41d4-a716-446655440000). This name is used as the PV name and the subdirectory name on the NFS share."
              isRequired
            >
              <Split hasGutter>
                <SplitItem isFilled>
                  <TextInput
                    id="sv-name"
                    value={form.name}
                    onChange={(_event, value) => set('name', value)}
                    isRequired
                    validated={
                      form.name.length === 0 || NAME_PATTERN.test(form.name)
                        ? 'default'
                        : 'error'
                    }
                    placeholder="pvc-550e8400-e29b-41d4-a716-446655440000"
                  />
                </SplitItem>
                <SplitItem>
                  <Button variant="secondary" onClick={generateName}>
                    Generate
                  </Button>
                </SplitItem>
              </Split>
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>{'Must match pattern: pvc-[a-f0-9-]{36}'}</HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="sv-pool"
              label="Pool Name"
              description="The FileSharePool that will host this SubVolume. Only pools in Ready phase are shown. The pool manager will allocate space on one of the pool's shares."
              isRequired
            >
              <FormSelect
                id="sv-pool"
                value={form.poolName}
                onChange={(_event, value) => set('poolName', value)}
                isRequired
              >
                <FormSelectOption value="" label="-- Select a pool --" isDisabled />
                {readyPools.map((p) => {
                  const pName = p.metadata?.name || '';
                  const alloc = p.status?.totalAllocatedGB ?? 0;
                  const cap = p.status?.totalCapacityGB ?? 0;
                  const shares = p.status?.shareCount ?? 0;
                  return (
                    <FormSelectOption
                      key={pName}
                      value={pName}
                      label={`${pName} (${alloc}/${cap} GB, ${shares} shares)`}
                    />
                  );
                })}
              </FormSelect>
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>
                    {poolsLoaded
                      ? `${readyPools.length} pool(s) in Ready phase`
                      : 'Loading pools...'}
                  </HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="sv-size"
              label="Requested Size (GB)"
              description="Logical size of this SubVolume. The pool manager tracks this for capacity accounting. Actual NFS usage may differ since NFS doesn't enforce per-directory quotas."
              isRequired
            >
              <NumberInput
                id="sv-size"
                value={form.requestedGB}
                min={1}
                onMinus={() => set('requestedGB', Math.max(1, form.requestedGB - 1))}
                onPlus={() => set('requestedGB', form.requestedGB + 1)}
                onChange={(event) => {
                  const val = parseInt(
                    (event.target as HTMLInputElement).value,
                    10,
                  );
                  if (!isNaN(val)) set('requestedGB', val);
                }}
              />
            </FieldPopover>
          </StackItem>

          {/* Capacity impact preview */}
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
                      <Label
                        color={afterPct >= 90 ? 'red' : afterPct >= 75 ? 'orange' : 'blue'}
                      >
                        {afterPct}% utilized
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
            <FieldPopover
              fieldId="sv-pvc-name"
              label="PVC Name"
              description="Name of the PersistentVolumeClaim this SubVolume backs. Used for tracking and display purposes."
              isRequired
            >
              <TextInput
                id="sv-pvc-name"
                value={form.pvcName}
                onChange={(_event, value) => set('pvcName', value)}
                isRequired
                placeholder="my-pvc"
              />
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="sv-pvc-namespace"
              label="PVC Namespace"
              description="Namespace of the PersistentVolumeClaim. Needed for the CSI driver to match SubVolumes to PVCs."
              isRequired
            >
              <TextInput
                id="sv-pvc-namespace"
                value={form.pvcNamespace}
                onChange={(_event, value) => set('pvcNamespace', value)}
                isRequired
                placeholder="default"
              />
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="sv-uid"
              label="UID"
              description="Owner UID for the subdirectory. Overrides the pool default. Set to 107 for KubeVirt VMs (QEMU user)."
            >
              <TextInput
                id="sv-uid"
                value={form.uid}
                onChange={(_event, value) => set('uid', value)}
                type="number"
                placeholder=""
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>Optional, e.g. 107 for KubeVirt</HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="sv-gid"
              label="GID"
              description="Owner GID for the subdirectory. Overrides the pool default."
            >
              <TextInput
                id="sv-gid"
                value={form.gid}
                onChange={(_event, value) => set('gid', value)}
                type="number"
                placeholder=""
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>Optional</HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="sv-permissions"
              label="Permissions"
              description="Unix permissions for the subdirectory. Overrides the pool default. Use 0755 for standard workloads, 0777 for KubeVirt."
            >
              <TextInput
                id="sv-permissions"
                value={form.permissions}
                onChange={(_event, value) => set('permissions', value)}
                placeholder="0755"
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>Octal, e.g. 0755</HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="sv-reclaim"
              label="Reclaim Policy"
              description="What happens when the PVC is deleted. Delete: remove subdirectory and data. Retain: keep data on the share. Archive: move to an archive directory."
              isRequired
            >
              <FormSelect
                id="sv-reclaim"
                value={form.reclaimPolicy}
                onChange={(_event, value) =>
                  set('reclaimPolicy', value as 'Delete' | 'Retain' | 'Archive')
                }
              >
                {RECLAIM_POLICIES.map((p) => (
                  <FormSelectOption key={p} value={p} label={p} />
                ))}
              </FormSelect>
            </FieldPopover>
          </StackItem>

          <StackItem>
            <FieldPopover
              fieldId="sv-source"
              label="Source Volume"
              description="Clone from an existing SubVolume. The clone worker will copy data from the source to this new SubVolume. Only Bound SubVolumes in the selected pool are shown."
            >
              <FormSelect
                id="sv-source"
                value={form.sourceVolume}
                onChange={(_event, value) => set('sourceVolume', value)}
                isDisabled={!form.poolName || !svLoaded}
              >
                <FormSelectOption value="" label="-- None (no clone) --" />
                {poolSubVolumes.map((sv) => {
                  const svName = sv.metadata?.name || '';
                  return (
                    <FormSelectOption
                      key={svName}
                      value={svName}
                      label={`${svName} (${sv.spec?.requestedGB ?? 0} GB)`}
                    />
                  );
                })}
              </FormSelect>
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>
                    {form.poolName
                      ? `${poolSubVolumes.length} Bound SubVolume(s) in pool`
                      : 'Select a pool first'}
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
                isDisabled={creating || !isFormValid}
                isLoading={creating}
              >
                Create
              </Button>
              <Button
                variant="link"
                onClick={() => history.push(ROUTES.SUBVOLUMES)}
                isDisabled={creating}
              >
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
