import React, { useState } from 'react';
import {
  useK8sWatchResource,
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
import { VolumeGroupSnapshotModel } from '../../models';
import { VolumeGroupSnapshot } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import '../common/table-layout.css';
import DeleteModal from '../common/DeleteModal';
import { ROUTES } from '../../constants';

const columns: TableColumn<VolumeGroupSnapshot>[] = [
  {
    title: 'Name',
    id: 'name',
    props: { style: { width: '20%' } },
  },
  {
    title: 'Pool',
    id: 'pool',
    props: { style: { width: '15%' } },
  },
  {
    title: 'Members',
    id: 'members',
    props: { style: { width: '10%' } },
  },
  {
    title: 'Ready',
    id: 'ready',
    props: { style: { width: '10%' } },
  },
  {
    title: 'Phase',
    id: 'phase',
    props: { style: { width: '10%' } },
  },
  {
    title: 'Age',
    id: 'age',
    props: { style: { width: '10%' } },
  },
  {
    title: '',
    id: 'actions',
    props: { className: 'pf-v6-c-table__action' },
  },
];

const VolumeGroupSnapshotRow: React.FC<RowProps<VolumeGroupSnapshot>> = ({
  obj,
  activeColumnIDs,
}) => {
  const [deleteModalOpen, setDeleteModalOpen] = useState(false);

  const name = obj.metadata?.name || '';
  const detailPath = ROUTES.GROUP_SNAPSHOT_DETAIL.replace(':name', name);
  const memberCount = obj.status?.memberCount ?? 0;
  const readyCount = obj.status?.readyCount ?? 0;

  const rowActions: IAction[] = [
    {
      title: <Link to={detailPath}>View Details</Link>,
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
      <TableData id="pool" activeColumnIDs={activeColumnIDs}>
        {obj.spec?.poolName || '-'}
      </TableData>
      <TableData id="members" activeColumnIDs={activeColumnIDs}>
        {memberCount}
      </TableData>
      <TableData id="ready" activeColumnIDs={activeColumnIDs}>
        {readyCount}/{memberCount}
      </TableData>
      <TableData id="phase" activeColumnIDs={activeColumnIDs}>
        <PhaseStatus phase={obj.status?.phase} />
      </TableData>
      <TableData id="age" activeColumnIDs={activeColumnIDs}>
        {obj.metadata?.creationTimestamp ? (
          <Timestamp timestamp={obj.metadata.creationTimestamp} />
        ) : (
          '-'
        )}
      </TableData>
      <TableData id="actions" activeColumnIDs={activeColumnIDs}>
        <ActionsColumn items={rowActions} />
      </TableData>
      <DeleteModal
        isOpen={deleteModalOpen}
        resourceName={name}
        resourceKind="VolumeGroupSnapshot"
        model={VolumeGroupSnapshotModel}
        onClose={() => setDeleteModalOpen(false)}
        onDeleted={() => setDeleteModalOpen(false)}
      />
    </>
  );
};

const VolumeGroupSnapshotListPage: React.FC = () => {
  const [snapshots, loaded, loadError] = useK8sWatchResource<VolumeGroupSnapshot[]>({
    groupVersionKind: {
      group: VolumeGroupSnapshotModel.apiGroup,
      version: VolumeGroupSnapshotModel.apiVersion,
      kind: VolumeGroupSnapshotModel.kind,
    },
    isList: true,
  });

  const [data, filteredData, onFilterChange] = useListPageFilter(snapshots);

  return (
    <>
      <ListPageHeader title="Volume Group Snapshots">
        <ListPageCreateLink
          to={ROUTES.GROUP_SNAPSHOT_CREATE}
          createAccessReview={{ groupVersionKind: { group: VolumeGroupSnapshotModel.apiGroup!, version: VolumeGroupSnapshotModel.apiVersion, kind: VolumeGroupSnapshotModel.kind } }}
        >
          Create VolumeGroupSnapshot
        </ListPageCreateLink>
      </ListPageHeader>
      <ListPageBody>
        <ListPageFilter
          data={data}
          loaded={loaded}
          onFilterChange={onFilterChange}
        />
        <div className="vpc-file-pool-fixed-table">
          <VirtualizedTable<VolumeGroupSnapshot>
            data={filteredData}
            unfilteredData={data}
            loaded={loaded}
            loadError={loadError}
            columns={columns}
            Row={VolumeGroupSnapshotRow}
          />
        </div>
      </ListPageBody>
    </>
  );
};

export default VolumeGroupSnapshotListPage;
