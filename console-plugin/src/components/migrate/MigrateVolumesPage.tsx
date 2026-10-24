import React, { useState } from 'react';
import { k8sCreate, useK8sWatchResource } from '@openshift-console/dynamic-plugin-sdk';
import { Link } from 'react-router-dom';
import {
  Alert,
  Bullseye,
  Button,
  FormSelect,
  FormSelectOption,
  Modal,
  ModalBody,
  ModalFooter,
  ModalHeader,
  ModalVariant,
  PageSection,
  Spinner,
  Stack,
  StackItem,
  Title,
} from '@patternfly/react-core';
import { Table, Tbody, Td, Th, Thead, Tr } from '@patternfly/react-table';
import { FileSharePoolModel, PersistentVolumeClaimModel, PodModel, StorageClassModel } from '../../models';
import { FileSharePool } from '../../types';
import { PersistentVolumeClaim, StorageClass, errorMessage } from '../../utils/k8s-objects';
import {
  MigrationPodLike,
  MigrationRow,
  migrationProgress,
  migrationRows,
  migrationTargets,
  sourceClasses,
} from '../../utils/migrate-helpers';
import { buildMigrationPod, buildTargetPVC, migrationNames } from '../../utils/migrate-objects';
import { ROUTES } from '../../constants';
import { useBackupPolicies } from '../../utils/use-backup-policies';
import { isSnapshotPool } from '../../utils/snapshot-pools';

const gvk = (model: { apiGroup?: string; apiVersion: string; kind: string }) => ({
  group: model.apiGroup,
  version: model.apiVersion,
  kind: model.kind,
});

/** Moves a claim of another storage class into a pool. It makes the same objects as `kubectl migrate execute`, and deletes nothing. */
const MigrateVolumesPage: React.FC = () => {
  const [pvcs, pvcsLoaded] = useK8sWatchResource<PersistentVolumeClaim[]>({
    groupVersionKind: gvk(PersistentVolumeClaimModel),
    isList: true,
  });
  const [pods, podsLoaded] = useK8sWatchResource<MigrationPodLike[]>({ groupVersionKind: gvk(PodModel), isList: true });
  const policies = useBackupPolicies();
  const [storageClasses, classesLoaded] = useK8sWatchResource<StorageClass[]>({
    groupVersionKind: gvk(StorageClassModel),
    isList: true,
  });
  const [pools, poolsLoaded] = useK8sWatchResource<FileSharePool[]>({
    groupVersionKind: gvk(FileSharePoolModel),
    isList: true,
  });

  const [namespaceChoice, setNamespaceChoice] = useState('');
  const [classChoice, setClassChoice] = useState('');
  const [moving, setMoving] = useState<MigrationRow | null>(null);
  const [targetChoice, setTargetChoice] = useState('');
  const [error, setError] = useState('');
  const [creating, setCreating] = useState(false);

  if (!pvcsLoaded || !podsLoaded || !classesLoaded || !poolsLoaded) {
    return (
      <Bullseye>
        <Spinner size="xl" />
      </Bullseye>
    );
  }

  const allPVCs = pvcs || [];
  const allPods = pods || [];
  const classes = sourceClasses(allPVCs, storageClasses || []);
  const sourceClass = classes.includes(classChoice) ? classChoice : classes[0] || '';
  const namespaces = [
    ...new Set(
      allPVCs
        .filter((p) => p.spec?.storageClassName === sourceClass)
        .map((p) => p.metadata?.namespace || ''),
    ),
  ].sort();
  const namespace = namespaces.includes(namespaceChoice) ? namespaceChoice : namespaces[0] || '';

  const rows = migrationRows({ pvcs: allPVCs, pods: allPods, namespace, sourceClass });
  const targets = migrationTargets(
    (pools || []).filter((p) => !isSnapshotPool(p.metadata?.name || '', policies)),
    storageClasses || [],
  );
  const target = targets.find((t) => t.storageClass === targetChoice) || targets[0];
  const doesNotFit = !!moving && !!target && target.freeGB !== undefined && moving.sizeGiB > target.freeGB;

  const progressOf = (row: MigrationRow) => {
    const names = migrationNames(row.name);
    return migrationProgress(
      allPVCs.find((p) => p.metadata?.namespace === namespace && p.metadata?.name === names.targetPVC),
      allPods.find((p) => p.metadata?.namespace === namespace && p.metadata?.name === names.pod),
    );
  };

  const close = () => {
    setMoving(null);
    setError('');
  };

  const start = async () => {
    if (!moving || !target) return;
    setError('');
    setCreating(true);
    try {
      // The new claim first. The pod mounts it, so the pod waits for it to bind.
      await k8sCreate({
        model: PersistentVolumeClaimModel,
        data: buildTargetPVC({
          namespace,
          sourcePVC: moving.name,
          pool: target.pool,
          storageClass: target.storageClass,
          size: `${moving.sizeGiB}Gi`,
        }),
      });
      await k8sCreate({ model: PodModel, data: buildMigrationPod({ namespace, sourcePVC: moving.name }) });
      close();
    } catch (err: unknown) {
      setError(errorMessage(err, 'Failed to start the migration'));
    } finally {
      setCreating(false);
    }
  };

  const statusCell = (row: MigrationRow) => {
    const progress = progressOf(row);
    const names = migrationNames(row.name);
    const logLink = <Link to={`/k8s/ns/${namespace}/pods/${names.pod}/logs`}>Pod log</Link>;
    switch (progress.state) {
      case 'binding':
        return <>Waiting for the new claim {names.targetPVC} to bind.</>;
      case 'starting':
        return <>The new claim is Bound. Starting the copy.</>;
      case 'copying':
        return (
          <>
            Copying ({progress.podPhase}). {logLink}
          </>
        );
      case 'done':
        return (
          <>
            Copied. Switch your application to {names.targetPVC}, then delete the old claim when you are sure.
          </>
        );
      case 'failed':
        return (
          <>
            Failed{progress.exitCode !== undefined ? ` (exit code ${progress.exitCode})` : ''}. The old claim is
            untouched. {logLink}
          </>
        );
      default:
        return row.canMove ? null : <>{row.reason}</>;
    }
  };

  return (
    <PageSection variant="default">
      <Title headingLevel="h1" style={{ marginBottom: '1rem' }}>
        Migrate volumes
      </Title>
      <Stack hasGutter>
        <StackItem>
          Move a claim of another storage class into a pool. A new claim is made and the data is copied. The old claim
          is never deleted here.
        </StackItem>

        {classes.length === 0 ? (
          <StackItem>
            <Alert variant="info" title="Nothing to migrate" isInline>
              No claims of other storage classes were found.
            </Alert>
          </StackItem>
        ) : (
          <>
            <StackItem>
              <label htmlFor="migrate-class">Source storage class </label>
              <FormSelect id="migrate-class" value={sourceClass} onChange={(_e, v) => setClassChoice(v)} style={{ maxWidth: 360 }}>
                {classes.map((c) => (
                  <FormSelectOption key={c} value={c} label={c} />
                ))}
              </FormSelect>
            </StackItem>
            <StackItem>
              <label htmlFor="migrate-namespace">Namespace </label>
              <FormSelect id="migrate-namespace" value={namespace} onChange={(_e, v) => setNamespaceChoice(v)} style={{ maxWidth: 360 }}>
                {namespaces.map((n) => (
                  <FormSelectOption key={n} value={n} label={n} />
                ))}
              </FormSelect>
            </StackItem>
            <StackItem>
              <Table aria-label="Claims to migrate" variant="compact">
                <Thead>
                  <Tr>
                    <Th>Name</Th>
                    <Th>Size</Th>
                    <Th>Phase</Th>
                    <Th>Migration</Th>
                    <Th screenReaderText="Actions" />
                  </Tr>
                </Thead>
                <Tbody>
                  {rows.map((row) => (
                    <Tr key={row.name}>
                      <Td dataLabel="Name">{row.name}</Td>
                      <Td dataLabel="Size">{row.sizeGiB} GiB</Td>
                      <Td dataLabel="Phase">{row.phase}</Td>
                      <Td dataLabel="Migration">{statusCell(row)}</Td>
                      <Td isActionCell>
                        <Button
                          variant="secondary"
                          isDisabled={!row.canMove || targets.length === 0}
                          onClick={() => setMoving(row)}
                        >
                          Migrate
                        </Button>
                      </Td>
                    </Tr>
                  ))}
                </Tbody>
              </Table>
            </StackItem>
          </>
        )}
      </Stack>

      {moving && (
        <Modal variant={ModalVariant.medium} isOpen onClose={close}>
          <ModalHeader title={`Migrate ${moving.name}?`} />
          <ModalBody>
            <Stack hasGutter>
              <StackItem>
                <label htmlFor="migrate-target">Target pool </label>
                <FormSelect id="migrate-target" value={target?.storageClass} onChange={(_e, v) => setTargetChoice(v)}>
                  {targets.map((t) => (
                    <FormSelectOption key={t.storageClass} value={t.storageClass} label={t.label} />
                  ))}
                </FormSelect>
                {target?.freeGB !== undefined && (
                  <div>
                    {target.freeGB} GB free in pool {target.pool}
                  </div>
                )}
              </StackItem>
              {doesNotFit && (
                <StackItem>
                  <Alert variant="danger" title="The volume does not fit" isInline>
                    The claim is {moving.sizeGiB} GiB, and the pool has less free space. Choose another pool or make
                    room first.
                  </Alert>
                </StackItem>
              )}
              <StackItem>
                <ul>
                  <li>
                    {`A new claim ${migrationNames(moving.name).targetPVC} is created in the pool.`}
                  </li>
                  <li>The data is copied into it. A large volume takes time.</li>
                  <li>The old claim stays. This page never deletes it.</li>
                  <li>You must point your application at the new claim.</li>
                </ul>
              </StackItem>
              {error && (
                <StackItem>
                  <Alert variant="danger" title="Migration failed to start" isInline>
                    {error}
                  </Alert>
                </StackItem>
              )}
            </Stack>
          </ModalBody>
          <ModalFooter>
            <Button variant="primary" onClick={start} isDisabled={creating || doesNotFit || !target} isLoading={creating}>
              Start migration
            </Button>
            <Button variant="link" onClick={close} isDisabled={creating}>
              Cancel
            </Button>
          </ModalFooter>
        </Modal>
      )}
      <p style={{ marginTop: 16 }}>
        <Link to={ROUTES.POOLS}>Back to pools</Link>
      </p>
    </PageSection>
  );
};

export default MigrateVolumesPage;
