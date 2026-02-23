import React from 'react';
import { ChartDonut } from '@patternfly/react-charts';

interface CapacityDonutProps {
  used: number;
  total: number;
  unit: string;
}

const CapacityDonut: React.FC<CapacityDonutProps> = ({ used, total, unit }) => {
  const free = Math.max(0, total - used);
  const pct = total > 0 ? Math.round((used / total) * 100) : 0;

  // Color based on utilization
  let usedColor = 'var(--pf-v6-chart-color-blue-300, #06c)';
  if (pct >= 90) {
    usedColor = 'var(--pf-v6-chart-color-red-100, #c9190b)';
  } else if (pct >= 75) {
    usedColor = 'var(--pf-v6-chart-color-gold-300, #f0ab00)';
  }

  return (
    <div style={{ height: 200, width: 200 }}>
      <ChartDonut
        ariaDesc="Capacity utilization"
        ariaTitle="Capacity donut chart"
        constrainToVisibleArea
        data={[
          { x: 'Used', y: used },
          { x: 'Free', y: free },
        ]}
        labels={({ datum }) => `${datum.x}: ${datum.y} ${unit}`}
        subTitle={unit}
        title={`${pct}%`}
        colorScale={[usedColor, 'var(--pf-v6-chart-color-black-200, #d2d2d2)']}
        innerRadius={65}
        padding={{ top: 0, bottom: 0, left: 0, right: 0 }}
      />
    </div>
  );
};

export default CapacityDonut;
