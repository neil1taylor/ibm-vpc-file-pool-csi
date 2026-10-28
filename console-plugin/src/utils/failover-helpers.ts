import { API_GROUP_VERSION } from '../constants';
import { Failover, FailoverSpec, FailoverVM, ReplicationPolicy } from '../types';
import { parseGoDuration } from './backup-policy-form';
import { LABEL_MADE_BY_CONSOLE } from './receiver-objects';

/** How long the driver accepts a finished Plan for an Execute (pkg/failover PlanMaxAge). */
const PLAN_MAX_AGE_MS = 15 * 60 * 1000;
/** The page stops offering a Plan a minute early, so the time it takes to read the dialog does not use up the Plan. */
export const PLAN_USABLE_MS = PLAN_MAX_AGE_MS - 60 * 1000;

const RECEIVER_COMPONENT = 'replication-receiver';
const RECEIVER_DATA_VOLUME = 'data';

// The refusal the Job gives while a copy is applied to a disk. Keep in step with pkg/failover/vm.go.
const BUSY_MESSAGE = 'a copy is still being applied to';

const ms = (iso: string | undefined): number => (iso ? new Date(iso).getTime() : NaN);
const sameSecond = (a: string | undefined, b: string | undefined): boolean =>
  Math.floor(ms(a) / 1000) === Math.floor(ms(b) / 1000);

/** An age as the pages show it: 45s, 30m, 5h, 2d. */
export function ageText(iso: string | undefined, now: number): string {
  if (!iso) return '-';
  const seconds = Math.max(0, Math.floor((now - ms(iso)) / 1000));
  if (seconds < 60) return `${seconds}s`;
  const minutes = Math.floor(seconds / 60);
  if (minutes < 60) return `${minutes}m`;
  const hours = Math.floor(minutes / 60);
  if (hours < 24) return `${hours}h`;
  return `${Math.floor(hours / 24)}d`;
}

function elapsedText(totalMs: number): string {
  const total = Math.max(0, Math.floor(totalMs / 1000));
  const h = Math.floor(total / 3600);
  const m = Math.floor((total % 3600) / 60);
  const s = total % 60;
  if (h > 0) return `${h}h ${m}m`;
  if (m > 0) return `${m}m ${s}s`;
  return `${s}s`;
}

export interface FailoverRequest {
  action: FailoverSpec['action'];
  source?: string;
  rewindTo?: string;
  /** Only these VMs ("namespace/name"). Empty or unset means every VM on the copy. */
  vms?: string[];
  force?: boolean;
  drPool?: string;
}

/** The Failover object the console creates. Only the settings that are used go in the spec. */
export function buildFailover(input: FailoverRequest, nowMs: number): Failover {
  const spec: FailoverSpec = { action: input.action };
  if (input.source) spec.source = input.source;
  if (input.rewindTo) spec.rewindTo = input.rewindTo;
  if (input.vms && input.vms.length > 0) spec.vms = [...input.vms];
  if (input.force) spec.force = true;
  if (input.drPool) spec.drPool = input.drPool;
  return {
    apiVersion: API_GROUP_VERSION,
    kind: 'Failover',
    metadata: {
      name: `console-${input.action.toLowerCase()}-${nowMs.toString(36)}`,
      labels: { [LABEL_MADE_BY_CONSOLE]: 'true' },
    },
    spec,
  };
}

/** The VMs a Failover is limited to. Empty means every VM on the copy. */
export const scopeOfPlan = (f: Failover): string[] => f.spec?.vms || [];

export const failoverSource = (f: Failover): string => f.status?.source || f.spec?.source || '';

export interface FailoverStateView {
  working: boolean;
  finished: boolean;
  failed: boolean;
  title: string;
  detail: string;
  message: string;
  elapsed: string;
}

/** What the page says about a Failover: working, done or failed. A rewind copies a whole disk, so a long wait is normal. */
export function describeFailoverState(f: Failover, now: number): FailoverStateView {
  const phase = f.status?.phase;
  const isExecute = f.spec?.action === 'Execute';
  const startedAt = f.status?.startedAt || f.metadata?.creationTimestamp;
  const elapsed = startedAt ? elapsedText(now - ms(startedAt)) : '';
  const message = f.status?.message || '';
  if (phase === 'Failed') {
    return { working: false, finished: false, failed: true, title: 'The failover failed', detail: '', message, elapsed };
  }
  if (phase === 'Done' || phase === 'Planned') {
    return {
      working: false,
      finished: true,
      failed: false,
      title: phase === 'Done' ? 'The failover is done' : 'The plan is ready',
      detail: '',
      message,
      elapsed,
    };
  }
  return {
    working: true,
    finished: false,
    failed: false,
    title: isExecute ? 'Still working: the failover is running' : 'Still working: the plan is being made',
    detail: isExecute
      ? 'A disk that is rewound is copied whole, which can take minutes. You can leave this page; the work goes on.'
      : 'The driver is reading the copy. This takes a short time, or minutes if the copy is large.',
    message,
    elapsed,
  };
}

export function isBusyMessage(message: string | undefined): boolean {
  return !!message && message.includes(BUSY_MESSAGE);
}

/** Time left in which the page still offers this Plan for a failover. Zero if it is not a finished Plan. */
export function planRemainingMs(plan: Failover, now: number): number {
  if (plan.status?.phase !== 'Planned' || !plan.status.finishedAt) return 0;
  return Math.max(0, ms(plan.status.finishedAt) + PLAN_USABLE_MS - now);
}

/**
 * The newest Plan for all VMs of a source: the one the VM table shows. A Plan for chosen VMs is made when the
 * operator fails over one VM, and is not a list of what the copy holds. An empty source means the default one:
 * the Plans that name no source.
 */
export function findPlan(all: Failover[], source: string): Failover | undefined {
  const matches = all.filter((f) => {
    if (f.spec?.action !== 'Plan' || scopeOfPlan(f).length > 0) return false;
    return source === '' ? !f.spec.source : failoverSource(f) === source;
  });
  return matches.sort((a, b) =>
    (b.metadata?.creationTimestamp || '').localeCompare(a.metadata?.creationTimestamp || ''),
  )[0];
}

export interface PlanRow {
  name: string;
  disks: number;
  rewindDisks: number;
  moment: string;
  age: string;
  momentCount: number;
}

/** One row for each VM in a plan. */
export function describePlanRows(plan: Failover | undefined, now: number): PlanRow[] {
  return (plan?.status?.vms || []).map((vm) => ({
    name: vm.name,
    disks: vm.disks.length,
    rewindDisks: vm.disks.filter((d) => d.rewind).length,
    moment: vm.moment,
    age: ageText(vm.moment, now),
    momentCount: momentsOf(vm).length,
  }));
}

/** The moments a VM can show, newest first. The moment the plan chose is always in the list. */
function momentsOf(vm: FailoverVM): string[] {
  const all = new Map<number, string>();
  for (const m of [...(vm.moments || []), vm.moment]) {
    if (m) all.set(Math.floor(ms(m) / 1000), m);
  }
  return [...all.entries()].sort((a, b) => b[0] - a[0]).map(([, m]) => m);
}

export interface Staleness {
  stale: boolean;
  /** False when there is no schedule to judge the age by. */
  known: boolean;
  text: string;
}

/** Stale means the newest copy is older than twice the schedule, or there is no copy. */
export function copyStaleness(copyMoment: string | undefined, schedule: string | undefined, now: number): Staleness {
  if (!copyMoment) return { stale: true, known: true, text: 'No copy yet' };
  const scheduleMs = schedule ? parseGoDuration(schedule) : undefined;
  if (!scheduleMs) {
    return {
      stale: false,
      known: false,
      text: 'The schedule of this copy is not known on this cluster, so the page cannot tell if it is late',
    };
  }
  if (now - ms(copyMoment) > 2 * scheduleMs) {
    return { stale: true, known: true, text: `Newest copy is older than twice the ${schedule} schedule` };
  }
  return { stale: false, known: true, text: '' };
}

/** The schedule of the policy that wrote a VM's disks, if that policy is on this cluster. */
export function scheduleOfVM(plan: Failover, vm: FailoverVM, policies: ReplicationPolicy[]): string | undefined {
  for (const disk of vm.disks) {
    const volume = plan.status?.volumes?.find((v) => v.subVolume === disk.subVolume);
    if (!volume?.policy) continue;
    const policy = policies.find((p) => p.metadata?.name === volume.policy);
    if (policy?.spec?.schedule) return policy.spec.schedule;
  }
  return undefined;
}

export interface DiskView {
  subVolume: string;
  /** namespace/name of the claim that will be made. */
  claim: string;
  rewind: boolean;
  note: string;
}

export interface MomentView {
  iso: string;
  age: string;
  /** True for the moment the plan chose for the VM. */
  isCurrent: boolean;
}

export interface VMDetailView {
  name: string;
  disks: DiskView[];
  moments: MomentView[];
}

export const REWIND_NOTE = 'Copies the whole disk; takes minutes.';
export const IN_PLACE_NOTE = 'Used in place.';

/** The detail of one VM of a plan: its disks, and the moments it can show, newest first. */
export function describeVMDetail(plan: Failover, vmName: string, now: number): VMDetailView | undefined {
  const vm = plan.status?.vms?.find((v) => v.name === vmName);
  if (!vm) return undefined;
  return {
    name: vm.name,
    disks: vm.disks.map((d) => {
      const volume = plan.status?.volumes?.find((v) => v.subVolume === d.subVolume);
      return {
        subVolume: d.subVolume,
        claim: volume ? `${volume.pvcNamespace}/${volume.pvcName}` : d.subVolume,
        rewind: d.rewind,
        note: d.rewind ? REWIND_NOTE : IN_PLACE_NOTE,
      };
    }),
    moments: momentsOf(vm).map((iso) => ({ iso, age: ageText(iso, now), isCurrent: sameSecond(iso, vm.moment) })),
  };
}

/**
 * True when the plan on screen does not show this moment for the VM. The driver accepts an Execute only for a
 * Plan with the same rewindTo, so another moment needs a new Plan first.
 */
export function needsNewPlan(plan: Failover, vmName: string, moment: string): boolean {
  const vm = plan.status?.vms?.find((v) => v.name === vmName);
  return !vm || !sameSecond(vm.moment, moment);
}

export interface PVCToCreate {
  namespace: string;
  name: string;
  subVolume: string;
}

export interface ExecuteSummary {
  vms: Array<{ name: string; moment: string; disks: number; rewindDisks: number }>;
  rewindDisks: number;
  /** Every PVC the failover makes. A plan for chosen VMs lists only theirs; a plan for all VMs lists all. */
  pvcs: PVCToCreate[];
  /** The PVCs of the VM the operator chose. */
  chosenPVCs: PVCToCreate[];
  otherVMs: number;
}

/** What an Execute of this plan does. It covers the VMs of the plan: one VM, or every VM on the copy. */
export function executeSummary(plan: Failover, chosenVM: string): ExecuteSummary {
  const vms = (plan.status?.vms || []).map((vm) => ({
    name: vm.name,
    moment: vm.moment,
    disks: vm.disks.length,
    rewindDisks: vm.disks.filter((d) => d.rewind).length,
  }));
  const toCreate = (v: { pvcNamespace: string; pvcName: string; subVolume: string }): PVCToCreate => ({
    namespace: plan.spec?.namespace || v.pvcNamespace,
    name: v.pvcName,
    subVolume: v.subVolume,
  });
  const chosenDisks = new Set(
    (plan.status?.vms || []).find((v) => v.name === chosenVM)?.disks.map((d) => d.subVolume) || [],
  );
  const volumes = plan.status?.volumes || [];
  return {
    vms,
    rewindDisks: vms.reduce((n, v) => n + v.rewindDisks, 0),
    pvcs: volumes.map(toCreate),
    chosenPVCs: volumes.filter((v) => chosenDisks.has(v.subVolume)).map(toCreate),
    otherVMs: Math.max(0, vms.length - 1),
  };
}

/** What the operator types to confirm: the VM name for one VM, or the source claim when every VM on the copy fails over. */
export function confirmationWord(plan: Failover, chosenVM: string): string {
  if (chosenVM) return chosenVM;
  return failoverSource(plan) || 'failover';
}

interface DeploymentLike {
  metadata?: { labels?: Record<string, string> };
  spec?: { template?: { spec?: { volumes?: Array<{ name: string; persistentVolumeClaim?: { claimName?: string } }> } } };
}

/** The data claims of the replication receivers in the driver namespace: the sources a failover can read. */
export function receiverClaims(deployments: DeploymentLike[]): string[] {
  const claims = new Set<string>();
  for (const d of deployments) {
    if (d.metadata?.labels?.['app.kubernetes.io/component'] !== RECEIVER_COMPONENT) continue;
    const volume = d.spec?.template?.spec?.volumes?.find((v) => v.name === RECEIVER_DATA_VOLUME);
    const claim = volume?.persistentVolumeClaim?.claimName;
    if (claim) claims.add(claim);
  }
  return [...claims].sort();
}
