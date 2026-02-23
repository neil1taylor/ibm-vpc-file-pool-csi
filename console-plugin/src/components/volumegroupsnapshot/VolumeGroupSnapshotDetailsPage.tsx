import React, { useState } from 'react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
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
import { VolumeGroupSnapshotModel } from '../../models';
import { VolumeGroupSnapshot, Hook } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import DeleteModal from '../common/DeleteModal';
import { formatAge, formatDuration } from '../../utils/resource-helpers';
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

const VolumeGroupSnapshotDetailsPage: React.FC = () => {
  const { name } = useParams<{ name: string }>();
  const history = useHistory();
  const [deleteModalOpen, setDeleteModalOpen] = useState(false);

  const [vgs, loaded, loadError] = useK8sWatchResource<VolumeGroupSnapshot>({
    groupVersionKind: {
      group: VolumeGroupSnapshotModel.apiGroup,
      version: VolumeGroupSnapshotModel.apiVersion,
      kind: VolumeGroupSnapshotModel.kind,
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
        <Alert variant="danger" title="Error loading VolumeGroupSnapshot" isInline>
          {loadError.message || 'An unexpected error occurred.'}
        </Alert>
      </PageSection>
    );
  }

  if (!vgs) {
    return (
      <PageSection>
        <Alert variant="warning" title="Not found" isInline>
          VolumeGroupSnapshot &quot;{name}&quot; was not found.
        </Alert>
      </PageSection>
    );
  }

  const spec = vgs.spec;
  const status = vgs.status;

  const renderOverview = () => (
    <Card>
      <CardTitle>Overview</CardTitle>
      <CardBody>
        <DescriptionList isHorizontal>
          <DescriptionListGroup>
            <DescriptionListTerm>Pool Name</DescriptionListTerm>
            <DescriptionListDescription>{spec.poolName}</DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Failure Policy</DescriptionListTerm>
            <DescriptionListDescription>
              <Label color={spec.failurePolicy === 'Continue' ? 'orange' : 'blue'}>
                {spec.failurePolicy}
              </Label>
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Source PVCs</DescriptionListTerm>
            <DescriptionListDescription>
              {spec.sourcePVCs && spec.sourcePVCs.length > 0
                ? spec.sourcePVCs.map((pvc) => (
                    <Label key={pvc} style={{ marginRight: 4, marginBottom: 4 }}>
                      {pvc}
                    </Label>
                  ))
                : '-'}
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Copy Order</DescriptionListTerm>
            <DescriptionListDescription>
              {spec.copyOrder && spec.copyOrder.length > 0
                ? spec.copyOrder.join(', ')
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
            <DescriptionListTerm>Member Count</DescriptionListTerm>
            <DescriptionListDescription>
              {status?.memberCount ?? 0}
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Ready Count</DescriptionListTerm>
            <DescriptionListDescription>
              {status?.readyCount ?? 0}
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Failed Count</DescriptionListTerm>
            <DescriptionListDescription>
              {status?.failedCount ?? 0}
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Consistency Level</DescriptionListTerm>
            <DescriptionListDescription>
              {status?.consistencyLevel || '-'}
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Inconsistency Window</DescriptionListTerm>
            <DescriptionListDescription>
              {status?.inconsistencyWindowMs != null
                ? `${status.inconsistencyWindowMs}ms`
                : '-'}
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Started At</DescriptionListTerm>
            <DescriptionListDescription>
              {status?.startedAt ? formatAge(status.startedAt) : '-'}
            </DescriptionListDescription>
          </DescriptionListGroup>
          <DescriptionListGroup>
            <DescriptionListTerm>Completed At</DescriptionListTerm>
            <DescriptionListDescription>
              {status?.completedAt ? formatAge(status.completedAt) : '-'}
            </DescriptionListDescription>
          </DescriptionListGroup>
        </DescriptionList>
      </CardBody>
    </Card>
  );

  const renderMembers = () => {
    const members = status?.members;
    if (!members || members.length === 0) {
      return (
        <Alert variant="info" title="No members" isInline>
          No snapshot members have been created yet.
        </Alert>
      );
    }

    return (
      <Card style={{ marginTop: 16 }}>
        <CardTitle>Members</CardTitle>
        <CardBody>
          <Table aria-label="Snapshot members" variant="compact">
            <Thead>
              <Tr>
                <Th>SubVolume Name</Th>
                <Th>Snapshot Name</Th>
                <Th>Phase</Th>
                <Th>Error</Th>
              </Tr>
            </Thead>
            <Tbody>
              {members.map((member) => (
                <Tr key={member.subVolumeName}>
                  <Td>{member.subVolumeName}</Td>
                  <Td>{member.snapshotName}</Td>
                  <Td>
                    <PhaseStatus phase={member.phase} />
                  </Td>
                  <Td>{member.error || '-'}</Td>
                </Tr>
              ))}
            </Tbody>
          </Table>
        </CardBody>
      </Card>
    );
  };

  const renderHookResults = () => {
    const hookResults = status?.hookResults;
    if (!hookResults || hookResults.length === 0) {
      return null;
    }

    return (
      <Card style={{ marginTop: 16 }}>
        <CardTitle>Hook Results</CardTitle>
        <CardBody>
          <Table aria-label="Hook results" variant="compact">
            <Thead>
              <Tr>
                <Th>Name</Th>
                <Th>Phase</Th>
                <Th>Success</Th>
                <Th>Message</Th>
                <Th>Started At</Th>
                <Th>Duration</Th>
              </Tr>
            </Thead>
            <Tbody>
              {hookResults.map((result, index) => (
                <Tr key={`${result.name}-${index}`}>
                  <Td>{result.name}</Td>
                  <Td>{result.phase}</Td>
                  <Td>
                    <Label color={result.success ? 'green' : 'red'}>
                      {result.success ? 'Success' : 'Failed'}
                    </Label>
                  </Td>
                  <Td>{result.message || '-'}</Td>
                  <Td>{formatAge(result.startedAt)}</Td>
                  <Td>{formatDuration(result.duration)}</Td>
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
              {vgs.metadata?.name}
            </Title>
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

        {renderMembers()}
        {renderHookResults()}
        {renderHooksReadOnly(spec.preSnapshotHooks, 'Pre-Snapshot Hooks')}
        {renderHooksReadOnly(spec.postSnapshotHooks, 'Post-Snapshot Hooks')}
      </PageSection>

      <DeleteModal
        isOpen={deleteModalOpen}
        resourceName={vgs.metadata?.name || ''}
        resourceKind="VolumeGroupSnapshot"
        model={VolumeGroupSnapshotModel}
        onClose={() => setDeleteModalOpen(false)}
        onDeleted={() => {
          setDeleteModalOpen(false);
          history.push(ROUTES.GROUP_SNAPSHOTS);
        }}
      />
    </>
  );
};

export default VolumeGroupSnapshotDetailsPage;
