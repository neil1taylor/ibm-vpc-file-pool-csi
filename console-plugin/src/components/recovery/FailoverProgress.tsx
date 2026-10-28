import React from 'react';
import { Link } from 'react-router-dom';
import { Alert, Spinner } from '@patternfly/react-core';
import { Failover } from '../../types';
import { describeFailoverState, isBusyMessage } from '../../utils/failover-helpers';
import { pvcPath } from '../../utils/k8s-objects';
import { formatDuration } from '../../utils/resource-helpers';
import ForceOption from './ForceOption';

interface Props {
  failover: Failover;
  now: number;
  /** Plan again with force. Offered only when the driver refused because a copy is being applied. */
  onPlanWithForce?: () => void;
  isBusy: boolean;
}

/** The phase and the result of an Execute: still working, done with the PVCs it made, or failed with the message. */
const FailoverProgress: React.FC<Props> = ({ failover, now, onPlanWithForce, isBusy }) => {
  const state = describeFailoverState(failover, now);
  const pvcs = failover.status?.createdPVCs || [];

  if (state.working) {
    return (
      <Alert variant="info" isInline customIcon={<Spinner size="md" />} title={state.title}>
        <p>{state.detail}</p>
        <p>{`Running for ${state.elapsed}. Phase: ${failover.status?.phase || 'Starting'}.`}</p>
      </Alert>
    );
  }
  if (state.failed) {
    return (
      <Alert variant="danger" isInline title={state.title}>
        <p>{state.message || 'The driver gave no reason.'}</p>
        {isBusyMessage(state.message) && onPlanWithForce && (
          <ForceOption onPlanWithForce={onPlanWithForce} isBusy={isBusy} />
        )}
      </Alert>
    );
  }
  return (
    <Alert variant="success" isInline title={state.title}>
      {failover.status?.rpo && <p>{`The oldest VM data is ${formatDuration(failover.status.rpo)} old.`}</p>}
      {pvcs.length > 0 ? (
        <>
          <p>These PVCs were made:</p>
          <ul>
            {pvcs.map((p) => (
              <li key={`${p.namespace}/${p.name}`}>
                <Link to={pvcPath(p.namespace, p.name)}>{`${p.namespace}/${p.name}`}</Link>
              </li>
            ))}
          </ul>
        </>
      ) : (
        <p>No PVC was made.</p>
      )}
    </Alert>
  );
};

export default FailoverProgress;
