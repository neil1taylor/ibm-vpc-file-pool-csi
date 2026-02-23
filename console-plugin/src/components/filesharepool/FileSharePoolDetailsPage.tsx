import React, { useState, useMemo } from 'react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { useParams, useHistory } from 'react-router-dom';
import {
  Tabs,
  Tab,
  TabTitleText,
  PageSection,
  Title,
  DescriptionList,
  DescriptionListGroup,
  DescriptionListTerm,
  DescriptionListDescription,
  Grid,
  GridItem,
  Card,
  CardTitle,
  CardBody,
  Label,
  Spinner,
  Alert,
  Bullseye,
  Button,
  Split,
  SplitItem,
  Stack,
  StackItem,
} from '@patternfly/react-core';
import {
  Table,
  Thead,
  Tr,
  Th,
  Tbody,
  Td,
} from '@patternfly/react-table';
import { FileSharePoolModel } from '../../models';
import { FileSharePool } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import CapacityBar from '../common/CapacityBar';
import ConditionsTable from '../common/ConditionsTable';
import { formatAge } from '../../utils/resource-helpers';
import { usePoolMetrics } from '../../utils/use-pool-metrics';
import { ROUTES } from '../../constants';
import DeleteModal from '../common/DeleteModal';
import YAMLEditorFallback from '../common/YAMLEditorFallback';

/** Format seconds to human-readable ms string. */
const formatLatencyMs = (seconds: number): string => {
  if (seconds === 0) return '-';
  const ms = seconds * 1000;
  if (ms < 1) return '<1 ms';
  return `${ms.toFixed(0)} ms`;
};

const FileSharePoolDetailsPage: React.FC = () => {
  const { name } = useParams<{ name: string }>();
  const history = useHistory();
  const [activeTab, setActiveTab] = useState<string | number>(0);
  const [deleteModalOpen, setDeleteModalOpen] = useState(false);

  const [pool, loaded, loadError] = useK8sWatchResource<FileSharePool>({
    groupVersionKind: {
      group: FileSharePoolModel.apiGroup,
      version: FileSharePoolModel.apiVersion,
      kind: FileSharePoolModel.kind,
    },
    name,
  });

  const poolNames = useMemo(() => (name ? [name] : []), [name]);
  const { metrics: poolMetrics, aggregate } = usePoolMetrics(poolNames);

  if (!loaded) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }

  if (loadError) {
    return (
      <PageSection>
        <Alert variant="danger" title="Error loading FileSharePool" isInline>
          {loadError.message || 'An unexpected error occurred.'}
        </Alert>
      </PageSection>
    );
  }

  if (!pool) {
    return (
      <PageSection>
        <Alert variant="warning" title="Not found" isInline>
          FileSharePool &quot;{name}&quot; was not found.
        </Alert>
      </PageSection>
    );
  }

  const spec = pool.spec;
  const status = pool.status;
  const pm = poolMetrics[name] || {
    capacityGB: 0,
    allocatedGB: 0,
    shareCount: 0,
    pvcCount: 0,
    allocationRate1h: 0,
    p95LatencySeconds: 0,
    replicationLagSeconds: 0,
  };

  const renderOverview = () => (
    <Grid hasGutter>
      <GridItem span={8}>
        <Card>
          <CardTitle>Details</CardTitle>
          <CardBody>
            <DescriptionList isHorizontal>
              <DescriptionListGroup>
                <DescriptionListTerm>Name</DescriptionListTerm>
                <DescriptionListDescription>
                  {pool.metadata?.name || '-'}
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Zone</DescriptionListTerm>
                <DescriptionListDescription>{spec.zone}</DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Profile</DescriptionListTerm>
                <DescriptionListDescription>{spec.profile}</DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Share Size (GB)</DescriptionListTerm>
                <DescriptionListDescription>{spec.shareSizeGB}</DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>IOPS</DescriptionListTerm>
                <DescriptionListDescription>{spec.iops ?? '-'}</DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Max Shares</DescriptionListTerm>
                <DescriptionListDescription>{spec.maxShares}</DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Initial Shares</DescriptionListTerm>
                <DescriptionListDescription>{spec.initialShares}</DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Auto Expand</DescriptionListTerm>
                <DescriptionListDescription>
                  <Label color={spec.autoExpand ? 'green' : 'grey'}>
                    {spec.autoExpand ? 'Enabled' : 'Disabled'}
                  </Label>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Expand Threshold (%)</DescriptionListTerm>
                <DescriptionListDescription>
                  {spec.expandThresholdPercent}%
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Allocation Strategy</DescriptionListTerm>
                <DescriptionListDescription>
                  <Label>{spec.allocationStrategy}</Label>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Default Permissions</DescriptionListTerm>
                <DescriptionListDescription>
                  <code>{spec.defaultPermissions}</code>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Default UID</DescriptionListTerm>
                <DescriptionListDescription>
                  {spec.defaultUID ?? '-'}
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Default GID</DescriptionListTerm>
                <DescriptionListDescription>
                  {spec.defaultGID ?? '-'}
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Encryption in Transit</DescriptionListTerm>
                <DescriptionListDescription>
                  <Label color={spec.encryptionInTransit ? 'green' : 'grey'}>
                    {spec.encryptionInTransit ? 'Enabled' : 'Disabled'}
                  </Label>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Resource Group</DescriptionListTerm>
                <DescriptionListDescription>
                  {spec.resourceGroup || '-'}
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Tags</DescriptionListTerm>
                <DescriptionListDescription>
                  {spec.tags && spec.tags.length > 0
                    ? spec.tags.map((tag) => (
                        <Label key={tag} style={{ marginRight: 4, marginBottom: 4 }}>
                          {tag}
                        </Label>
                      ))
                    : '-'}
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Mount Options</DescriptionListTerm>
                <DescriptionListDescription>
                  {spec.mountOptions && spec.mountOptions.length > 0
                    ? <code>{spec.mountOptions.join(', ')}</code>
                    : '-'}
                </DescriptionListDescription>
              </DescriptionListGroup>
            </DescriptionList>
          </CardBody>
        </Card>
      </GridItem>
      <GridItem span={4}>
        <Card>
          <CardTitle>Status</CardTitle>
          <CardBody>
            <DescriptionList>
              <DescriptionListGroup>
                <DescriptionListTerm>Phase</DescriptionListTerm>
                <DescriptionListDescription>
                  <PhaseStatus phase={status?.phase} />
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Total Capacity</DescriptionListTerm>
                <DescriptionListDescription>
                  <CapacityBar
                    allocated={status?.totalAllocatedGB ?? 0}
                    total={status?.totalCapacityGB ?? 0}
                  />
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Shares</DescriptionListTerm>
                <DescriptionListDescription>
                  {status?.shareCount ?? 0}
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Total PVCs</DescriptionListTerm>
                <DescriptionListDescription>
                  {status?.totalPVCCount ?? 0}
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Last Reconcile</DescriptionListTerm>
                <DescriptionListDescription>
                  {status?.lastReconcileTime
                    ? formatAge(status.lastReconcileTime)
                    : '-'}
                </DescriptionListDescription>
              </DescriptionListGroup>
            </DescriptionList>
          </CardBody>
        </Card>
      </GridItem>
    </Grid>
  );

  const renderShares = () => {
    const shares = status?.shares;
    if (!shares || shares.length === 0) {
      return (
        <Alert variant="info" title="No shares" isInline>
          No shares provisioned yet.
        </Alert>
      );
    }

    return (
      <Table aria-label="Shares" variant="compact">
        <Thead>
          <Tr>
            <Th>Share Name</Th>
            <Th>Share ID</Th>
            <Th>Zone</Th>
            <Th>State</Th>
            <Th>Capacity</Th>
            <Th>PVCs</Th>
            <Th>Mount Target IP</Th>
            <Th>Export Path</Th>
            <Th>Created</Th>
          </Tr>
        </Thead>
        <Tbody>
          {shares.map((share) => (
            <Tr key={share.shareID}>
              <Td>{share.shareName}</Td>
              <Td>
                <code>{share.shareID}</code>
              </Td>
              <Td>{share.zone}</Td>
              <Td>
                <PhaseStatus phase={share.state} />
              </Td>
              <Td>
                <CapacityBar
                  allocated={share.allocatedGB}
                  total={share.totalGB}
                />
              </Td>
              <Td>{share.pvcCount}</Td>
              <Td>
                <code>{share.mountTargetIP}</code>
              </Td>
              <Td>{share.exportPath ? <code>{share.exportPath}</code> : '-'}</Td>
              <Td>{formatAge(share.createdAt)}</Td>
            </Tr>
          ))}
        </Tbody>
      </Table>
    );
  };

  const renderMetrics = () => (
    <Grid hasGutter>
      <GridItem sm={12} md={6} lg={3}>
        <Card isFullHeight>
          <CardTitle>Allocation Rate</CardTitle>
          <CardBody>
            <span style={{ fontSize: '1.5rem', fontWeight: 'bold' }}>
              {pm.allocationRate1h.toFixed(2)}
            </span>
            <span style={{ marginLeft: 4, color: 'var(--pf-v6-global--Color--200)' }}>/hr</span>
          </CardBody>
        </Card>
      </GridItem>

      <GridItem sm={12} md={6} lg={3}>
        <Card isFullHeight>
          <CardTitle>P95 Latency</CardTitle>
          <CardBody>
            {(() => {
              const ms = pm.p95LatencySeconds * 1000;
              const color: 'green' | 'orange' | 'red' | 'grey' =
                pm.p95LatencySeconds === 0 ? 'grey' : ms < 100 ? 'green' : ms < 500 ? 'orange' : 'red';
              return (
                <Split hasGutter>
                  <SplitItem>
                    <span style={{ fontSize: '1.5rem', fontWeight: 'bold' }}>
                      {formatLatencyMs(pm.p95LatencySeconds)}
                    </span>
                  </SplitItem>
                  <SplitItem>
                    <Label color={color}>
                      {color === 'green' ? 'Good' : color === 'orange' ? 'Elevated' : color === 'red' ? 'High' : '-'}
                    </Label>
                  </SplitItem>
                </Split>
              );
            })()}
          </CardBody>
        </Card>
      </GridItem>

      <GridItem sm={12} md={6} lg={3}>
        <Card isFullHeight>
          <CardTitle>VPC API Calls</CardTitle>
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
                  {(() => {
                    const pct =
                      aggregate.apiCallRate5m > 0
                        ? Math.round((aggregate.apiErrorRate5m / aggregate.apiCallRate5m) * 100)
                        : 0;
                    return (
                      <Label color={pct > 5 ? 'red' : pct > 0 ? 'orange' : 'green'}>
                        {pct}%
                      </Label>
                    );
                  })()}
                </DescriptionListDescription>
              </DescriptionListGroup>
            </DescriptionList>
          </CardBody>
        </Card>
      </GridItem>

      <GridItem sm={12} md={6} lg={3}>
        <Card isFullHeight>
          <CardTitle>Replication Lag</CardTitle>
          <CardBody>
            {pm.replicationLagSeconds === 0 ? (
              <Label color="green">In sync</Label>
            ) : (
              <Split hasGutter>
                <SplitItem>
                  <span style={{ fontSize: '1.5rem', fontWeight: 'bold' }}>
                    {Math.round(pm.replicationLagSeconds)}s
                  </span>
                </SplitItem>
                <SplitItem>
                  <Label color={pm.replicationLagSeconds > 900 ? 'red' : 'orange'}>
                    Behind
                  </Label>
                </SplitItem>
              </Split>
            )}
          </CardBody>
        </Card>
      </GridItem>

      {/* Per-share capacity */}
      <GridItem span={12}>
        <Card>
          <CardTitle>Per-Share Capacity</CardTitle>
          <CardBody>
            {status?.shares && status.shares.length > 0 ? (
              <Stack hasGutter>
                {status.shares.map((share) => (
                  <StackItem key={share.shareID}>
                    <Split hasGutter>
                      <SplitItem style={{ minWidth: 140 }}>
                        <strong>{share.shareName}</strong>
                      </SplitItem>
                      <SplitItem isFilled>
                        <CapacityBar allocated={share.allocatedGB} total={share.totalGB} />
                      </SplitItem>
                      <SplitItem style={{ minWidth: 80, textAlign: 'right' }}>
                        {share.pvcCount} PVCs
                      </SplitItem>
                      <SplitItem>
                        <PhaseStatus phase={share.state} />
                      </SplitItem>
                    </Split>
                  </StackItem>
                ))}
              </Stack>
            ) : (
              <Bullseye>No shares provisioned yet.</Bullseye>
            )}
          </CardBody>
        </Card>
      </GridItem>
    </Grid>
  );

  const renderGoldenImages = () => {
    if (!spec.goldenImages || !spec.goldenImages.enabled) {
      return (
        <Alert variant="info" title="Golden images not configured" isInline>
          Golden image syncing is not enabled for this pool. Set{' '}
          <code>spec.goldenImages.enabled: true</code> to activate it.
        </Alert>
      );
    }

    const config = spec.goldenImages;
    const images = status?.goldenImages;

    return (
      <>
        <Card style={{ marginBottom: 16 }}>
          <CardTitle>Configuration</CardTitle>
          <CardBody>
            <DescriptionList isHorizontal>
              <DescriptionListGroup>
                <DescriptionListTerm>Enabled</DescriptionListTerm>
                <DescriptionListDescription>
                  <Label color="green">Yes</Label>
                </DescriptionListDescription>
              </DescriptionListGroup>
              <DescriptionListGroup>
                <DescriptionListTerm>Target Namespaces</DescriptionListTerm>
                <DescriptionListDescription>
                  {config.targetNamespaces.map((ns) => (
                    <Label key={ns} style={{ marginRight: 4 }}>
                      {ns}
                    </Label>
                  ))}
                </DescriptionListDescription>
              </DescriptionListGroup>
              {config.imageFilter && config.imageFilter.length > 0 && (
                <DescriptionListGroup>
                  <DescriptionListTerm>Image Filter</DescriptionListTerm>
                  <DescriptionListDescription>
                    {config.imageFilter.map((f) => (
                      <Label key={f} style={{ marginRight: 4 }}>
                        {f}
                      </Label>
                    ))}
                  </DescriptionListDescription>
                </DescriptionListGroup>
              )}
              {config.refreshInterval && (
                <DescriptionListGroup>
                  <DescriptionListTerm>Refresh Interval</DescriptionListTerm>
                  <DescriptionListDescription>
                    {config.refreshInterval}
                  </DescriptionListDescription>
                </DescriptionListGroup>
              )}
              {config.converterImage && (
                <DescriptionListGroup>
                  <DescriptionListTerm>Converter Image</DescriptionListTerm>
                  <DescriptionListDescription>
                    <code>{config.converterImage}</code>
                  </DescriptionListDescription>
                </DescriptionListGroup>
              )}
              {config.pvcSizeGB != null && (
                <DescriptionListGroup>
                  <DescriptionListTerm>PVC Size (GB)</DescriptionListTerm>
                  <DescriptionListDescription>
                    {config.pvcSizeGB}
                  </DescriptionListDescription>
                </DescriptionListGroup>
              )}
            </DescriptionList>
          </CardBody>
        </Card>

        {images && images.length > 0 ? (
          <Table aria-label="Golden images" variant="compact">
            <Thead>
              <Tr>
                <Th>Image Name</Th>
                <Th>Source URL</Th>
                <Th>Namespace</Th>
                <Th>Phase</Th>
                <Th>PVC</Th>
                <Th>Template</Th>
                <Th>Last Sync</Th>
                <Th>Message</Th>
              </Tr>
            </Thead>
            <Tbody>
              {images.flatMap((img) =>
                (img.namespaces || []).map((ns) => (
                  <Tr key={`${img.name}-${ns.namespace}`}>
                    <Td>{img.name}</Td>
                    <Td>
                      {img.sourceURL ? (
                        <code style={{ fontSize: '0.85em' }}>{img.sourceURL}</code>
                      ) : (
                        '-'
                      )}
                    </Td>
                    <Td>{ns.namespace}</Td>
                    <Td>
                      <PhaseStatus phase={ns.phase} />
                    </Td>
                    <Td>{ns.pvcName || '-'}</Td>
                    <Td>{ns.templateName || '-'}</Td>
                    <Td>{formatAge(ns.lastSyncTime)}</Td>
                    <Td>{ns.message || '-'}</Td>
                  </Tr>
                )),
              )}
            </Tbody>
          </Table>
        ) : (
          <Alert variant="info" title="No golden images synced yet" isInline>
            The syncer has not yet provisioned any golden images.
          </Alert>
        )}
      </>
    );
  };

  const renderDrainStatus = () => {
    if (!spec.drainShares || spec.drainShares.length === 0) {
      return (
        <Alert variant="info" title="No drains in progress" isInline>
          No shares are configured for draining.
        </Alert>
      );
    }

    const drainStatuses = status?.drainStatus;
    if (!drainStatuses || drainStatuses.length === 0) {
      return (
        <Alert variant="info" title="Drain status pending" isInline>
          Drains have been requested for shares{' '}
          {spec.drainShares.map((id) => (
            <code key={id} style={{ marginRight: 4 }}>
              {id}
            </code>
          ))}{' '}
          but no status has been reported yet.
        </Alert>
      );
    }

    return (
      <Table aria-label="Drain status" variant="compact">
        <Thead>
          <Tr>
            <Th>Share ID</Th>
            <Th>Remaining SubVolumes</Th>
            <Th>Drained</Th>
            <Th>Drain Started</Th>
          </Tr>
        </Thead>
        <Tbody>
          {drainStatuses.map((ds) => (
            <Tr key={ds.shareID}>
              <Td>
                <code>{ds.shareID}</code>
              </Td>
              <Td>{ds.remainingSubVolumes}</Td>
              <Td>
                <Label color={ds.drained ? 'green' : 'orange'}>
                  {ds.drained ? 'Yes' : 'No'}
                </Label>
              </Td>
              <Td>{formatAge(ds.drainStartedAt)}</Td>
            </Tr>
          ))}
        </Tbody>
      </Table>
    );
  };

  const renderConditions = () => (
    <ConditionsTable conditions={status?.conditions} />
  );

  const renderYAML = () => {
    const yamlString = JSON.stringify(pool, null, 2);
    return <YAMLEditorFallback yaml={yamlString} />;
  };

  return (
    <>
      <PageSection variant="default">
        <Split hasGutter>
          <SplitItem isFilled>
            <Title headingLevel="h1" size="2xl">
              {pool.metadata?.name}
            </Title>
          </SplitItem>
          <SplitItem>
            <Button
              variant="danger"
              onClick={() => setDeleteModalOpen(true)}
            >
              Delete
            </Button>
          </SplitItem>
        </Split>
      </PageSection>
      <PageSection>
        <Tabs
          activeKey={activeTab}
          onSelect={(_event, tabIndex) => setActiveTab(tabIndex)}
          isBox
        >
          <Tab eventKey={0} title={<TabTitleText>Overview</TabTitleText>}>
            <PageSection variant="default" padding={{ default: 'padding' }}>
              {renderOverview()}
            </PageSection>
          </Tab>
          <Tab eventKey={1} title={<TabTitleText>Shares</TabTitleText>}>
            <PageSection variant="default" padding={{ default: 'padding' }}>
              {renderShares()}
            </PageSection>
          </Tab>
          <Tab eventKey={2} title={<TabTitleText>Metrics</TabTitleText>}>
            <PageSection variant="default" padding={{ default: 'padding' }}>
              {renderMetrics()}
            </PageSection>
          </Tab>
          <Tab eventKey={3} title={<TabTitleText>Golden Images</TabTitleText>}>
            <PageSection variant="default" padding={{ default: 'padding' }}>
              {renderGoldenImages()}
            </PageSection>
          </Tab>
          <Tab eventKey={4} title={<TabTitleText>Drain Status</TabTitleText>}>
            <PageSection variant="default" padding={{ default: 'padding' }}>
              {renderDrainStatus()}
            </PageSection>
          </Tab>
          <Tab eventKey={5} title={<TabTitleText>Conditions</TabTitleText>}>
            <PageSection variant="default" padding={{ default: 'padding' }}>
              {renderConditions()}
            </PageSection>
          </Tab>
          <Tab eventKey={6} title={<TabTitleText>YAML</TabTitleText>}>
            <PageSection variant="default" padding={{ default: 'padding' }}>
              {renderYAML()}
            </PageSection>
          </Tab>
        </Tabs>
      </PageSection>

      <DeleteModal
        isOpen={deleteModalOpen}
        resourceName={pool.metadata?.name || ''}
        resourceKind="FileSharePool"
        model={FileSharePoolModel}
        onClose={() => setDeleteModalOpen(false)}
        onDeleted={() => {
          setDeleteModalOpen(false);
          history.push(ROUTES.POOLS);
        }}
        requireConfirmation
      />
    </>
  );
};

export default FileSharePoolDetailsPage;
