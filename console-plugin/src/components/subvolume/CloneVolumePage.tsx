import React, { useEffect, useState } from 'react';
import { k8sCreate, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Link, useHistory, useParams } from 'react-router-dom';
import {
  ActionGroup,
  Alert,
  Bullseye,
  Button,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  PageSection,
  Spinner,
  Stack,
  StackItem,
  TextInput,
  Title,
} from '@patternfly/react-core';
import {
  FileSharePoolModel,
  PersistentVolumeClaimModel,
  StorageClassModel,
  SubVolumeModel,
  VirtualMachineModel,
} from '../../models';
import { FileSharePool, SubVolume } from '../../types';
import { ROUTES } from '../../constants';
import {
  CSI_DRIVER_NAME,
  PersistentVolumeClaim,
  StorageClass,
  VirtualMachineLike,
  errorMessage,
} from '../../utils/k8s-objects';
import {
  RestoreForm,
  buildClonePVC,
  cloneDefaults,
  cloneDisabledReason,
  validateRestore,
  vmUsesPVC,
} from '../../utils/restore-helpers';
import FieldPopover from '../common/FieldPopover';
import RestoreProgressBanner from '../snapshot/RestoreProgressBanner';

const gvk = (model: { apiGroup?: string; apiVersion: string; kind: string }) => ({
  group: model.apiGroup,
  version: model.apiVersion,
  kind: model.kind,
});

const ACCESS_MODES = ['ReadWriteMany', 'ReadWriteOnce', 'ReadOnlyMany', 'ReadWriteOncePod'];
const VOLUME_MODES = ['Filesystem', 'Block'];

/** Clones a volume to a new claim: a PVC whose data source is the claim of the volume. */
const CloneVolumePage: React.FC = () => {
  const { name } = useParams<{ name: string }>();
  const history = useHistory();

  const [subvolume, subvolumeLoaded, subvolumeError] = useK8sWatchResource<SubVolume>({
    groupVersionKind: gvk(SubVolumeModel),
    name,
  });
  const [storageClasses, classesLoaded] = useK8sWatchResource<StorageClass[]>({
    groupVersionKind: gvk(StorageClassModel),
    isList: true,
  });

  const namespace = subvolume?.spec?.pvcNamespace;
  const [pvcs, pvcsLoaded] = useK8sWatchResource<PersistentVolumeClaim[]>(
    namespace ? { groupVersionKind: gvk(PersistentVolumeClaimModel), namespace, isList: true } : null,
  );
  const [pool] = useK8sWatchResource<FileSharePool>(
    subvolume ? { groupVersionKind: gvk(FileSharePoolModel), name: subvolume.spec?.poolName } : null,
  );
  // KubeVirt may not be installed: then the watch fails, and there are no VMs to warn about.
  const [vms, , vmsError] = useK8sWatchResource<VirtualMachineLike[]>(
    namespace ? { groupVersionKind: gvk(VirtualMachineModel), namespace, isList: true } : null,
  );

  const [form, setForm] = useState<RestoreForm | null>(null);
  const [error, setError] = useState('');
  const [creating, setCreating] = useState(false);
  const [createdName, setCreatedName] = useState('');

  const sourcePVCName = subvolume?.spec?.pvcName || '';
  const sourcePVC = (pvcs || []).find((p) => p.metadata?.name === sourcePVCName);
  const ready = subvolumeLoaded && classesLoaded && (!namespace || pvcsLoaded);

  useEffect(() => {
    if (form || !ready || !subvolume || !sourcePVC) return;
    setForm(cloneDefaults({ subvolume, sourcePVC, storageClasses: storageClasses || [] }));
  }, [form, ready, subvolume, sourcePVC, storageClasses]);

  if (subvolumeError) {
    return (
      <PageSection>
        <Alert variant="danger" title="Error loading SubVolume" isInline>
          {subvolumeError.message || 'An unexpected error occurred.'}
        </Alert>
      </PageSection>
    );
  }

  if (!subvolumeLoaded || !classesLoaded) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }

  if (!subvolume) {
    return (
      <PageSection>
        <Alert variant="warning" title="Not found" isInline>
          SubVolume &quot;{name}&quot; was not found.
        </Alert>
      </PageSection>
    );
  }

  const backPath = ROUTES.SUBVOLUME_DETAIL.replace(':name', name);
  const disabledReason =
    cloneDisabledReason(subvolume) ||
    (pvcsLoaded && !sourcePVC ? 'The claim data of this volume no longer exists, so it cannot be cloned.' : undefined);
  if (disabledReason) {
    return (
      <PageSection variant="default">
        <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
          Clone to new PVC
        </Title>
        <Alert variant="warning" title="This volume cannot be cloned yet" isInline>
          {disabledReason} <Link to={backPath}>Back to the volume</Link>
        </Alert>
      </PageSection>
    );
  }

  if (!form || !namespace) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }

  const minGiB = cloneDefaults({ subvolume, sourcePVC, storageClasses: storageClasses || [] }).sizeGiB;
  const errors = validateRestore(form, {
    existingNames: (pvcs || []).map((p) => p.metadata?.name || ''),
    minGiB,
    minLabel: "the volume's",
  });
  const valid = !errors.name && !errors.size && !!form.storageClass;
  const driverClasses = (storageClasses || []).filter((sc) => sc.provisioner === CSI_DRIVER_NAME);
  const showVMWarning = !vmsError && vmUsesPVC(vms || [], namespace, sourcePVCName);
  const freeGB = pool?.status ? pool.status.totalCapacityGB - pool.status.totalAllocatedGB : undefined;

  const set = <K extends keyof RestoreForm>(key: K, value: RestoreForm[K]) => setForm({ ...form, [key]: value });

  const handleClone = async () => {
    setError('');
    setCreating(true);
    try {
      await k8sCreate({
        model: PersistentVolumeClaimModel,
        data: buildClonePVC(form, { name: sourcePVCName, namespace }),
      });
      setCreatedName(form.name);
    } catch (err: unknown) {
      setError(errorMessage(err, 'Failed to create the claim'));
    } finally {
      setCreating(false);
    }
  };

  return (
    <PageSection variant="default">
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Clone to new PVC
      </Title>
      <Stack hasGutter>
        {createdName && (
          <StackItem>
            <RestoreProgressBanner namespace={namespace} name={createdName} source="volume" />
          </StackItem>
        )}

        {showVMWarning && (
          <StackItem>
            <Alert variant="warning" title="A VM uses this claim" isInline>
              A clone is a plain file copy made while the VM is running, so the cloned disk may not boot. For a safe
              copy, take a snapshot of the VM and restore it: a snapshot copies the disk at one moment.
            </Alert>
          </StackItem>
        )}

        {error && (
          <StackItem>
            <Alert variant="danger" title="Clone failed" isInline>
              {error}
            </Alert>
          </StackItem>
        )}

        <StackItem>
          <FieldPopover fieldId="clone-name" label="Name" description="The name of the new claim." isRequired>
            <TextInput
              id="clone-name"
              value={form.name}
              onChange={(_event, value) => set('name', value)}
              isRequired
              isDisabled={!!createdName}
              validated={errors.name ? 'error' : 'default'}
            />
            {errors.name && (
              <FormHelperText>
                <HelperText>
                  <HelperTextItem variant="error">{errors.name}</HelperTextItem>
                </HelperText>
              </FormHelperText>
            )}
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="clone-namespace"
            label="Namespace"
            description="A cloned claim must be in the namespace of the source claim. This is a Kubernetes rule."
          >
            <span id="clone-namespace">{namespace}</span>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="clone-size"
            label="Size (GiB)"
            description="The size of the new claim. It cannot be less than the source volume. A bigger size is allowed."
            isRequired
          >
            <TextInput
              id="clone-size"
              type="number"
              value={String(form.sizeGiB)}
              onChange={(_event, value) => set('sizeGiB', Number(value))}
              isRequired
              isDisabled={!!createdName}
              validated={errors.size ? 'error' : 'default'}
            />
            <FormHelperText>
              <HelperText>
                {errors.size && <HelperTextItem variant="error">{errors.size}</HelperTextItem>}
                {freeGB !== undefined && (
                  <HelperTextItem>
                    {freeGB} GB free in pool {subvolume.spec.poolName}
                  </HelperTextItem>
                )}
                {freeGB !== undefined && form.sizeGiB > freeGB && (
                  <HelperTextItem variant="warning">
                    The size is more than the free space in the pool. The claim stays Pending until the pool can hold
                    it. The pool may add a share if auto-expand is on.
                  </HelperTextItem>
                )}
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="clone-class"
            label="Storage class"
            description="Only storage classes of this driver are offered. The default is the class of the source claim."
            isRequired
          >
            <FormSelect
              id="clone-class"
              value={form.storageClass}
              onChange={(_event, value) => set('storageClass', value)}
              isDisabled={!!createdName}
            >
              {driverClasses.map((sc) => (
                <FormSelectOption key={sc.metadata?.name} value={sc.metadata?.name} label={sc.metadata?.name || ''} />
              ))}
            </FormSelect>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover fieldId="clone-access" label="Access mode" description="Copied from the source claim.">
            <FormSelect
              id="clone-access"
              value={form.accessMode}
              onChange={(_event, value) => set('accessMode', value)}
              isDisabled={!!createdName}
            >
              {ACCESS_MODES.map((m) => (
                <FormSelectOption key={m} value={m} label={m} />
              ))}
            </FormSelect>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover fieldId="clone-volume-mode" label="Volume mode" description="Copied from the source claim.">
            <FormSelect
              id="clone-volume-mode"
              value={form.volumeMode}
              onChange={(_event, value) => set('volumeMode', value)}
              isDisabled={!!createdName}
            >
              {VOLUME_MODES.map((m) => (
                <FormSelectOption key={m} value={m} label={m} />
              ))}
            </FormSelect>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <ActionGroup>
            {!createdName && (
              <Button variant="primary" onClick={handleClone} isDisabled={creating || !valid} isLoading={creating}>
                Clone
              </Button>
            )}
            <Button
              variant="link"
              onClick={() => history.push(createdName ? ROUTES.SUBVOLUMES : backPath)}
              isDisabled={creating}
            >
              {createdName ? 'Back to volumes' : 'Cancel'}
            </Button>
          </ActionGroup>
        </StackItem>
      </Stack>
    </PageSection>
  );
};

export default CloneVolumePage;
