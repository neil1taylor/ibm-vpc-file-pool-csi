import React, { useState, useMemo } from 'react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import {
  PageSection,
  Grid,
  GridItem,
  Card,
  CardTitle,
  CardBody,
  Split,
  SplitItem,
  ToggleGroup,
  ToggleGroupItem,
  Label,
  DescriptionList,
  DescriptionListGroup,
  DescriptionListTerm,
  DescriptionListDescription,
} from '@patternfly/react-core';
import { FileSharePoolModel } from '../../models';
import { FileSharePool } from '../../types';
import { usePoolMetrics } from '../../utils/use-pool-metrics';
import { TIME_RANGES, TimeRange } from '../../utils/use-pool-metrics';
import { METRICS } from '../../constants';
import TimeSeriesChart from './TimeSeriesChart';

/** Format seconds to human-readable ms string. */
const formatLatencyMs = (seconds: number): string => {
  if (seconds === 0) return '-';
  const ms = seconds * 1000;
  if (ms < 1) return '<1 ms';
  return `${ms.toFixed(0)} ms`;
};

const MonitoringPage: React.FC = () => {
  const [range, setRange] = useState<TimeRange>(TIME_RANGES[0]);

  const [pools] = useK8sWatchResource<FileSharePool[]>({
    groupVersionKind: {
      group: FileSharePoolModel.apiGroup,
      version: FileSharePoolModel.apiVersion,
      kind: FileSharePoolModel.kind,
    },
    isList: true,
  });

  const poolNames = useMemo(
    () => (pools || []).map((p) => p.metadata?.name || '').filter(Boolean),
    [pools],
  );

  const { metrics: poolMetrics, aggregate } = usePoolMetrics(poolNames);

  // Compute stat card values
  const allocationRate = Object.values(poolMetrics).reduce(
    (sum, m) => sum + m.allocationRate1h,
    0,
  );
  const maxLatency = Math.max(
    ...Object.values(poolMetrics).map((m) => m.p95LatencySeconds),
    0,
  );
  const apiErrorPct =
    aggregate.apiCallRate5m > 0
      ? Math.round(
          (aggregate.apiErrorRate5m / aggregate.apiCallRate5m) * 100,
        )
      : 0;
  const failedPolicies = Object.values(
    aggregate.replicationConsecutiveFailures,
  ).filter((c) => c > 0).length;

  return (
    <PageSection>
      {/* Time range selector */}
      <Split hasGutter style={{ marginBottom: 16 }}>
        <SplitItem isFilled />
        <SplitItem>
          <ToggleGroup aria-label="Time range selector">
            {TIME_RANGES.map((tr) => (
              <ToggleGroupItem
                key={tr.label}
                text={tr.label}
                isSelected={range.label === tr.label}
                onChange={() => setRange(tr)}
              />
            ))}
          </ToggleGroup>
        </SplitItem>
      </Split>

      {/* Stat cards */}
      <Grid hasGutter style={{ marginBottom: 16 }}>
        <GridItem sm={12} md={6} lg={3}>
          <Card isFullHeight>
            <CardTitle>Allocation Rate</CardTitle>
            <CardBody>
              <span style={{ fontSize: '1.5rem', fontWeight: 'bold' }}>
                {allocationRate.toFixed(1)}
              </span>
              <span
                style={{
                  marginLeft: 4,
                  color: 'var(--pf-v6-global--Color--200)',
                }}
              >
                /hr
              </span>
            </CardBody>
          </Card>
        </GridItem>

        <GridItem sm={12} md={6} lg={3}>
          <Card isFullHeight>
            <CardTitle>P95 Latency</CardTitle>
            <CardBody>
              <Split hasGutter>
                <SplitItem>
                  <span style={{ fontSize: '1.5rem', fontWeight: 'bold' }}>
                    {formatLatencyMs(maxLatency)}
                  </span>
                </SplitItem>
                <SplitItem>
                  <Label
                    color={
                      maxLatency === 0
                        ? 'grey'
                        : maxLatency * 1000 < 100
                          ? 'green'
                          : maxLatency * 1000 < 500
                            ? 'orange'
                            : 'red'
                    }
                  >
                    {maxLatency === 0
                      ? '-'
                      : maxLatency * 1000 < 100
                        ? 'Good'
                        : maxLatency * 1000 < 500
                          ? 'Elevated'
                          : 'High'}
                  </Label>
                </SplitItem>
              </Split>
            </CardBody>
          </Card>
        </GridItem>

        <GridItem sm={12} md={6} lg={3}>
          <Card isFullHeight>
            <CardTitle>VPC API Health</CardTitle>
            <CardBody>
              <DescriptionList isCompact>
                <DescriptionListGroup>
                  <DescriptionListTerm>Calls/min</DescriptionListTerm>
                  <DescriptionListDescription>
                    {aggregate.apiCallRate5m.toFixed(1)}
                  </DescriptionListDescription>
                </DescriptionListGroup>
                <DescriptionListGroup>
                  <DescriptionListTerm>Error rate</DescriptionListTerm>
                  <DescriptionListDescription>
                    <Label
                      color={
                        apiErrorPct > 5
                          ? 'red'
                          : apiErrorPct > 0
                            ? 'orange'
                            : 'green'
                      }
                    >
                      {apiErrorPct}%
                    </Label>
                  </DescriptionListDescription>
                </DescriptionListGroup>
              </DescriptionList>
            </CardBody>
          </Card>
        </GridItem>

        <GridItem sm={12} md={6} lg={3}>
          <Card isFullHeight>
            <CardTitle>Replication Status</CardTitle>
            <CardBody>
              <Split hasGutter>
                <SplitItem>
                  <Label color={failedPolicies > 0 ? 'red' : 'green'}>
                    {failedPolicies > 0
                      ? `${failedPolicies} failing`
                      : 'Healthy'}
                  </Label>
                </SplitItem>
                <SplitItem>
                  <span
                    style={{ color: 'var(--pf-v6-global--Color--200)' }}
                  >
                    {aggregate.replicationSyncRate5m.toFixed(1)} syncs/min
                  </span>
                </SplitItem>
              </Split>
            </CardBody>
          </Card>
        </GridItem>
      </Grid>

      {/* Time-series charts */}
      <Grid hasGutter>
        <GridItem sm={12} md={6}>
          <Card>
            <CardTitle>Allocation Rate Over Time</CardTitle>
            <CardBody>
              <TimeSeriesChart
                query={`sum(rate(${METRICS.ALLOCATIONS_TOTAL}[5m])) * 3600`}
                range={range}
                yLabel="alloc/hr"
                chartType="area"
              />
            </CardBody>
          </Card>
        </GridItem>

        <GridItem sm={12} md={6}>
          <Card>
            <CardTitle>P95 Latency Over Time</CardTitle>
            <CardBody>
              <TimeSeriesChart
                query={`histogram_quantile(0.95, sum(rate(${METRICS.ALLOCATION_DURATION}[5m])) by (le)) * 1000`}
                range={range}
                yLabel="ms"
                chartType="line"
              />
            </CardBody>
          </Card>
        </GridItem>

        <GridItem sm={12} md={6}>
          <Card>
            <CardTitle>VPC API Call Rate</CardTitle>
            <CardBody>
              <TimeSeriesChart
                query={`sum by (status) (rate(${METRICS.VPC_API_CALLS_TOTAL}[5m])) * 60`}
                range={range}
                yLabel="calls/min"
                chartType="area"
              />
            </CardBody>
          </Card>
        </GridItem>

        <GridItem sm={12} md={6}>
          <Card>
            <CardTitle>Replication Lag</CardTitle>
            <CardBody>
              <TimeSeriesChart
                query={METRICS.REPLICATION_LAG}
                range={range}
                yLabel="seconds"
                chartType="line"
              />
            </CardBody>
          </Card>
        </GridItem>
      </Grid>
    </PageSection>
  );
};

export default MonitoringPage;
