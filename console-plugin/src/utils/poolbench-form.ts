import { PoolBench } from '../types';
import { API_GROUP_VERSION } from '../constants';
import { ibmOpsPerIO } from './poolbench-queries';

/**
 * IBM's dp2 table of allowed IOPS for a share size. The Go admission check uses
 * the same table (pkg/ibmcloud/share_profiles.go); a test compares the two.
 */
export const DP2_RANGES = [
  { maxIOPS: 1000, maxSizeGB: 39, minIOPS: 100, minSizeGB: 10 },
  { maxIOPS: 2000, maxSizeGB: 79, minIOPS: 100, minSizeGB: 40 },
  { maxIOPS: 4000, maxSizeGB: 99, minIOPS: 100, minSizeGB: 80 },
  { maxIOPS: 6000, maxSizeGB: 499, minIOPS: 100, minSizeGB: 100 },
  { maxIOPS: 10000, maxSizeGB: 999, minIOPS: 100, minSizeGB: 500 },
  { maxIOPS: 20000, maxSizeGB: 1999, minIOPS: 100, minSizeGB: 1000 },
  { maxIOPS: 40000, maxSizeGB: 3999, minIOPS: 200, minSizeGB: 2000 },
  { maxIOPS: 40000, maxSizeGB: 7999, minIOPS: 300, minSizeGB: 4000 },
  { maxIOPS: 64000, maxSizeGB: 15999, minIOPS: 500, minSizeGB: 8000 },
  { maxIOPS: 96000, maxSizeGB: 32000, minIOPS: 2000, minSizeGB: 16000 },
];

export const dp2Range = (sizeGB: number): { minIOPS: number; maxIOPS: number } | undefined => {
  const r = DP2_RANGES.find((x) => sizeGB >= x.minSizeGB && sizeGB <= x.maxSizeGB);
  return r ? { minIOPS: r.minIOPS, maxIOPS: r.maxIOPS } : undefined;
};

/** The root disk of a test VM: a copy of a catalog disk must be at least 30 GiB. */
const ROOT_DISK_GIB = 30;
/** The memory KubeVirt's launcher pod needs on top of the guest, as the Go service counts it. */
const VM_OVERHEAD_GIB = 0.3;
/** About how long the VMs of a step take to be made and send results, from the lab runs. */
const READY_MINUTES = 5;
const MAX_NAME = 40;
const NAME_RE = /^[a-z0-9]([-a-z0-9]*[a-z0-9])?$/;

/** An event the person asks for: a test at one step of the run. */
export interface FormEvent {
  type: string;
  atVMs: number;
}

/** The events a run can do, in plain words. */
export const EVENT_OPTIONS: { type: string; label: string; text: string }[] = [
  { type: 'BootStorm', label: 'Boot storm', text: 'Restarts every VM at once. The time is how long until the last VM sends results again.' },
  { type: 'PatchWindow', label: 'Patch window', text: 'Every VM runs only bursts for 10 minutes, as a patch scan would. The time is how long until the VMs are back under the latency limit.' },
  { type: 'Migration', label: 'Live migration', text: 'Moves 10% of the VMs (at most 10) to other nodes at once, while they run. The time is how long until the last move ends.' },
  { type: 'Snapshot', label: 'Snapshot', text: 'Takes a snapshot of 10% of the VMs (at most 10) while they run. The time is how long until the last snapshot is ready.' },
  { type: 'Backup', label: 'Backup', text: 'Backs up 10% of the VMs (at most 10) while they run, to a second small share the run makes. The time is how long until the last backup ends.' },
];

const eventLabel = (type: string): string => EVENT_OPTIONS.find((e) => e.type === type)?.label ?? type;

/** The step an event gets when the person ticks it: the second step, so the VMs are already busy. */
export const defaultEventStep = (f: PoolBenchForm): number => Math.min(f.maxVMs, 2 * f.stepVMs);

export interface PoolBenchForm {
  name: string;
  shareSizeGB: number;
  iops: number;
  /** IOPS of each run of a series; empty for a single run at `iops`. */
  tiers: number[];
  stepVMs: number;
  maxVMs: number;
  windowsEvery: number;
  linuxCPU: number;
  linuxMemoryGiB: number;
  windowsCPU: number;
  windowsMemoryGiB: number;
  steadyIOPS: number;
  burstIOPS: number;
  burstSeconds: number;
  periodSeconds: number;
  readPercent: number;
  blockMix: string;
  areaGiB: number;
  settleMinutes: number;
  measureMinutes: number;
  latencyLimitMs: number;
  slowVMPercent: number;
  stallSeconds: number;
  stopAtKnee: boolean;
  keepAfterEnd: boolean;
  events: FormEvent[];
}

/** The 10,000 IOPS tier of the performance test spec. */
export const defaultForm = (): PoolBenchForm => ({
  name: 'perf-test',
  shareSizeGB: 3900,
  iops: 10000,
  tiers: [],
  stepVMs: 15,
  maxVMs: 90,
  windowsEvery: 3,
  linuxCPU: 1,
  linuxMemoryGiB: 2,
  windowsCPU: 2,
  windowsMemoryGiB: 4,
  steadyIOPS: 30,
  burstIOPS: 300,
  burstSeconds: 45,
  periodSeconds: 600,
  readPercent: 70,
  blockMix: '4k:50 16k:30 64k:20',
  areaGiB: 3,
  settleMinutes: 15,
  measureMinutes: 45,
  latencyLimitMs: 20,
  slowVMPercent: 25,
  stallSeconds: 5,
  stopAtKnee: true,
  keepAfterEnd: false,
  events: [],
});

export interface Preset {
  id: string;
  label: string;
  description: string;
  apply: (f: PoolBenchForm) => PoolBenchForm;
}

export const PRESETS: Preset[] = [
  {
    id: 'quick',
    label: 'Quick check',
    description: '5 VMs on a small share, to prove the images, the network and the cleanup. About 15 to 25 minutes.',
    apply: (f) => ({ ...f, shareSizeGB: 200, iops: 3000, tiers: [], stepVMs: 5, maxVMs: 5, settleMinutes: 2, measureMinutes: 5, periodSeconds: 120, burstSeconds: 20 }),
  },
  {
    id: 'tier',
    label: 'One tier',
    description: 'One share speed, 15 VMs more at each step up to 90, until users would notice. About 6 to 7 hours.',
    apply: (f) => ({ ...defaultForm(), name: f.name, iops: f.iops }),
  },
  {
    id: 'three',
    label: 'Three tiers',
    description: 'The same test at 3,000, 10,000 and 20,000 IOPS, one after another. About a day.',
    apply: (f) => ({ ...defaultForm(), name: f.name, tiers: [3000, 10000, 20000] }),
  },
];

const windowsCount = (f: PoolBenchForm, vms: number): number =>
  f.windowsEvery > 0 ? Math.floor(vms / f.windowsEvery) : 0;

/** The disks of the largest step, in GB, as the admission check counts them. */
export const diskNeedGB = (f: PoolBenchForm): number =>
  Math.ceil((f.maxVMs * (ROOT_DISK_GIB + f.areaGiB + 1) * 2 ** 30) / 1e9);

/** The memory of the largest step, in GiB, with the launcher overhead. */
export const memoryNeedGiB = (f: PoolBenchForm): number => {
  const win = windowsCount(f, f.maxVMs);
  return Math.ceil(
    (f.maxVMs - win) * (f.linuxMemoryGiB + VM_OVERHEAD_GIB) + win * (f.windowsMemoryGiB + VM_OVERHEAD_GIB) - 1e-9,
  );
};

export interface StepDemand {
  vms: number;
  windows: number;
  steadyIOPS: number;
  ibmOps: number;
  percentOfLimit: number;
  /** The IOPS if every VM bursts at once, as in a patch window. */
  burstIOPS: number;
}

export const stepDemand = (f: PoolBenchForm): StepDemand[] => {
  const steps: StepDemand[] = [];
  const factor = ibmOpsPerIO(f.blockMix);
  for (let vms = f.stepVMs; vms <= f.maxVMs && f.stepVMs > 0; vms += f.stepVMs) {
    const steadyIOPS = vms * f.steadyIOPS;
    const ibmOps = Math.round(steadyIOPS * factor);
    steps.push({
      vms,
      windows: windowsCount(f, vms),
      steadyIOPS,
      ibmOps,
      percentOfLimit: f.iops ? Math.round((ibmOps / f.iops) * 100) : 0,
      burstIOPS: vms * f.burstIOPS,
    });
  }
  return steps;
};

/** Minutes for one run that reaches its last step. */
export const runMinutes = (f: PoolBenchForm): number =>
  (f.stepVMs > 0 ? Math.floor(f.maxVMs / f.stepVMs) : 0) * (READY_MINUTES + f.settleMinutes + f.measureMinutes);

const blockMixError = (mix: string): string | undefined => {
  let total = 0;
  for (const entry of mix.trim().split(/\s+/)) {
    const m = /^(\d+)([km]):(\d+)$/i.exec(entry);
    if (!m) return `"${entry}" is not size:percent, for example 4k:50`;
    total += Number(m[3]);
  }
  return total === 100 ? undefined : `The shares add up to ${total}, not 100`;
};

/** The form's errors by field. The admission check makes the same checks again. */
export const validateForm = (f: PoolBenchForm): Partial<Record<keyof PoolBenchForm, string>> => {
  const e: Partial<Record<keyof PoolBenchForm, string>> = {};
  const longest = f.tiers.length > 0 ? `${f.name}-${Math.max(...f.tiers)}` : f.name;
  if (!NAME_RE.test(f.name) || longest.length > MAX_NAME) {
    e.name = `Lower-case letters, digits and dashes, at most ${MAX_NAME} characters${f.tiers.length ? ' with the tier added' : ''}`;
  }
  const range = dp2Range(f.shareSizeGB);
  if (!range) {
    e.shareSizeGB = 'A dp2 share is 10 to 32000 GB';
  } else {
    for (const iops of f.tiers.length ? f.tiers : [f.iops]) {
      if (iops < range.minIOPS || iops > range.maxIOPS) {
        e[f.tiers.length ? 'tiers' : 'iops'] =
          `IBM allows ${range.minIOPS} to ${range.maxIOPS} IOPS for a ${f.shareSizeGB} GB share`;
      }
    }
  }
  if (!e.shareSizeGB && diskNeedGB(f) > f.shareSizeGB) {
    e.shareSizeGB = `The disks of ${f.maxVMs} VMs need ${diskNeedGB(f)} GB`;
  }
  if (f.stepVMs < 1) e.stepVMs = 'At least 1';
  if (f.stepVMs >= 1 && (f.maxVMs < f.stepVMs || f.maxVMs % f.stepVMs !== 0)) {
    e.maxVMs = `A whole number of steps of ${f.stepVMs} VMs`;
  }
  if (f.maxVMs > 500) e.maxVMs = 'At most 500';
  if (f.steadyIOPS < 1) e.steadyIOPS = 'At least 1';
  if (f.burstIOPS < f.steadyIOPS) e.burstIOPS = 'At least the steady rate';
  if (f.burstSeconds < 10 || f.burstSeconds >= f.periodSeconds) e.burstSeconds = '10 seconds or more, and less than the period';
  if (f.readPercent < 0 || f.readPercent > 100) e.readPercent = '0 to 100';
  const mix = blockMixError(f.blockMix);
  if (mix) e.blockMix = mix;
  if (f.areaGiB < 1) e.areaGiB = 'At least 1';
  if (f.settleMinutes < 0) e.settleMinutes = '0 or more';
  const needSeconds = 2 * f.periodSeconds - f.burstSeconds;
  if (f.measureMinutes * 60 < needSeconds) e.measureMinutes = `At least ${Math.ceil(needSeconds / 60)} min: a period and a steady phase`;
  if (f.latencyLimitMs < 1) e.latencyLimitMs = 'At least 1';
  if (f.slowVMPercent < 1 || f.slowVMPercent > 100) e.slowVMPercent = '1 to 100';
  if (f.stallSeconds < 1) e.stallSeconds = 'At least 1';
  for (const ev of f.events) {
    if (f.stepVMs >= 1 && (ev.atVMs < f.stepVMs || ev.atVMs > f.maxVMs || ev.atVMs % f.stepVMs !== 0)) {
      e.events = `${eventLabel(ev.type)} must be a step of the run: a multiple of ${f.stepVMs} VMs, up to ${f.maxVMs}`;
    }
  }
  return e;
};

/** The PoolBench objects of the form: one, or one per tier, each starting after the one before. */
export const toPoolBenches = (f: PoolBenchForm): PoolBench[] => {
  const tiers = f.tiers.length ? f.tiers : [f.iops];
  return tiers.map((iops, i) => ({
    apiVersion: API_GROUP_VERSION,
    kind: 'PoolBench',
    metadata: { name: f.tiers.length ? `${f.name}-${iops}` : f.name },
    spec: {
      profile: 'dp2',
      shareSizeGB: f.shareSizeGB,
      iops,
      stepVMs: f.stepVMs,
      maxVMs: f.maxVMs,
      windowsEvery: f.windowsEvery,
      linux: { cpu: f.linuxCPU, memoryGiB: f.linuxMemoryGiB },
      windows: { cpu: f.windowsCPU, memoryGiB: f.windowsMemoryGiB },
      load: {
        steadyIOPS: f.steadyIOPS,
        burstIOPS: f.burstIOPS,
        burstSeconds: f.burstSeconds,
        periodSeconds: f.periodSeconds,
        readPercent: f.readPercent,
        blockMix: f.blockMix,
        areaGiB: f.areaGiB,
      },
      settleMinutes: f.settleMinutes,
      measureMinutes: f.measureMinutes,
      knee: {
        latencyLimitMs: f.latencyLimitMs,
        slowVMPercent: f.slowVMPercent,
        stallSeconds: f.stallSeconds,
        stopAtKnee: f.stopAtKnee,
      },
      keepAfterEnd: f.keepAfterEnd,
      events: f.events.length ? [...f.events].sort((a, b) => a.atVMs - b.atVMs).map((ev) => ({ type: ev.type, atVMs: ev.atVMs })) : undefined,
      startAfter: i > 0 ? `${f.name}-${tiers[i - 1]}` : undefined,
    },
  }));
};

const UNITS: Record<string, number> = {
  Ki: 2 ** 10, Mi: 2 ** 20, Gi: 2 ** 30, Ti: 2 ** 40, K: 1e3, M: 1e6, G: 1e9, T: 1e12, m: 1e-3, '': 1,
};

/** A Kubernetes memory quantity in GiB. The unit m is a thousandth of a byte, which some clusters use for node memory. */
export const quantityGiB = (q: string | undefined): number => {
  const m = /^(\d+(?:\.\d+)?)(Ki|Mi|Gi|Ti|K|M|G|T|m)?$/.exec(q || '');
  return m ? (Number(m[1]) * UNITS[m[2] || '']) / 2 ** 30 : 0;
};

export interface NodeLike {
  metadata?: { labels?: Record<string, string> };
  spec?: { unschedulable?: boolean };
  status?: { allocatable?: Record<string, string> };
}

/**
 * The memory of the schedulable workers with the largest one down, in GiB. The
 * service checks again at the start with the memory that pods already use.
 */
export const nPlusOneMemoryGiB = (nodes: NodeLike[]): number => {
  const mem = nodes
    .filter((n) => n.metadata?.labels && 'node-role.kubernetes.io/worker' in n.metadata.labels && !n.spec?.unschedulable)
    .map((n) => quantityGiB(n.status?.allocatable?.memory));
  if (mem.length < 2) return 0;
  return Math.floor(mem.reduce((a, b) => a + b, 0) - Math.max(...mem));
};
