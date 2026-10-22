import React from 'react';
import { Link } from 'react-router-dom';
import { Alert } from '@patternfly/react-core';
import { BackupPolicy } from '../../types';
import { policyProblem } from '../../utils/backup-helpers';
import { ROUTES } from '../../constants';

/** A banner for a policy in the Error phase, in plain words, with a link to the pool when its space is the problem. */
const BackupProblemBanner: React.FC<{ policy: BackupPolicy; showName?: boolean }> = ({ policy, showName }) => {
  const problem = policyProblem(policy);
  if (!problem) return null;
  return (
    <Alert
      variant="danger"
      isInline
      style={{ marginBottom: 16 }}
      title={showName ? `${policy.metadata?.name}: ${problem.title}` : problem.title}
    >
      <p>{problem.text}</p>
      {problem.poolName && (
        <Link to={ROUTES.POOL_DETAIL.replace(':name', problem.poolName)}>Open the pool {problem.poolName}</Link>
      )}
    </Alert>
  );
};

export default BackupProblemBanner;
