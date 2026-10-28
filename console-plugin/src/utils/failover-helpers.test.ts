import { Failover } from '../types';
import {
  PLAN_USABLE_MS,
  buildFailover,
  confirmationWord,
  copyStaleness,
  describeFailoverState,
  describePlanRows,
  describeVMDetail,
  executeSummary,
  findPlan,
  isBusyMessage,
  needsNewPlan,
  planRemainingMs,
  receiverClaims,
  scheduleOfVM,
} from './failover-helpers';

const NOW = new Date('2026-10-28T12:00:00Z').getTime();

const plan = (over: Partial<Failover> = {}): Failover => ({
  metadata: { name: 'p1', creationTimestamp: '2026-10-28T11:55:00Z' },
  spec: { action: 'Plan', source: 'dr-data' },
  status: {
    phase: 'Planned',
    source: 'dr-data',
    finishedAt: '2026-10-28T11:58:00Z',
    vms: [
      {
        name: 'apps/web',
        moment: '2026-10-28T11:30:00Z',
        moments: ['2026-10-28T11:30:00Z', '2026-10-28T11:00:00Z', '2026-10-28T10:30:00Z'],
        disks: [
          { subVolume: 'pvc-a', rewind: false },
          { subVolume: 'pvc-b', rewind: true },
        ],
      },
      {
        name: 'apps/db',
        moment: '2026-10-28T11:30:00Z',
        moments: ['2026-10-28T11:30:00Z'],
        disks: [{ subVolume: 'pvc-c', rewind: false }],
      },
    ],
    volumes: [
      { subVolume: 'pvc-a', pvcName: 'web-root', pvcNamespace: 'apps', requestedGB: 10, subPath: '/a', policy: 'nightly', lastSync: '2026-10-28T11:30:00Z' },
      { subVolume: 'pvc-b', pvcName: 'web-data', pvcNamespace: 'apps', requestedGB: 10, subPath: '/b', policy: 'nightly', lastSync: '2026-10-28T11:30:00Z' },
      { subVolume: 'pvc-c', pvcName: 'db-root', pvcNamespace: 'apps', requestedGB: 10, subPath: '/c', policy: 'nightly', lastSync: '2026-10-28T11:30:00Z' },
      { subVolume: 'pvc-d', pvcName: 'files', pvcNamespace: 'other', requestedGB: 5, subPath: '/d', lastSync: '2026-10-28T11:30:00Z' },
    ],
  },
  ...over,
});

describe('buildFailover', () => {
  it('makes a Plan that sets only what is asked', () => {
    const f = buildFailover({ action: 'Plan' }, NOW);
    expect(f).toMatchObject({
      apiVersion: 'storage.ibmcloud.io/v1alpha1',
      kind: 'Failover',
      spec: { action: 'Plan' },
    });
    expect(f.spec).toEqual({ action: 'Plan' });
    expect(f.metadata?.name).toMatch(/^console-plan-[a-z0-9]+$/);
    expect(f.metadata?.labels).toMatchObject({ 'vpc-file-pool-csi.ibm.io/made-by-console': 'true' });
  });

  it('sets source, rewindTo, force and drPool when given', () => {
    const f = buildFailover(
      { action: 'Execute', source: 'dr-data', rewindTo: '2026-10-28T11:00:00Z', force: true, drPool: 'dr-pool' },
      NOW,
    );
    expect(f.spec).toEqual({
      action: 'Execute',
      source: 'dr-data',
      rewindTo: '2026-10-28T11:00:00Z',
      force: true,
      drPool: 'dr-pool',
    });
    expect(f.metadata?.name).toMatch(/^console-execute-/);
  });

  it('leaves force out when it is false', () => {
    expect(buildFailover({ action: 'Plan', force: false }, NOW).spec).toEqual({ action: 'Plan' });
  });
});

describe('describeFailoverState', () => {
  it('says a Planning or Executing failover is still working, with the time it has run', () => {
    const s = describeFailoverState(
      { spec: { action: 'Execute' }, status: { phase: 'Executing', startedAt: '2026-10-28T11:57:30Z' } },
      NOW,
    );
    expect(s).toMatchObject({ working: true, finished: false, failed: false, elapsed: '2m 30s' });
    expect(s.title).toMatch(/still working/i);
    expect(s.detail).toMatch(/minutes/);
  });

  it('treats a new object with no phase as starting', () => {
    expect(describeFailoverState({ spec: { action: 'Plan' } }, NOW)).toMatchObject({ working: true });
  });

  it('shows Failed with the message', () => {
    const s = describeFailoverState(
      { spec: { action: 'Plan' }, status: { phase: 'Failed', message: 'claim "x" not found' } },
      NOW,
    );
    expect(s).toMatchObject({ failed: true, working: false, message: 'claim "x" not found' });
  });

  it('shows Done as finished', () => {
    expect(describeFailoverState({ spec: { action: 'Execute' }, status: { phase: 'Done' } }, NOW)).toMatchObject({
      finished: true,
      working: false,
    });
  });
});

describe('isBusyMessage', () => {
  it('recognises the refusal while a copy is being applied', () => {
    expect(
      isBusyMessage('a copy is still being applied to pvc-a; wait for it to end, or use --force (an unfinished copy is rolled back first)'),
    ).toBe(true);
    expect(isBusyMessage('claim "x" not found')).toBe(false);
    expect(isBusyMessage(undefined)).toBe(false);
  });
});

describe('plan age', () => {
  it('counts down from 15 minutes after the plan finished', () => {
    expect(PLAN_USABLE_MS).toBeLessThan(15 * 60 * 1000);
    expect(planRemainingMs(plan(), NOW)).toBe(PLAN_USABLE_MS - 2 * 60 * 1000);
  });
  it('is zero for an old plan, and for a plan that is not Planned', () => {
    expect(planRemainingMs(plan({ status: { phase: 'Planned', finishedAt: '2026-10-28T11:00:00Z' } }), NOW)).toBe(0);
    expect(planRemainingMs(plan({ status: { phase: 'Planning' } }), NOW)).toBe(0);
  });
});

describe('findPlan', () => {
  const older = plan({ metadata: { name: 'old', creationTimestamp: '2026-10-28T10:00:00Z' } });
  const newer = plan({ metadata: { name: 'new', creationTimestamp: '2026-10-28T11:59:00Z' } });
  const exec = plan({ metadata: { name: 'x', creationTimestamp: '2026-10-28T11:59:30Z' }, spec: { action: 'Execute', source: 'dr-data' } });

  it('takes the newest Plan for the source and ignores Executes', () => {
    expect(findPlan([older, exec, newer], 'dr-data')?.metadata?.name).toBe('new');
  });
  it('ignores Plans for another source', () => {
    expect(findPlan([plan({ spec: { action: 'Plan', source: 'other' }, status: { source: 'other' } })], 'dr-data')).toBeUndefined();
  });
  it('matches a Plan that used the default source when no source is chosen', () => {
    const p = plan({ spec: { action: 'Plan' } });
    expect(findPlan([p], '')?.metadata?.name).toBe('p1');
  });
});

describe('describePlanRows', () => {
  it('has a row for each VM with disks, disks to rewind and moments', () => {
    const rows = describePlanRows(plan(), NOW);
    expect(rows).toHaveLength(2);
    expect(rows[0]).toMatchObject({ name: 'apps/web', disks: 2, rewindDisks: 1, moment: '2026-10-28T11:30:00Z', age: '30m', momentCount: 3 });
    expect(rows[1]).toMatchObject({ name: 'apps/db', disks: 1, rewindDisks: 0, momentCount: 1 });
  });
  it('is empty without a status', () => {
    expect(describePlanRows(undefined, NOW)).toEqual([]);
  });
});

describe('copyStaleness', () => {
  it('is stale when the copy is older than twice the schedule', () => {
    expect(copyStaleness('2026-10-28T09:00:00Z', '1h', NOW)).toMatchObject({ stale: true, known: true });
    expect(copyStaleness('2026-10-28T09:00:00Z', '1h', NOW).text).toMatch(/older than twice the 1h schedule/);
  });
  it('is not stale within twice the schedule', () => {
    expect(copyStaleness('2026-10-28T10:30:00Z', '1h', NOW)).toMatchObject({ stale: false, known: true });
  });
  it('says the schedule is unknown when there is none', () => {
    const s = copyStaleness('2026-10-28T09:00:00Z', undefined, NOW);
    expect(s).toMatchObject({ stale: false, known: false });
    expect(s.text).toMatch(/schedule/i);
  });
  it('is stale when there is no copy moment', () => {
    expect(copyStaleness(undefined, '1h', NOW)).toMatchObject({ stale: true });
  });
});

describe('scheduleOfVM', () => {
  const policies = [
    { metadata: { name: 'nightly' }, spec: { schedule: '6h' } },
    { metadata: { name: 'other' }, spec: { schedule: '1h' } },
  ];
  it('finds the schedule of the policy that wrote the disks', () => {
    const p = plan();
    expect(scheduleOfVM(p, p.status!.vms![0], policies as never)).toBe('6h');
  });
  it('is undefined when the policy is not on this cluster, or no volume names one', () => {
    const p = plan();
    expect(scheduleOfVM(p, p.status!.vms![0], [])).toBeUndefined();
    expect(scheduleOfVM(p, { name: 'x', moment: '', disks: [{ subVolume: 'pvc-d', rewind: false }] }, policies as never)).toBeUndefined();
  });
});

describe('describeVMDetail', () => {
  it('lists disks with the rewind note, and moments newest first', () => {
    const d = describeVMDetail(plan(), 'apps/web', NOW)!;
    expect(d.disks).toEqual([
      { subVolume: 'pvc-a', claim: 'apps/web-root', rewind: false, note: 'Used in place.' },
      { subVolume: 'pvc-b', claim: 'apps/web-data', rewind: true, note: 'Copies the whole disk; takes minutes.' },
    ]);
    expect(d.moments.map((m) => m.iso)).toEqual(['2026-10-28T11:30:00Z', '2026-10-28T11:00:00Z', '2026-10-28T10:30:00Z']);
    expect(d.moments[0]).toMatchObject({ isCurrent: true, age: '30m' });
    expect(d.moments[1].isCurrent).toBe(false);
  });
  it('sorts moments newest first even if the list is not', () => {
    const p = plan();
    p.status!.vms![0].moments = ['2026-10-28T10:30:00Z', '2026-10-28T11:30:00Z'];
    expect(describeVMDetail(p, 'apps/web', NOW)!.moments.map((m) => m.iso)[0]).toBe('2026-10-28T11:30:00Z');
  });
  it('includes the plan moment when the list leaves it out', () => {
    const p = plan();
    p.status!.vms![1].moments = undefined;
    expect(describeVMDetail(p, 'apps/db', NOW)!.moments.map((m) => m.iso)).toEqual(['2026-10-28T11:30:00Z']);
  });
  it('is undefined for a VM that is not in the plan', () => {
    expect(describeVMDetail(plan(), 'apps/nope', NOW)).toBeUndefined();
  });
});

describe('needsNewPlan', () => {
  it('is false when the chosen moment is the one the plan shows for the VM', () => {
    expect(needsNewPlan(plan(), 'apps/web', '2026-10-28T11:30:00Z')).toBe(false);
  });
  it('is true for any other moment', () => {
    expect(needsNewPlan(plan(), 'apps/web', '2026-10-28T11:00:00Z')).toBe(true);
  });
  it('is true for a VM that is not in the plan', () => {
    expect(needsNewPlan(plan(), 'apps/nope', '2026-10-28T11:30:00Z')).toBe(true);
  });
});

describe('executeSummary', () => {
  it('names every VM, the disks to rewind and the PVCs that will be made', () => {
    const s = executeSummary(plan(), 'apps/web');
    expect(s.vms).toEqual([
      { name: 'apps/web', moment: '2026-10-28T11:30:00Z', rewindDisks: 1, disks: 2 },
      { name: 'apps/db', moment: '2026-10-28T11:30:00Z', rewindDisks: 0, disks: 1 },
    ]);
    expect(s.rewindDisks).toBe(1);
    expect(s.pvcs.map((p) => `${p.namespace}/${p.name}`)).toEqual(['apps/web-root', 'apps/web-data', 'apps/db-root', 'other/files']);
    expect(s.otherVMs).toBe(1);
    expect(s.chosenPVCs.map((p) => p.name)).toEqual(['web-root', 'web-data']);
  });
  it('moves the PVCs to the namespace override when there is one', () => {
    const p = plan();
    p.spec.namespace = 'dr';
    expect(executeSummary(p, 'apps/web').pvcs.map((x) => x.namespace)).toEqual(['dr', 'dr', 'dr', 'dr']);
  });
});

describe('confirmationWord', () => {
  it('is the VM name when the plan has one VM, and the source claim when it has several', () => {
    const p = plan();
    expect(confirmationWord(p, 'apps/web')).toBe('dr-data');
    p.status!.vms = [p.status!.vms![0]];
    expect(confirmationWord(p, 'apps/web')).toBe('apps/web');
  });
});

describe('receiverClaims', () => {
  const deployment = (claim: string | undefined, component = 'replication-receiver') => ({
    metadata: { name: 'r', labels: { 'app.kubernetes.io/component': component } },
    spec: {
      template: {
        spec: {
          volumes: [{ name: 'auth-token', secret: { secretName: 's' } }, { name: 'data', persistentVolumeClaim: claim ? { claimName: claim } : undefined }],
        },
      },
    },
  });
  it('reads the data claim of each receiver Deployment, without repeats', () => {
    expect(receiverClaims([deployment('dr-data'), deployment('dr-data'), deployment('b-data')] as never)).toEqual(['b-data', 'dr-data']);
  });
  it('skips Deployments that are not receivers or have no claim', () => {
    expect(receiverClaims([deployment('x', 'controller'), deployment(undefined)] as never)).toEqual([]);
  });
});
