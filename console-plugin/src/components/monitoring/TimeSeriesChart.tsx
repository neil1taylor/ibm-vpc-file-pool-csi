import React, { useMemo } from 'react';
import {
  Chart,
  ChartArea,
  ChartAxis,
  ChartLine,
  ChartVoronoiContainer,
  ChartThemeColor,
} from '@patternfly/react-charts';
import { Bullseye, Spinner } from '@patternfly/react-core';
import {
  usePrometheusRange,
  TimeRange,
  PrometheusRangeResult,
} from '../../utils/use-pool-metrics';

interface TimeSeriesChartProps {
  query: string;
  range: TimeRange;
  yLabel: string;
  chartType?: 'area' | 'line';
}

function formatTimestamp(epoch: number): string {
  const d = new Date(epoch * 1000);
  return `${d.getHours().toString().padStart(2, '0')}:${d.getMinutes().toString().padStart(2, '0')}`;
}

function buildChartData(series: PrometheusRangeResult[]) {
  return series.map((s) => {
    const label =
      s.metric.__name__ ||
      s.metric.pool ||
      s.metric.policy ||
      s.metric.status ||
      'value';
    return {
      label,
      data: s.values.map(([ts, val]) => ({
        x: new Date(ts * 1000),
        y: parseFloat(val),
        name: label,
      })),
    };
  });
}

const TimeSeriesChart: React.FC<TimeSeriesChartProps> = ({
  query,
  range,
  yLabel,
  chartType = 'area',
}) => {
  const { series, loading, error } = usePrometheusRange(query, range);

  const chartSeries = useMemo(() => buildChartData(series), [series]);

  if (loading) {
    return (
      <Bullseye>
        <Spinner size="lg" />
      </Bullseye>
    );
  }

  if (error || chartSeries.length === 0) {
    return (
      <Bullseye>
        <span style={{ color: 'var(--pf-v6-global--Color--200)' }}>
          {error || 'No data available'}
        </span>
      </Bullseye>
    );
  }

  const ChartComponent = chartType === 'line' ? ChartLine : ChartArea;

  // Compute tick values: 5 evenly spaced timestamps
  const allTimestamps = chartSeries.flatMap((s) =>
    s.data.map((d) => d.x.getTime()),
  );
  const minTs = Math.min(...allTimestamps);
  const maxTs = Math.max(...allTimestamps);
  const tickCount = 5;
  const tickValues: Date[] = [];
  for (let i = 0; i < tickCount; i++) {
    tickValues.push(new Date(minTs + ((maxTs - minTs) * i) / (tickCount - 1)));
  }

  return (
    <div style={{ height: 250 }}>
      <Chart
        height={250}
        padding={{ top: 20, right: 20, bottom: 40, left: 60 }}
        themeColor={ChartThemeColor.multiOrdered}
        containerComponent={
          <ChartVoronoiContainer
            labels={({ datum }: { datum: { name?: string; y?: number } }) =>
              `${datum.name}: ${datum.y?.toFixed(2) ?? '-'}`
            }
          />
        }
      >
        <ChartAxis
          tickValues={tickValues}
          tickFormat={(t: Date) =>
            formatTimestamp(t.getTime() / 1000)
          }
        />
        <ChartAxis dependentAxis label={yLabel} />
        {chartSeries.map((s) => (
          <ChartComponent key={s.label} data={s.data} name={s.label} />
        ))}
      </Chart>
    </div>
  );
};

export default TimeSeriesChart;
