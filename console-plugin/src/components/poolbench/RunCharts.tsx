import React, { useMemo } from 'react';
import {
  Chart,
  ChartAxis,
  ChartGroup,
  ChartLine,
  ChartThemeColor,
  ChartVoronoiContainer,
} from '@patternfly/react-charts';
import { Alert, Bullseye, Card, CardBody, CardTitle, Gallery, Spinner } from '@patternfly/react-core';
import { PoolBench } from '../../types';
import { ChartSpec, chartWindow, runCharts } from '../../utils/poolbench-queries';
import { SeriesData, useRunMetrics } from '../../utils/use-run-metrics';
import { isActive } from '../../utils/poolbench';

const HEIGHT = 220;

const clock = (x: Date): string =>
  `${x.getHours().toString().padStart(2, '0')}:${x.getMinutes().toString().padStart(2, '0')}`;

interface Mark {
  label: string;
  x: number;
}

/** One graph: its series, flat lines (limits) and the step marks, over the run's window. */
const RunChart: React.FC<{ spec: ChartSpec; data: SeriesData[]; marks: Mark[]; start: number; end: number }> = ({
  spec,
  data,
  marks,
  start,
  end,
}) => {
  const series = data.filter((s) => s.points.length > 0);
  if (series.length === 0) {
    return <Bullseye style={{ height: HEIGHT, color: 'var(--pf-t--global--color--subtle)' }}>No data yet</Bullseye>;
  }
  const top = Math.max(1, ...series.flatMap((s) => s.points.map((p) => p.y)), ...spec.lines.map((l) => l.y)) * 1.1;
  const d = (x: number) => new Date(x * 1000);
  return (
    <Chart
      ariaTitle={spec.title}
      height={HEIGHT}
      padding={{ top: 10, right: 20, bottom: 40, left: 60 }}
      domain={{ x: [d(start), d(end)], y: [0, top] }}
      themeColor={ChartThemeColor.multiOrdered}
      containerComponent={
        <ChartVoronoiContainer
          labels={({ datum }: { datum: { name?: string; y?: number } }) => `${datum.name}: ${datum.y?.toFixed(1) ?? '-'}`}
          constrainToVisibleArea
        />
      }
    >
      <ChartAxis tickFormat={(t: Date) => clock(t)} tickCount={5} />
      <ChartAxis dependentAxis label={spec.yLabel} />
      <ChartGroup>
        {series.map((s) => (
          <ChartLine key={s.label} name={s.label} data={s.points.map((p) => ({ x: d(p.x), y: p.y, name: s.label }))} />
        ))}
        {spec.lines.map((l) => (
          <ChartLine
            key={l.label}
            name={l.label}
            data={[{ x: d(start), y: l.y, name: l.label }, { x: d(end), y: l.y, name: l.label }]}
            style={{ data: { strokeDasharray: '6,4', stroke: 'var(--pf-t--global--color--status--danger--default)' } }}
          />
        ))}
        {marks.map((m) => (
          <ChartLine
            key={m.label}
            name={m.label}
            data={[{ x: d(m.x), y: 0, name: m.label }, { x: d(m.x), y: top, name: m.label }]}
            style={{ data: { strokeDasharray: '2,4', stroke: 'var(--pf-t--global--color--subtle)' } }}
          />
        ))}
      </ChartGroup>
    </Chart>
  );
};

/** The five live graphs of a run, with a mark at the start of each step. */
const RunCharts: React.FC<{ pb: PoolBench }> = ({ pb }) => {
  const live = isActive(pb);
  // While the run is live, the window's end moves with each read; it is fixed after the end.
  const win = useMemo(() => chartWindow(pb, new Date()), [pb.status?.startTime, pb.status?.endTime]); // eslint-disable-line react-hooks/exhaustive-deps
  const specs = useMemo(() => runCharts(pb), [pb.metadata?.name, pb.spec]); // eslint-disable-line react-hooks/exhaustive-deps
  const { charts, loading, error } = useRunMetrics(specs, win, live);
  const marks: Mark[] = (pb.status?.steps || [])
    .filter((s) => s.started)
    .map((s, i) => ({ label: `Step ${i + 1} (${s.vms} VMs)`, x: Date.parse(s.started as string) / 1000 }));

  if (!win) return <p>The graphs start when the run starts.</p>;
  if (error) {
    return (
      <Alert variant="warning" isInline title={`The graphs need Prometheus: ${error}`}>
        The step results below do not need it. Check that the PoolBench metrics are scraped (docs/MONITORING.md).
      </Alert>
    );
  }
  const end = live ? Date.now() / 1000 : win.end;
  return (
    <Gallery hasGutter minWidths={{ default: '100%', lg: '45%' }}>
      {specs.map((spec) => (
        <Card key={spec.title} isCompact>
          <CardTitle>{spec.title}</CardTitle>
          <CardBody>
            {loading ? (
              <Bullseye style={{ height: HEIGHT }}>
                <Spinner size="lg" />
              </Bullseye>
            ) : (
              <RunChart spec={spec} data={charts[spec.title] || []} marks={marks} start={win.start} end={end} />
            )}
          </CardBody>
        </Card>
      ))}
    </Gallery>
  );
};

export default RunCharts;
