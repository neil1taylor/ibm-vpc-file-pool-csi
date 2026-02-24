import React, { useState } from 'react';
import {
  useK8sWatchResource,
  k8sUpdate,
  ListPageHeader,
  ListPageBody,
  ListPageCreateLink,
  ListPageFilter,
  useListPageFilter,
  Timestamp,
} from '@openshift-console/dynamic-plugin-sdk';
import { Link } from 'react-router-dom';
import {
  Table,
  Thead,
  Tr,
  Th,
  Td,
  Tbody,
  ActionsColumn,
  IAction,
} from '@patternfly/react-table';
import { Bullseye, Spinner } from '@patternfly/react-core';
import { ReplicationPolicyModel } from '../../models';
import { ReplicationPolicy } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import DeleteModal from '../common/DeleteModal';
import { formatAge } from '../../utils/resource-helpers';
import { ROUTES } from '../../constants';

const ReplicationPolicyRow: React.FC<{
  policy: ReplicationPolicy;
  onDelete: (name: string) => void;
}> = ({ policy, onDelete }) => {
  const name = policy.metadata?.name || '';
  const detailPath = ROUTES.REPLICATION_DETAIL.replace(':name', name);
  const currentPhase = policy.status?.phase || '';
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

  const rowActions: IAction[] = [
    {
      title: <Link to={detailPath}>View Details</Link>,
    },
    {
      title: isPaused ? 'Resume' : 'Pause',
      onClick: handlePauseResume,
    },
    {
      title: 'Delete',
      onClick: () => onDelete(name),
    },
  ];

  return (
    <Tr>
      <Td dataLabel="Name"><Link to={detailPath}>{name}</Link></Td>
      <Td dataLabel="Source Pool">{policy.spec?.sourcePoolName || '-'}</Td>
      <Td dataLabel="Destination">
        <code>{policy.spec?.destinationNFSServer || '-'}:{policy.spec?.destinationBasePath || '/pvcs'}</code>
      </Td>
      <Td dataLabel="Schedule">{policy.spec?.schedule || '-'}</Td>
      <Td dataLabel="Phase"><PhaseStatus phase={policy.status?.phase} /></Td>
      <Td dataLabel="Last Sync">{formatAge(policy.status?.lastSyncTime)}</Td>
      <Td dataLabel="Failures">{policy.status?.consecutiveFailures ?? 0}</Td>
      <Td isActionCell>
        <ActionsColumn items={rowActions} />
      </Td>
    </Tr>
  );
};

const ReplicationPolicyListPage: React.FC = () => {
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);

  const [policies, loaded, loadError] = useK8sWatchResource<ReplicationPolicy[]>({
    groupVersionKind: {
      group: ReplicationPolicyModel.apiGroup,
      version: ReplicationPolicyModel.apiVersion,
      kind: ReplicationPolicyModel.kind,
    },
    isList: true,
  });

  const [data, filteredData, onFilterChange] = useListPageFilter(policies);

  return (
    <>
      <ListPageHeader title="Replication Policies">
        <ListPageCreateLink
          to={ROUTES.REPLICATION_CREATE}
          createAccessReview={{ groupVersionKind: { group: ReplicationPolicyModel.apiGroup!, version: ReplicationPolicyModel.apiVersion, kind: ReplicationPolicyModel.kind } }}
        >
          Create ReplicationPolicy
        </ListPageCreateLink>
      </ListPageHeader>
      <p style={{ padding: '0 24px', color: 'var(--pf-t--global--color--subtle)', margin: '0 0 8px 0' }}>
        Replication Policies define cross-region disaster recovery by syncing SubVolume data from a source pool to a destination pool on a configurable schedule.
      </p>
      <ListPageBody>
        <ListPageFilter
          data={data}
          loaded={loaded}
          onFilterChange={onFilterChange}
        />
        {!loaded && (
          <Bullseye style={{ padding: '48px 0' }}>
            <Spinner size="xl" />
          </Bullseye>
        )}
        {loadError && (
          <div style={{ padding: '24px', color: 'var(--pf-t--global--color--status--danger--default)' }}>
            Error loading ReplicationPolicies
          </div>
        )}
        {loaded && !loadError && filteredData.length === 0 && (
          <Bullseye style={{ padding: '48px 0', color: 'var(--pf-t--global--color--subtle)' }}>
            No ReplicationPolicies found
          </Bullseye>
        )}
        {loaded && filteredData.length > 0 && (
          <Table aria-label="Replication Policies" variant="compact">
            <Thead>
              <Tr>
                <Th>Name</Th>
                <Th>Source Pool</Th>
                <Th>Destination</Th>
                <Th>Schedule</Th>
                <Th>Phase</Th>
                <Th>Last Sync</Th>
                <Th>Failures</Th>
                <Th></Th>
              </Tr>
            </Thead>
            <Tbody>
              {filteredData.map((policy) => (
                <ReplicationPolicyRow
                  key={policy.metadata?.uid || policy.metadata?.name}
                  policy={policy}
                  onDelete={setDeleteTarget}
                />
              ))}
            </Tbody>
          </Table>
        )}
      </ListPageBody>
      {deleteTarget && (
        <DeleteModal
          isOpen={!!deleteTarget}
          resourceName={deleteTarget}
          resourceKind="ReplicationPolicy"
          model={ReplicationPolicyModel}
          onClose={() => setDeleteTarget(null)}
          onDeleted={() => setDeleteTarget(null)}
        />
      )}
    </>
  );
};

export default ReplicationPolicyListPage;
