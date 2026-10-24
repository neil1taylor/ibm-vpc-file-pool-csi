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
  SnapshotModel,
  StorageClassModel,
  VirtualMachineModel,
  VolumeSnapshotContentModel,
  VolumeSnapshotModel,
} from '../../models';
import { FileSharePool, Snapshot } from '../../types';
import { ROUTES } from '../../constants';
import {
  PersistentVolumeClaim,
  StorageClass,
  VirtualMachineLike,
  VolumeSnapshot,
  VolumeSnapshotContent,
  errorMessage,
} from '../../utils/k8s-objects';
import {
  RestoreForm,
  buildRestorePVC,
  findVolumeSnapshotRef,
  restoreDefaults,
  restoreDisabledReason,
  validateRestore,
  vmUsesPVC,
} from '../../utils/restore-helpers';
import FieldPopover from '../common/FieldPopover';
import RestoreProgressBanner from './RestoreProgressBanner';
import NoAccessNote from '../common/NoAccessNote';
import { useBackupPolicies } from '../../utils/use-backup-policies';
import { volumeClasses } from '../../utils/snapshot-pools';

const gvk = (model: { apiGroup?: string; apiVersion: string; kind: string }) => ({
  group: model.apiGroup,
  version: model.apiVersion,
  kind: model.kind,
});

const ACCESS_MODES = ['ReadWriteMany', 'ReadWriteOnce', 'ReadOnlyMany', 'ReadWriteOncePod'];
const VOLUME_MODES = ['Filesystem', 'Block'];

/** Restores a snapshot to a new claim: a PVC whose data source is the snapshot's VolumeSnapshot. */
const RestoreSnapshotPage: React.FC = () => {
  const { name } = useParams<{ name: string }>();
  const history = useHistory();

  const [snapshot, snapshotLoaded, snapshotError] = useK8sWatchResource<Snapshot>({
    groupVersionKind: gvk(SnapshotModel),
    name,
  });
  const [contents, contentsLoaded, contentsError] = useK8sWatchResource<VolumeSnapshotContent[]>({
    groupVersionKind: gvk(VolumeSnapshotContentModel),
    isList: true,
  });
  const policies = useBackupPolicies();
  const [storageClasses, classesLoaded] = useK8sWatchResource<StorageClass[]>({
    groupVersionKind: gvk(StorageClassModel),
    isList: true,
  });

  const ref = snapshot ? findVolumeSnapshotRef(snapshot, contents || []) : undefined;
  const namespace = ref?.namespace;

  const [volumeSnapshot] = useK8sWatchResource<VolumeSnapshot>(
    ref ? { groupVersionKind: gvk(VolumeSnapshotModel), namespace: ref.namespace, name: ref.name } : null,
  );
  const [pvcs, pvcsLoaded] = useK8sWatchResource<PersistentVolumeClaim[]>(
    namespace ? { groupVersionKind: gvk(PersistentVolumeClaimModel), namespace, isList: true } : null,
  );
  const [pool] = useK8sWatchResource<FileSharePool>(
    snapshot ? { groupVersionKind: gvk(FileSharePoolModel), name: snapshot.spec?.poolName } : null,
  );
  // KubeVirt may not be installed: then the watch fails, and there are no VMs to warn about.
  const [vms, , vmsError] = useK8sWatchResource<VirtualMachineLike[]>(
    namespace ? { groupVersionKind: gvk(VirtualMachineModel), namespace, isList: true } : null,
  );

  const [form, setForm] = useState<RestoreForm | null>(null);
  const [error, setError] = useState('');
  const [creating, setCreating] = useState(false);
  const [createdName, setCreatedName] = useState('');

  const sourcePVCName = volumeSnapshot?.spec?.source?.persistentVolumeClaimName || snapshot?.spec?.sourceSubVolume || '';
  const sourcePVC = (pvcs || []).find((p) => p.metadata?.name === sourcePVCName);
  const ready = snapshotLoaded && contentsLoaded && classesLoaded && (!namespace || (pvcsLoaded && !!volumeSnapshot));

  useEffect(() => {
    if (form || !ready || !snapshot || !ref) return;
    setForm(restoreDefaults({ snapshot, sourcePVC, sourcePVCName, storageClasses: storageClasses || [] }));
  }, [form, ready, snapshot, ref, sourcePVC, sourcePVCName, storageClasses]);

  if (snapshotError) {
    return (
      <PageSection>
        <Alert variant="danger" title="Error loading Snapshot" isInline>
          {snapshotError.message || 'An unexpected error occurred.'}
        </Alert>
      </PageSection>
    );
  }

  if (contentsError) {
    return (
      <PageSection>
        <NoAccessNote right="read VolumeSnapshotContents, which the page needs to find the snapshot's VolumeSnapshot" />
      </PageSection>
    );
  }

  if (!snapshotLoaded || !contentsLoaded) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }

  if (!snapshot) {
    return (
      <PageSection>
        <Alert variant="warning" title="Not found" isInline>
          Snapshot &quot;{name}&quot; was not found.
        </Alert>
      </PageSection>
    );
  }

  const backPath = ROUTES.SNAPSHOT_DETAIL.replace(':name', name);
  const disabledReason = restoreDisabledReason(snapshot, ref);
  if (disabledReason) {
    return (
      <PageSection variant="default">
        <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
          Restore to new PVC
        </Title>
        <Alert variant="warning" title="This snapshot cannot be restored yet" isInline>
          {disabledReason} <Link to={backPath}>Back to the snapshot</Link>
        </Alert>
      </PageSection>
    );
  }

  if (!form || !ref) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }

  const minGiB = restoreDefaults({ snapshot, sourcePVC, sourcePVCName, storageClasses: storageClasses || [] }).sizeGiB;
  const errors = validateRestore(form, {
    // The claim this page made is not a name taken by someone else.
    existingNames: (pvcs || []).map((p) => p.metadata?.name || '').filter((n) => n !== createdName),
    minGiB,
  });
  const valid = !errors.name && !errors.size && !!form.storageClass;
  const driverClasses = volumeClasses(storageClasses || [], policies);
  const showVMWarning = !vmsError && vmUsesPVC(vms || [], ref.namespace, sourcePVCName);
  const freeGB = pool?.status ? pool.status.totalCapacityGB - pool.status.totalAllocatedGB : undefined;

  const set = <K extends keyof RestoreForm>(key: K, value: RestoreForm[K]) => setForm({ ...form, [key]: value });

  const handleRestore = async () => {
    setError('');
    setCreating(true);
    try {
      await k8sCreate({ model: PersistentVolumeClaimModel, data: buildRestorePVC(form, ref) });
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
        Restore to new PVC
      </Title>
      <Stack hasGutter>
        {createdName && (
          <StackItem>
            <RestoreProgressBanner namespace={ref.namespace} name={createdName} />
          </StackItem>
        )}

        {showVMWarning && (
          <StackItem>
            <Alert variant="warning" title="A VM uses the source claim" isInline>
              This snapshot is of a claim that a VM uses. If it was taken while a VM was running, the driver froze the
              guest and copied the disk at one moment. If the guest could not freeze, the copy is crash consistent: like
              a sudden power cut, so the guest may need to repair its file system when it starts. The snapshot&apos;s
              status says which.
            </Alert>
          </StackItem>
        )}

        {error && (
          <StackItem>
            <Alert variant="danger" title="Restore failed" isInline>
              {error}
            </Alert>
          </StackItem>
        )}

        <StackItem>
          <FieldPopover
            fieldId="restore-name"
            label="Name"
            description="The name of the new claim."
            isRequired
          >
            <TextInput
              id="restore-name"
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
            fieldId="restore-namespace"
            label="Namespace"
            description="A restored claim must be in the namespace of the snapshot. This is a Kubernetes rule."
          >
            <span id="restore-namespace">{ref.namespace}</span>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="restore-size"
            label="Size (GiB)"
            description="The size of the new claim. It cannot be less than the snapshot. A bigger size is allowed."
            isRequired
          >
            <TextInput
              id="restore-size"
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
                    {freeGB} GB free in pool {snapshot.spec.poolName}
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
            fieldId="restore-class"
            label="Storage class"
            description="Only storage classes of this driver are offered. The default is the class of the source claim."
            isRequired
          >
            <FormSelect
              id="restore-class"
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
          <FieldPopover
            fieldId="restore-access"
            label="Access mode"
            description="Copied from the source claim when it still exists."
          >
            <FormSelect
              id="restore-access"
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
          <FieldPopover
            fieldId="restore-volume-mode"
            label="Volume mode"
            description="Copied from the source claim when it still exists."
          >
            <FormSelect
              id="restore-volume-mode"
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
              <Button variant="primary" onClick={handleRestore} isDisabled={creating || !valid} isLoading={creating}>
                Restore
              </Button>
            )}
            <Button variant="link" onClick={() => history.push(createdName ? ROUTES.SNAPSHOTS : backPath)} isDisabled={creating}>
              {createdName ? 'Back to snapshots' : 'Cancel'}
            </Button>
          </ActionGroup>
        </StackItem>
      </Stack>
    </PageSection>
  );
};

export default RestoreSnapshotPage;
