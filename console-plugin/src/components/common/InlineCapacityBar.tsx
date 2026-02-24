import React from 'react';
import { Progress, ProgressMeasureLocation, ProgressVariant } from '@patternfly/react-core';
import { utilizationPercent } from '../../utils/resource-helpers';

interface InlineCapacityBarProps {
  allocated: number;
  total: number;
  unit?: string;
}

/**
 * Compact capacity bar for table cells. Renders the text label inline above
 * a progress bar with measureLocation=inside, avoiding the multi-line overflow
 * that CapacityBar (measureLocation=outside) causes in fixed-layout tables.
 */
const InlineCapacityBar: React.FC<InlineCapacityBarProps> = ({
  allocated,
  total,
  unit = 'GB',
}) => {
  const percent = utilizationPercent(allocated, total);
  let variant: ProgressVariant | undefined;
  if (percent >= 95) {
    variant = ProgressVariant.danger;
  } else if (percent >= 80) {
    variant = ProgressVariant.warning;
  }

  return (
    <div style={{ overflow: 'hidden' }}>
      <span style={{ fontSize: '0.85em', whiteSpace: 'nowrap' }}>
        {allocated} / {total} {unit} ({percent}%)
      </span>
      <Progress
        value={percent}
        measureLocation={ProgressMeasureLocation.inside}
        variant={variant}
        style={{ marginTop: 2 }}
        aria-label={`${allocated} of ${total} ${unit} used`}
      />
    </div>
  );
};

export default InlineCapacityBar;
