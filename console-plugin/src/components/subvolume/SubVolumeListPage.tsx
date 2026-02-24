import React, { useState, useMemo } from 'react';
import {
  useK8sWatchResource,
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
  ThProps,
  Td,
  Tbody,
  ActionsColumn,
  IAction,
} from '@patternfly/react-table';
import { Bullseye, Spinner } from '@patternfly/react-core';
import { SubVolumeModel } from '../../models';
import { SubVolume } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import DeleteModal from '../common/DeleteModal';
import { ROUTES } from '../../constants';

const SubVolumeRow: React.FC<{
  sv: SubVolume;
  onDelete: (name: string) => void;
}> = ({ sv, onDelete }) => {
  const name = sv.metadata?.name || '';
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
      onClick: () => onDelete(name),
    },
  ];

  return (
    <Tr>
      <Td dataLabel="Name"><Link to={detailPath}>{name}</Link></Td>
      <Td dataLabel="Pool">
        {sv.spec?.poolName ? (
          <Link to={ROUTES.POOL_DETAIL.replace(':name', sv.spec.poolName)}>
            {sv.spec.poolName}
          </Link>
        ) : (
          '-'
        )}
      </Td>
      <Td dataLabel="PVC">
        {sv.spec?.pvcNamespace && sv.spec?.pvcName
          ? `${sv.spec.pvcNamespace}/${sv.spec.pvcName}`
          : '-'}
      </Td>
      <Td dataLabel="Size">
        {sv.spec?.requestedGB != null ? `${sv.spec.requestedGB} GB` : '-'}
      </Td>
      <Td dataLabel="Phase"><PhaseStatus phase={sv.status?.phase} /></Td>
      <Td dataLabel="Clone Status">
        {sv.status?.cloneStatus ? (
          <PhaseStatus phase={sv.status.cloneStatus} />
        ) : (
          '-'
        )}
      </Td>
      <Td dataLabel="Age">
        {sv.metadata?.creationTimestamp ? (
          <Timestamp timestamp={sv.metadata.creationTimestamp} />
        ) : (
          '-'
        )}
      </Td>
      <Td isActionCell>
        <ActionsColumn items={rowActions} />
      </Td>
    </Tr>
  );
};

const SubVolumeListPage: React.FC = () => {
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [activeSortIndex, setActiveSortIndex] = useState<number | undefined>(undefined);
  const [activeSortDirection, setActiveSortDirection] = useState<'asc' | 'desc' | undefined>(undefined);

  const [subVolumes, loaded, loadError] = useK8sWatchResource<SubVolume[]>({
    groupVersionKind: {
      group: SubVolumeModel.apiGroup,
      version: SubVolumeModel.apiVersion,
      kind: SubVolumeModel.kind,
    },
    isList: true,
  });

  const [data, filteredData, onFilterChange] = useListPageFilter(subVolumes);

  const getSortableRowValues = (sv: SubVolume): (string | number)[] => [
    sv.metadata?.name || '',                                              // 0 Name
    sv.spec?.poolName || '',                                              // 1 Pool
    sv.spec?.pvcNamespace && sv.spec?.pvcName
      ? `${sv.spec.pvcNamespace}/${sv.spec.pvcName}` : '',               // 2 PVC
    sv.spec?.requestedGB ?? 0,                                            // 3 Size
    sv.status?.phase || '',                                               // 4 Phase
    sv.status?.cloneStatus || '',                                         // 5 Clone Status
    new Date(sv.metadata?.creationTimestamp || 0).getTime(),              // 6 Age
  ];

  const sortedData = useMemo(() => {
    if (activeSortIndex == null || activeSortDirection == null) return filteredData;
    return [...filteredData].sort((a, b) => {
      const aVal = getSortableRowValues(a)[activeSortIndex];
      const bVal = getSortableRowValues(b)[activeSortIndex];
      if (typeof aVal === 'number' && typeof bVal === 'number') {
        return activeSortDirection === 'asc' ? aVal - bVal : bVal - aVal;
      }
      const result = String(aVal).localeCompare(String(bVal));
      return activeSortDirection === 'asc' ? result : -result;
    });
  }, [filteredData, activeSortIndex, activeSortDirection]);

  const getSortParams = (columnIndex: number): ThProps['sort'] => ({
    sortBy: { index: activeSortIndex, direction: activeSortDirection },
    onSort: (_event, index, direction) => {
      setActiveSortIndex(index);
      setActiveSortDirection(direction);
    },
    columnIndex,
  });

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
      <p style={{ padding: '0 24px', color: 'var(--pf-t--global--color--subtle)', margin: '0 0 8px 0' }}>
        SubVolumes are subdirectories within pool shares, each backing a single PVC. They are created automatically when a PVC is provisioned by the CSI driver.
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
            Error loading SubVolumes
          </div>
        )}
        {loaded && !loadError && filteredData.length === 0 && (
          <Bullseye style={{ padding: '48px 0', color: 'var(--pf-t--global--color--subtle)' }}>
            No SubVolumes found
          </Bullseye>
        )}
        {loaded && filteredData.length > 0 && (
          <Table aria-label="SubVolumes" variant="compact">
            <Thead>
              <Tr>
                <Th sort={getSortParams(0)}>Name</Th>
                <Th sort={getSortParams(1)}>Pool</Th>
                <Th sort={getSortParams(2)}>PVC</Th>
                <Th sort={getSortParams(3)}>Size</Th>
                <Th sort={getSortParams(4)}>Phase</Th>
                <Th sort={getSortParams(5)}>Clone Status</Th>
                <Th sort={getSortParams(6)}>Age</Th>
                <Th></Th>
              </Tr>
            </Thead>
            <Tbody>
              {sortedData.map((sv) => (
                <SubVolumeRow
                  key={sv.metadata?.uid || sv.metadata?.name}
                  sv={sv}
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
          resourceKind="SubVolume"
          model={SubVolumeModel}
          onClose={() => setDeleteTarget(null)}
          onDeleted={() => setDeleteTarget(null)}
        />
      )}
    </>
  );
};

export default SubVolumeListPage;
