import React, { useState } from 'react';
import {
  Alert,
  Button,
  Form,
  FormGroup,
  FormSelect,
  FormSelectOption,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Stack,
  StackItem,
  TextInput,
} from '@patternfly/react-core';
import { Failover } from '../../types';
import { ageText, confirmationWord, executeSummary } from '../../utils/failover-helpers';

interface Props {
  plan: Failover;
  vmName: string;
  /** Names of the FileSharePools on this cluster. */
  pools: string[];
  creating: boolean;
  error: string;
  onConfirm: (drPool: string) => void;
  onClose: () => void;
}

/**
 * The last step before a failover. It says what will happen: the VM and its moment, the disks that are
 * rewound, and the PVCs that are made. A failover covers everything on the copy, so every VM is listed.
 * The operator types a word to confirm.
 */
const FailoverConfirmModal: React.FC<Props> = ({ plan, vmName, pools, creating, error, onConfirm, onClose }) => {
  const [typed, setTyped] = useState('');
  const [chosenPool, setChosenPool] = useState('');
  const summary = executeSummary(plan, vmName);
  const word = confirmationWord(plan, vmName);
  const chosen = summary.vms.find((v) => v.name === vmName);
  const pool = chosenPool || (pools.length === 1 ? pools[0] : '');
  const canGo = typed === word && !!pool && !creating;
  const disks = (n: number) => `${n} ${n === 1 ? 'disk' : 'disks'} to rewind`;

  return (
    <Modal variant={ModalVariant.medium} isOpen onClose={onClose} aria-label={`Fail over ${vmName}`}>
      <ModalHeader title={`Fail over ${vmName}?`} />
      <ModalBody>
        <Stack hasGutter>
          {chosen && (
            <StackItem>
              {`${vmName} will show the copy from ${chosen.moment} (${ageText(chosen.moment, Date.now())} ago).`}
            </StackItem>
          )}
          {summary.otherVMs > 0 && (
            <StackItem>
              <Alert variant="warning" isInline title={`This fails over all ${summary.vms.length} VMs on the copy`}>
                A failover covers everything the copy holds, not only {vmName}. Each other VM shows the newest moment
                it has at or before the chosen one.
                <ul>
                  {summary.vms.map((v) => (
                    <li key={v.name}>{`${v.name}: ${v.moment}, ${disks(v.rewindDisks)}`}</li>
                  ))}
                </ul>
              </Alert>
            </StackItem>
          )}
          <StackItem>
            {disks(summary.rewindDisks)}. A rewound disk is copied whole, which takes minutes. The latest copy of the
            disk is never changed.
          </StackItem>
          <StackItem>
            These PVCs will be made:
            <ul>
              {summary.pvcs.map((p) => (
                <li key={`${p.namespace}/${p.name}`}>{`${p.namespace}/${p.name}`}</li>
              ))}
            </ul>
          </StackItem>
          <StackItem>
            <Form onSubmit={(e) => e.preventDefault()}>
              <FormGroup label="DR pool" fieldId="failover-dr-pool">
                <FormSelect id="failover-dr-pool" value={pool} onChange={(_e, v) => setChosenPool(v)}>
                  {pools.length !== 1 && <FormSelectOption value="" label="Choose a pool" />}
                  {pools.map((p) => (
                    <FormSelectOption key={p} value={p} label={p} />
                  ))}
                </FormSelect>
              </FormGroup>
              <FormGroup label={`Type ${word} to confirm`} fieldId="failover-confirm">
                <TextInput id="failover-confirm" value={typed} onChange={(_e, v) => setTyped(v)} autoComplete="off" />
              </FormGroup>
            </Form>
          </StackItem>
          {pools.length === 0 && (
            <StackItem>
              <Alert variant="warning" isInline title="No FileSharePool found">
                The new volumes belong to a FileSharePool on this cluster, and there is none.
              </Alert>
            </StackItem>
          )}
          {error && (
            <StackItem>
              <Alert variant="danger" isInline title="The failover was refused">
                {error}
              </Alert>
            </StackItem>
          )}
        </Stack>
      </ModalBody>
      <ModalFooter>
        <Button variant="danger" onClick={() => onConfirm(pool)} isDisabled={!canGo} isLoading={creating}>
          Fail over
        </Button>
        <Button variant="link" onClick={onClose} isDisabled={creating}>
          Cancel
        </Button>
      </ModalFooter>
    </Modal>
  );
};

export default FailoverConfirmModal;
