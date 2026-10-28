import { PersistentVolumeClaim } from './k8s-objects';

// These objects are the same ones `kubectl migrate execute` makes (pkg/migrate/pod.go).
// pkg/migrate/golden_test.go writes them to a golden file, and migrate-objects.test.ts compares
// these builders with that file, so the two cannot drift apart.

/** The OpenShift tools image: it holds rsync and find, needs no pull secret and runs as any user id (issue #196). */
export const MIGRATION_IMAGE = 'image-registry.openshift-image-registry.svc:5000/openshift/tools:latest';
export const LABEL_MIGRATION = 'vpc-file-pool-csi.ibm.io/migration';
export const ANNOTATION_SOURCE_PVC = 'vpc-file-pool-csi.ibm.io/source-pvc';
export const ANNOTATION_TARGET_PVC = 'vpc-file-pool-csi.ibm.io/target-pvc';
export const ANNOTATION_POOL = 'vpc-file-pool-csi.ibm.io/pool';

/**
 * Copies, then compares file counts. A count mismatch fails the pod.
 * The copy runs as the user the cluster gives the pod, so it sets no owners or modes, and it leaves the
 * times of the new claim's top folder alone (it belongs to the pool's user).
 */
const MIGRATION_SCRIPT = `
set -e
echo "=== Migration started ==="
echo "Source: /source"
echo "Target: /target"
SOURCE_COUNT=$(find /source -type f | wc -l)
echo "Source file count: $SOURCE_COUNT"
rsync -rltSv --omit-dir-times --progress /source/ /target/
TARGET_COUNT=$(find /target -type f | wc -l)
echo "Target file count: $TARGET_COUNT"
echo "=== Migration complete ==="
if [ "$SOURCE_COUNT" = "$TARGET_COUNT" ]; then
  echo "VERIFICATION: OK (file counts match: $SOURCE_COUNT)"
  exit 0
else
  echo "VERIFICATION: FAILED (source=$SOURCE_COUNT target=$TARGET_COUNT)"
  exit 1
fi
`;

export interface MigrationPod {
  apiVersion: string;
  kind: string;
  metadata: { name: string; namespace: string; labels: Record<string, string>; annotations: Record<string, string> };
  spec: Record<string, unknown>;
}

/** The names the kubectl migrate tool gives the new claim and the pod. */
export function migrationNames(sourcePVC: string): { targetPVC: string; pod: string } {
  return { targetPVC: `${sourcePVC}-pooled`, pod: `migrate-${sourcePVC}` };
}

export function buildTargetPVC(input: {
  namespace: string;
  sourcePVC: string;
  pool: string;
  storageClass: string;
  size: string;
}): PersistentVolumeClaim {
  return {
    apiVersion: 'v1',
    kind: 'PersistentVolumeClaim',
    metadata: {
      name: migrationNames(input.sourcePVC).targetPVC,
      namespace: input.namespace,
      labels: { [LABEL_MIGRATION]: 'true' },
      annotations: { [ANNOTATION_SOURCE_PVC]: input.sourcePVC, [ANNOTATION_POOL]: input.pool },
    },
    spec: {
      accessModes: ['ReadWriteMany'],
      storageClassName: input.storageClass,
      resources: { requests: { storage: input.size } },
    },
  };
}

export function buildMigrationPod(input: { namespace: string; sourcePVC: string }): MigrationPod {
  const names = migrationNames(input.sourcePVC);
  return {
    apiVersion: 'v1',
    kind: 'Pod',
    metadata: {
      name: names.pod,
      namespace: input.namespace,
      labels: { [LABEL_MIGRATION]: 'true' },
      annotations: { [ANNOTATION_SOURCE_PVC]: input.sourcePVC, [ANNOTATION_TARGET_PVC]: names.targetPVC },
    },
    spec: {
      restartPolicy: 'Never',
      securityContext: { seccompProfile: { type: 'RuntimeDefault' } },
      containers: [
        {
          name: 'migrate',
          image: MIGRATION_IMAGE,
          securityContext: { allowPrivilegeEscalation: false, capabilities: { drop: ['ALL'] } },
          command: ['sh', '-c', MIGRATION_SCRIPT],
          volumeMounts: [
            { name: 'source', mountPath: '/source', readOnly: true },
            { name: 'target', mountPath: '/target' },
          ],
          resources: {
            requests: { cpu: '100m', memory: '128Mi' },
            limits: { cpu: '500m', memory: '256Mi' },
          },
        },
      ],
      volumes: [
        { name: 'source', persistentVolumeClaim: { claimName: input.sourcePVC, readOnly: true } },
        { name: 'target', persistentVolumeClaim: { claimName: names.targetPVC } },
      ],
    },
  };
}
