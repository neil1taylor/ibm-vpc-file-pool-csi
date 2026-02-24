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
import { FileSharePoolModel } from '../../models';
import { FileSharePool } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import InlineCapacityBar from '../common/InlineCapacityBar';
import DeleteModal from '../common/DeleteModal';
import { ROUTES } from '../../constants';

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

const FileSharePoolRow: React.FC<{
  pool: FileSharePool;
  onDelete: (name: string) => void;
}> = ({ pool, onDelete }) => {
  const name = pool.metadata?.name || '';
  const detailPath = ROUTES.POOL_DETAIL.replace(':name', name);

  const rowActions: IAction[] = [
    {
      title: <Link to={detailPath}>Edit</Link>,
    },
    {
      title: 'Delete',
      onClick: () => onDelete(name),
    },
  ];

  return (
    <Tr>
      <Td dataLabel="Name"><Link to={detailPath}>{name}</Link></Td>
      <Td dataLabel="Zone">{pool.spec?.zone || '-'}</Td>
      <Td dataLabel="Profile">{pool.spec?.profile || '-'}</Td>
      <Td dataLabel="IOPS">{getIOPS(pool)}</Td>
      <Td dataLabel="Shares">{pool.status?.shareCount ?? 0}</Td>
      <Td dataLabel="Capacity">
        <InlineCapacityBar
          allocated={pool.status?.totalAllocatedGB ?? 0}
          total={pool.status?.totalCapacityGB ?? 0}
        />
      </Td>
      <Td dataLabel="PVCs">{pool.status?.totalPVCCount ?? 0}</Td>
      <Td dataLabel="Phase"><PhaseStatus phase={pool.status?.phase} /></Td>
      <Td dataLabel="Age">
        {pool.metadata?.creationTimestamp ? (
          <Timestamp timestamp={pool.metadata.creationTimestamp} />
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

/** Extract numeric IOPS value for sorting. */
function getIOPSNumber(pool: FileSharePool): number {
  if (pool.spec?.iops != null && pool.spec.iops > 0) return pool.spec.iops;
  if (pool.spec?.profile === 'dp2' && pool.spec?.shareSizeGB) return pool.spec.shareSizeGB * 100;
  return 0;
}

const FileSharePoolListPage: React.FC = () => {
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [activeSortIndex, setActiveSortIndex] = useState<number | undefined>(undefined);
  const [activeSortDirection, setActiveSortDirection] = useState<'asc' | 'desc' | undefined>(undefined);

  const [pools, loaded, loadError] = useK8sWatchResource<FileSharePool[]>({
    groupVersionKind: {
      group: FileSharePoolModel.apiGroup,
      version: FileSharePoolModel.apiVersion,
      kind: FileSharePoolModel.kind,
    },
    isList: true,
  });

  const [data, filteredData, onFilterChange] = useListPageFilter(pools);

  const getSortableRowValues = (pool: FileSharePool): (string | number)[] => [
    pool.metadata?.name || '',                              // 0 Name
    pool.spec?.zone || '',                                  // 1 Zone
    pool.spec?.profile || '',                               // 2 Profile
    getIOPSNumber(pool),                                    // 3 IOPS
    pool.status?.shareCount ?? 0,                           // 4 Shares
    0,                                                      // 5 Capacity (not sortable)
    pool.status?.totalPVCCount ?? 0,                        // 6 PVCs
    pool.status?.phase || '',                               // 7 Phase
    new Date(pool.metadata?.creationTimestamp || 0).getTime(), // 8 Age
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
      <ListPageHeader title="File Share Pools">
        <ListPageCreateLink
          to={ROUTES.POOL_CREATE}
          createAccessReview={{ groupVersionKind: { group: FileSharePoolModel.apiGroup!, version: FileSharePoolModel.apiVersion, kind: FileSharePoolModel.kind } }}
        >
          Create FileSharePool
        </ListPageCreateLink>
      </ListPageHeader>
      <p style={{ padding: '0 24px', color: 'var(--pf-t--global--color--subtle)', margin: '0 0 8px 0' }}>
        Pools group multiple VPC file shares into a single managed unit. The pool manager automatically provisions, expands, and balances shares across availability zones.
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
            Error loading FileSharePools
          </div>
        )}
        {loaded && !loadError && filteredData.length === 0 && (
          <Bullseye style={{ padding: '48px 0', color: 'var(--pf-t--global--color--subtle)' }}>
            No FileSharePools found
          </Bullseye>
        )}
        {loaded && filteredData.length > 0 && (
          <Table aria-label="File Share Pools" variant="compact">
            <Thead>
              <Tr>
                <Th sort={getSortParams(0)}>Name</Th>
                <Th sort={getSortParams(1)}>Zone</Th>
                <Th sort={getSortParams(2)}>Profile</Th>
                <Th sort={getSortParams(3)}>IOPS</Th>
                <Th sort={getSortParams(4)}>Shares</Th>
                <Th>Capacity</Th>
                <Th sort={getSortParams(6)}>PVCs</Th>
                <Th sort={getSortParams(7)}>Phase</Th>
                <Th sort={getSortParams(8)}>Age</Th>
                <Th></Th>
              </Tr>
            </Thead>
            <Tbody>
              {sortedData.map((pool) => (
                <FileSharePoolRow
                  key={pool.metadata?.uid || pool.metadata?.name}
                  pool={pool}
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
          resourceKind="FileSharePool"
          model={FileSharePoolModel}
          onClose={() => setDeleteTarget(null)}
          onDeleted={() => setDeleteTarget(null)}
          requireConfirmation
        />
      )}
    </>
  );
};

export default FileSharePoolListPage;
