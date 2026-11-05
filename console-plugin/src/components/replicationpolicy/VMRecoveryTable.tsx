import React from 'react';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { Card, CardBody, CardTitle, Label } from '@patternfly/react-core';
import { VMReplicationStatus } from '../../types';
import { describeVMRecovery } from '../../utils/replication-helpers';
import { columnInfo } from '../../utils/column-help';

interface Props {
  vms: VMReplicationStatus[] | undefined;
  schedule: string | undefined;
}

/**
 * One row for each running VM of a policy: the restore point its copy shows, whether the copy is consistent, and the
 * restore points a recovery can use (the restore points all of the VM's disks share). Read-only. It is the source cluster's
 * view of the destination and can be a few minutes old.
 */
const VMRecoveryTable: React.FC<Props> = ({ vms, schedule }) => {
  const rows = describeVMRecovery(vms, schedule);
  if (rows.length === 0) return null;

  return (
    <Card style={{ marginTop: 16 }}>
      <CardTitle>Recovery</CardTitle>
      <CardBody>
        <Table aria-label="Running VMs and the restore points they can be recovered to" variant="compact">
          <Thead>
            <Tr>
              <Th>VM</Th>
              <Th>Disks</Th>
              <Th info={columnInfo('recoveryCopyMoment')}>Copy Restore Point</Th>
              <Th info={columnInfo('syncConsistency')}>Consistency</Th>
              <Th info={columnInfo('recoveryMoments')}>Restore Points to Recover To</Th>
            </Tr>
          </Thead>
          <Tbody>
            {rows.map((v) => (
              <Tr key={v.name}>
                <Td>{v.name}</Td>
                <Td>{v.disks}</Td>
                <Td>
                  {v.copyMoment}
                  {v.stale && (
                    <div>
                      <Label color="orange" isCompact>
                        {v.staleText}
                      </Label>
                    </div>
                  )}
                </Td>
                <Td>
                  {v.consistencyWarning ? (
                    <Label color="orange" isCompact>
                      {v.consistency}
                    </Label>
                  ) : (
                    v.consistency
                  )}
                </Td>
                <Td>
                  {v.momentCount > 0 ? (
                    `${v.momentCount} ${v.momentCount === 1 ? 'restore point' : 'restore points'}: ${v.moments.join(', ')}`
                  ) : (
                    <Label color="orange" isCompact>
                      {v.disks > 1 ? 'The disks share no restore point' : 'No restore point yet'}
                    </Label>
                  )}
                </Td>
              </Tr>
            ))}
          </Tbody>
        </Table>
      </CardBody>
    </Card>
  );
};

export default VMRecoveryTable;
