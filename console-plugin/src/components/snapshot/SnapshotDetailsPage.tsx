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
  Label,
  Spinner,
  Alert,
  Bullseye,
  Button,
  Split,
  SplitItem,
} from '@patternfly/react-core';
import { SnapshotModel } from '../../models';
import { Snapshot } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import DeleteModal from '../common/DeleteModal';
import { humanizeBytes } from '../../utils/resource-helpers';
import { ROUTES } from '../../constants';

const SnapshotDetailsPage: React.FC = () => {
  const { name } = useParams<{ name: string }>();
  const history = useHistory();
  const [deleteModalOpen, setDeleteModalOpen] = useState(false);

  const [snapshot, loaded, loadError] = useK8sWatchResource<Snapshot>({
    groupVersionKind: {
      group: SnapshotModel.apiGroup,
      version: SnapshotModel.apiVersion,
      kind: SnapshotModel.kind,
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
        <Alert variant="danger" title="Error loading Snapshot" isInline>
          {loadError.message || 'An unexpected error occurred.'}
        </Alert>
      </PageSection>
    );
  }

  if (!snapshot) {
    return (
      <PageSection>
        <Alert variant="warning" title="Not found" isInline>
          Snapshot &quot;{name}&quot; was not found.
        </Alert>
      </PageSection>
    );
  }

  const spec = snapshot.spec;
  const status = snapshot.status;

  const subvolumeDetailPath = ROUTES.SUBVOLUME_DETAIL.replace(':name', spec.sourceSubVolume);
  const poolDetailPath = ROUTES.POOL_DETAIL.replace(':name', spec.poolName);

  return (
    <>
      <PageSection variant="default">
        <Split hasGutter>
          <SplitItem isFilled>
            <Title headingLevel="h1" size="2xl">
              {snapshot.metadata?.name}
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
          <GridItem span={8}>
            <Card>
              <CardTitle>Spec</CardTitle>
              <CardBody>
                <DescriptionList isHorizontal>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Source SubVolume</DescriptionListTerm>
                    <DescriptionListDescription>
                      <Link to={subvolumeDetailPath}>{spec.sourceSubVolume}</Link>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Pool Name</DescriptionListTerm>
                    <DescriptionListDescription>
                      <Link to={poolDetailPath}>{spec.poolName}</Link>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Share ID</DescriptionListTerm>
                    <DescriptionListDescription>
                      <code>{spec.shareID}</code>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Share Mount Target IP</DescriptionListTerm>
                    <DescriptionListDescription>
                      <code>{spec.shareMountTargetIP}</code>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Snapshot Path</DescriptionListTerm>
                    <DescriptionListDescription>
                      <code>{spec.snapshotPath}</code>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Source Sub Path</DescriptionListTerm>
                    <DescriptionListDescription>
                      <code>{spec.sourceSubPath}</code>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Size (GB)</DescriptionListTerm>
                    <DescriptionListDescription>
                      {spec.sizeGB}
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
                    <DescriptionListTerm>Ready to Use</DescriptionListTerm>
                    <DescriptionListDescription>
                      <Label color={status?.readyToUse ? 'green' : 'grey'}>
                        {status?.readyToUse ? 'True' : 'False'}
                      </Label>
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Size (Bytes)</DescriptionListTerm>
                    <DescriptionListDescription>
                      {status?.sizeBytes != null ? humanizeBytes(status.sizeBytes) : '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                  <DescriptionListGroup>
                    <DescriptionListTerm>Creation Time</DescriptionListTerm>
                    <DescriptionListDescription>
                      {status?.creationTime || '-'}
                    </DescriptionListDescription>
                  </DescriptionListGroup>
                </DescriptionList>
              </CardBody>
            </Card>
          </GridItem>
        </Grid>
      </PageSection>

      <DeleteModal
        isOpen={deleteModalOpen}
        resourceName={snapshot.metadata?.name || ''}
        resourceKind="Snapshot"
        model={SnapshotModel}
        onClose={() => setDeleteModalOpen(false)}
        onDeleted={() => {
          setDeleteModalOpen(false);
          history.push(ROUTES.SNAPSHOTS);
        }}
      />
    </>
  );
};

export default SnapshotDetailsPage;
