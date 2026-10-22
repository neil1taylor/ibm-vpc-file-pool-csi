import React, { useState } from 'react';
import { k8sPatch, useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Link, useHistory, useParams } from 'react-router-dom';
import {
  Alert,
  Bullseye,
  Button,
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
import DeleteModal from '../common/DeleteModal';
import NoAccessNote from '../common/NoAccessNote';
import { BACKUP_DELETE_NOTE } from './backup-texts';
import BackupProblemBanner from './BackupProblemBanner';
import BackupVolumesTable from './BackupVolumesTable';
import { vmCopySettingsText } from '../../utils/replication-helpers';
import { suspendPatch } from '../../utils/backup-helpers';
import { errorMessage } from '../../utils/k8s-objects';
import { toYAML } from '../../utils/yaml-lite';
import { API_GROUP, ROUTES } from '../../constants';

const Term: React.FC<{ term: string; children: React.ReactNode }> = ({ term, children }) => (
  <DescriptionListGroup>
    <DescriptionListTerm>{term}</DescriptionListTerm>
    <DescriptionListDescription>{children}</DescriptionListDescription>
  </DescriptionListGroup>
);

const BackupDetailsPage: React.FC = () => {
  const { name } = useParams<{ name: string }>();
  const history = useHistory();
  const [deleteOpen, setDeleteOpen] = useState(false);
  const [actionError, setActionError] = useState('');
  const [canUpdate] = useAccessReview({ group: API_GROUP, resource: 'backuppolicies', verb: 'update' });
  const [canDelete] = useAccessReview({ group: API_GROUP, resource: 'backuppolicies', verb: 'delete' });
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
  const suspended = !!spec.suspend;
  const toggleSuspend = async () => {
    setActionError('');
    try {
      await k8sPatch({ model: BackupPolicyModel, resource: policy, data: suspendPatch(!suspended) });
    } catch (err) {
      setActionError(errorMessage(err, 'Failed to change the policy'));
    }
  };
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
          <SplitItem>
            {canUpdate ? (
              <Button variant="secondary" component={(props) => <Link {...props} to={ROUTES.BACKUP_EDIT.replace(':name', name)} />}>
                Edit
              </Button>
            ) : (
              <Button variant="secondary" isDisabled>
                Edit
              </Button>
            )}
          </SplitItem>
          <SplitItem>
            <Button variant="secondary" onClick={toggleSuspend} isDisabled={!canUpdate}>
              {suspended ? 'Resume' : 'Suspend'}
            </Button>
          </SplitItem>
          <SplitItem>
            <Button variant="danger" onClick={() => setDeleteOpen(true)} isDisabled={!canDelete}>
              Delete
            </Button>
          </SplitItem>
        </Split>
      </PageSection>
      <PageSection>
        {actionError && (
          <Alert variant="danger" isInline title="The change failed" style={{ marginBottom: 16 }}>
            {actionError}
          </Alert>
        )}
        {canUpdate === false && (
          <div style={{ marginBottom: 16 }}>
            <NoAccessNote
              right="update BackupPolicy objects"
              yaml={toYAML({
                apiVersion: policy.apiVersion,
                kind: 'BackupPolicy',
                metadata: { name },
                spec: { suspend: !suspended },
              })}
            />
          </div>
        )}
        {canDelete === false && (
          <div style={{ marginBottom: 16 }}>
            <NoAccessNote right="delete BackupPolicy objects" />
          </div>
        )}
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
      <DeleteModal
        isOpen={deleteOpen}
        resourceName={name}
        resourceKind="BackupPolicy"
        model={BackupPolicyModel}
        note={BACKUP_DELETE_NOTE}
        onClose={() => setDeleteOpen(false)}
        onDeleted={() => {
          setDeleteOpen(false);
          history.push(ROUTES.BACKUPS);
        }}
      />
    </>
  );
};

export default BackupDetailsPage;
