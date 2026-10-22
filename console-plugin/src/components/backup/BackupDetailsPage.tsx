import React from 'react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Link, useParams } from 'react-router-dom';
import {
  Alert,
  Bullseye,
  Card,
  CardBody,
  DescriptionList,
  DescriptionListDescription,
  DescriptionListGroup,
  DescriptionListTerm,
  Label,
  PageSection,
  Spinner,
  Split,
  SplitItem,
  Title,
} from '@patternfly/react-core';
import { BackupPolicyModel } from '../../models';
import { BackupPolicy } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import CardHelpTitle from '../common/CardHelpTitle';
import BackupProblemBanner from './BackupProblemBanner';
import BackupVolumesTable from './BackupVolumesTable';
import { vmCopySettingsText } from '../../utils/replication-helpers';
import { ROUTES } from '../../constants';

const Term: React.FC<{ term: string; children: React.ReactNode }> = ({ term, children }) => (
  <DescriptionListGroup>
    <DescriptionListTerm>{term}</DescriptionListTerm>
    <DescriptionListDescription>{children}</DescriptionListDescription>
  </DescriptionListGroup>
);

const BackupDetailsPage: React.FC = () => {
  const { name } = useParams<{ name: string }>();
  const [policy, loaded, loadError] = useK8sWatchResource<BackupPolicy>({
    groupVersionKind: {
      group: BackupPolicyModel.apiGroup,
      version: BackupPolicyModel.apiVersion,
      kind: BackupPolicyModel.kind,
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
        <Alert variant="danger" title="Error loading BackupPolicy" isInline>
          {loadError.message || 'An unexpected error occurred.'}
        </Alert>
      </PageSection>
    );
  }
  if (!policy) {
    return (
      <PageSection>
        <Alert variant="warning" title="Not found" isInline>
          BackupPolicy &quot;{name}&quot; was not found.
        </Alert>
      </PageSection>
    );
  }

  const spec = policy.spec;
  const labels = spec.selector?.matchLabels ? Object.entries(spec.selector.matchLabels) : [];
  const expressions = spec.selector?.matchExpressions || [];

  return (
    <>
      <PageSection variant="default">
        <Split hasGutter>
          <SplitItem isFilled>
            <Title headingLevel="h1" size="2xl">
              {policy.metadata?.name}
            </Title>
          </SplitItem>
          <SplitItem>
            <PhaseStatus phase={policy.status?.phase} />
          </SplitItem>
        </Split>
      </PageSection>
      <PageSection>
        <BackupProblemBanner policy={policy} />
        {policy.status?.phase === 'Suspended' && policy.status.message && (
          <Alert variant="info" isInline title="Backups are suspended" style={{ marginBottom: 16 }}>
            {policy.status.message}
          </Alert>
        )}
        <Card>
          <CardHelpTitle helpKey="backupPolicy" />
          <CardBody>
            <DescriptionList isHorizontal>
              <Term term="Snapshot Pool">
                <Link to={ROUTES.POOL_DETAIL.replace(':name', spec.snapshotPool)}>{spec.snapshotPool}</Link>
              </Term>
              <Term term="Schedule">A backup every {spec.schedule}</Term>
              <Term term="Keep">{spec.keepFor || '7d'}</Term>
              <Term term="Volumes picked by label">
                {labels.length + expressions.length === 0
                  ? '-'
                  : [
                      ...labels.map(([k, v]) => (
                        <Label key={k} style={{ marginRight: 4, marginBottom: 4 }}>
                          {k}={v}
                        </Label>
                      )),
                      ...expressions.map((e) => (
                        <Label key={e.key} style={{ marginRight: 4, marginBottom: 4 }}>
                          {`${e.key} ${e.operator}${e.values ? ` (${e.values.join(', ')})` : ''}`}
                        </Label>
                      )),
                    ]}
              </Term>
              <Term term="Namespaces">
                {spec.namespaces && spec.namespaces.length > 0 ? spec.namespaces.join(', ') : 'All namespaces'}
              </Term>
              <Term term="VM Copy Speed Limit">
                {vmCopySettingsText({ vmCopySpeedLimitMBps: spec.vmCopySpeedLimitMBps } as never).speed}
              </Term>
              <Term term="Export to COS">{spec.export ? `On, bucket ${spec.export.bucket}` : 'Off'}</Term>
            </DescriptionList>
          </CardBody>
        </Card>
        <BackupVolumesTable policy={policy} />
      </PageSection>
    </>
  );
};

export default BackupDetailsPage;
