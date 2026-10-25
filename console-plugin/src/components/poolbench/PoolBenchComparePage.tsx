import React from 'react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Link, useLocation } from 'react-router-dom';
import { Chart, ChartAxis, ChartGroup, ChartLine, ChartThemeColor, ChartVoronoiContainer } from '@patternfly/react-charts';
import CardHelpTitle from '../common/CardHelpTitle';
import { columnInfo } from '../../utils/column-help';
import { Bullseye, Card, CardBody, Label, PageSection, Spinner, Title } from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { PoolBenchModel } from '../../models';
import { PoolBench } from '../../types';
import { ROUTES } from '../../constants';
import { compareRows, compareSeries, runLabel } from '../../utils/poolbench-compare';

const summary = (pb: PoolBench): string => {
  const st = pb.status || {};
  const steps = (st.steps || []).filter((s) => s.ended);
  const last = steps.length ? steps[steps.length - 1].vms : 0;
  if (!st.kneeVMs) return `${pb.metadata?.name}: no knee up to ${last} VMs`;
  return `${pb.metadata?.name}: knee at ${st.kneeVMs} VMs, recommended ${st.recommendedVMs || 'none'}`;
};

/** Two or more runs side by side, such as the three tiers of a series. */
const PoolBenchComparePage: React.FC = () => {
  const { search } = useLocation();
  const names = (new URLSearchParams(search).get('runs') || '').split(',').filter(Boolean);
  const [all, loaded] = useK8sWatchResource<PoolBench[]>({
    groupVersionKind: { group: PoolBenchModel.apiGroup, version: PoolBenchModel.apiVersion, kind: PoolBenchModel.kind },
    isList: true,
  });
  if (!loaded) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }
  const runs = names.map((n) => (all || []).find((pb) => pb.metadata?.name === n)).filter((pb): pb is PoolBench => !!pb);
  const rows = compareRows(runs);
  const series = compareSeries(runs).filter((s) => s.points.length > 0);
  const top = Math.max(1, ...series.flatMap((s) => s.points.map((p) => p.y))) * 1.1;

  return (
    <PageSection>
      <Title headingLevel="h1" style={{ marginBottom: 16 }}>
        <Link to={ROUTES.POOLBENCH}>PoolBench</Link> / Compare
      </Title>
      {runs.length < 2 && <p>Choose two or more runs on the PoolBench list to compare them.</p>}
      <ul style={{ marginBottom: 16 }}>
        {runs.map((pb) => (
          <li key={pb.metadata?.name}>{summary(pb)}</li>
        ))}
      </ul>

      {series.length > 0 && (
        <Card style={{ marginBottom: 16 }}>
          <CardHelpTitle helpKey="pbCompareGraph" />
          <CardBody>
            <Chart
              ariaTitle="p95 latency against VMs"
              height={260}
              padding={{ top: 10, right: 20, bottom: 50, left: 60 }}
              domain={{ y: [0, top] }}
              themeColor={ChartThemeColor.multiOrdered}
              containerComponent={
                <ChartVoronoiContainer labels={({ datum }: { datum: { name?: string; x?: number; y?: number } }) => `${datum.name}: ${datum.y} ms at ${datum.x} VMs`} />
              }
            >
              <ChartAxis label="VMs" />
              <ChartAxis dependentAxis label="ms" />
              <ChartGroup>
                {series.map((s) => (
                  <ChartLine key={s.label} name={s.label} data={s.points.map((p) => ({ ...p, name: s.label }))} />
                ))}
              </ChartGroup>
            </Chart>
          </CardBody>
        </Card>
      )}

      <Card>
        <CardHelpTitle helpKey="pbStepResults" />
        <CardBody>
          <Table aria-label="Runs side by side" variant="compact" role="grid">
            <Thead>
              <Tr>
                <Th info={columnInfo('poolbenchStepVMs')}>VMs</Th>
                {runs.map((pb) => (
                  <Th key={pb.metadata?.name}>{runLabel(pb)}</Th>
                ))}
              </Tr>
            </Thead>
            <Tbody>
              {rows.map((r) => (
                <Tr key={r.vms}>
                  <Td dataLabel="VMs">{r.vms}</Td>
                  {r.cells.map((c, i) => (
                    <Td key={runs[i].metadata?.name} dataLabel={runLabel(runs[i])}>
                      {c ? (
                        <>
                          {c.p95Ms.toFixed(1)} ms, {c.slowPercent}% slow{' '}
                          <Label isCompact color={c.pass ? 'green' : 'red'}>
                            {c.pass ? 'Pass' : 'Fail'}
                          </Label>{' '}
                          {c.knee && (
                            <Label isCompact color="orange">
                              Knee
                            </Label>
                          )}
                        </>
                      ) : (
                        '-'
                      )}
                    </Td>
                  ))}
                </Tr>
              ))}
            </Tbody>
          </Table>
        </CardBody>
      </Card>
    </PageSection>
  );
};

export default PoolBenchComparePage;
