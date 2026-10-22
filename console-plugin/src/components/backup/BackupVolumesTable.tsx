import React, { useState } from 'react';
import { useAccessReview } from '@openshift-console/dynamic-plugin-sdk';
import { Link } from 'react-router-dom';
import { Table, Thead, Tr, Th, Tbody, Td, ActionsColumn, IAction } from '@patternfly/react-table';
import { Alert, Card, CardBody, Label } from '@patternfly/react-core';
import { BackupPolicy, BackupVolumeStatus } from '../../types';
import {
  DriverSettings,
  browseFilesAvailable,
  describeBackupVolume,
  exportStateText,
} from '../../utils/backup-helpers';
import { restorePath } from '../../utils/backup-helpers';
import { columnInfo } from '../../utils/column-help';
import { API_GROUP } from '../../constants';
import CardHelpTitle from '../common/CardHelpTitle';
import DeleteBackupsDialog from './DeleteBackupsDialog';

interface Props {
  policy: BackupPolicy;
  /** What the console knows about the driver. The Browse files action shows only when file restore is present. */
  settings?: DriverSettings;
  /** A claim ("namespace/name") whose Delete backups dialog opens at once, as when a link names it. */
  deleteClaim?: string;
  /** Called when the Delete backups dialog is closed. */
  onDeleteClosed?: () => void;
}

const EXPORT_COLOR = { ok: 'green', warning: 'orange', danger: 'red' } as const;

/** One row for each volume the policy backs up, or still keeps backups of. */
const BackupVolumesTable: React.FC<Props> = ({ policy, settings, deleteClaim, onDeleteClosed }) => {
  const volumes = policy.status?.volumes || [];
  const [deleting, setDeleting] = useState<BackupVolumeStatus | undefined>(() =>
    deleteClaim ? volumes.find((v) => `${v.pvcNamespace}/${v.pvcName}` === deleteClaim) : undefined,
  );
  const [canRestore] = useAccessReview({ group: API_GROUP, resource: 'backuprestores', verb: 'create' });
  const rowActions = (volume: BackupVolumeStatus): IAction[] => {
    const actions: IAction[] = [
      canRestore
        ? {
            title: (
              <Link to={restorePath(policy.metadata?.name || '', `${volume.pvcNamespace}/${volume.pvcName}`)}>Restore</Link>
            ),
          }
        : { title: 'Restore (needs the right to create BackupRestore objects)', isDisabled: true },
      { title: 'Delete backups', onClick: () => setDeleting(volume) },
    ];
    if (browseFilesAvailable(settings)) {
      actions.push({ title: 'Browse files' });
    }
    return actions;
  };

  return (
    <Card style={{ marginTop: 16 }}>
      <CardHelpTitle helpKey="backupVolumes" />
      <CardBody>
        {volumes.length === 0 ? (
          <Alert variant="info" title="No volumes yet" isInline>
            No volumes are backed up yet. Check that the label selector matches the claims you want to back up.
          </Alert>
        ) : (
          <Table aria-label="Backed-up volumes" variant="compact">
            <Thead>
              <Tr>
                <Th>Claim</Th>
                <Th>VM</Th>
                <Th info={columnInfo('backupLast')}>Last Backup</Th>
                <Th info={columnInfo('backupOldest')}>Oldest Backup</Th>
                <Th info={columnInfo('backupRestorePoints')}>Restore Points</Th>
                <Th>Size</Th>
                <Th info={columnInfo('backupConsistency')}>Consistency</Th>
                <Th>Error</Th>
                <Th info={columnInfo('backupVolumeExport')}>Export</Th>
                <Th></Th>
              </Tr>
            </Thead>
            <Tbody>
              {volumes.map((volume) => {
                const v = describeBackupVolume(volume, policy);
                const exported = exportStateText(volume, policy);
                const actions = rowActions(volume);
                return (
                  <Tr key={volume.subVolumeName || v.claim}>
                    <Td dataLabel="Claim">
                      {v.claim}
                      {v.retained && <div>{v.retainedNote}</div>}
                    </Td>
                    <Td dataLabel="VM">{v.vm}</Td>
                    <Td dataLabel="Last Backup">{v.lastBackup}</Td>
                    <Td dataLabel="Oldest Backup">{v.oldestBackup}</Td>
                    <Td dataLabel="Restore Points">{v.restorePoints}</Td>
                    <Td dataLabel="Size">{v.size}</Td>
                    <Td dataLabel="Consistency">
                      {v.consistencyWarning ? (
                        <Label color="orange" isCompact>
                          {v.consistency}
                        </Label>
                      ) : (
                        v.consistency
                      )}
                    </Td>
                    <Td dataLabel="Error">
                      {v.error ? (
                        <Label color="red" isCompact>
                          {v.error}
                        </Label>
                      ) : (
                        '-'
                      )}
                    </Td>
                    <Td dataLabel="Export">
                      {exported.severity === 'none' ? (
                        exported.text
                      ) : (
                        <Label color={EXPORT_COLOR[exported.severity]} isCompact>
                          {exported.text}
                        </Label>
                      )}
                    </Td>
                    <Td isActionCell>{actions.length > 0 && <ActionsColumn items={actions} />}</Td>
                  </Tr>
                );
              })}
            </Tbody>
          </Table>
        )}
      </CardBody>
      {deleting && (
        <DeleteBackupsDialog
          policy={policy}
          volume={deleting}
          onClose={() => {
            setDeleting(undefined);
            onDeleteClosed?.();
          }}
        />
      )}
    </Card>
  );
};

export default BackupVolumesTable;
