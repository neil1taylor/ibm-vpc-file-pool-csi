import React, { useEffect, useRef, useState } from 'react';
import { useAccessReview, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Table, Thead, Tr, Th, Tbody, Td } from '@patternfly/react-table';
import {
  Alert,
  Button,
  FormHelperText,
  HelperText,
  HelperTextItem,
  Spinner,
  Stack,
  StackItem,
  TextInput,
} from '@patternfly/react-core';
import { DeploymentModel } from '../../models';
import { DRIVER_NAMESPACE } from '../../constants';
import { COSRepo, KubeObject, buildCOSListJob } from '../../utils/backup-jobs';
import { COSListing, cosListAccessRights, driverImageOf, parseCOSListing } from '../../utils/backup-cos';
import { errorMessage } from '../../utils/k8s-objects';
import { cleanUpShareObjects, createShareObjects, useShareJob } from '../../utils/use-share-job';
import { toYAML } from '../../utils/yaml-lite';
import NoAccessNote from '../common/NoAccessNote';
import { shareJobName } from '../../utils/backup-jobs';

interface Props {
  /** Where the repository is. Undefined when there are no settings to list with. */
  repo: COSRepo | undefined;
  /** Why there are no settings, said in the box. */
  unavailable?: string;
  /** If given, each moment has a button that calls it with the volume and the moment. */
  onPick?: (volume: string, moment: string) => void;
}

type Step = 'idle' | 'starting' | 'running' | 'done' | 'error';

const COS_VOLUME = /^pvc-[a-f0-9-]{36}$/;

/**
 * Lists the volumes and the export moments in a bucket with a read-only Job in the driver namespace, as
 * `kubectl backup cos-list` does. The Job reads the keys from Secrets; the console never sees them.
 */
const COSContents: React.FC<Props> = ({ repo, unavailable, onPick }) => {
  const [volume, setVolume] = useState('');
  const [step, setStep] = useState<Step>('idle');
  const [error, setError] = useState('');
  const [listing, setListing] = useState<COSListing | undefined>();
  const [jobName, setJobName] = useState('');
  const job = useRef<KubeObject | undefined>(undefined);

  const rights = cosListAccessRights(DRIVER_NAMESPACE);
  const checks = [useAccessReview(rights[0]), useAccessReview(rights[1]), useAccessReview(rights[2])];
  const missing = rights.filter((_, i) => !checks[i][0] && !checks[i][1]).map((r) => `${r.verb} ${r.resource}`);

  const [deployments] = useK8sWatchResource<Array<Record<string, unknown>>>({
    groupVersionKind: { group: DeploymentModel.apiGroup, version: DeploymentModel.apiVersion, kind: DeploymentModel.kind },
    namespace: DRIVER_NAMESPACE,
    selector: { matchLabels: { 'app.kubernetes.io/component': 'controller' } },
    isList: true,
  });
  const driverImage = driverImageOf(deployments);

  const outcome = useShareJob(step === 'running' ? jobName : '');
  const volumeError = volume && !COS_VOLUME.test(volume) ? 'Enter a volume name: pvc- and a UUID.' : '';

  useEffect(() => {
    if (step !== 'running' || outcome.state === 'running') return;
    if (job.current) cleanUpShareObjects({ job: job.current });
    if (outcome.state === 'failed') {
      setError('Listing the bucket failed. Check the bucket, the keys and the Key Protect settings; see the log of the Job in the driver namespace.');
      setStep('error');
      return;
    }
    const parsed = parseCOSListing(outcome.message);
    if (!parsed) {
      setError(`The list Job did not report a listing (got "${outcome.message.slice(0, 80)}").`);
      setStep('error');
      return;
    }
    setListing(parsed);
    setStep('done');
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [step, outcome.state]);

  const makeJob = (): KubeObject | undefined =>
    repo && driverImage
      ? buildCOSListJob({ key: `coslist-${Date.now().toString(36)}`, namespace: DRIVER_NAMESPACE, driverImage, repo, volume })
      : undefined;

  const run = async () => {
    setError('');
    setListing(undefined);
    if (!driverImage) {
      setError(`Cannot find the driver controller in ${DRIVER_NAMESPACE}, so the list cannot start.`);
      setStep('error');
      return;
    }
    setStep('starting');
    try {
      const built = makeJob();
      if (!built) throw new Error('There are no settings to list with.');
      job.current = built;
      await createShareObjects({ job: built });
      setJobName(shareJobName(built.metadata.name.replace('share-data-', '')));
      setStep('running');
    } catch (err) {
      setError(errorMessage(err, 'Could not start the list Job'));
      setStep('error');
    }
  };

  const busy = step === 'starting' || step === 'running';
  const adminJob = missing.length > 0 ? makeJob() : undefined;

  return (
    <Stack hasGutter>
      {unavailable && (
        <StackItem>
          <Alert variant="info" isInline title="Nothing to list">
            {unavailable}
          </Alert>
        </StackItem>
      )}
      <StackItem>
        <TextInput
          aria-label="Limit to one volume (optional)"
          placeholder="Limit to one volume (optional): pvc-..."
          value={volume}
          onChange={(_e, v) => setVolume(v)}
          isDisabled={busy}
          validated={volumeError ? 'error' : 'default'}
        />
        <FormHelperText>
          <HelperText>
            <HelperTextItem variant={volumeError ? 'error' : 'default'}>
              {volumeError || 'With one volume, the list shows all of its moments. Without, a long list is cut.'}
            </HelperTextItem>
          </HelperText>
        </FormHelperText>
      </StackItem>
      {missing.length > 0 && (
        <StackItem>
          <NoAccessNote
            right={`make and remove a Job in ${DRIVER_NAMESPACE} (missing: ${missing.join(', ')})`}
            yaml={adminJob ? toYAML(adminJob) : undefined}
          />
        </StackItem>
      )}
      <StackItem>
        <Button variant="secondary" onClick={run} isDisabled={!repo || !!volumeError || busy || missing.length > 0} isLoading={busy}>
          List COS contents
        </Button>
        {busy && (
          <span style={{ marginLeft: 12 }}>
            <Spinner size="md" /> Listing the bucket with a Job in {DRIVER_NAMESPACE}. This can take a minute.
          </span>
        )}
      </StackItem>
      {step === 'error' && (
        <StackItem>
          <Alert variant="danger" isInline title="The list failed">
            {error}
          </Alert>
        </StackItem>
      )}
      {step === 'done' && listing && listing.volumes.length === 0 && (
        <StackItem>
          <Alert variant="info" isInline title="Nothing found">
            No backups found in bucket {repo?.bucket}, folder {repo?.prefix}.
          </Alert>
        </StackItem>
      )}
      {step === 'done' && listing && listing.volumes.length > 0 && (
        <StackItem>
          <Table aria-label="Backups in COS" variant="compact">
            <Thead>
              <Tr>
                <Th>Volume</Th>
                <Th>Moment</Th>
                {onPick && <Th></Th>}
              </Tr>
            </Thead>
            <Tbody>
              {listing.volumes.flatMap((v) =>
                v.moments.length === 0
                  ? [
                      <Tr key={v.name}>
                        <Td dataLabel="Volume">{v.name}</Td>
                        <Td dataLabel="Moment">-</Td>
                        {onPick && <Td></Td>}
                      </Tr>,
                    ]
                  : v.moments.map((m, i) => (
                      <Tr key={`${v.name}/${m}`}>
                        <Td dataLabel="Volume">{i === 0 ? v.name : ''}</Td>
                        <Td dataLabel="Moment">{m}</Td>
                        {onPick && (
                          <Td>
                            <Button variant="link" isInline onClick={() => onPick(v.name, m)}>
                              Restore this moment
                            </Button>
                          </Td>
                        )}
                      </Tr>
                    )),
              )}
            </Tbody>
          </Table>
          {listing.truncated && (
            <Alert variant="warning" isInline title="The list is cut" style={{ marginTop: 16 }}>
              There are more moments than the Job can report. Enter a volume name above and list again to see all of its moments.
            </Alert>
          )}
        </StackItem>
      )}
    </Stack>
  );
};

export default COSContents;
