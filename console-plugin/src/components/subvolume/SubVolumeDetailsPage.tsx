import React, { useState } from 'react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { useParams, Link, useHistory } from 'react-router-dom';
import {
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
  Spinner,
  Alert,
  Bullseye,
  Button,
  Split,
  SplitItem,
  Progress,
  ProgressMeasureLocation,
  ProgressVariant,
} from '@patternfly/react-core';
import { SubVolumeModel } from '../../models';
import { SubVolume } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import ConditionsTable from '../common/ConditionsTable';
import DeleteModal from '../common/DeleteModal';
import { humanizeBytes, formatAge } from '../../utils/resource-helpers';
import { ROUTES } from '../../constants';

const SubVolumeDetailsPage: React.FC = () => {
  const { name } = useParams<{ name: string }>();
  const history = useHistory();
  const [deleteModalOpen, setDeleteModalOpen] = useState(false);

  const [subVolume, loaded, loadError] = useK8sWatchResource<SubVolume>({
    groupVersionKind: {
      group: SubVolumeModel.apiGroup,
      version: SubVolumeModel.apiVersion,
      kind: SubVolumeModel.kind,
    },
    name,
  });

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
        <Alert variant="danger" title="Error loading SubVolume" isInline>
          {loadError.message || 'An unexpected error occurred.'}
        </Alert>
      </PageSection>
    );
  }

  if (!subVolume) {
    return (
      <PageSection>
        <Alert variant="warning" title="Not found" isInline>
          SubVolume &quot;{name}&quot; was not found.
        </Alert>
      </PageSection>
    );
  }

  const spec = subVolume.spec;
  const status = subVolume.status;
  const cloneProgress = status?.cloneProgress;

  const clonePercent =
    cloneProgress && cloneProgress.totalBytes > 0
      ? Math.round((cloneProgress.bytesCopied / cloneProgress.totalBytes) * 100)
      : 0;

  const cloneVariant = cloneProgress?.error
    ? ProgressVariant.danger
    : cloneProgress?.completedAt
      ? ProgressVariant.success
      : undefined;

  return (
    <>
      <PageSection variant="default">
        <Split hasGutter>
          <SplitItem isFilled>
            <Title headingLevel="h1" size="2xl">
              {subVolume.metadata?.name}
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
        <Grid hasGutter>
          {/* Section 1 - Details */}
          <GridItem span={8}>
            <Card>
              <CardTitle>Details</CardTitle>
              <CardBody>
                <DescriptionList isHorizontal>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Pool Name</DescriptionListTerm>
                    <DescriptionListDescription>
                      <Link to={ROUTES.POOL_DETAIL.replace(':name', spec.poolName)}>
                        {spec.poolName}
                      </Link>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Share ID</DescriptionListTerm>
                    <DescriptionListDescription>
                      <code>{spec.shareID || '-'}</code>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Share Mount Target IP</DescriptionListTerm>
                    <DescriptionListDescription>
                      <code>{spec.shareMountTargetIP || '-'}</code>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Share Export Path</DescriptionListTerm>
                    <DescriptionListDescription>
                      {spec.shareExportPath ? <code>{spec.shareExportPath}</code> : '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Sub Path</DescriptionListTerm>
                    <DescriptionListDescription>
                      <code>{spec.subPath}</code>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Requested Size</DescriptionListTerm>
                    <DescriptionListDescription>
                      {spec.requestedGB} GB
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>PV Name</DescriptionListTerm>
                    <DescriptionListDescription>
                      {spec.pvName || '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>PVC Name</DescriptionListTerm>
                    <DescriptionListDescription>
                      {spec.pvcName || '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>PVC Namespace</DescriptionListTerm>
                    <DescriptionListDescription>
                      {spec.pvcNamespace || '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>UID</DescriptionListTerm>
                    <DescriptionListDescription>
                      {spec.uid != null ? spec.uid : '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>GID</DescriptionListTerm>
                    <DescriptionListDescription>
                      {spec.gid != null ? spec.gid : '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Permissions</DescriptionListTerm>
                    <DescriptionListDescription>
                      {spec.permissions ? <code>{spec.permissions}</code> : '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Reclaim Policy</DescriptionListTerm>
                    <DescriptionListDescription>
                      {spec.reclaimPolicy}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Source Volume</DescriptionListTerm>
                    <DescriptionListDescription>
                      {spec.sourceVolume ? (
                        <Link to={ROUTES.SUBVOLUME_DETAIL.replace(':name', spec.sourceVolume)}>
                          {spec.sourceVolume}
                        </Link>
                      ) : (
                        '-'
                      )}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Source Share ID</DescriptionListTerm>
                    <DescriptionListDescription>
                      {spec.sourceShareID ? <code>{spec.sourceShareID}</code> : '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
              </CardBody>
            </Card>
          </GridItem>

          {/* Section 2 - Status */}
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
                    <DescriptionListTerm>Actual Usage</DescriptionListTerm>
                    <DescriptionListDescription>
                      {status?.actualUsageBytes != null
                        ? humanizeBytes(status.actualUsageBytes)
                        : '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Last Usage Check</DescriptionListTerm>
                    <DescriptionListDescription>
                      {formatAge(status?.lastUsageCheck)}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Created At</DescriptionListTerm>
                    <DescriptionListDescription>
                      {formatAge(status?.createdAt)}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
              </CardBody>
            </Card>
          </GridItem>

          {/* Section 3 - Clone Progress (conditional) */}
          {cloneProgress && (
            <GridItem span={12}>
              <Card>
                <CardTitle>Clone Progress</CardTitle>
                <CardBody>
                  <Progress
                    value={clonePercent}
                    title={`${humanizeBytes(cloneProgress.bytesCopied)} / ${humanizeBytes(cloneProgress.totalBytes)}`}
                    measureLocation={ProgressMeasureLocation.outside}
                    variant={cloneVariant}
                    aria-label="Clone progress"
                  />
                  <DescriptionList isHorizontal style={{ marginTop: 16 }}>
                    <DescriptionListGroup>
                      <DescriptionListTerm>Started At</DescriptionListTerm>
                      <DescriptionListDescription>
                        {formatAge(cloneProgress.startedAt)}
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                    <DescriptionListGroup>
                      <DescriptionListTerm>Completed At</DescriptionListTerm>
                      <DescriptionListDescription>
                        {formatAge(cloneProgress.completedAt)}
                      </DescriptionListDescription>
                    </DescriptionListGroup>
                    {cloneProgress.error && (
                      <DescriptionListGroup>
                        <DescriptionListTerm>Error</DescriptionListTerm>
                        <DescriptionListDescription>
                          <Alert variant="danger" title="Clone error" isInline isPlain>
                            {cloneProgress.error}
                          </Alert>
                        </DescriptionListDescription>
                      </DescriptionListGroup>
                    )}
                  </DescriptionList>
                </CardBody>
              </Card>
            </GridItem>
          )}

          {/* Section 4 - Conditions */}
          <GridItem span={12}>
            <Card>
              <CardTitle>Conditions</CardTitle>
              <CardBody>
                <ConditionsTable conditions={status?.conditions} />
              </CardBody>
            </Card>
          </GridItem>
        </Grid>
      </PageSection>

      <DeleteModal
        isOpen={deleteModalOpen}
        resourceName={subVolume.metadata?.name || ''}
        resourceKind="SubVolume"
        model={SubVolumeModel}
        onClose={() => setDeleteModalOpen(false)}
        onDeleted={() => {
          setDeleteModalOpen(false);
          history.push(ROUTES.SUBVOLUMES);
        }}
      />
    </>
  );
};

export default SubVolumeDetailsPage;
