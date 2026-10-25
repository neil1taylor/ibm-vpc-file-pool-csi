import React, { useState } from 'react';
import { k8sPatch, useAccessReview } from '@openshift-console/dynamic-plugin-sdk';
import { useHistory } from 'react-router-dom';
import {
  Alert,
  Button,
  Flex,
  FlexItem,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  Tooltip,
} from '@patternfly/react-core';
import { PoolBenchModel } from '../../models';
import { PoolBench } from '../../types';
import { API_GROUP, ROUTES } from '../../constants';
import { ControlAction, availableActions, controlPatch, downloadText, reportMarkdown, stepsCSV } from '../../utils/poolbench-control';
import { isActive } from '../../utils/poolbench';
import { errorMessage } from '../../utils/k8s-objects';
import { RAW_UNAVAILABLE, fetchRawCSV } from '../../utils/poolbench-raw';
import DeleteModal from '../common/DeleteModal';

const EXTEND_MINUTES = 15;

const ACTIONS: Record<ControlAction, { label: string; tip: string }> = {
  pause: { label: 'Pause', tip: 'The VMs hold their load before their next phase, and the clock of the step stands still.' },
  resume: { label: 'Resume', tip: 'The VMs go on with their load, and the clock of the step goes on.' },
  hold: { label: 'Hold at this step', tip: 'Keep measuring this VM count until you release it.' },
  release: { label: 'Release the hold', tip: 'End the measuring of this step, then go on.' },
  extend: { label: `Measure ${EXTEND_MINUTES} min longer`, tip: 'Add minutes to the measuring of this step.' },
  skip: { label: 'Skip to the next step', tip: 'End this step now; its row is marked as cut short.' },
  stop: { label: 'Stop and clean up', tip: 'End the run and delete its VMs, namespace and pool.' },
  cleanup: { label: 'Clean up now', tip: 'Delete the VMs, namespace and pool kept after the end. The results stay.' },
};

/** Actions that cannot be undone ask first. */
const CONFIRM: Partial<Record<ControlAction, { title: string; text: string; button: string }>> = {
  stop: {
    title: 'Stop the run?',
    text: 'The run ends now and deletes the test VMs, the namespace and the pool. The steps measured so far stay on this page.',
    button: 'Stop the run',
  },
  skip: {
    title: 'Skip to the next step?',
    text: 'The measuring of this step ends now. Its result counts only the time measured, and its row is marked as cut short.',
    button: 'Skip the step',
  },
  cleanup: {
    title: 'Clean up now?',
    text: 'This deletes the test VMs, the namespace and the pool that were kept after the end. The results stay.',
    button: 'Clean up',
  },
};

/** The controls of a run, and its downloads. */
const RunControls: React.FC<{ pb: PoolBench }> = ({ pb }) => {
  const history = useHistory();
  const [canUpdate] = useAccessReview({ group: API_GROUP, resource: 'poolbenches', verb: 'update' });
  const [canDelete] = useAccessReview({ group: API_GROUP, resource: 'poolbenches', verb: 'delete' });
  const [confirm, setConfirm] = useState<ControlAction | null>(null);
  const [deleting, setDeleting] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState('');
  const name = pb.metadata?.name || 'run';

  const act = async (action: ControlAction) => {
    setConfirm(null);
    setError('');
    setBusy(true);
    try {
      await k8sPatch({ model: PoolBenchModel, resource: pb, data: controlPatch(pb, action, EXTEND_MINUTES) });
    } catch (err) {
      setError(errorMessage(err, 'The change failed'));
    } finally {
      setBusy(false);
    }
  };

  const downloadRaw = async () => {
    setError('');
    try {
      downloadText(`${name}-raw.csv`, 'text/csv', await fetchRawCSV(name));
    } catch (err) {
      setError(`${RAW_UNAVAILABLE} ${errorMessage(err, '')}`);
    }
  };

  const ask = confirm ? CONFIRM[confirm] : undefined;
  return (
    <>
      {error && (
        <Alert variant="danger" isInline title="That did not work" style={{ marginBottom: 8 }}>
          {error}
        </Alert>
      )}
      {!canUpdate && availableActions(pb).length > 0 && (
        <p style={{ marginBottom: 8 }}>The controls need the right to update PoolBench objects.</p>
      )}
      <Flex spaceItems={{ default: 'spaceItemsSm' }} style={{ marginBottom: 16 }}>
        {availableActions(pb).map((a) => (
          <FlexItem key={a}>
            <Tooltip content={ACTIONS[a].tip}>
              <Button
                variant={a === 'stop' || a === 'cleanup' ? 'danger' : 'secondary'}
                isDisabled={!canUpdate || busy}
                onClick={() => (CONFIRM[a] ? setConfirm(a) : act(a))}
              >
                {ACTIONS[a].label}
              </Button>
            </Tooltip>
          </FlexItem>
        ))}
        <FlexItem align={{ default: 'alignRight' }}>
          <Tooltip content="One row for each step with all its numbers, for a spreadsheet.">
            <Button variant="link" onClick={() => downloadText(`${name}-steps.csv`, 'text/csv', stepsCSV(pb))}>
            Step table (CSV)
          </Button>
          </Tooltip>
          <Tooltip content="A short written report of the run, with the result and the settings.">
            <Button variant="link" onClick={() => downloadText(`${name}-report.md`, 'text/markdown', reportMarkdown(pb))}>
            Report (Markdown)
          </Button>
          </Tooltip>
          <Tooltip content="Every result line of every VM, for a spreadsheet. The file can be large.">
            <Button variant="link" onClick={downloadRaw}>
            Raw results (CSV)
          </Button>
          </Tooltip>
          <Tooltip content="Opens the print dialog of the browser: print the page or save it as a PDF.">
            <Button variant="link" onClick={() => window.print()}>
            Print or save as PDF
          </Button>
          </Tooltip>
          {!isActive(pb) && (
            <Tooltip content="Deletes this run and its results. The test VMs are already gone unless you kept them.">
            <Button variant="link" isDanger isDisabled={!canDelete} onClick={() => setDeleting(true)}>
              Delete run
            </Button>
          </Tooltip>
          )}
        </FlexItem>
      </Flex>
      {ask && confirm && (
        <Modal variant={ModalVariant.small} isOpen onClose={() => setConfirm(null)}>
          <ModalHeader title={ask.title} />
          <ModalBody>{ask.text}</ModalBody>
          <ModalFooter>
            <Button variant="danger" onClick={() => act(confirm)}>
              {ask.button}
            </Button>
            <Button variant="link" onClick={() => setConfirm(null)}>
              Cancel
            </Button>
          </ModalFooter>
        </Modal>
      )}
      {deleting && (
        <DeleteModal
          isOpen
          resourceName={name}
          resourceKind="PoolBench"
          model={PoolBenchModel}
          note="The results of the run go with it: the step table, the log and the raw results. Download them first if you want to keep them."
          onClose={() => setDeleting(false)}
          onDeleted={() => {
            setDeleting(false);
            history.push(ROUTES.POOLBENCH);
          }}
        />
      )}
    </>
  );
};

export default RunControls;
