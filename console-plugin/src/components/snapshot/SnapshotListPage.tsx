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
import { Label } from '@patternfly/react-core';
import { SnapshotModel } from '../../models';
import { Snapshot } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import '../common/table-layout.css';
import DeleteModal from '../common/DeleteModal';
import { ROUTES } from '../../constants';

const columns: TableColumn<Snapshot>[] = [
  {
    title: 'Name',
    id: 'name',
    props: { style: { width: '15%' } },
  },
  {
    title: 'Pool',
    id: 'pool',
    props: { style: { width: '10%' } },
  },
  {
    title: 'Source SubVolume',
    id: 'sourceSubVolume',
    props: { style: { width: '15%' } },
  },
  {
    title: 'Size',
    id: 'size',
    props: { style: { width: '10%' } },
  },
  {
    title: 'Phase',
    id: 'phase',
    props: { style: { width: '10%' } },
  },
  {
    title: 'Ready',
    id: 'ready',
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

const SnapshotRow: React.FC<RowProps<Snapshot>> = ({ obj, activeColumnIDs }) => {
  const [deleteModalOpen, setDeleteModalOpen] = useState(false);

  const name = obj.metadata?.name || '';
  const detailPath = ROUTES.SNAPSHOT_DETAIL.replace(':name', name);

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
      <TableData id="sourceSubVolume" activeColumnIDs={activeColumnIDs}>
        {obj.spec?.sourceSubVolume || '-'}
      </TableData>
      <TableData id="size" activeColumnIDs={activeColumnIDs}>
        {obj.spec?.sizeGB != null ? `${obj.spec.sizeGB} GB` : '-'}
      </TableData>
      <TableData id="phase" activeColumnIDs={activeColumnIDs}>
        <PhaseStatus phase={obj.status?.phase} />
      </TableData>
      <TableData id="ready" activeColumnIDs={activeColumnIDs}>
        <Label color={obj.status?.readyToUse ? 'green' : 'grey'}>
          {obj.status?.readyToUse ? 'True' : 'False'}
        </Label>
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
        resourceKind="Snapshot"
        model={SnapshotModel}
        onClose={() => setDeleteModalOpen(false)}
        onDeleted={() => setDeleteModalOpen(false)}
      />
    </>
  );
};

const SnapshotListPage: React.FC = () => {
  const [snapshots, loaded, loadError] = useK8sWatchResource<Snapshot[]>({
    groupVersionKind: {
      group: SnapshotModel.apiGroup,
      version: SnapshotModel.apiVersion,
      kind: SnapshotModel.kind,
    },
    isList: true,
  });

  const [data, filteredData, onFilterChange] = useListPageFilter(snapshots);

  return (
    <>
      <ListPageHeader title="Snapshots">
        <ListPageCreateLink
          to={ROUTES.SNAPSHOT_CREATE}
          createAccessReview={{ groupVersionKind: { group: SnapshotModel.apiGroup!, version: SnapshotModel.apiVersion, kind: SnapshotModel.kind } }}
        >
          Create Snapshot
        </ListPageCreateLink>
      </ListPageHeader>
      <ListPageBody>
        <ListPageFilter
          data={data}
          loaded={loaded}
          onFilterChange={onFilterChange}
        />
        <div className="vpc-file-pool-fixed-table">
          <VirtualizedTable<Snapshot>
            data={filteredData}
            unfilteredData={data}
            loaded={loaded}
            loadError={loadError}
            columns={columns}
            Row={SnapshotRow}
          />
        </div>
      </ListPageBody>
    </>
  );
};

export default SnapshotListPage;
