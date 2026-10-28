import React from 'react';
import { useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Link } from 'react-router-dom';
import { Alert, Progress, ProgressMeasureLocation } from '@patternfly/react-core';
import { PersistentVolumeClaimModel, SubVolumeModel } from '../../models';
import { SubVolume } from '../../types';
import { PersistentVolumeClaim, pvcPath } from '../../utils/k8s-objects';
import { humanizeBytes } from '../../utils/resource-helpers';
import { restoreProgress } from '../../utils/restore-helpers';
import { TROUBLESHOOTING } from '../../utils/help-links';
import HelpLink from '../common/HelpLink';

/**
 * Follows a new claim made from a snapshot (a restore) or from another volume (a clone): the copy percent,
 * then ready, or the copy error. The claim is never deleted here.
 */
const RestoreProgressBanner: React.FC<{ namespace: string; name: string; source?: 'snapshot' | 'volume' }> = ({
  namespace,
  name,
  source = 'snapshot',
}) => {
  const clone = source === 'volume';
  const doing = clone ? 'Cloning' : 'Restoring';
  const [pvc] = useK8sWatchResource<PersistentVolumeClaim>({
    groupVersionKind: { version: PersistentVolumeClaimModel.apiVersion, kind: PersistentVolumeClaimModel.kind },
    namespace,
    name,
  });
  // The SubVolume has the name of the claim's volume, which exists once the driver has made it.
  const volumeName = pvc?.spec?.volumeName;
  const [subvolume, subvolumeLoaded] = useK8sWatchResource<SubVolume>(
    volumeName
      ? {
          groupVersionKind: {
            group: SubVolumeModel.apiGroup,
            version: SubVolumeModel.apiVersion,
            kind: SubVolumeModel.kind,
          },
          name: volumeName,
        }
      : null,
  );

  const progress = restoreProgress(pvc, subvolume, subvolumeLoaded);
  const claimLink = <Link to={pvcPath(namespace, name)}>{name}</Link>;

  switch (progress.state) {
    case 'ready':
      return (
        <Alert variant="success" title={`The ${clone ? 'cloned' : 'restored'} volume is ready`} isInline>
          The claim {claimLink} is Bound. Use it in a workload from the Add storage page of the workload.
        </Alert>
      );
    case 'failed':
      return (
        <Alert variant="danger" title={`${doing} failed`} isInline>
          {progress.error} The claim {claimLink} was not deleted. See the log of the copy Job in the driver namespace.{' '}
          <HelpLink href={TROUBLESHOOTING.pvc} label="Troubleshooting" testId="banner-help" />
        </Alert>
      );
    case 'copying':
      return (
        <Alert variant="info" title={`${doing}: copying ${humanizeBytes(progress.totalBytes)} from the ${source}`} isInline>
          <Progress
            value={progress.percent}
            title={`Copied ${humanizeBytes(progress.copiedBytes)} of ${humanizeBytes(progress.totalBytes)}`}
            measureLocation={ProgressMeasureLocation.outside}
            label={`${progress.percent}%`}
          />
        </Alert>
      );
    default:
      return (
        <Alert variant="info" title={`${doing}: the copy is starting`} isInline>
          The claim {claimLink} stays Pending until the copy ends.
        </Alert>
      );
  }
};

export default RestoreProgressBanner;
