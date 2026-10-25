import React, { useState } from 'react';
import {
  useK8sWatchResource,
  ListPageHeader,
  ListPageBody,
  ListPageCreateLink,
} from '@openshift-console/dynamic-plugin-sdk';
import { Link } from 'react-router-dom';
import { Table, Thead, Tr, Th, Td, Tbody } from '@patternfly/react-table';
import { Alert, Bullseye, Checkbox, Spinner } from '@patternfly/react-core';
import { PoolBenchModel } from '../../models';
import { PoolBench } from '../../types';
import PhaseStatus from '../common/PhaseStatus';
import { columnInfo } from '../../utils/column-help';
import { ROUTES } from '../../constants';
import { POOLBENCH_INTRO } from './poolbench-texts';

/** Hours and minutes from start to end (or to now while the run goes on). */
export const runTime = (start?: string, end?: string, now: Date = new Date()): string => {
  if (!start) return '-';
  const mins = Math.max(0, Math.round(((end ? new Date(end) : now).getTime() - new Date(start).getTime()) / 60000));
  return `${Math.floor(mins / 60)}h ${mins % 60}m`;
};

const stateText = (pb: PoolBench): string => {
  const st = pb.status || {};
  if (st.phase === 'Failed') return st.message || '';
  if ((st.phase === 'Running' || st.phase === 'Paused') && st.step) return `step ${st.step} of ${st.totalSteps}`;
  return '';
};

const PoolBenchListPage: React.FC = () => {
  const [runs, loaded, loadError] = useK8sWatchResource<PoolBench[]>({
    groupVersionKind: { group: PoolBenchModel.apiGroup, version: PoolBenchModel.apiVersion, kind: PoolBenchModel.kind },
    isList: true,
  });
  const sorted = [...(runs || [])].sort((a, b) =>
    (b.metadata?.creationTimestamp || '').localeCompare(a.metadata?.creationTimestamp || ''),
  );
  const [chosen, setChosen] = useState<string[]>([]);
  const toggle = (name: string) =>
    setChosen((c) => (c.includes(name) ? c.filter((x) => x !== name) : [...c, name]));
  // Compare in the order the runs were made, so a series reads from the slowest tier up.
  const compareOrder = sorted
    .map((pb) => pb.metadata?.name || '')
    .filter((n) => chosen.includes(n))
    .reverse();

  return (
    <>
      <ListPageHeader title="PoolBench">
        <ListPageCreateLink
          to={ROUTES.POOLBENCH_CREATE}
          createAccessReview={{ groupVersionKind: { group: PoolBenchModel.apiGroup!, version: PoolBenchModel.apiVersion, kind: PoolBenchModel.kind } }}
        >
          New run
        </ListPageCreateLink>
        {chosen.length >= 2 && (
          <Link to={`${ROUTES.POOLBENCH_COMPARE}?runs=${compareOrder.join(',')}`} style={{ marginLeft: 16 }}>
            Compare {chosen.length} runs
          </Link>
        )}
      </ListPageHeader>
      <p style={{ padding: '0 24px', color: 'var(--pf-t--global--color--subtle)', margin: '0 0 8px 0' }}>{POOLBENCH_INTRO}</p>
      <ListPageBody>
        {!loaded && (
          <Bullseye style={{ padding: '48px 0' }}>
            <Spinner size="xl" />
          </Bullseye>
        )}
        {loadError && (
          <Alert variant="info" isInline title="PoolBench is not installed">
            The PoolBench resource is not in this cluster. Install the CRDs of the driver and set poolBench.enabled in the Helm chart to run tests.
          </Alert>
        )}
        {loaded && !loadError && sorted.length === 0 && (
          <Bullseye style={{ padding: '48px 0', color: 'var(--pf-t--global--color--subtle)' }}>No runs yet.</Bullseye>
        )}
        {loaded && !loadError && sorted.length > 0 && (
          <Table aria-label="PoolBench runs" variant="compact">
            <Thead>
              <Tr>
                <Th screenReaderText="Choose to compare" />
                <Th>Name</Th>
                <Th>Share</Th>
                <Th info={columnInfo('poolbenchPhase')}>State</Th>
                <Th>VMs now</Th>
                <Th info={columnInfo('poolbenchKnee')}>Knee</Th>
                <Th info={columnInfo('poolbenchRecommended')}>Recommended</Th>
                <Th>Started</Th>
                <Th>Run time</Th>
              </Tr>
            </Thead>
            <Tbody>
              {sorted.map((pb) => {
                const name = pb.metadata?.name || '';
                const st = pb.status || {};
                return (
                  <Tr key={pb.metadata?.uid || name}>
                    <Td>
                      <Checkbox
                        id={`pb-choose-${name}`}
                        aria-label={`Choose ${name}`}
                        isChecked={chosen.includes(name)}
                        onChange={() => toggle(name)}
                      />
                    </Td>
                    <Td dataLabel="Name">
                      <Link to={ROUTES.POOLBENCH_DETAIL.replace(':name', name)}>{name}</Link>
                    </Td>
                    <Td dataLabel="Share">
                      {pb.spec.shareSizeGB} GB, {pb.spec.iops} IOPS
                    </Td>
                    <Td dataLabel="State">
                      <PhaseStatus phase={st.phase || 'Pending'} /> {stateText(pb)}
                    </Td>
                    <Td dataLabel="VMs now">{st.vms ?? 0}</Td>
                    <Td dataLabel="Knee">{st.kneeVMs ? st.kneeVMs : '-'}</Td>
                    <Td dataLabel="Recommended">{st.recommendedVMs ? st.recommendedVMs : '-'}</Td>
                    <Td dataLabel="Started">{st.startTime ? new Date(st.startTime).toLocaleString() : '-'}</Td>
                    <Td dataLabel="Run time">{runTime(st.startTime, st.endTime)}</Td>
                  </Tr>
                );
              })}
            </Tbody>
          </Table>
        )}
      </ListPageBody>
    </>
  );
};

export default PoolBenchListPage;
