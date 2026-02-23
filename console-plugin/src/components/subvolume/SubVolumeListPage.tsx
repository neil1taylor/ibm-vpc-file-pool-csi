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
import { SubVolumeModel } from '../../models';
import { SubVolume } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import '../common/table-layout.css';
import DeleteModal from '../common/DeleteModal';
import { ROUTES } from '../../constants';

const columns: TableColumn<SubVolume>[] = [
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
    title: 'PVC',
    id: 'pvc',
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
    title: 'Clone Status',
    id: 'cloneStatus',
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

const SubVolumeRow: React.FC<RowProps<SubVolume>> = ({
  obj,
  activeColumnIDs,
}) => {
  const [deleteModalOpen, setDeleteModalOpen] = useState(false);

  const name = obj.metadata?.name || '';
  const detailPath = ROUTES.SUBVOLUME_DETAIL.replace(':name', name);

  const rowActions: IAction[] = [
    {
      title: <Link to={detailPath}>View Details</Link>,
    },
    {
      title: (
        <Link to={`${ROUTES.SNAPSHOT_CREATE}?source=${encodeURIComponent(name)}`}>
          Create Snapshot
        </Link>
      ),
    },
    {
      isSeparator: true,
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
        {obj.spec?.poolName ? (
          <Link to={ROUTES.POOL_DETAIL.replace(':name', obj.spec.poolName)}>
            {obj.spec.poolName}
          </Link>
        ) : (
          '-'
        )}
      </TableData>
      <TableData id="pvc" activeColumnIDs={activeColumnIDs}>
        {obj.spec?.pvcNamespace && obj.spec?.pvcName
          ? `${obj.spec.pvcNamespace}/${obj.spec.pvcName}`
          : '-'}
      </TableData>
      <TableData id="size" activeColumnIDs={activeColumnIDs}>
        {obj.spec?.requestedGB != null ? `${obj.spec.requestedGB} GB` : '-'}
      </TableData>
      <TableData id="phase" activeColumnIDs={activeColumnIDs}>
        <PhaseStatus phase={obj.status?.phase} />
      </TableData>
      <TableData id="cloneStatus" activeColumnIDs={activeColumnIDs}>
        {obj.status?.cloneStatus ? (
          <PhaseStatus phase={obj.status.cloneStatus} />
        ) : (
          '-'
        )}
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
        resourceKind="SubVolume"
        model={SubVolumeModel}
        onClose={() => setDeleteModalOpen(false)}
        onDeleted={() => setDeleteModalOpen(false)}
      />
    </>
  );
};

const SubVolumeListPage: React.FC = () => {
  const [subVolumes, loaded, loadError] = useK8sWatchResource<SubVolume[]>({
    groupVersionKind: {
      group: SubVolumeModel.apiGroup,
      version: SubVolumeModel.apiVersion,
      kind: SubVolumeModel.kind,
    },
    isList: true,
  });

  const [data, filteredData, onFilterChange] = useListPageFilter(subVolumes);

  return (
    <>
      <ListPageHeader title="SubVolumes">
        <ListPageCreateLink
          to={ROUTES.SUBVOLUME_CREATE}
          createAccessReview={{ groupVersionKind: { group: SubVolumeModel.apiGroup!, version: SubVolumeModel.apiVersion, kind: SubVolumeModel.kind } }}
        >
          Create SubVolume
        </ListPageCreateLink>
      </ListPageHeader>
      <ListPageBody>
        <ListPageFilter
          data={data}
          loaded={loaded}
          onFilterChange={onFilterChange}
        />
        <div className="vpc-file-pool-fixed-table">
          <VirtualizedTable<SubVolume>
            data={filteredData}
            unfilteredData={data}
            loaded={loaded}
            loadError={loadError}
            columns={columns}
            Row={SubVolumeRow}
          />
        </div>
      </ListPageBody>
    </>
  );
};

export default SubVolumeListPage;
