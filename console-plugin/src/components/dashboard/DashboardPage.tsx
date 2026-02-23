import React, { useMemo } from 'react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Link } from 'react-router-dom';
import {
  PageSection,
  Grid,
  GridItem,
  Card,
  CardTitle,
  CardBody,
  Spinner,
  Alert,
  Bullseye,
  Stack,
  StackItem,
  Split,
  SplitItem,
  Label,
  DescriptionList,
  DescriptionListGroup,
  DescriptionListTerm,
  DescriptionListDescription,
  Divider,
} from '@patternfly/react-core';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import { FileSharePoolModel, SubVolumeModel, ReplicationPolicyModel } from '../../models';
import { FileSharePool, SubVolume, ReplicationPolicy } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import CapacityBar from '../common/CapacityBar';
import CapacityDonut from './CapacityDonut';
import { utilizationPercent, formatAge } from '../../utils/resource-helpers';
import { usePoolMetrics } from '../../utils/use-pool-metrics';
import { ROUTES } from '../../constants';

/** Format seconds to human-readable ms string. */
const formatLatencyMs = (seconds: number): string => {
  if (seconds === 0) return '-';
  const ms = seconds * 1000;
  if (ms < 1) return '<1 ms';
  return `${ms.toFixed(0)} ms`;
};

/** Color code for latency values. */
const latencyColor = (seconds: number): 'green' | 'gold' | 'red' | 'grey' => {
  if (seconds === 0) return 'grey';
  const ms = seconds * 1000;
  if (ms < 100) return 'green';
  if (ms < 500) return 'gold';
  return 'red';
};

const DashboardPage: React.FC = () => {
  const [pools, poolsLoaded, poolsError] = useK8sWatchResource<FileSharePool[]>({
    groupVersionKind: {
      group: FileSharePoolModel.apiGroup,
      version: FileSharePoolModel.apiVersion,
      kind: FileSharePoolModel.kind,
    },
    isList: true,
  });

  const [subVolumes, svLoaded, svError] = useK8sWatchResource<SubVolume[]>({
    groupVersionKind: {
      group: SubVolumeModel.apiGroup,
      version: SubVolumeModel.apiVersion,
      kind: SubVolumeModel.kind,
    },
    isList: true,
  });

  const [replicationPolicies, rpLoaded] = useK8sWatchResource<ReplicationPolicy[]>({
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

  const loaded = poolsLoaded && svLoaded;
  const error = poolsError || svError;

  if (!loaded) {
    return (
      <PageSection>
        <Bullseye>
          <Spinner size="xl" />
        </Bullseye>
      </PageSection>
    );
  }

  if (error) {
    return (
      <PageSection>
        <Alert variant="danger" title="Error loading resources" isInline>
          {String(error)}
        </Alert>
      </PageSection>
    );
  }

  // Aggregate stats
  const totalPools = pools.length;
  const totalCapacityGB = pools.reduce(
    (sum, p) => sum + (p.status?.totalCapacityGB ?? 0),
    0,
  );
  const totalAllocatedGB = pools.reduce(
    (sum, p) => sum + (p.status?.totalAllocatedGB ?? 0),
    0,
  );
  const totalSubVolumes = subVolumes.length;
  const alertPools = pools.filter((p) => p.status?.phase !== 'Ready');
  const activeAlerts = alertPools.length;

  // Recent SubVolumes
  const recentSubVolumes = [...subVolumes]
    .sort((a, b) => {
      const tsA = a.metadata?.creationTimestamp ?? '';
      const tsB = b.metadata?.creationTimestamp ?? '';
      return tsB.localeCompare(tsA);
    })
    .slice(0, 10);

  const poolCountColor: 'green' | 'orange' | 'red' | 'blue' | 'grey' =
    totalPools === 0 ? 'grey' : activeAlerts > 0 ? 'orange' : 'green';

  // Replication data
  const hasReplication = rpLoaded && (replicationPolicies || []).length > 0;

  // API health
  const apiErrorPct =
    aggregate.apiCallRate5m > 0
      ? Math.round((aggregate.apiErrorRate5m / aggregate.apiCallRate5m) * 100)
      : 0;

  return (
    <PageSection>
      <Stack hasGutter>
        {/* Row 1: Stat Cards */}
        <StackItem>
          <Grid hasGutter>
            <GridItem sm={12} md={6} lg={3}>
              <Card isFullHeight>
                <CardTitle>Total Pools</CardTitle>
                <CardBody>
                  <Split hasGutter>
                    <SplitItem>
                      <span style={{ fontSize: '2rem', fontWeight: 'bold' }}>
                        {totalPools}
                      </span>
                    </SplitItem>
                    <SplitItem>
                      <Label color={poolCountColor}>
                        {totalPools === 0
                          ? 'No pools'
                          : activeAlerts > 0
                            ? `${activeAlerts} unhealthy`
                            : 'All healthy'}
                      </Label>
                    </SplitItem>
                  </Split>
                </CardBody>
              </Card>
            </GridItem>

            <GridItem sm={12} md={6} lg={3}>
              <Card isFullHeight>
                <CardTitle>Total Capacity</CardTitle>
                <CardBody>
                  <DescriptionList isCompact>
                    <DescriptionListGroup>
                      <DescriptionListTerm>Capacity</DescriptionListTerm>
                      <DescriptionListDescription>
                        {totalAllocatedGB} / {totalCapacityGB} GB
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                    <DescriptionListGroup>
                      <DescriptionListTerm>Utilization</DescriptionListTerm>
                      <DescriptionListDescription>
                        {utilizationPercent(totalAllocatedGB, totalCapacityGB)}%
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                  </DescriptionList>
                </CardBody>
              </Card>
            </GridItem>

            <GridItem sm={12} md={6} lg={3}>
              <Card isFullHeight>
                <CardTitle>Total SubVolumes</CardTitle>
                <CardBody>
                  <span style={{ fontSize: '2rem', fontWeight: 'bold' }}>
                    {totalSubVolumes}
                  </span>
                </CardBody>
              </Card>
            </GridItem>

            <GridItem sm={12} md={6} lg={3}>
              <Card isFullHeight>
                <CardTitle>Active Alerts</CardTitle>
                <CardBody>
                  <Split hasGutter>
                    <SplitItem>
                      <span style={{ fontSize: '2rem', fontWeight: 'bold' }}>
                        {activeAlerts}
                      </span>
                    </SplitItem>
                    <SplitItem>
                      {activeAlerts > 0 ? (
                        <Label color="orange">
                          {activeAlerts} pool{activeAlerts !== 1 ? 's' : ''} not
                          Ready
                        </Label>
                      ) : (
                        <Label color="green">All pools Ready</Label>
                      )}
                    </SplitItem>
                  </Split>
                </CardBody>
              </Card>
            </GridItem>
          </Grid>
        </StackItem>

        {/* Row 2: Performance Metrics */}
        <StackItem>
          <Grid hasGutter>
            <GridItem sm={12} md={4}>
              <Card isFullHeight>
                <CardTitle>Allocation Rate</CardTitle>
                <CardBody>
                  <span style={{ fontSize: '1.5rem', fontWeight: 'bold' }}>
                    {Object.values(poolMetrics).reduce((sum, m) => sum + m.allocationRate1h, 0).toFixed(1)}
                  </span>
                  <span style={{ marginLeft: 4, color: 'var(--pf-v6-global--Color--200)' }}>/hr</span>
                </CardBody>
              </Card>
            </GridItem>

            <GridItem sm={12} md={4}>
              <Card isFullHeight>
                <CardTitle>P95 Allocation Latency</CardTitle>
                <CardBody>
                  {(() => {
                    const maxLatency = Math.max(
                      ...Object.values(poolMetrics).map((m) => m.p95LatencySeconds),
                      0,
                    );
                    const color = latencyColor(maxLatency);
                    return (
                      <Split hasGutter>
                        <SplitItem>
                          <span style={{ fontSize: '1.5rem', fontWeight: 'bold' }}>
                            {formatLatencyMs(maxLatency)}
                          </span>
                        </SplitItem>
                        <SplitItem>
                          <Label color={color === 'gold' ? 'orange' : color}>
                            {color === 'green' ? 'Good' : color === 'gold' ? 'Elevated' : color === 'red' ? 'High' : '-'}
                          </Label>
                        </SplitItem>
                      </Split>
                    );
                  })()}
                </CardBody>
              </Card>
            </GridItem>

            <GridItem sm={12} md={4}>
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
                        <Label color={apiErrorPct > 5 ? 'red' : apiErrorPct > 0 ? 'orange' : 'green'}>
                          {apiErrorPct}%
                        </Label>
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                    <DescriptionListGroup>
                      <DescriptionListTerm>P95 latency</DescriptionListTerm>
                      <DescriptionListDescription>
                        {formatLatencyMs(aggregate.apiP95LatencySeconds)}
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                  </DescriptionList>
                </CardBody>
              </Card>
            </GridItem>
          </Grid>
        </StackItem>

        {/* Row 3: Capacity Breakdown */}
        <StackItem>
          <Grid hasGutter>
            <GridItem sm={12} md={4}>
              <Card isFullHeight>
                <CardTitle>Aggregate Capacity</CardTitle>
                <CardBody>
                  <Bullseye>
                    <CapacityDonut used={totalAllocatedGB} total={totalCapacityGB} unit="GB" />
                  </Bullseye>
                </CardBody>
              </Card>
            </GridItem>
            <GridItem sm={12} md={8}>
              <Card isFullHeight>
                <CardTitle>Pool Capacity</CardTitle>
                <CardBody>
                  {pools.length === 0 ? (
                    <Bullseye>No pools configured.</Bullseye>
                  ) : (
                    <Stack hasGutter>
                      {pools.map((pool) => {
                        const name = pool.metadata?.name || 'unknown';
                        const allocated = pool.status?.totalAllocatedGB ?? 0;
                        const total = pool.status?.totalCapacityGB ?? 0;
                        const shareCount = pool.status?.shareCount ?? 0;
                        const maxShares = pool.spec?.maxShares ?? 0;
                        return (
                          <StackItem key={name}>
                            <Split hasGutter>
                              <SplitItem style={{ minWidth: 140, fontWeight: 'bold' }}>
                                <Link to={ROUTES.POOL_DETAIL.replace(':name', name)}>
                                  {name}
                                </Link>
                              </SplitItem>
                              <SplitItem isFilled>
                                <CapacityBar allocated={allocated} total={total} />
                              </SplitItem>
                              <SplitItem style={{ minWidth: 100, textAlign: 'right' }}>
                                <Label isCompact>{shareCount}/{maxShares} shares</Label>
                              </SplitItem>
                            </Split>
                          </StackItem>
                        );
                      })}
                    </Stack>
                  )}
                </CardBody>
              </Card>
            </GridItem>
          </Grid>
        </StackItem>

        {/* Row 4: Replication Status (only if policies exist) */}
        {hasReplication && (
          <StackItem>
            <Grid hasGutter>
              <GridItem sm={12} md={4}>
                <Card isFullHeight>
                  <CardTitle>Replication Lag</CardTitle>
                  <CardBody>
                    {(replicationPolicies || []).map((rp) => {
                      const rpName = rp.metadata?.name || '';
                      const lagSeconds = poolMetrics[rp.spec?.sourcePoolName]?.replicationLagSeconds ?? 0;
                      const lagColor: 'green' | 'orange' | 'red' =
                        lagSeconds === 0 ? 'green' : lagSeconds < 300 ? 'green' : lagSeconds < 900 ? 'orange' : 'red';
                      return (
                        <Split hasGutter key={rpName} style={{ marginBottom: 8 }}>
                          <SplitItem style={{ minWidth: 120 }}>
                            <Link to={ROUTES.REPLICATION_DETAIL.replace(':name', rpName)}>
                              {rpName}
                            </Link>
                          </SplitItem>
                          <SplitItem>
                            <Label color={lagColor}>
                              {lagSeconds === 0 ? 'In sync' : `${Math.round(lagSeconds)}s behind`}
                            </Label>
                          </SplitItem>
                        </Split>
                      );
                    })}
                  </CardBody>
                </Card>
              </GridItem>

              <GridItem sm={12} md={4}>
                <Card isFullHeight>
                  <CardTitle>Sync Rate</CardTitle>
                  <CardBody>
                    <span style={{ fontSize: '1.5rem', fontWeight: 'bold' }}>
                      {aggregate.replicationSyncRate5m.toFixed(1)}
                    </span>
                    <span style={{ marginLeft: 4, color: 'var(--pf-v6-global--Color--200)' }}>/min</span>
                  </CardBody>
                </Card>
              </GridItem>

              <GridItem sm={12} md={4}>
                <Card isFullHeight>
                  <CardTitle>Consecutive Failures</CardTitle>
                  <CardBody>
                    {Object.keys(aggregate.replicationConsecutiveFailures).length === 0 ? (
                      <Label color="green">None</Label>
                    ) : (
                      <Stack hasGutter>
                        {Object.entries(aggregate.replicationConsecutiveFailures).map(
                          ([policy, count]) => (
                            <StackItem key={policy}>
                              <Split hasGutter>
                                <SplitItem>{policy}</SplitItem>
                                <SplitItem>
                                  <Label color={count > 0 ? 'red' : 'green'}>{count}</Label>
                                </SplitItem>
                              </Split>
                            </StackItem>
                          ),
                        )}
                      </Stack>
                    )}
                  </CardBody>
                </Card>
              </GridItem>
            </Grid>
          </StackItem>
        )}

        {/* Row 5: Operations Activity */}
        <StackItem>
          <Grid hasGutter>
            <GridItem sm={12} md={4}>
              <Card isFullHeight>
                <CardTitle>Snapshots</CardTitle>
                <CardBody>
                  <span style={{ fontSize: '1.5rem', fontWeight: 'bold' }}>
                    {Math.round(aggregate.snapshotCount)}
                  </span>
                  <span style={{ marginLeft: 8, color: 'var(--pf-v6-global--Color--200)' }}>total</span>
                </CardBody>
              </Card>
            </GridItem>

            <GridItem sm={12} md={4}>
              <Card isFullHeight>
                <CardTitle>Clones</CardTitle>
                <CardBody>
                  <DescriptionList isCompact>
                    <DescriptionListGroup>
                      <DescriptionListTerm>Total</DescriptionListTerm>
                      <DescriptionListDescription>
                        {Math.round(aggregate.cloneCount)}
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                    <DescriptionListGroup>
                      <DescriptionListTerm>Avg duration</DescriptionListTerm>
                      <DescriptionListDescription>
                        {aggregate.cloneAvgDurationSeconds > 0
                          ? `${Math.round(aggregate.cloneAvgDurationSeconds)}s`
                          : '-'}
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                  </DescriptionList>
                </CardBody>
              </Card>
            </GridItem>

            <GridItem sm={12} md={4}>
              <Card isFullHeight>
                <CardTitle>Group Snapshots</CardTitle>
                <CardBody>
                  <span style={{ fontSize: '1.5rem', fontWeight: 'bold' }}>
                    {Math.round(aggregate.groupSnapshotCount)}
                  </span>
                  <span style={{ marginLeft: 8, color: 'var(--pf-v6-global--Color--200)' }}>total</span>
                </CardBody>
              </Card>
            </GridItem>
          </Grid>
        </StackItem>

        <StackItem>
          <Divider />
        </StackItem>

        {/* Recent SubVolume Activity */}
        <StackItem>
          <Card>
            <CardTitle>Recent Activity</CardTitle>
            <CardBody>
              {recentSubVolumes.length === 0 ? (
                <Bullseye>No SubVolumes found.</Bullseye>
              ) : (
                <Table aria-label="Recent SubVolume activity" variant="compact">
                  <Thead>
                    <Tr>
                      <Th>Name</Th>
                      <Th>Pool</Th>
                      <Th>PVC</Th>
                      <Th>Size</Th>
                      <Th>Phase</Th>
                      <Th>Age</Th>
                    </Tr>
                  </Thead>
                  <Tbody>
                    {recentSubVolumes.map((sv) => {
                      const name = sv.metadata?.name || '';
                      const detailPath = ROUTES.SUBVOLUME_DETAIL.replace(
                        ':name',
                        name,
                      );
                      return (
                        <Tr key={name}>
                          <Td dataLabel="Name">
                            <Link to={detailPath}>{name}</Link>
                          </Td>
                          <Td dataLabel="Pool">{sv.spec?.poolName || '-'}</Td>
                          <Td dataLabel="PVC">
                            {sv.spec?.pvcNamespace && sv.spec?.pvcName
                              ? `${sv.spec.pvcNamespace}/${sv.spec.pvcName}`
                              : '-'}
                          </Td>
                          <Td dataLabel="Size">
                            {sv.spec?.requestedGB != null
                              ? `${sv.spec.requestedGB} GB`
                              : '-'}
                          </Td>
                          <Td dataLabel="Phase">
                            <PhaseStatus phase={sv.status?.phase} />
                          </Td>
                          <Td dataLabel="Age">
                            {formatAge(sv.metadata?.creationTimestamp)}
                          </Td>
                        </Tr>
                      );
                    })}
                  </Tbody>
                </Table>
              )}
            </CardBody>
          </Card>
        </StackItem>
      </Stack>
    </PageSection>
  );
};

export default DashboardPage;
