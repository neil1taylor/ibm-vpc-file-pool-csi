import React from 'react';
import {
  Table,
  Thead,
  Tr,
  Th,
  Tbody,
  Td,
} from '@patternfly/react-table';
import { Label } from '@patternfly/react-core';
import { Condition } from '../../types';
import { formatAge } from '../../utils/resource-helpers';

interface ConditionsTableProps {
  conditions: Condition[] | undefined;
}

const ConditionsTable: React.FC<ConditionsTableProps> = ({ conditions }) => {
  if (!conditions || conditions.length === 0) {
    return <p>No conditions reported.</p>;
  }

  return (
    <Table aria-label="Conditions" variant="compact">
      <Thead>
        <Tr>
          <Th>Type</Th>
          <Th>Status</Th>
          <Th>Reason</Th>
          <Th>Message</Th>
          <Th>Last Transition</Th>
        </Tr>
      </Thead>
      <Tbody>
        {conditions.map((c) => (
          <Tr key={c.type}>
            <Td>{c.type}</Td>
            <Td>
              <Label color={c.status === 'True' ? 'green' : c.status === 'False' ? 'red' : 'grey'}>
                {c.status}
              </Label>
            </Td>
            <Td>{c.reason || '-'}</Td>
            <Td>{c.message || '-'}</Td>
            <Td>{formatAge(c.lastTransitionTime)}</Td>
          </Tr>
        ))}
      </Tbody>
    </Table>
  );
};

export default ConditionsTable;
