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
import { FileSharePoolModel } from '../../models';
import { FileSharePool } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import '../common/table-layout.css';
import CapacityBar from '../common/CapacityBar';
import DeleteModal from '../common/DeleteModal';
import { ROUTES } from '../../constants';

const columns: TableColumn<FileSharePool>[] = [
  {
    title: 'Name',
    id: 'name',
    props: { style: { width: '18%' } },
  },
  {
    title: 'Zone',
    id: 'zone',
    props: { style: { width: '8%' } },
  },
  {
    title: 'Profile',
    id: 'profile',
    props: { style: { width: '8%' } },
  },
  {
    title: 'IOPS',
    id: 'iops',
    props: { style: { width: '8%' } },
  },
  {
    title: 'Shares',
    id: 'shares',
    props: { style: { width: '5%' } },
  },
  {
    title: 'Capacity',
    id: 'capacity',
    props: { style: { width: '18%' } },
  },
  {
    title: 'PVCs',
    id: 'pvcs',
    props: { style: { width: '5%' } },
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

/** Calculate IOPS display: use spec.iops if set, otherwise derive from profile. */
function getIOPS(pool: FileSharePool): string {
  if (pool.spec?.iops != null && pool.spec.iops > 0) {
    return pool.spec.iops.toLocaleString();
  }
  // dp2 profile: 100 IOPS/GB
  if (pool.spec?.profile === 'dp2' && pool.spec?.shareSizeGB) {
    return `${(pool.spec.shareSizeGB * 100).toLocaleString()}`;
  }
  return '-';
}

const FileSharePoolRow: React.FC<RowProps<FileSharePool>> = ({
  obj,
  activeColumnIDs,
}) => {
  const [deleteModalOpen, setDeleteModalOpen] = useState(false);

  const name = obj.metadata?.name || '';
  const detailPath = ROUTES.POOL_DETAIL.replace(':name', name);

  const rowActions: IAction[] = [
    {
      title: <Link to={detailPath}>Edit</Link>,
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
      <TableData id="zone" activeColumnIDs={activeColumnIDs}>
        {obj.spec?.zone || '-'}
      </TableData>
      <TableData id="profile" activeColumnIDs={activeColumnIDs}>
        {obj.spec?.profile || '-'}
      </TableData>
      <TableData id="iops" activeColumnIDs={activeColumnIDs}>
        {getIOPS(obj)}
      </TableData>
      <TableData id="shares" activeColumnIDs={activeColumnIDs}>
        {obj.status?.shareCount ?? 0}
      </TableData>
      <TableData id="capacity" activeColumnIDs={activeColumnIDs}>
        <CapacityBar
          allocated={obj.status?.totalAllocatedGB ?? 0}
          total={obj.status?.totalCapacityGB ?? 0}
        />
      </TableData>
      <TableData id="pvcs" activeColumnIDs={activeColumnIDs}>
        {obj.status?.totalPVCCount ?? 0}
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
        resourceKind="FileSharePool"
        model={FileSharePoolModel}
        onClose={() => setDeleteModalOpen(false)}
        onDeleted={() => setDeleteModalOpen(false)}
        requireConfirmation
      />
    </>
  );
};

const FileSharePoolListPage: React.FC = () => {
  const [pools, loaded, loadError] = useK8sWatchResource<FileSharePool[]>({
    groupVersionKind: {
      group: FileSharePoolModel.apiGroup,
      version: FileSharePoolModel.apiVersion,
      kind: FileSharePoolModel.kind,
    },
    isList: true,
  });

  const [data, filteredData, onFilterChange] = useListPageFilter(pools);

  return (
    <>
      <ListPageHeader title="File Share Pools">
        <ListPageCreateLink
          to={ROUTES.POOL_CREATE}
          createAccessReview={{ groupVersionKind: { group: FileSharePoolModel.apiGroup!, version: FileSharePoolModel.apiVersion, kind: FileSharePoolModel.kind } }}
        >
          Create FileSharePool
        </ListPageCreateLink>
      </ListPageHeader>
      <ListPageBody>
        <ListPageFilter
          data={data}
          loaded={loaded}
          onFilterChange={onFilterChange}
        />
        <div className="vpc-file-pool-fixed-table">
          <VirtualizedTable<FileSharePool>
            data={filteredData}
            unfilteredData={data}
            loaded={loaded}
            loadError={loadError}
            columns={columns}
            Row={FileSharePoolRow}
          />
        </div>
      </ListPageBody>
    </>
  );
};

export default FileSharePoolListPage;
