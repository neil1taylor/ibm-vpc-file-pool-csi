import React, { useState } from 'react';
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
  Td,
  Tbody,
  ActionsColumn,
  IAction,
} from '@patternfly/react-table';
import { Bullseye, Spinner } from '@patternfly/react-core';
import { VolumeGroupSnapshotModel } from '../../models';
import { VolumeGroupSnapshot } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import DeleteModal from '../common/DeleteModal';
import { ROUTES } from '../../constants';

const VolumeGroupSnapshotRow: React.FC<{
  snap: VolumeGroupSnapshot;
  onDelete: (name: string) => void;
}> = ({ snap, onDelete }) => {
  const name = snap.metadata?.name || '';
  const detailPath = ROUTES.GROUP_SNAPSHOT_DETAIL.replace(':name', name);
  const memberCount = snap.status?.memberCount ?? 0;
  const readyCount = snap.status?.readyCount ?? 0;

  const rowActions: IAction[] = [
    {
      title: <Link to={detailPath}>View Details</Link>,
    },
    {
      title: 'Delete',
      onClick: () => onDelete(name),
    },
  ];

  return (
    <Tr>
      <Td dataLabel="Name"><Link to={detailPath}>{name}</Link></Td>
      <Td dataLabel="Pool">{snap.spec?.poolName || '-'}</Td>
      <Td dataLabel="Members">{memberCount}</Td>
      <Td dataLabel="Ready">{readyCount}/{memberCount}</Td>
      <Td dataLabel="Phase"><PhaseStatus phase={snap.status?.phase} /></Td>
      <Td dataLabel="Age">
        {snap.metadata?.creationTimestamp ? (
          <Timestamp timestamp={snap.metadata.creationTimestamp} />
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

const VolumeGroupSnapshotListPage: React.FC = () => {
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);

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
      <p style={{ padding: '0 24px', color: 'var(--pf-t--global--color--subtle)', margin: '0 0 8px 0' }}>
        Group Snapshots capture consistent point-in-time copies of multiple SubVolumes within a pool in a single coordinated operation.
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
            Error loading VolumeGroupSnapshots
          </div>
        )}
        {loaded && !loadError && filteredData.length === 0 && (
          <Bullseye style={{ padding: '48px 0', color: 'var(--pf-t--global--color--subtle)' }}>
            No VolumeGroupSnapshots found
          </Bullseye>
        )}
        {loaded && filteredData.length > 0 && (
          <Table aria-label="Volume Group Snapshots" variant="compact">
            <Thead>
              <Tr>
                <Th>Name</Th>
                <Th>Pool</Th>
                <Th>Members</Th>
                <Th>Ready</Th>
                <Th>Phase</Th>
                <Th>Age</Th>
                <Th></Th>
              </Tr>
            </Thead>
            <Tbody>
              {filteredData.map((snap) => (
                <VolumeGroupSnapshotRow
                  key={snap.metadata?.uid || snap.metadata?.name}
                  snap={snap}
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
          resourceKind="VolumeGroupSnapshot"
          model={VolumeGroupSnapshotModel}
          onClose={() => setDeleteTarget(null)}
          onDeleted={() => setDeleteTarget(null)}
        />
      )}
    </>
  );
};

export default VolumeGroupSnapshotListPage;
