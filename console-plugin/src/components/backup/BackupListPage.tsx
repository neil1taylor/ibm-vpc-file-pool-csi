import React, { useMemo, useState } from 'react';
import {
  k8sPatch,
  useAccessReview,
  useK8sWatchResource,
  ListPageHeader,
  ListPageBody,
  ListPageCreateLink,
  ListPageFilter,
  useListPageFilter,
} from '@openshift-console/dynamic-plugin-sdk';
import { Link } from 'react-router-dom';
import { Table, Thead, Tr, Th, ThProps, Td, Tbody, ActionsColumn, IAction } from '@patternfly/react-table';
import { Alert, Bullseye, Spinner } from '@patternfly/react-core';
import { BackupPolicyModel } from '../../models';
import { BackupPolicy } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import DeleteModal from '../common/DeleteModal';
import { BACKUP_DELETE_NOTE } from './backup-texts';
import BackupProblemBanner from './BackupProblemBanner';
import { stalestBackupAge, suspendPatch } from '../../utils/backup-helpers';
import { errorMessage } from '../../utils/k8s-objects';
import { columnInfo } from '../../utils/column-help';
import { API_GROUP, ROUTES } from '../../constants';

const DEFAULT_KEEP_FOR = '7d';

const BackupListPage: React.FC = () => {
  const [activeSortIndex, setActiveSortIndex] = useState<number | undefined>(undefined);
  const [activeSortDirection, setActiveSortDirection] = useState<'asc' | 'desc' | undefined>(undefined);

  const [policies, loaded, loadError] = useK8sWatchResource<BackupPolicy[]>({
    groupVersionKind: {
      group: BackupPolicyModel.apiGroup,
      version: BackupPolicyModel.apiVersion,
      kind: BackupPolicyModel.kind,
    },
    isList: true,
  });

  const [data, filteredData, onFilterChange] = useListPageFilter(policies);
  const [canUpdate] = useAccessReview({ group: API_GROUP, resource: 'backuppolicies', verb: 'update' });
  const [canDelete] = useAccessReview({ group: API_GROUP, resource: 'backuppolicies', verb: 'delete' });
  const [deleteTarget, setDeleteTarget] = useState<string | null>(null);
  const [actionError, setActionError] = useState('');

  const toggleSuspend = async (p: BackupPolicy) => {
    setActionError('');
    try {
      await k8sPatch({ model: BackupPolicyModel, resource: p, data: suspendPatch(!p.spec?.suspend) });
    } catch (err) {
      setActionError(errorMessage(err, 'Failed to change the policy'));
    }
  };

  const rowActions = (p: BackupPolicy): IAction[] => {
    const name = p.metadata?.name || '';
    const needUpdate = ' (needs the right to update BackupPolicy objects)';
    const needDelete = ' (needs the right to delete BackupPolicy objects)';
    return [
      { title: <Link to={ROUTES.BACKUP_DETAIL.replace(':name', name)}>View Details</Link> },
      canUpdate
        ? { title: <Link to={ROUTES.BACKUP_EDIT.replace(':name', name)}>Edit</Link> }
        : { title: `Edit${needUpdate}`, isDisabled: true },
      canUpdate
        ? { title: p.spec?.suspend ? 'Resume' : 'Suspend', onClick: () => toggleSuspend(p) }
        : { title: `${p.spec?.suspend ? 'Resume' : 'Suspend'}${needUpdate}`, isDisabled: true },
      canDelete
        ? { title: 'Delete', onClick: () => setDeleteTarget(name) }
        : { title: `Delete${needDelete}`, isDisabled: true },
    ];
  };

  const sortValues = (p: BackupPolicy): (string | number)[] => [
    p.metadata?.name || '',
    p.spec?.snapshotPool || '',
    p.spec?.schedule || '',
    p.spec?.keepFor || DEFAULT_KEEP_FOR,
    p.status?.phase || '',
    p.status?.volumeCount ?? 0,
  ];

  const sortedData = useMemo(() => {
    if (activeSortIndex == null || activeSortDirection == null) return filteredData;
    return [...filteredData].sort((a, b) => {
      const aVal = sortValues(a)[activeSortIndex];
      const bVal = sortValues(b)[activeSortIndex];
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
      <ListPageHeader title="Backups">
        <ListPageCreateLink
          to={ROUTES.BACKUP_CREATE}
          createAccessReview={{ groupVersionKind: { group: BackupPolicyModel.apiGroup!, version: BackupPolicyModel.apiVersion, kind: BackupPolicyModel.kind } }}
        >
          Create BackupPolicy
        </ListPageCreateLink>
        <Link to={ROUTES.BACKUP_COS_RESTORE} style={{ marginLeft: 16 }}>
          Restore from COS
        </Link>
      </ListPageHeader>
      <p style={{ padding: '0 24px', color: 'var(--pf-t--global--color--subtle)', margin: '0 0 8px 0' }}>
        A backup policy copies the volumes it selects into a snapshot pool on a schedule, and keeps the copies for a set time.
        Open a policy to see each volume and the moments you can restore to.
      </p>
      <ListPageBody>
        {(policies || []).map((p) => (
          <BackupProblemBanner key={p.metadata?.name} policy={p} showName />
        ))}
        {actionError && (
          <Alert variant="danger" isInline title="The change failed" style={{ marginBottom: 16 }}>
            {actionError}
          </Alert>
        )}
        <ListPageFilter data={data} loaded={loaded} onFilterChange={onFilterChange} />
        {!loaded && (
          <Bullseye style={{ padding: '48px 0' }}>
            <Spinner size="xl" />
          </Bullseye>
        )}
        {loadError && (
          <div style={{ padding: '24px', color: 'var(--pf-t--global--color--status--danger--default)' }}>
            Error loading BackupPolicies
          </div>
        )}
        {loaded && !loadError && filteredData.length === 0 && (
          <Bullseye style={{ padding: '48px 0', color: 'var(--pf-t--global--color--subtle)' }}>
            No BackupPolicies found
          </Bullseye>
        )}
        {loaded && filteredData.length > 0 && (
          <Table aria-label="Backup policies" variant="compact">
            <Thead>
              <Tr>
                <Th sort={getSortParams(0)}>Name</Th>
                <Th sort={getSortParams(1)}>Snapshot Pool</Th>
                <Th sort={getSortParams(2)}>Schedule</Th>
                <Th sort={getSortParams(3)}>Keep</Th>
                <Th sort={getSortParams(4)} info={columnInfo('backupPhase')}>Phase</Th>
                <Th sort={getSortParams(5)}>Volumes</Th>
                <Th info={columnInfo('backupStalest')}>Stalest Backup</Th>
                <Th info={columnInfo('backupListExport')}>Export</Th>
                <Th></Th>
              </Tr>
            </Thead>
            <Tbody>
              {sortedData.map((p) => {
                const name = p.metadata?.name || '';
                return (
                  <Tr key={p.metadata?.uid || name}>
                    <Td dataLabel="Name">
                      <Link to={ROUTES.BACKUP_DETAIL.replace(':name', name)}>{name}</Link>
                    </Td>
                    <Td dataLabel="Snapshot Pool">{p.spec?.snapshotPool || '-'}</Td>
                    <Td dataLabel="Schedule">{p.spec?.schedule || '-'}</Td>
                    <Td dataLabel="Keep">{p.spec?.keepFor || DEFAULT_KEEP_FOR}</Td>
                    <Td dataLabel="Phase">
                      <PhaseStatus phase={p.status?.phase} />
                    </Td>
                    <Td dataLabel="Volumes">{p.status?.volumeCount ?? 0}</Td>
                    <Td dataLabel="Stalest Backup">{stalestBackupAge(p)}</Td>
                    <Td dataLabel="Export">{p.spec?.export ? 'On' : 'Off'}</Td>
                    <Td isActionCell>
                      <ActionsColumn items={rowActions(p)} />
                    </Td>
                  </Tr>
                );
              })}
            </Tbody>
          </Table>
        )}
      </ListPageBody>
      {deleteTarget && (
        <DeleteModal
          isOpen
          resourceName={deleteTarget}
          resourceKind="BackupPolicy"
          model={BackupPolicyModel}
          note={BACKUP_DELETE_NOTE}
          onClose={() => setDeleteTarget(null)}
          onDeleted={() => setDeleteTarget(null)}
        />
      )}
    </>
  );
};

export default BackupListPage;
