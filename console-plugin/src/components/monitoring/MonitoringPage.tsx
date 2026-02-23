import React, { useState, useEffect, useMemo } from 'react';
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
  Alert,
  DescriptionList,
  DescriptionListGroup,
  DescriptionListTerm,
  DescriptionListDescription,
  ExpandableSection,
  Bullseye,
  Divider,
} from '@patternfly/react-core';
import { FileSharePoolModel, ReplicationPolicyModel } from '../../models';
import { FileSharePool, ReplicationPolicy } from '../../types';
import { usePoolMetrics, checkPrometheusConnection } from '../../utils/use-pool-metrics';
import { TIME_RANGES, TimeRange } from '../../utils/use-pool-metrics';
import { METRICS } from '../../constants';
import TimeSeriesChart from './TimeSeriesChart';
import CapacityDonut from '../dashboard/CapacityDonut';
import CapacityBar from '../common/CapacityBar';

const MonitoringPage: React.FC = () => {
  const [range, setRange] = useState<TimeRange>(TIME_RANGES[0]);
  const [connStatus, setConnStatus] = useState<{
    connected: boolean;
    hasMetrics: boolean;
    error?: string;
  } | null>(null);
  const [allocationExpanded, setAllocationExpanded] = useState(false);

  useEffect(() => {
    checkPrometheusConnection().then(setConnStatus);
  }, []);

  const [pools] = useK8sWatchResource<FileSharePool[]>({
    groupVersionKind: {
      group: FileSharePoolModel.apiGroup,
      version: FileSharePoolModel.apiVersion,
      kind: FileSharePoolModel.kind,
    },
    isList: true,
  });

  const [replicationPolicies] = useK8sWatchResource<ReplicationPolicy[]>({
    groupVersionKind: {
      group: ReplicationPolicyModel.apiGroup,
      version: ReplicationPolicyModel.apiVersion,
      kind: ReplicationPolicyModel.kind,
    },
    isList: true,
  });

  const poolNames = useMemo(
    () => (pools || []).map((p) => p.metadata?.name || '').filter(Boolean),
    [pools],
  );

  const { metrics: poolMetrics, aggregate } = usePoolMetrics(poolNames);

  // Computed aggregate values
  const totalCapacity = Object.values(poolMetrics).reduce((s, m) => s + m.capacityGB, 0);
  const totalAllocated = Object.values(poolMetrics).reduce((s, m) => s + m.allocatedGB, 0);
  const totalPVCs = Object.values(poolMetrics).reduce((s, m) => s + m.pvcCount, 0);
  const totalShares = Object.values(poolMetrics).reduce((s, m) => s + m.shareCount, 0);
  const utilizationPct = totalCapacity > 0 ? Math.round((totalAllocated / totalCapacity) * 100) : 0;
  const freeGB = Math.max(0, totalCapacity - totalAllocated);

  const apiErrorPct =
    aggregate.apiCallRate5m > 0
      ? Math.round(
          (aggregate.apiErrorRate5m / aggregate.apiCallRate5m) * 100,
        )
      : 0;
  const failedPolicies = Object.values(
    aggregate.replicationConsecutiveFailures,
  ).filter((c) => c > 0).length;

  const hasReplication = (replicationPolicies || []).length > 0;

  return (
    <PageSection>
      {/* A. Time range selector */}
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

      {/* Diagnostic banner */}
      {connStatus && !connStatus.hasMetrics && (
        <Alert
          variant="warning"
          title="Monitoring data unavailable"
          isInline
          style={{ marginBottom: 16 }}
        >
          {!connStatus.connected ? (
            <>
              Cannot reach Prometheus. Ensure{' '}
              <a
                href="/monitoring/targets"
                target="_blank"
                rel="noopener noreferrer"
              >
                user workload monitoring
              </a>{' '}
              is enabled on this cluster.
              {connStatus.error && (
                <div style={{ marginTop: 4, fontSize: '0.85em', opacity: 0.8 }}>
                  Error: {connStatus.error}
                </div>
              )}
            </>
          ) : (
            <>
              No CSI driver metrics found in Prometheus. Enable the ServiceMonitor:
              <code style={{ display: 'block', marginTop: 8 }}>
                helm upgrade ibm-vpc-file-pool-csi ... --set metrics.serviceMonitor.enabled=true
              </code>
            </>
          )}
        </Alert>
      )}

      {/* B. Pool Capacity Overview */}
      <Card style={{ marginBottom: 16 }}>
        <CardTitle>Pool Capacity Overview</CardTitle>
        <CardBody>
          <Grid hasGutter>
            <GridItem sm={12} md={4} lg={3}>
              <Bullseye>
                <CapacityDonut used={totalAllocated} total={totalCapacity} unit="GB" />
              </Bullseye>
            </GridItem>
            <GridItem sm={12} md={8} lg={9}>
              {poolNames.length === 0 ? (
                <Bullseye>
                  <span style={{ color: 'var(--pf-v6-global--Color--200)' }}>
                    No pools found
                  </span>
                </Bullseye>
              ) : (
                poolNames.map((name) => {
                  const m = poolMetrics[name];
                  if (!m) return null;
                  return (
                    <div key={name} style={{ marginBottom: 12 }}>
                      <CapacityBar
                        allocated={m.allocatedGB}
                        total={m.capacityGB}
                        title={`${name}: ${m.allocatedGB} / ${m.capacityGB} GB`}
                      />
                      <span
                        style={{
                          fontSize: '0.85em',
                          color: 'var(--pf-v6-global--Color--200)',
                          marginLeft: 4,
                        }}
                      >
                        {m.pvcCount} PVCs &middot; {m.shareCount} shares
                      </span>
                    </div>
                  );
                })
              )}
            </GridItem>
          </Grid>
        </CardBody>
      </Card>

      {/* C. Stat cards */}
      <Grid hasGutter style={{ marginBottom: 16 }}>
        <GridItem sm={12} md={6} lg={3}>
          <Card isFullHeight>
            <CardTitle>Total PVCs</CardTitle>
            <CardBody>
              <span style={{ fontSize: '1.5rem', fontWeight: 'bold' }}>
                {totalPVCs}
              </span>
              <div
                style={{
                  color: 'var(--pf-v6-global--Color--200)',
                  fontSize: '0.85em',
                }}
              >
                across {totalShares} shares
              </div>
            </CardBody>
          </Card>
        </GridItem>

        <GridItem sm={12} md={6} lg={3}>
          <Card isFullHeight>
            <CardTitle>Utilization</CardTitle>
            <CardBody>
              <Split hasGutter>
                <SplitItem>
                  <span style={{ fontSize: '1.5rem', fontWeight: 'bold' }}>
                    {utilizationPct}%
                  </span>
                </SplitItem>
                <SplitItem>
                  <Label
                    color={
                      utilizationPct >= 90
                        ? 'red'
                        : utilizationPct >= 75
                          ? 'orange'
                          : 'green'
                    }
                  >
                    {utilizationPct >= 90
                      ? 'Critical'
                      : utilizationPct >= 75
                        ? 'Warning'
                        : 'Healthy'}
                  </Label>
                </SplitItem>
              </Split>
              <div
                style={{
                  color: 'var(--pf-v6-global--Color--200)',
                  fontSize: '0.85em',
                }}
              >
                {freeGB} GB free
              </div>
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

      {/* D. Time-series charts (always-populated) */}
      <Grid hasGutter style={{ marginBottom: 16 }}>
        <GridItem sm={12} md={6}>
          <Card>
            <CardTitle>Capacity Utilization Over Time</CardTitle>
            <CardBody>
              <TimeSeriesChart
                query={`(${METRICS.POOL_ALLOCATED_GB} / (${METRICS.POOL_CAPACITY_GB} > 0)) * 100`}
                range={range}
                yLabel="%"
                chartType="area"
              />
            </CardBody>
          </Card>
        </GridItem>

        <GridItem sm={12} md={6}>
          <Card>
            <CardTitle>PVC Count Over Time</CardTitle>
            <CardBody>
              <TimeSeriesChart
                query={`sum(${METRICS.POOL_PVC_COUNT}) by (pool)`}
                range={range}
                yLabel="PVCs"
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
            <CardTitle>VPC API P95 Latency</CardTitle>
            <CardBody>
              <TimeSeriesChart
                query={`histogram_quantile(0.95, sum(rate(${METRICS.VPC_API_CALL_DURATION}[5m])) by (le)) * 1000`}
                range={range}
                yLabel="ms"
                chartType="line"
              />
            </CardBody>
          </Card>
        </GridItem>
      </Grid>

      {/* E. Allocation Activity (expandable) */}
      <div style={{ marginBottom: 16 }}>
        <ExpandableSection
          toggleText={allocationExpanded ? 'Hide Allocation Activity' : 'Show Allocation Activity'}
          onToggle={(_event, isExpanded) => setAllocationExpanded(isExpanded)}
          isExpanded={allocationExpanded}
        >
          <Grid hasGutter style={{ marginTop: 8 }}>
            <GridItem sm={12} md={6}>
              <Card>
                <CardTitle>Allocation Rate Over Time</CardTitle>
                <CardBody>
                  <TimeSeriesChart
                    query={`sum(rate(${METRICS.ALLOCATIONS_TOTAL}[5m])) * 3600`}
                    range={range}
                    yLabel="alloc/hr"
                    chartType="area"
                    emptyMessage="No allocation events in this time range"
                  />
                </CardBody>
              </Card>
            </GridItem>

            <GridItem sm={12} md={6}>
              <Card>
                <CardTitle>P95 Allocation Latency Over Time</CardTitle>
                <CardBody>
                  <TimeSeriesChart
                    query={`histogram_quantile(0.95, sum(rate(${METRICS.ALLOCATION_DURATION}[5m])) by (le)) * 1000`}
                    range={range}
                    yLabel="ms"
                    chartType="line"
                    emptyMessage="No allocation latency data in this time range"
                  />
                </CardBody>
              </Card>
            </GridItem>
          </Grid>
        </ExpandableSection>
      </div>

      {/* F. Replication (conditional) */}
      {hasReplication && (
        <>
          <Divider style={{ marginBottom: 16 }} />
          <Grid hasGutter>
            <GridItem sm={12} md={6}>
              <Card>
                <CardTitle>Replication Lag</CardTitle>
                <CardBody>
                  <TimeSeriesChart
                    query={METRICS.REPLICATION_LAG}
                    range={range}
                    yLabel="seconds"
                    chartType="line"
                    emptyMessage="No replication lag data available"
                  />
                </CardBody>
              </Card>
            </GridItem>

            <GridItem sm={12} md={6}>
              <Card>
                <CardTitle>Replication Sync Rate</CardTitle>
                <CardBody>
                  <TimeSeriesChart
                    query={`sum by (result) (rate(${METRICS.REPLICATION_SYNC_TOTAL}[5m])) * 60`}
                    range={range}
                    yLabel="syncs/min"
                    chartType="area"
                    emptyMessage="No replication sync data available"
                  />
                </CardBody>
              </Card>
            </GridItem>
          </Grid>
        </>
      )}
    </PageSection>
  );
};

export default MonitoringPage;
