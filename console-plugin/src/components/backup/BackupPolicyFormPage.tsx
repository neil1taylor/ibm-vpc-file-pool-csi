import React, { useEffect, useMemo, useState } from 'react';
import { k8sCreate, k8sUpdate, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { useHistory, useParams } from 'react-router-dom';
import {
  ActionGroup,
  Alert,
  Bullseye,
  Button,
  Checkbox,
  FormHelperText,
  FormSelect,
  FormSelectOption,
  HelperText,
  HelperTextItem,
  NumberInput,
  PageSection,
  Spinner,
  Stack,
  StackItem,
  TextInput,
  Title,
} from '@patternfly/react-core';
import {
  BackupPolicyModel,
  FileSharePoolModel,
  PersistentVolumeClaimModel,
  SubVolumeModel,
  VirtualMachineModel,
} from '../../models';
import { API_GROUP, ROUTES } from '../../constants';
import { BackupPolicy, FileSharePool, SubVolume } from '../../types';
import {
  BackupPolicyForm,
  EMPTY_BACKUP_FORM,
  buildBackupPolicy,
  parseNamespaces,
  policyToForm,
  selectionPreview,
  snapshotPoolChoices,
  validateBackupPolicyForm,
} from '../../utils/backup-policy-form';
import { PersistentVolumeClaim, VirtualMachineLike, errorMessage } from '../../utils/k8s-objects';
import { toYAML } from '../../utils/yaml-lite';
import FieldPopover from '../common/FieldPopover';
import LabelPairsEditor from '../common/LabelPairsEditor';
import NoAccessNote from '../common/NoAccessNote';
import RunningVMCopyNote from '../common/RunningVMCopyNote';
import SchedulePresets from '../common/SchedulePresets';

const gvk = (model: { apiGroup?: string; apiVersion: string; kind: string }) => ({
  group: model.apiGroup,
  version: model.apiVersion,
  kind: model.kind,
});

const SCHEDULE_PRESETS = [
  { label: '1h', value: '1h' },
  { label: '6h', value: '6h' },
  { label: '12h', value: '12h' },
  { label: '24h', value: '24h' },
];

const KEEP_PRESETS = [
  { label: '1 day', value: '1d' },
  { label: '7 days', value: '7d' },
  { label: '30 days', value: '30d' },
];

const Helper: React.FC<{ error?: string; children?: React.ReactNode }> = ({ error, children }) => (
  <FormHelperText>
    <HelperText>
      {error && <HelperTextItem variant="error">{error}</HelperTextItem>}
      {children && <HelperTextItem>{children}</HelperTextItem>}
    </HelperText>
  </FormHelperText>
);

/** Makes a BackupPolicy, or edits one (the route has a name). */
const BackupPolicyFormPage: React.FC = () => {
  const { name: routeName } = useParams<{ name?: string }>();
  const editing = !!routeName;
  const history = useHistory();

  const [existing, existingLoaded, existingError] = useK8sWatchResource<BackupPolicy>(
    editing ? { groupVersionKind: gvk(BackupPolicyModel), name: routeName } : null,
  );
  const [policies] = useK8sWatchResource<BackupPolicy[]>({ groupVersionKind: gvk(BackupPolicyModel), isList: true });
  const [pools] = useK8sWatchResource<FileSharePool[]>({ groupVersionKind: gvk(FileSharePoolModel), isList: true });
  const [subvolumes] = useK8sWatchResource<SubVolume[]>({ groupVersionKind: gvk(SubVolumeModel), isList: true });

  const [form, setForm] = useState<BackupPolicyForm>(EMPTY_BACKUP_FORM);
  const [filled, setFilled] = useState(!editing);
  const [error, setError] = useState('');
  const [saving, setSaving] = useState(false);

  const [canWrite] = useAccessReview({
    group: API_GROUP,
    resource: 'backuppolicies',
    verb: editing ? 'update' : 'create',
  });

  useEffect(() => {
    if (editing && existing && !filled) {
      setForm(policyToForm(existing));
      setFilled(true);
    }
  }, [editing, existing, filled]);

  const labelSelector = useMemo(
    () => (form.labels.length > 0 ? { matchLabels: Object.fromEntries(form.labels.map((l) => [l.key, l.value])) } : null),
    [form.labels],
  );
  const namespaces = parseNamespaces(form.namespaces);
  const [matching] = useK8sWatchResource<PersistentVolumeClaim[]>(
    labelSelector ? { groupVersionKind: gvk(PersistentVolumeClaimModel), isList: true, selector: labelSelector } : null,
  );
  // KubeVirt may not be installed: then the watch fails, and there are no VMs to add.
  const [vms] = useK8sWatchResource<VirtualMachineLike[]>(
    labelSelector ? { groupVersionKind: gvk(VirtualMachineModel), isList: true } : null,
  );

  if (editing && existingError) {
    return (
      <PageSection>
        <Alert variant="danger" title="Error loading BackupPolicy" isInline>
          {existingError.message || 'An unexpected error occurred.'}
        </Alert>
      </PageSection>
    );
  }
  if (editing && !existingLoaded) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }
  if (editing && !existing) {
    return (
      <PageSection>
        <Alert variant="warning" title="Not found" isInline>
          BackupPolicy &quot;{routeName}&quot; was not found.
        </Alert>
      </PageSection>
    );
  }

  const hasExpressions = (existing?.spec?.selector?.matchExpressions?.length || 0) > 0;
  const errors = validateBackupPolicyForm(form, {
    existingNames: (policies || []).map((p) => p.metadata?.name || ''),
    editing,
    hasExpressions,
  });
  const valid = Object.keys(errors).length === 0;
  const choices = snapshotPoolChoices(pools || [], subvolumes || []);
  const preview = labelSelector ? selectionPreview(matching || [], vms || [], namespaces) : undefined;
  const set = <K extends keyof BackupPolicyForm>(key: K, value: BackupPolicyForm[K]) => setForm({ ...form, [key]: value });

  const policy = buildBackupPolicy(form, existing);
  const detailPath = ROUTES.BACKUP_DETAIL.replace(':name', form.name);

  const handleSave = async () => {
    setError('');
    setSaving(true);
    try {
      if (editing) await k8sUpdate({ model: BackupPolicyModel, data: policy });
      else await k8sCreate({ model: BackupPolicyModel, data: policy });
      history.push(detailPath);
    } catch (err: unknown) {
      setError(errorMessage(err, `Failed to ${editing ? 'save' : 'create'} the BackupPolicy`));
    } finally {
      setSaving(false);
    }
  };

  return (
    <PageSection variant="default">
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        {editing ? `Edit BackupPolicy ${routeName}` : 'Create BackupPolicy'}
      </Title>
      <Stack hasGutter>
        {error && (
          <StackItem>
            <Alert variant="danger" title={editing ? 'Save failed' : 'Creation failed'} isInline>
              {error}
            </Alert>
          </StackItem>
        )}

        <StackItem>
          <Alert variant="info" isInline title="Backups stop when the snapshot pool is more than 85% full">
            When a share of the snapshot pool is more than 85% full, the policy makes no new backups for the volumes on
            it, and its phase is Error. Backups are never deleted to make room. They start again by themselves when the
            use falls to 85% or less. Size the pool for one whole copy of the selected data plus the changes kept for
            the keep time.
          </Alert>
        </StackItem>

        <StackItem>
          <FieldPopover fieldId="bp-name" label="Name" description="The name of the policy. It is also used as a label value on the volumes it backs up, so it cannot be changed later." isRequired>
            <TextInput
              id="bp-name"
              value={form.name}
              onChange={(_e, v) => set('name', v)}
              isDisabled={editing}
              validated={form.name && errors.name ? 'error' : 'default'}
            />
            <Helper error={form.name ? errors.name : undefined} />
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="bp-pool"
            label="Snapshot pool"
            description="The pool that holds the backups. It must hold no workload claims, so a lost or full data share does not take the backups with it."
            isRequired
          >
            <FormSelect id="bp-pool" value={form.snapshotPool} onChange={(_e, v) => set('snapshotPool', v)}>
              <FormSelectOption value="" label="Choose a pool" isPlaceholder />
              {choices.map((c) => (
                <FormSelectOption
                  key={c.name}
                  value={c.name}
                  label={c.eligible ? c.name : `${c.name} (${c.reason})`}
                  isDisabled={!c.eligible}
                />
              ))}
            </FormSelect>
            <Helper>Only a pool with no workload claims and a ready share can hold backups.</Helper>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="bp-schedule"
            label="Schedule"
            description="How often a backup is made. A Go duration such as 30m, 6h or 1h30m, at least 1 minute. Only changed blocks of a VM disk are copied after the first backup."
            isRequired
          >
            <SchedulePresets presets={SCHEDULE_PRESETS} value={form.schedule} onChange={(v) => set('schedule', v)} inputId="bp-schedule" />
            <Helper error={errors.schedule} />
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="bp-keep"
            label="Keep for"
            description="How long backups are kept: a number of days such as 30d, or a duration such as 36h. At least 1 hour. A longer time needs more room in the snapshot pool."
            isRequired
          >
            <TextInput id="bp-keep" value={form.keepFor} onChange={(_e, v) => set('keepFor', v)} validated={errors.keepFor ? 'error' : 'default'} />
            <Helper error={errors.keepFor}>
              {KEEP_PRESETS.map((p) => (
                <Button key={p.value} variant="link" isInline onClick={() => set('keepFor', p.value)} style={{ marginRight: 12 }}>
                  {p.label}
                </Button>
              ))}
            </Helper>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="bp-labels"
            label="Labels"
            description="Volumes (claims) with all of these labels are backed up. A VM is picked through its disks: if any disk matches, all disks of the VM are backed up."
            isRequired
          >
            <LabelPairsEditor labels={form.labels} onChange={(labels) => set('labels', labels)} />
            <Helper error={errors.labels}>{preview?.text}</Helper>
            {hasExpressions && (
              <Helper>
                This policy also has match expressions. The form keeps them as they are; to change them, edit the YAML.
              </Helper>
            )}
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="bp-namespaces"
            label="Namespaces"
            description="Limit the backup to these namespaces, separated by commas. Empty means all namespaces."
          >
            <TextInput id="bp-namespaces" value={form.namespaces} onChange={(_e, v) => set('namespaces', v)} placeholder="all namespaces" />
          </FieldPopover>
        </StackItem>

        <StackItem>
          <RunningVMCopyNote />
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="bp-vm-speed"
            label="VM copy highest speed (MB/s per disk)"
            description="The copy of a running VM's disk starts slow, speeds up while the share is quiet, and slows down when the share gets busy. This is the highest speed it may reach. 0 means there is no highest speed. Default 0."
          >
            <NumberInput
              inputAriaLabel="VM copy highest speed"
              value={form.vmCopySpeed}
              min={0}
              max={10000}
              onMinus={() => set('vmCopySpeed', Math.max(0, form.vmCopySpeed - 5))}
              onPlus={() => set('vmCopySpeed', Math.min(10000, form.vmCopySpeed + 5))}
              onChange={(event) => {
                const val = parseInt((event.target as HTMLInputElement).value, 10);
                if (!isNaN(val)) set('vmCopySpeed', val);
              }}
            />
            <Helper error={errors.vmCopySpeed} />
          </FieldPopover>
        </StackItem>

        <StackItem>
          <Checkbox
            id="bp-suspend"
            label="Suspend: make no new backups (existing backups stay)"
            isChecked={form.suspend}
            onChange={(_e, checked) => set('suspend', checked)}
          />
        </StackItem>

        {existing?.spec?.export && (
          <StackItem>
            <Alert variant="info" isInline title="The export to COS is kept as it is">
              This policy sends its backups to the bucket {existing.spec.export.bucket}. Saving does not change that.
            </Alert>
          </StackItem>
        )}

        {canWrite === false && (
          <StackItem>
            <NoAccessNote right={`${editing ? 'update' : 'create'} BackupPolicy objects`} yaml={toYAML(policy)} />
          </StackItem>
        )}

        <StackItem>
          <ActionGroup>
            <Button variant="primary" onClick={handleSave} isDisabled={!valid || saving || !canWrite} isLoading={saving}>
              {editing ? 'Save' : 'Create'}
            </Button>
            <Button
              variant="link"
              onClick={() => history.push(editing ? ROUTES.BACKUP_DETAIL.replace(':name', routeName!) : ROUTES.BACKUPS)}
              isDisabled={saving}
            >
              Cancel
            </Button>
          </ActionGroup>
        </StackItem>
      </Stack>
    </PageSection>
  );
};

export default BackupPolicyFormPage;
