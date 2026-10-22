import React from 'react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Link } from 'react-router-dom';
import { Alert } from '@patternfly/react-core';
import { BackupRestoreModel } from '../../models';
import { BackupRestore } from '../../types';
import { backupRestoreProgress } from '../../utils/backup-helpers';
import { pvcPath } from '../../utils/k8s-objects';
import { TROUBLESHOOTING } from '../../utils/help-links';
import HelpLink from '../common/HelpLink';

interface Props {
  restoreName: string;
  targetNamespace: string;
  targetName: string;
}

/** Follows a BackupRestore: its phase, the moment that was restored, and the message when it fails. */
const BackupRestoreBanner: React.FC<Props> = ({ restoreName, targetNamespace, targetName }) => {
  const [restore] = useK8sWatchResource<BackupRestore>({
    groupVersionKind: {
      group: BackupRestoreModel.apiGroup,
      version: BackupRestoreModel.apiVersion,
      kind: BackupRestoreModel.kind,
    },
    name: restoreName,
  });
  const view = backupRestoreProgress(restore);
  const claimLink = <Link to={pvcPath(targetNamespace, targetName)}>{targetName}</Link>;

  switch (view.state) {
    case 'done':
      return (
        <Alert variant="success" title={view.title} isInline>
          {view.text} The new claim {claimLink} is ready to use in a workload.
        </Alert>
      );
    case 'failed':
      return (
        <Alert variant="danger" title={view.title} isInline>
          {view.text} Nothing was written over an existing volume. See the log of the restore Job in the driver
          namespace. <HelpLink href={TROUBLESHOOTING.pvc} label="Troubleshooting" testId="banner-help" />
        </Alert>
      );
    default:
      return (
        <Alert variant="info" title={view.title} isInline>
          {view.text} The new claim {claimLink} cannot be used until the restore is done.
        </Alert>
      );
  }
};

export default BackupRestoreBanner;
