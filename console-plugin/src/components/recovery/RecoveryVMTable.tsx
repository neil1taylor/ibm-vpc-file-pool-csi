import React from 'react';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { Button, Label } from '@patternfly/react-core';
import { PlanRow, Staleness } from '../../utils/failover-helpers';
import { columnInfo } from '../../utils/column-help';

interface Props {
  rows: PlanRow[];
  /** The stale-data check for each VM, by VM name. */
  staleness: Record<string, Staleness>;
  onSelect: (vm: string) => void;
}

/** One row for each running VM found on the copy. The VM name opens its detail. */
const RecoveryVMTable: React.FC<Props> = ({ rows, staleness, onSelect }) => (
  <Table aria-label="Running VMs found on the copy" variant="compact">
    <Thead>
      <Tr>
        <Th>VM</Th>
        <Th>Disks</Th>
        <Th info={columnInfo('recoveryRewindDisks')}>Disks to Rewind</Th>
        <Th info={columnInfo('recoveryPlanMoment')}>Will Show</Th>
        <Th info={columnInfo('recoveryMoments')}>Moments to Choose From</Th>
      </Tr>
    </Thead>
    <Tbody>
      {rows.map((row) => {
        const stale = staleness[row.name];
        return (
          <Tr key={row.name}>
            <Td dataLabel="VM">
              <Button variant="link" isInline onClick={() => onSelect(row.name)}>
                {row.name}
              </Button>
            </Td>
            <Td dataLabel="Disks">{row.disks}</Td>
            <Td dataLabel="Disks to Rewind">{row.rewindDisks}</Td>
            <Td dataLabel="Will Show">
              {row.age}
              {stale?.stale && (
                <div>
                  <Label color="orange" isCompact>
                    {stale.text}
                  </Label>
                </div>
              )}
              {stale && !stale.known && !stale.stale && (
                <div style={{ color: 'var(--pf-t--global--color--subtle)' }}>{stale.text}</div>
              )}
            </Td>
            <Td dataLabel="Moments to Choose From">{row.momentCount}</Td>
          </Tr>
        );
      })}
    </Tbody>
  </Table>
);

export default RecoveryVMTable;
