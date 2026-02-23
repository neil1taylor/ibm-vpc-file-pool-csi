import React, { useState } from 'react';
import {
  useK8sWatchResource,
  k8sUpdate,
  ListPageHeader,
  ListPageBody,
  ListPageCreateLink,
  ListPageFilter,
  useListPageFilter,
  VirtualizedTable,
  TableColumn,
  RowProps,
  TableData,
  Timestamp,
} from '@openshift-console/dynamic-plugin-sdk';
import { Link } from 'react-router-dom';
import { ActionsColumn, IAction } from '@patternfly/react-table';
import { ReplicationPolicyModel } from '../../models';
import { ReplicationPolicy } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import '../common/table-layout.css';
import DeleteModal from '../common/DeleteModal';
import { formatAge } from '../../utils/resource-helpers';
import { ROUTES } from '../../constants';

const columns: TableColumn<ReplicationPolicy>[] = [
  {
    title: 'Name',
    id: 'name',
    props: { style: { width: '15%' } },
  },
  {
    title: 'Source Pool',
    id: 'sourcePool',
    props: { style: { width: '15%' } },
  },
  {
    title: 'Destination',
    id: 'destination',
    props: { style: { width: '15%' } },
  },
  {
    title: 'Schedule',
    id: 'schedule',
    props: { style: { width: '10%' } },
  },
  {
    title: 'Phase',
    id: 'phase',
    props: { style: { width: '10%' } },
  },
  {
    title: 'Last Sync',
    id: 'lastSync',
    props: { style: { width: '10%' } },
  },
  {
    title: 'Failures',
    id: 'failures',
    props: { style: { width: '5%' } },
  },
  {
    title: '',
    id: 'actions',
    props: { className: 'pf-v6-c-table__action' },
  },
];

const ReplicationPolicyRow: React.FC<RowProps<ReplicationPolicy>> = ({
  obj,
  activeColumnIDs,
}) => {
  const [deleteModalOpen, setDeleteModalOpen] = useState(false);

  const name = obj.metadata?.name || '';
  const detailPath = ROUTES.REPLICATION_DETAIL.replace(':name', name);
  const currentPhase = obj.status?.phase || '';
  const isPaused = currentPhase === 'Paused';

  const handlePauseResume = async () => {
    const annotations = { ...(obj.metadata?.annotations || {}) };
    if (isPaused) {
      delete annotations['storage.ibmcloud.io/paused'];
    } else {
      annotations['storage.ibmcloud.io/paused'] = 'true';
    }

    const updated: ReplicationPolicy = {
      ...obj,
      metadata: {
        ...obj.metadata,
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
      onClick: () => setDeleteModalOpen(true),
    },
  ];

  return (
    <>
      <TableData id="name" activeColumnIDs={activeColumnIDs}>
        <Link to={detailPath}>{name}</Link>
      </TableData>
      <TableData id="sourcePool" activeColumnIDs={activeColumnIDs}>
        {obj.spec?.sourcePoolName || '-'}
      </TableData>
      <TableData id="destination" activeColumnIDs={activeColumnIDs}>
        <code>{obj.spec?.destinationNFSServer || '-'}:{obj.spec?.destinationBasePath || '/pvcs'}</code>
      </TableData>
      <TableData id="schedule" activeColumnIDs={activeColumnIDs}>
        {obj.spec?.schedule || '-'}
      </TableData>
      <TableData id="phase" activeColumnIDs={activeColumnIDs}>
        <PhaseStatus phase={obj.status?.phase} />
      </TableData>
      <TableData id="lastSync" activeColumnIDs={activeColumnIDs}>
        {formatAge(obj.status?.lastSyncTime)}
      </TableData>
      <TableData id="failures" activeColumnIDs={activeColumnIDs}>
        {obj.status?.consecutiveFailures ?? 0}
      </TableData>
      <TableData id="actions" activeColumnIDs={activeColumnIDs}>
        <ActionsColumn items={rowActions} />
      </TableData>
      <DeleteModal
        isOpen={deleteModalOpen}
        resourceName={name}
        resourceKind="ReplicationPolicy"
        model={ReplicationPolicyModel}
        onClose={() => setDeleteModalOpen(false)}
        onDeleted={() => setDeleteModalOpen(false)}
      />
    </>
  );
};

const ReplicationPolicyListPage: React.FC = () => {
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
      <ListPageBody>
        <ListPageFilter
          data={data}
          loaded={loaded}
          onFilterChange={onFilterChange}
        />
        <div className="vpc-file-pool-fixed-table">
          <VirtualizedTable<ReplicationPolicy>
            data={filteredData}
            unfilteredData={data}
            loaded={loaded}
            loadError={loadError}
            columns={columns}
            Row={ReplicationPolicyRow}
          />
        </div>
      </ListPageBody>
    </>
  );
};

export default ReplicationPolicyListPage;
