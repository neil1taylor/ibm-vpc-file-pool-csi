import React from 'react';
import { Progress, ProgressMeasureLocation, ProgressVariant } from '@patternfly/react-core';
import { utilizationPercent } from '../../utils/resource-helpers';

interface CapacityBarProps {
  allocated: number;
  total: number;
  unit?: string;
  title?: string;
}

const CapacityBar: React.FC<CapacityBarProps> = ({
  allocated,
  total,
  unit = 'GB',
  title,
}) => {
  const percent = utilizationPercent(allocated, total);
  let variant: ProgressVariant | undefined;
  if (percent >= 95) {
    variant = ProgressVariant.danger;
  } else if (percent >= 80) {
    variant = ProgressVariant.warning;
  }

  return (
    <Progress
      value={percent}
      title={title || `${allocated} / ${total} ${unit}`}
      label={`${percent}%`}
      measureLocation={ProgressMeasureLocation.outside}
      variant={variant}
    />
  );
};

export default CapacityBar;
