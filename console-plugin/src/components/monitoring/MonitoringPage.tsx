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
import { metricQueries } from '../../utils/metric-queries';
import TimeSeriesChart from './TimeSeriesChart';
import CardHelpTitle from '../common/CardHelpTitle';
import CapacityDonut from '../dashboard/CapacityDonut';
import CapacityBar from '../common/CapacityBar';
import HelpLink from '../common/HelpLink';
import { TROUBLESHOOTING } from '../../utils/help-links';

const MonitoringPage: React.FC = () => {
  const [range, setRange] = useState<TimeRange>(TIME_RANGES[0]);
  const [connStatus, setConnStatus] = useState<{
    connected: boolean;
    hasMetrics: boolean;
    error?: string;
  } | null>(null);
  const [allocationExpanded, setAllocationExpanded] = useState(false);
  const [detailsExpanded, setDetailsExpanded] = useState(false);

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
      <p style={{ color: 'var(--pf-t--global--color--subtle)', margin: '0 0 8px 0' }}>
        Prometheus metrics for pool capacity, allocation performance, VPC API health, and replication status.
      </p>
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
          <div style={{ marginTop: 8 }}>
            <HelpLink href={TROUBLESHOOTING.metrics} label="Troubleshooting" testId="banner-help" />
          </div>
        </Alert>
      )}

      {/* B. Pool Capacity Overview */}
      <Card style={{ marginBottom: 16 }}>
        <CardHelpTitle helpKey="poolCapacityOverview" />
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
            <CardHelpTitle helpKey="totalPVCs" />
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
            <CardHelpTitle helpKey="utilization" />
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
            <CardHelpTitle helpKey="vpcApiHealth" />
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
            <CardHelpTitle helpKey="replicationStatus" />
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
            <CardHelpTitle helpKey="capacityOverTime" />
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
            <CardHelpTitle helpKey="pvcCountOverTime" />
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
            <CardHelpTitle helpKey="vpcApiCallRate" />
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
            <CardHelpTitle helpKey="vpcApiP95Latency" />
            <CardBody>
              <TimeSeriesChart
                query={metricQueries.apiP95Ms}
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
                <CardHelpTitle helpKey="allocationRateOverTime" />
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
                <CardHelpTitle helpKey="allocationLatencyOverTime" />
                <CardBody>
                  <TimeSeriesChart
                    query={metricQueries.allocationP95Ms}
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

      {/* E2. Group snapshots and CDI (expandable) */}
      <div style={{ marginBottom: 16 }}>
        <ExpandableSection
          toggleText={detailsExpanded ? 'Hide Group Snapshot and CDI Activity' : 'Show Group Snapshot and CDI Activity'}
          onToggle={(_event, isExpanded) => setDetailsExpanded(isExpanded)}
          isExpanded={detailsExpanded}
        >
          <Grid hasGutter style={{ marginTop: 8 }}>
            <GridItem sm={12} md={6}>
              <Card>
                <CardHelpTitle helpKey="groupSnapshotWindow" />
                <CardBody>
                  <TimeSeriesChart
                    query={metricQueries.groupSnapshotWindowP95Ms}
                    range={range}
                    yLabel="ms"
                    chartType="line"
                    emptyMessage="No group snapshots in this time range"
                  />
                </CardBody>
              </Card>
            </GridItem>

            <GridItem sm={12} md={6}>
              <Card>
                <CardHelpTitle helpKey="groupSnapshotMembers" />
                <CardBody>
                  <TimeSeriesChart
                    query={metricQueries.groupSnapshotMembersAvg}
                    range={range}
                    yLabel="members"
                    chartType="line"
                    emptyMessage="No group snapshots in this time range"
                  />
                </CardBody>
              </Card>
            </GridItem>

            <GridItem sm={12} md={6}>
              <Card>
                <CardHelpTitle helpKey="cdiLimitRaises" />
                <CardBody>
                  <p style={{ color: 'var(--pf-t--global--color--subtle)', margin: '0 0 8px 0' }}>
                    The driver raises the memory limit of CDI&apos;s copy pod when it starts. A raise later on means
                    something reset the limit, for example an add-on update.
                  </p>
                  <TimeSeriesChart
                    query={metricQueries.cdiLimitRaises1h}
                    range={range}
                    yLabel="raises"
                    chartType="line"
                    emptyMessage="No raises in this time range"
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
                <CardHelpTitle helpKey="replicationLag" />
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
                <CardHelpTitle helpKey="replicationSyncRateOverTime" />
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
