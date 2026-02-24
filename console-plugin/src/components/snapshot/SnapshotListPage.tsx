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
import { Bullseye, Spinner, Label } from '@patternfly/react-core';
import { SnapshotModel } from '../../models';
import { Snapshot } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import DeleteModal from '../common/DeleteModal';
import { ROUTES } from '../../constants';

const SnapshotRow: React.FC<{
  snap: Snapshot;
  onDelete: (name: string) => void;
}> = ({ snap, onDelete }) => {
  const name = snap.metadata?.name || '';
  const detailPath = ROUTES.SNAPSHOT_DETAIL.replace(':name', name);

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
      <Td dataLabel="Source SubVolume">{snap.spec?.sourceSubVolume || '-'}</Td>
      <Td dataLabel="Size">
        {snap.spec?.sizeGB != null ? `${snap.spec.sizeGB} GB` : '-'}
      </Td>
      <Td dataLabel="Phase"><PhaseStatus phase={snap.status?.phase} /></Td>
      <Td dataLabel="Ready">
        <Label color={snap.status?.readyToUse ? 'green' : 'grey'}>
          {snap.status?.readyToUse ? 'True' : 'False'}
        </Label>
      </Td>
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

const SnapshotListPage: React.FC = () => {
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);

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
      <p style={{ padding: '0 24px', color: 'var(--pf-t--global--color--subtle)', margin: '0 0 8px 0' }}>
        Snapshots capture point-in-time copies of SubVolume data for backup and recovery.
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
            Error loading Snapshots
          </div>
        )}
        {loaded && !loadError && filteredData.length === 0 && (
          <Bullseye style={{ padding: '48px 0', color: 'var(--pf-t--global--color--subtle)' }}>
            No Snapshots found
          </Bullseye>
        )}
        {loaded && filteredData.length > 0 && (
          <Table aria-label="Snapshots" variant="compact">
            <Thead>
              <Tr>
                <Th>Name</Th>
                <Th>Pool</Th>
                <Th>Source SubVolume</Th>
                <Th>Size</Th>
                <Th>Phase</Th>
                <Th>Ready</Th>
                <Th>Age</Th>
                <Th></Th>
              </Tr>
            </Thead>
            <Tbody>
              {filteredData.map((snap) => (
                <SnapshotRow
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
          resourceKind="Snapshot"
          model={SnapshotModel}
          onClose={() => setDeleteTarget(null)}
          onDeleted={() => setDeleteTarget(null)}
        />
      )}
    </>
  );
};

export default SnapshotListPage;
