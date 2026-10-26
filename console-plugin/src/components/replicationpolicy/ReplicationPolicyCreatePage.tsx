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
  TextArea,
  NumberInput,
  Switch,
  ActionGroup,
  Button,
  Stack,
  StackItem,
  Card,
  CardTitle,
  CardBody,
  Label,
  Split,
  SplitItem,
  FormSelect,
  FormSelectOption,
} from '@patternfly/react-core';
import { FileSharePoolModel, ReplicationPolicyModel } from '../../models';
import {
  FileSharePool,
  ReplicationPolicy,
  ReplicationPolicySpec,
  Hook,
  LabelSelector,
} from '../../types';
import { ROUTES } from '../../constants';
import FieldPopover from '../common/FieldPopover';
import SchedulePresets from '../common/SchedulePresets';
import HookEditor from '../common/HookEditor';
import ReceiverSetup from './ReceiverSetup';
import {
  DEFAULT_RESTORE_POINT_KEEP_HOURS,
  DEFAULT_VM_COPY_SPEED_MBPS,
  destinationSpecFields,
  vmCopySpecFields,
} from '../../utils/replication-helpers';
import RunningVMCopyNote from '../common/RunningVMCopyNote';

const SCHEDULE_PRESETS = [
  { label: '5m', value: '5m' },
  { label: '15m', value: '15m' },
  { label: '1h', value: '1h' },
  { label: '6h', value: '6h' },
  { label: '24h', value: '24h' },
];

const ReplicationPolicyCreatePage: React.FC = () => {
  const history = useHistory();

  const [name, setName] = useState('');
  const [replicationMode, setReplicationMode] = useState<'direct' | 'driver-to-driver'>('direct');
  const [sourcePoolName, setSourcePoolName] = useState('');
  const [destinationNFSServer, setDestinationNFSServer] = useState('');
  const [destinationExportPath, setDestinationExportPath] = useState('/');
  const [destinationBasePath, setDestinationBasePath] = useState('/pvcs');
  const [destinationEndpoint, setDestinationEndpoint] = useState('');
  const [destinationAuthSecretRef, setDestinationAuthSecretRef] = useState('');
  const [destinationCACertSecretRef, setDestinationCACertSecretRef] = useState('');
  const [schedule, setSchedule] = useState('15m');
  const [maxRetries, setMaxRetries] = useState(3);
  const [incrementalSync, setIncrementalSync] = useState(true);
  const [bandwidthLimitMbps, setBandwidthLimitMbps] = useState(0);
  const [maxParallelSyncs, setMaxParallelSyncs] = useState(1);
  const [vmCopySpeedLimitMBps, setVmCopySpeedLimitMBps] = useState(DEFAULT_VM_COPY_SPEED_MBPS);
  const [restorePointKeepHours, setRestorePointKeepHours] = useState(DEFAULT_RESTORE_POINT_KEEP_HOURS);
  const [rsyncOptions, setRsyncOptions] = useState('');

  const isDriverToDriver = replicationMode === 'driver-to-driver';
  // The receiver passed the check (or the user cannot run it). A policy for a receiver is not made before that.
  const [receiverChecked, setReceiverChecked] = useState(false);

  // Label selector as key-value pairs
  const [labelPairs, setLabelPairs] = useState<Array<{ key: string; value: string }>>([]);
  const [newLabelKey, setNewLabelKey] = useState('');
  const [newLabelValue, setNewLabelValue] = useState('');

  const [preSyncHooks, setPreSyncHooks] = useState<Hook[]>([]);
  const [postSyncHooks, setPostSyncHooks] = useState<Hook[]>([]);
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

  const addLabel = () => {
    if (newLabelKey.trim() && newLabelValue.trim()) {
      setLabelPairs((prev) => [...prev, { key: newLabelKey.trim(), value: newLabelValue.trim() }]);
      setNewLabelKey('');
      setNewLabelValue('');
    }
  };

  const removeLabel = (index: number) => {
    setLabelPairs((prev) => prev.filter((_, i) => i !== index));
  };

  const isValid =
    name.length > 0 &&
    sourcePoolName.length > 0 &&
    destinationBasePath.length > 0 &&
    schedule.length > 0 &&
    (isDriverToDriver
      ? destinationEndpoint.length > 0 && destinationAuthSecretRef.length > 0 && receiverChecked
      : destinationNFSServer.length > 0);

  const handleCreate = async () => {
    setError('');
    setCreating(true);
    try {
      const spec: ReplicationPolicySpec = {
        sourcePoolName,
        destinationExportPath: isDriverToDriver ? undefined : (destinationExportPath !== '/' ? destinationExportPath : undefined),
        destinationBasePath,
        schedule,
        maxRetries,
        incrementalSync: isDriverToDriver ? undefined : incrementalSync,
        bandwidthLimitMbps: isDriverToDriver ? undefined : (bandwidthLimitMbps > 0 ? bandwidthLimitMbps : undefined),
        maxParallelSyncs: maxParallelSyncs > 1 ? maxParallelSyncs : undefined,
        rsyncOptions: isDriverToDriver ? undefined : (rsyncOptions.trim() ? rsyncOptions.trim().split('\n').filter(Boolean) : undefined),
        ...vmCopySpecFields({ speedLimitMBps: vmCopySpeedLimitMBps, restorePointKeepHours }),
        ...destinationSpecFields({
          isDriverToDriver,
          nfsServer: destinationNFSServer,
          endpoint: destinationEndpoint,
          authSecretRef: destinationAuthSecretRef,
          caCertSecretRef: destinationCACertSecretRef,
        }),
      };

      if (labelPairs.length > 0) {
        const matchLabels: Record<string, string> = {};
        for (const pair of labelPairs) {
          matchLabels[pair.key] = pair.value;
        }
        const selector: LabelSelector = { matchLabels };
        spec.subVolumeSelector = selector;
      }

      if (preSyncHooks.length > 0) spec.preSyncHooks = preSyncHooks;
      if (postSyncHooks.length > 0) spec.postSyncHooks = postSyncHooks;

      const resource: ReplicationPolicy = {
        apiVersion: 'storage.ibmcloud.io/v1alpha1',
        kind: 'ReplicationPolicy',
        metadata: { name },
        spec,
      };

      await k8sCreate({ model: ReplicationPolicyModel, data: resource });
      history.push(ROUTES.REPLICATION_DETAIL.replace(':name', name));
    } catch (err: unknown) {
      const message =
        err instanceof Error
          ? err.message
          : typeof err === 'object' && err !== null && 'message' in err
            ? String((err as { message: unknown }).message)
            : 'Failed to create ReplicationPolicy';
      setError(message);
    } finally {
      setCreating(false);
    }
  };

  return (
    <PageSection variant="default">
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Create ReplicationPolicy
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
            fieldId="rp-name"
            label="Name"
            description="Unique name for this replication policy. The replication controller uses this to track sync state and report metrics."
            isRequired
          >
            <TextInput
              id="rp-name"
              value={name}
              onChange={(_event, value) => setName(value)}
              isRequired
              placeholder="my-replication-policy"
            />
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="rp-source-pool"
            label="Source Pool Name"
            description="The pool whose SubVolumes will be replicated. The replication controller watches SubVolumes in this pool and syncs their data to the destination."
            isRequired
          >
            <FormSelect
              id="rp-source-pool"
              value={sourcePoolName}
              onChange={(_event, value) => setSourcePoolName(value)}
              isDisabled={!poolsLoaded}
            >
              <FormSelectOption value="" label="-- Select a pool --" isDisabled />
              {(pools || []).map((pool) => {
                const pName = pool.metadata?.name || '';
                const svCount = pool.status?.totalPVCCount ?? 0;
                return (
                  <FormSelectOption
                    key={pName}
                    value={pName}
                    label={`${pName} (${svCount} PVCs)`}
                  />
                );
              })}
            </FormSelect>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="rp-mode"
            label="Replication Mode"
            description="Direct NFS mounts both source and destination shares in a Job (single-region or Transit Gateway). Driver-to-Driver uploads data via HTTPS to a receiver on the destination cluster (cross-region)."
            isRequired
          >
            <FormSelect
              id="rp-mode"
              value={replicationMode}
              onChange={(_event, value) => {
                setReplicationMode(value as 'direct' | 'driver-to-driver');
                // The receiver setup starts over when it is shown again.
                setReceiverChecked(false);
              }}
            >
              <FormSelectOption value="direct" label="Direct NFS" />
              <FormSelectOption value="driver-to-driver" label="Driver-to-Driver (cross-region)" />
            </FormSelect>
          </FieldPopover>
        </StackItem>

        {!isDriverToDriver && (
          <>
            <StackItem>
              <FieldPopover
                fieldId="rp-dest-server"
                label="Destination NFS Server"
                description="Hostname or IP of the remote NFS server for replication. This is typically a VPC file share mount target IP in the disaster recovery region."
                isRequired
              >
                <TextInput
                  id="rp-dest-server"
                  value={destinationNFSServer}
                  onChange={(_event, value) => setDestinationNFSServer(value)}
                  isRequired
                  placeholder="10.240.0.50"
                />
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem>Hostname or IP of the destination NFS server</HelperTextItem>
                  </HelperText>
                </FormHelperText>
              </FieldPopover>
            </StackItem>

            <StackItem>
              <FieldPopover
                fieldId="rp-dest-export-path"
                label="Destination Export Path"
                description="NFS export path on the destination server. For VPC access-mode shares, this differentiates shares under a shared FQDN. Defaults to '/' if not specified."
              >
                <TextInput
                  id="rp-dest-export-path"
                  value={destinationExportPath}
                  onChange={(_event, value) => setDestinationExportPath(value)}
                  placeholder="/"
                />
                <FormHelperText>
                  <HelperText>
                    <HelperTextItem>NFS export path (e.g. /share_abc123). Defaults to /</HelperTextItem>
                  </HelperText>
                </FormHelperText>
              </FieldPopover>
            </StackItem>
          </>
        )}

        {isDriverToDriver && (
          <StackItem>
            <ReceiverSetup
              policyName={name}
              endpoint={destinationEndpoint}
              onEndpointChange={setDestinationEndpoint}
              authSecretRef={destinationAuthSecretRef}
              onAuthSecretRefChange={setDestinationAuthSecretRef}
              caCertSecretRef={destinationCACertSecretRef}
              onCaCertSecretRefChange={setDestinationCACertSecretRef}
              onCheckedChange={setReceiverChecked}
            />
          </StackItem>
        )}

        <StackItem>
          <FieldPopover
            fieldId="rp-dest-path"
            label="Destination Base Path"
            description="Base path on the destination where replicated data will be stored. SubVolume directories are created under this path."
            isRequired
          >
            <TextInput
              id="rp-dest-path"
              value={destinationBasePath}
              onChange={(_event, value) => setDestinationBasePath(value)}
              isRequired
              placeholder="/pvcs"
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>Base path on the destination</HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="rp-schedule"
            label="Schedule"
            description="How often to sync data to the destination. Shorter intervals mean less data loss in a disaster (lower RPO) but increase network and NFS load. Use a Go duration format."
            isRequired
          >
            <SchedulePresets
              presets={SCHEDULE_PRESETS}
              value={schedule}
              onChange={setSchedule}
              inputId="rp-schedule"
            />
            <FormHelperText>
              <HelperText>
                <HelperTextItem>Sync interval as a Go duration, e.g. 15m, 1h, 30s</HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="rp-max-retries"
            label="Max Retries"
            description="Number of times to retry a failed sync before marking the policy as Failed. After max retries, the consecutive failures counter increments and alerts fire."
          >
            <NumberInput
              id="rp-max-retries"
              value={maxRetries}
              min={0}
              max={100}
              onMinus={() => setMaxRetries(Math.max(0, maxRetries - 1))}
              onPlus={() => setMaxRetries(Math.min(100, maxRetries + 1))}
              onChange={(event) => {
                const val = parseInt((event.target as HTMLInputElement).value, 10);
                if (!isNaN(val)) setMaxRetries(val);
              }}
            />
          </FieldPopover>
        </StackItem>

        {!isDriverToDriver && (
          <StackItem>
            <FieldPopover
              fieldId="rp-incremental"
              label="Incremental Sync"
              description="Use rsync delta transfer to only sync changed blocks. Much faster than full copies for subsequent syncs. Disable only if you need byte-exact copies every time."
            >
              <Switch
                id="rp-incremental"
                label="Incremental sync (rsync delta transfer)"
                isChecked={incrementalSync}
                onChange={(_event, checked) => setIncrementalSync(checked)}
              />
            </FieldPopover>
          </StackItem>
        )}

        {!isDriverToDriver && (
          <StackItem>
            <FieldPopover
              fieldId="rp-bandwidth"
              label="Bandwidth Limit (Mbps)"
              description="Limit rsync bandwidth in megabits per second. Only applies when incremental sync is enabled. 0 means no limit."
            >
              <NumberInput
                id="rp-bandwidth"
                value={bandwidthLimitMbps}
                min={0}
                max={10000}
                onMinus={() => setBandwidthLimitMbps(Math.max(0, bandwidthLimitMbps - 10))}
                onPlus={() => setBandwidthLimitMbps(Math.min(10000, bandwidthLimitMbps + 10))}
                onChange={(event) => {
                  const val = parseInt((event.target as HTMLInputElement).value, 10);
                  if (!isNaN(val)) setBandwidthLimitMbps(val);
                }}
                isDisabled={!incrementalSync}
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>0 = unlimited. Only applies with incremental sync.</HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>
        )}

        <StackItem>
          <FieldPopover
            fieldId="rp-parallel"
            label="Max Parallel Syncs"
            description="How many SubVolumes to sync concurrently. Higher values reduce total sync time but increase NFS and network load."
          >
            <NumberInput
              id="rp-parallel"
              value={maxParallelSyncs}
              min={1}
              max={32}
              onMinus={() => setMaxParallelSyncs(Math.max(1, maxParallelSyncs - 1))}
              onPlus={() => setMaxParallelSyncs(Math.min(32, maxParallelSyncs + 1))}
              onChange={(event) => {
                const val = parseInt((event.target as HTMLInputElement).value, 10);
                if (!isNaN(val)) setMaxParallelSyncs(val);
              }}
            />
          </FieldPopover>
        </StackItem>

        <StackItem>
          <RunningVMCopyNote />
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="rp-vm-speed"
            label="VM Copy Highest Speed (MB/s per disk)"
            description="The copy of a running VM's disk starts slow, speeds up while the share is quiet, and slows down when the share gets busy. This is the highest speed it may reach. 0 means there is no highest speed. Default 0."
          >
            <NumberInput
              id="rp-vm-speed"
              value={vmCopySpeedLimitMBps}
              min={0}
              max={10000}
              onMinus={() => setVmCopySpeedLimitMBps(Math.max(0, vmCopySpeedLimitMBps - 5))}
              onPlus={() => setVmCopySpeedLimitMBps(Math.min(10000, vmCopySpeedLimitMBps + 5))}
              onChange={(event) => {
                const val = parseInt((event.target as HTMLInputElement).value, 10);
                if (!isNaN(val)) setVmCopySpeedLimitMBps(val);
              }}
            />
          </FieldPopover>
        </StackItem>

        <StackItem>
          <FieldPopover
            fieldId="rp-vm-keep"
            label="Restore Point Keep Time (hours)"
            description="How long the destination keeps earlier copies of a running VM's disks, so you can recover to an earlier moment. Longer needs more space at the destination. Default 24."
          >
            <NumberInput
              id="rp-vm-keep"
              value={restorePointKeepHours}
              min={1}
              max={8760}
              onMinus={() => setRestorePointKeepHours(Math.max(1, restorePointKeepHours - 1))}
              onPlus={() => setRestorePointKeepHours(Math.min(8760, restorePointKeepHours + 1))}
              onChange={(event) => {
                const val = parseInt((event.target as HTMLInputElement).value, 10);
                if (!isNaN(val)) setRestorePointKeepHours(val);
              }}
            />
          </FieldPopover>
        </StackItem>

        {!isDriverToDriver && (
          <StackItem>
            <FieldPopover
              fieldId="rp-rsync-options"
              label="Rsync Options"
              description="Extra rsync flags, one per line. These are appended after the base flags (-a --delete). Dangerous flags like --daemon, --server, --rsh, --rsync-path are rejected."
            >
              <TextArea
                id="rp-rsync-options"
                value={rsyncOptions}
                onChange={(_event, value) => setRsyncOptions(value)}
                placeholder="--compress&#10;--checksum"
                rows={3}
                isDisabled={!incrementalSync}
              />
              <FormHelperText>
                <HelperText>
                  <HelperTextItem>One flag per line. Only applies with incremental sync.</HelperTextItem>
                </HelperText>
              </FormHelperText>
            </FieldPopover>
          </StackItem>
        )}

        <StackItem>
          <FieldPopover
            fieldId="rp-match-labels"
            label="SubVolume Selector"
            description="Filter which SubVolumes in the pool are replicated using label selectors. If no labels are specified, all SubVolumes in the pool are replicated."
          >
            <Card isPlain isCompact>
              <CardBody>
                <Stack hasGutter>
                  {/* Existing labels as tags */}
                  {labelPairs.length > 0 && (
                    <StackItem>
                      <Split hasGutter>
                        {labelPairs.map((pair, index) => (
                          <SplitItem key={index}>
                            <Label
                              onClose={() => removeLabel(index)}
                              closeBtnAriaLabel={`Remove ${pair.key}`}
                            >
                              {pair.key}={pair.value}
                            </Label>
                          </SplitItem>
                        ))}
                      </Split>
                    </StackItem>
                  )}
                  {/* Add new label */}
                  <StackItem>
                    <Split hasGutter>
                      <SplitItem>
                        <TextInput
                          id="rp-label-key"
                          value={newLabelKey}
                          onChange={(_event, value) => setNewLabelKey(value)}
                          placeholder="key"
                          style={{ width: 150 }}
                        />
                      </SplitItem>
                      <SplitItem>=</SplitItem>
                      <SplitItem>
                        <TextInput
                          id="rp-label-value"
                          value={newLabelValue}
                          onChange={(_event, value) => setNewLabelValue(value)}
                          placeholder="value"
                          style={{ width: 150 }}
                          onKeyDown={(e) => {
                            if (e.key === 'Enter') {
                              e.preventDefault();
                              addLabel();
                            }
                          }}
                        />
                      </SplitItem>
                      <SplitItem>
                        <Button
                          variant="secondary"
                          size="sm"
                          onClick={addLabel}
                          isDisabled={!newLabelKey.trim() || !newLabelValue.trim()}
                        >
                          Add
                        </Button>
                      </SplitItem>
                    </Split>
                  </StackItem>
                </Stack>
              </CardBody>
            </Card>
            <FormHelperText>
              <HelperText>
                <HelperTextItem>
                  {labelPairs.length === 0
                    ? 'No labels — all SubVolumes in the pool will be replicated'
                    : `${labelPairs.length} label(s) configured`}
                </HelperTextItem>
              </HelperText>
            </FormHelperText>
          </FieldPopover>
        </StackItem>

        <StackItem>
          <Card isPlain>
            <CardTitle>
              <FieldPopover
                fieldId="rp-hooks"
                label="Hooks"
                description="Execute commands or HTTP webhooks before and after each replication sync. Pre-sync hooks can quiesce applications. Post-sync hooks can verify data integrity or send notifications."
              >
                <span />
              </FieldPopover>
            </CardTitle>
            <CardBody>
              <Stack hasGutter>
                <StackItem>
                  <HookEditor
                    hooks={preSyncHooks}
                    onChange={setPreSyncHooks}
                    label="Pre-Sync Hooks"
                  />
                </StackItem>
                <StackItem>
                  <HookEditor
                    hooks={postSyncHooks}
                    onChange={setPostSyncHooks}
                    label="Post-Sync Hooks"
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

export default ReplicationPolicyCreatePage;
