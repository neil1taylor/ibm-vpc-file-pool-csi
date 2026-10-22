import { k8sCreate, k8sDelete, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { JobModel, PersistentVolumeClaimModel, PersistentVolumeModel, PodModel } from '../models';
import { DRIVER_NAMESPACE } from '../constants';
import { KubeObject, SHARE_JOB_LABEL } from './backup-jobs';
import { JobOutcome, jobOutcome } from './backup-delete';
import { isAlreadyExistsError } from './k8s-objects';

const gvk = (model: { apiGroup?: string; apiVersion: string; kind: string }) => ({
  group: model.apiGroup,
  version: model.apiVersion,
  kind: model.kind,
});

/** Follows a share Job in the driver namespace: the Job, and the pods whose termination message holds its result. */
export function useShareJob(name: string): JobOutcome {
  const [job] = useK8sWatchResource<Record<string, unknown>>(
    name ? { groupVersionKind: gvk(JobModel), namespace: DRIVER_NAMESPACE, name } : null,
  );
  const [pods] = useK8sWatchResource<Array<Record<string, unknown>>>(
    name
      ? {
          groupVersionKind: gvk(PodModel),
          namespace: DRIVER_NAMESPACE,
          selector: { matchLabels: { [SHARE_JOB_LABEL]: name } },
          isList: true,
        }
      : null,
  );
  return name ? jobOutcome(job, pods || []) : { state: 'running' };
}

export interface ShareObjects {
  pv?: KubeObject;
  pvc?: KubeObject;
  job: KubeObject;
}

/** Makes the PV, the claim and the Job (those the Job has). One that exists already is the same step run twice, and is kept. */
export async function createShareObjects(objects: ShareObjects): Promise<void> {
  const steps = [
    [PersistentVolumeModel, objects.pv],
    [PersistentVolumeClaimModel, objects.pvc],
    [JobModel, objects.job],
  ] as const;
  for (const [model, data] of steps) {
    if (!data) continue;
    try {
      await k8sCreate({ model, data });
    } catch (err) {
      if (!isAlreadyExistsError(err)) throw err;
    }
  }
}

/** Removes the Job, its claim and its PV once the result is read. A failure is logged, not fatal. */
export async function cleanUpShareObjects(objects: ShareObjects): Promise<void> {
  const steps = [
    [JobModel, objects.job],
    [PersistentVolumeClaimModel, objects.pvc],
    [PersistentVolumeModel, objects.pv],
  ] as const;
  for (const [model, resource] of steps) {
    if (!resource) continue;
    try {
      await k8sDelete({
        model,
        resource,
        json: { kind: 'DeleteOptions', apiVersion: 'v1', propagationPolicy: 'Background' },
      });
    } catch (err) {
      console.error(`Could not remove ${model.kind} ${resource.metadata.name}:`, err);
    }
  }
}
