import React from 'react';
import { Label } from '@patternfly/react-core';
import { PoolShareStatus } from '../../types';
import { shareHealth } from '../../utils/pool-helpers';

const ShareStatus: React.FC<{ share: PoolShareStatus }> = ({ share }) => {
  const { label, status } = shareHealth(share);
  return <Label status={status}>{label}</Label>;
};

export default ShareStatus;
