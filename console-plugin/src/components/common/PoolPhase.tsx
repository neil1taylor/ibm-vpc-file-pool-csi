import React from 'react';
import { FileSharePool } from '../../types';
import { poolPhaseReason } from '../../utils/pool-helpers';
import PhaseStatus from './PhaseStatus';

/** Pool phase, with the reason under it when the pool is not Ready. */
const PoolPhase: React.FC<{ pool: FileSharePool }> = ({ pool }) => {
  const reason = poolPhaseReason(pool);
  return (
    <>
      <PhaseStatus phase={pool.status?.phase} />
      {reason && (
        <div>
          <small style={{ color: 'var(--pf-t--global--text--color--subtle)' }}>{reason}</small>
        </div>
      )}
    </>
  );
};

export default PoolPhase;
