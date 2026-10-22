import React from 'react';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { Alert, Card, CardBody, CardTitle, Label } from '@patternfly/react-core';
import { SubVolumeReplicationStatus } from '../../types';
import { describeVolumeStatus } from '../../utils/replication-helpers';
import { formatAge, humanizeBytes } from '../../utils/resource-helpers';
import { columnInfo } from '../../utils/column-help';

interface Props {
  statuses: SubVolumeReplicationStatus[] | undefined;
}

const SEVERITY_COLOR = { warning: 'orange', danger: 'red' } as const;

/**
 * One row for each volume of a policy. A running VM's disk also shows the
 * moment its last copy shows, whether the copy is consistent, and its restore
 * points. A message that only says a volume is waiting is a warning, not an error.
 */
const VolumeStatusTable: React.FC<Props> = ({ statuses }) => {
  if (!statuses || statuses.length === 0) {
    return (
      <Alert variant="info" title="No sync status" isInline style={{ marginTop: 16 }}>
        No SubVolume sync statuses have been reported yet.
      </Alert>
    );
  }

  return (
    <Card style={{ marginTop: 16 }}>
      <CardTitle>Sync Status</CardTitle>
      <CardBody>
        <Table aria-label="SubVolume sync statuses" variant="compact">
          <Thead>
            <Tr>
              <Th>SubVolume Name</Th>
              <Th info={columnInfo('syncCopyMoment')}>Last Sync / Copy Moment</Th>
              <Th info={columnInfo('syncConsistency')}>Consistency</Th>
              <Th info={columnInfo('syncRestorePoints')}>Restore Points</Th>
              <Th info={columnInfo('syncNextCopy')}>Next Copy</Th>
              <Th>Bytes Synced</Th>
              <Th info={columnInfo('syncStatus')}>Status</Th>
            </Tr>
          </Thead>
          <Tbody>
            {statuses.map((s) => {
              const v = describeVolumeStatus(s);
              return (
                <Tr key={s.subVolumeName}>
                  <Td>{s.subVolumeName}</Td>
                  <Td>{v.isVMDisk ? v.copyMoment : s.lastSyncTime ? formatAge(s.lastSyncTime) : '-'}</Td>
                  <Td>
                    {v.consistencyWarning ? (
                      <Label color="orange" isCompact>
                        {v.consistency}
                      </Label>
                    ) : (
                      v.consistency
                    )}
                    {v.consistencyReason && <div>{v.consistencyReason}</div>}
                  </Td>
                  <Td>{v.restorePoints}</Td>
                  <Td>{v.nextCopy || '-'}</Td>
                  <Td>{s.bytesSynced != null ? humanizeBytes(s.bytesSynced) : '-'}</Td>
                  <Td>
                    {v.copyPhase && <div>{v.copyPhase}</div>}
                    {v.message ? (
                      <span data-severity={v.severity}>
                        <Label color={SEVERITY_COLOR[v.severity as 'warning' | 'danger']} isCompact>
                          {v.message}
                        </Label>
                      </span>
                    ) : (
                      !v.copyPhase && '-'
                    )}
                  </Td>
                </Tr>
              );
            })}
          </Tbody>
        </Table>
      </CardBody>
    </Card>
  );
};

export default VolumeStatusTable;
