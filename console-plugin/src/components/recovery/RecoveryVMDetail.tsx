import React, { useState } from 'react';
import { Button, Card, CardBody, CardTitle, Label, Radio, Stack, StackItem } from '@patternfly/react-core';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { VMDetailView } from '../../utils/failover-helpers';

interface Props {
  detail: VMDetailView;
  onBack: () => void;
  /** Called with the moment the operator chose. */
  onFailOver: (moment: string) => void;
  isDisabled: boolean;
  disabledReason?: string;
}

/** One VM: its disks, and the moments it can show, newest first. The operator chooses a moment and fails over this VM only. */
const RecoveryVMDetail: React.FC<Props> = ({ detail, onBack, onFailOver, isDisabled, disabledReason }) => {
  const current = detail.moments.find((m) => m.isCurrent) || detail.moments[0];
  const [moment, setMoment] = useState(current?.iso || '');

  return (
    <Stack hasGutter>
      <StackItem>
        <Button variant="link" isInline onClick={onBack}>
          Back to the VMs
        </Button>
      </StackItem>
      <StackItem>
        <Card>
          <CardTitle>Disks of {detail.name}</CardTitle>
          <CardBody>
            <Table aria-label="Disks of the VM" variant="compact">
              <Thead>
                <Tr>
                  <Th>Claim</Th>
                  <Th>Volume</Th>
                  <Th>What happens</Th>
                </Tr>
              </Thead>
              <Tbody>
                {detail.disks.map((d) => (
                  <Tr key={d.subVolume}>
                    <Td dataLabel="Claim">{d.claim}</Td>
                    <Td dataLabel="Volume">{d.subVolume}</Td>
                    <Td dataLabel="What happens">
                      {d.rewind ? (
                        <Label color="orange" isCompact>
                          {d.note}
                        </Label>
                      ) : (
                        d.note
                      )}
                    </Td>
                  </Tr>
                ))}
              </Tbody>
            </Table>
          </CardBody>
        </Card>
      </StackItem>
      <StackItem>
        <Card>
          <CardTitle>Moments to fail over to</CardTitle>
          <CardBody>
            <p>
              Choose the moment the VM will show. Every disk of the VM can show these moments. Newest first. The moment marked &quot;in the plan&quot; is the one the plan on screen shows.
            </p>
            {detail.moments.map((m) => (
              <Radio
                key={m.iso}
                id={`moment-${m.iso}`}
                name="recovery-moment"
                value={m.iso}
                label={`${m.age} ago (${m.iso})${m.isCurrent ? ' - newest' : ''}`}
                isChecked={moment === m.iso}
                onChange={() => setMoment(m.iso)}
              />
            ))}
          </CardBody>
        </Card>
      </StackItem>
      <StackItem>
        <Button variant="danger" onClick={() => onFailOver(moment)} isDisabled={isDisabled || !moment}>
          Fail over this VM
        </Button>
        {disabledReason && <span style={{ marginLeft: 12 }}>{disabledReason}</span>}
      </StackItem>
    </Stack>
  );
};

export default RecoveryVMDetail;
