import React, { useState } from 'react';
import { useK8sWatchResource, k8sUpdate } from '@openshift-console/dynamic-plugin-sdk';
import { useParams, useHistory } from 'react-router-dom';
import {
  PageSection,
  Title,
  DescriptionList,
  DescriptionListGroup,
  DescriptionListTerm,
  DescriptionListDescription,
  Grid,
  GridItem,
  Card,
  CardTitle,
  CardBody,
  Label,
  Spinner,
  Alert,
  Bullseye,
  Button,
  Split,
  SplitItem,
} from '@patternfly/react-core';
import {
  Table,
  Thead,
  Tr,
  Th,
  Tbody,
  Td,
} from '@patternfly/react-table';
import { ReplicationPolicyModel } from '../../models';
import { ReplicationPolicy, Hook } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import DeleteModal from '../common/DeleteModal';
import { formatAge, formatDuration, humanizeBytes } from '../../utils/resource-helpers';
import { ROUTES } from '../../constants';

const renderHooksReadOnly = (hooks: Hook[] | undefined, title: string) => {
  if (!hooks || hooks.length === 0) {
    return null;
  }

  return (
    <Card style={{ marginTop: 16 }}>
      <CardTitle>{title}</CardTitle>
      <CardBody>
        <Table aria-label={title} variant="compact">
          <Thead>
            <Tr>
              <Th>Name</Th>
              <Th>Type</Th>
              <Th>On Error</Th>
              <Th>Timeout</Th>
              <Th>Details</Th>
            </Tr>
          </Thead>
          <Tbody>
            {hooks.map((hook) => (
              <Tr key={hook.name}>
                <Td>{hook.name}</Td>
                <Td>
                  <Label>{hook.type}</Label>
                </Td>
                <Td>{hook.onError || 'Abort'}</Td>
                <Td>{hook.timeoutSeconds ?? 30}s</Td>
                <Td>
                  {hook.type === 'exec' && hook.exec && (
                    <code>
                      {hook.exec.namespace}/{Object.entries(hook.exec.podSelector).map(([k, v]) => `${k}=${v}`).join(',')}: {hook.exec.command.join(' ')}
                    </code>
                  )}
                  {hook.type === 'http' && hook.http && (
                    <code>
                      {hook.http.method || 'POST'} {hook.http.url}
                    </code>
                  )}
                </Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      </CardBody>
    </Card>
  );
};

const ReplicationPolicyDetailsPage: React.FC = () => {
  const { name } = useParams<{ name: string }>();
  const history = useHistory();
  const [deleteModalOpen, setDeleteModalOpen] = useState(false);

  const [policy, loaded, loadError] = useK8sWatchResource<ReplicationPolicy>({
    groupVersionKind: {
      group: ReplicationPolicyModel.apiGroup,
      version: ReplicationPolicyModel.apiVersion,
      kind: ReplicationPolicyModel.kind,
    },
    name,
  });

  if (!loaded) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }

  if (loadError) {
    return (
      <PageSection>
        <Alert variant="danger" title="Error loading ReplicationPolicy" isInline>
          {loadError.message || 'An unexpected error occurred.'}
        </Alert>
      </PageSection>
    );
  }

  if (!policy) {
    return (
      <PageSection>
        <Alert variant="warning" title="Not found" isInline>
          ReplicationPolicy &quot;{name}&quot; was not found.
        </Alert>
      </PageSection>
    );
  }

  const spec = policy.spec;
  const status = policy.status;
  const currentPhase = status?.phase || '';
  const isPaused = currentPhase === 'Paused';

  const handlePauseResume = async () => {
    const annotations = { ...(policy.metadata?.annotations || {}) };
    if (isPaused) {
      delete annotations['storage.ibmcloud.io/paused'];
    } else {
      annotations['storage.ibmcloud.io/paused'] = 'true';
    }

    const updated: ReplicationPolicy = {
      ...policy,
      metadata: {
        ...policy.metadata,
        annotations,
      },
    };

    try {
      await k8sUpdate({ model: ReplicationPolicyModel, data: updated });
    } catch (err) {
      console.error('Failed to toggle pause/resume:', err);
    }
  };

  const isReceiverMode = !!spec.destinationEndpoint;

  const renderOverview = () => (
    <Card>
      <CardTitle>Overview</CardTitle>
      <CardBody>
        <DescriptionList isHorizontal>
          <DescriptionListGroup>
            <DescriptionListTerm>Source Pool</DescriptionListTerm>
            <DescriptionListDescription>{spec.sourcePoolName}</DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Replication Mode</DescriptionListTerm>
            <DescriptionListDescription>
              <Label color={isReceiverMode ? 'blue' : 'green'}>
                {isReceiverMode ? 'Driver-to-Driver' : 'Direct NFS'}
              </Label>
            </DescriptionListDescription>
          </DescriptionListGroup>
          {isReceiverMode && (
            <>
              <DescriptionListGroup>
                <DescriptionListTerm>Receiver Endpoint</DescriptionListTerm>
                <DescriptionListDescription>
                  <code>{spec.destinationEndpoint}</code>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Auth Secret</DescriptionListTerm>
                <DescriptionListDescription>
                  <code>{spec.destinationAuthSecretRef}</code>
                </DescriptionListDescription>
              </DescriptionListGroup>
            </>
          )}
          {!isReceiverMode && (
            <>
              <DescriptionListGroup>
                <DescriptionListTerm>Destination NFS Server</DescriptionListTerm>
                <DescriptionListDescription>
                  <code>{spec.destinationNFSServer}</code>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Destination Export Path</DescriptionListTerm>
                <DescriptionListDescription>
                  <code>{spec.destinationExportPath || '/'}</code>
                </DescriptionListDescription>
              </DescriptionListGroup>
            </>
          )}
          <DescriptionListGroup>
            <DescriptionListTerm>Destination Base Path</DescriptionListTerm>
            <DescriptionListDescription>
              <code>{spec.destinationBasePath}</code>
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Schedule</DescriptionListTerm>
            <DescriptionListDescription>{spec.schedule}</DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Max Retries</DescriptionListTerm>
            <DescriptionListDescription>{spec.maxRetries}</DescriptionListDescription>
          </DescriptionListGroup>
          {!isReceiverMode && (
            <>
              <DescriptionListGroup>
                <DescriptionListTerm>Incremental Sync</DescriptionListTerm>
                <DescriptionListDescription>
                  <Label color={spec.incrementalSync !== false ? 'green' : 'grey'}>
                    {spec.incrementalSync !== false ? 'Enabled' : 'Disabled'}
                  </Label>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Bandwidth Limit</DescriptionListTerm>
                <DescriptionListDescription>
                  {spec.bandwidthLimitMbps ? `${spec.bandwidthLimitMbps} Mbps` : 'Unlimited'}
                </DescriptionListDescription>
              </DescriptionListGroup>
            </>
          )}
          <DescriptionListGroup>
            <DescriptionListTerm>Max Parallel Syncs</DescriptionListTerm>
            <DescriptionListDescription>
              {spec.maxParallelSyncs ?? 1}
            </DescriptionListDescription>
          </DescriptionListGroup>
          {!isReceiverMode && (
            <DescriptionListGroup>
              <DescriptionListTerm>Rsync Options</DescriptionListTerm>
              <DescriptionListDescription>
                {spec.rsyncOptions && spec.rsyncOptions.length > 0
                  ? <code>{spec.rsyncOptions.join(' ')}</code>
                  : '-'}
              </DescriptionListDescription>
            </DescriptionListGroup>
          )}
          <DescriptionListGroup>
            <DescriptionListTerm>SubVolume Selector</DescriptionListTerm>
            <DescriptionListDescription>
              {spec.subVolumeSelector?.matchLabels
                ? Object.entries(spec.subVolumeSelector.matchLabels)
                    .map(([k, v]) => (
                      <Label key={k} style={{ marginRight: 4, marginBottom: 4 }}>
                        {k}={v}
                      </Label>
                    ))
                : '-'}
            </DescriptionListDescription>
          </DescriptionListGroup>
        </DescriptionList>
      </CardBody>
    </Card>
  );

  const renderStatus = () => (
    <Card>
      <CardTitle>Status</CardTitle>
      <CardBody>
        <DescriptionList>
          <DescriptionListGroup>
            <DescriptionListTerm>Phase</DescriptionListTerm>
            <DescriptionListDescription>
              <PhaseStatus phase={status?.phase} />
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Last Sync Time</DescriptionListTerm>
            <DescriptionListDescription>
              {status?.lastSyncTime ? formatAge(status.lastSyncTime) : '-'}
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Last Sync Duration</DescriptionListTerm>
            <DescriptionListDescription>
              {status?.lastSyncDuration
                ? formatDuration(status.lastSyncDuration)
                : '-'}
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Consecutive Failures</DescriptionListTerm>
            <DescriptionListDescription>
              <Label color={status?.consecutiveFailures ? 'red' : 'green'}>
                {status?.consecutiveFailures ?? 0}
              </Label>
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Last Error</DescriptionListTerm>
            <DescriptionListDescription>
              {status?.lastError || '-'}
            </DescriptionListDescription>
          </DescriptionListGroup>
        </DescriptionList>
      </CardBody>
    </Card>
  );

  const renderSyncStatuses = () => {
    const statuses = status?.subVolumeStatuses;
    if (!statuses || statuses.length === 0) {
      return (
        <Alert variant="info" title="No sync status" isInline style={{ marginTop: 16 }}>
          No SubVolume sync statuses have been reported yet.
        </Alert>
      );
    }

    return (
      <Card style={{ marginTop: 16 }}>
        <CardTitle>Sync Status</CardTitle>
        <CardBody>
          <Table aria-label="SubVolume sync statuses" variant="compact">
            <Thead>
              <Tr>
                <Th>SubVolume Name</Th>
                <Th>Last Sync Time</Th>
                <Th>Bytes Synced</Th>
                <Th>Last Error</Th>
              </Tr>
            </Thead>
            <Tbody>
              {statuses.map((svStatus) => (
                <Tr key={svStatus.subVolumeName}>
                  <Td>{svStatus.subVolumeName}</Td>
                  <Td>{svStatus.lastSyncTime ? formatAge(svStatus.lastSyncTime) : '-'}</Td>
                  <Td>
                    {svStatus.bytesSynced != null
                      ? humanizeBytes(svStatus.bytesSynced)
                      : '-'}
                  </Td>
                  <Td>{svStatus.lastError || '-'}</Td>
                </Tr>
              ))}
            </Tbody>
          </Table>
        </CardBody>
      </Card>
    );
  };

  return (
    <>
      <PageSection variant="default">
        <Split hasGutter>
          <SplitItem isFilled>
            <Title headingLevel="h1" size="2xl">
              {policy.metadata?.name}
            </Title>
          </SplitItem>
          <SplitItem>
            <Button
              variant={isPaused ? 'primary' : 'warning'}
              onClick={handlePauseResume}
              style={{ marginRight: 8 }}
            >
              {isPaused ? 'Resume' : 'Pause'}
            </Button>
          </SplitItem>
          <SplitItem>
            <Button
              variant="danger"
              onClick={() => setDeleteModalOpen(true)}
            >
              Delete
            </Button>
          </SplitItem>
        </Split>
      </PageSection>
      <PageSection>
        <Grid hasGutter>
          <GridItem span={8}>
            {renderOverview()}
          </GridItem>
          <GridItem span={4}>
            {renderStatus()}
          </GridItem>
        </Grid>

        {renderSyncStatuses()}
        {renderHooksReadOnly(spec.preSyncHooks, 'Pre-Sync Hooks')}
        {renderHooksReadOnly(spec.postSyncHooks, 'Post-Sync Hooks')}
      </PageSection>

      <DeleteModal
        isOpen={deleteModalOpen}
        resourceName={policy.metadata?.name || ''}
        resourceKind="ReplicationPolicy"
        model={ReplicationPolicyModel}
        onClose={() => setDeleteModalOpen(false)}
        onDeleted={() => {
          setDeleteModalOpen(false);
          history.push(ROUTES.REPLICATION);
        }}
      />
    </>
  );
};

export default ReplicationPolicyDetailsPage;
