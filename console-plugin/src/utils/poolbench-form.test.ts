import { readFileSync } from 'fs';
import { join } from 'path';
import {
  DP2_RANGES,
  EVENT_OPTIONS,
  PRESETS,
  defaultEventStep,
  defaultForm,
  diskNeedGB,
  dp2Range,
  memoryNeedGiB,
  NodeLike,
  nPlusOneMemoryGiB,
  quantityGiB,
  runMinutes,
  stepDemand,
  toPoolBenches,
  validateForm,
} from './poolbench-form';

describe('dp2 table', () => {
  it('is the table the Go admission check uses', () => {
    const golden = JSON.parse(
      readFileSync(join(__dirname, '../../../pkg/ibmcloud/testdata/dp2-ranges.golden.json'), 'utf8'),
    );
    expect(DP2_RANGES).toEqual(golden);
  });

  it('gives the allowed IOPS for a size', () => {
    expect(dp2Range(3900)).toEqual({ minIOPS: 200, maxIOPS: 40000 });
    expect(dp2Range(200)).toEqual({ minIOPS: 100, maxIOPS: 6000 });
    expect(dp2Range(5)).toBeUndefined();
  });
});

describe('numbers the form shows', () => {
  it('needs the disks of the largest step on the share, as the webhook counts them', () => {
    expect(diskNeedGB(defaultForm())).toBe(3286); // 90 VMs of 30 GiB + 4 GiB
  });

  it('needs the memory of the largest step, with the launcher overhead', () => {
    // 60 Linux of 2 GiB and 30 Windows of 4 GiB, each with 0.3 GiB more.
    expect(memoryNeedGiB(defaultForm())).toBe(267);
  });

  it('shows the load of each step in guest IOPS and IBM operations against the limit', () => {
    const steps = stepDemand(defaultForm());
    expect(steps.map((s) => s.vms)).toEqual([15, 30, 45, 60, 75, 90]);
    expect(steps[2]).toEqual({ vms: 45, windows: 15, steadyIOPS: 1350, ibmOps: 2160, percentOfLimit: 22, burstIOPS: 13500 });
  });

  it('says how long a run takes if it reaches its last step', () => {
    expect(runMinutes(defaultForm())).toBe(6 * (5 + 15 + 45));
  });
});

describe('validateForm', () => {
  it('accepts the default', () => {
    expect(validateForm(defaultForm())).toEqual({});
  });

  it('refuses IOPS that IBM does not allow for the size', () => {
    expect(validateForm({ ...defaultForm(), iops: 50000 }).iops).toMatch(/200 to 40000 IOPS/);
  });

  it('refuses a share too small for the disks', () => {
    expect(validateForm({ ...defaultForm(), shareSizeGB: 1000 }).shareSizeGB).toMatch(/3286 GB/);
  });

  it('refuses a largest count that is not a whole number of steps', () => {
    expect(validateForm({ ...defaultForm(), maxVMs: 100 }).maxVMs).toMatch(/whole number of steps/);
  });

  it('refuses a bad name and a bad block mix', () => {
    expect(validateForm({ ...defaultForm(), name: 'Bad Name' }).name).toBeDefined();
    expect(validateForm({ ...defaultForm(), blockMix: '4k:50' }).blockMix).toMatch(/100/);
  });

  it('refuses a measuring shorter than one load period', () => {
    expect(validateForm({ ...defaultForm(), measureMinutes: 5 }).measureMinutes).toBeDefined();
  });

  it('refuses a measuring that cannot hold a whole steady phase', () => {
    // 600 s period, 45 s burst: the window must cover 2 x 600 - 45 = 1155 s, so 20 minutes.
    expect(validateForm({ ...defaultForm(), measureMinutes: 19 }).measureMinutes).toBeDefined();
    expect(validateForm({ ...defaultForm(), measureMinutes: 20 }).measureMinutes).toBeUndefined();
  });
});

describe('presets', () => {
  it('has a quick check, one tier and three tiers', () => {
    expect(PRESETS.map((p) => p.id)).toEqual(['quick', 'tier', 'three']);
    const quick = PRESETS[0].apply(defaultForm());
    expect(validateForm(quick)).toEqual({});
    expect(runMinutes(quick)).toBeLessThanOrEqual(20);
    expect(quick.periodSeconds).toBe(120);
    expect(PRESETS[2].apply(defaultForm()).tiers).toEqual([3000, 10000, 20000]);
  });
});

describe('toPoolBenches', () => {
  it('makes one run from the form', () => {
    const [pb] = toPoolBenches(defaultForm());
    expect(pb.metadata?.name).toBe('perf-test');
    expect(pb.spec).toMatchObject({
      shareSizeGB: 3900, iops: 10000, stepVMs: 15, maxVMs: 90, windowsEvery: 3,
      settleMinutes: 15, measureMinutes: 45,
      load: { steadyIOPS: 30, burstIOPS: 300, burstSeconds: 45, periodSeconds: 600, readPercent: 70, blockMix: '4k:50 16k:30 64k:20', areaGiB: 3 },
      knee: { latencyLimitMs: 20, slowVMPercent: 25, stallSeconds: 5, stopAtKnee: true },
      keepAfterEnd: false,
    });
  });

  it('makes one run per tier, each starting after the one before', () => {
    const runs = toPoolBenches({ ...defaultForm(), tiers: [3000, 10000, 20000] });
    expect(runs.map((r) => [r.metadata?.name, r.spec.iops, r.spec.startAfter])).toEqual([
      ['perf-test-3000', 3000, undefined],
      ['perf-test-10000', 10000, 'perf-test-3000'],
      ['perf-test-20000', 20000, 'perf-test-10000'],
    ]);
  });
});

describe('room on the nodes', () => {
  it('reads memory quantities', () => {
    expect(quantityGiB('128Gi')).toBe(128);
    expect(quantityGiB('1048576Ki')).toBe(1);
    expect(quantityGiB('2048Mi')).toBe(2);
    expect(quantityGiB(String(2 ** 30))).toBe(1);
    expect(quantityGiB(undefined)).toBe(0);
  });

  it('counts the memory of the workers with the largest one down', () => {
    const n = (mem: string, worker = true, unschedulable = false): NodeLike => ({
      metadata: { labels: worker ? { 'node-role.kubernetes.io/worker': '' } : ({} as Record<string, string>) },
      spec: { unschedulable },
      status: { allocatable: { memory: mem } },
    });
    expect(nPlusOneMemoryGiB([n('100Gi'), n('120Gi'), n('110Gi'), n('500Gi', false), n('90Gi', true, true)])).toBe(210);
  });
});

describe('events', () => {
  it('offers the four events that exist, each with a plain sentence', () => {
    expect(EVENT_OPTIONS.map((e) => e.type)).toEqual(['BootStorm', 'PatchWindow', 'Migration', 'Snapshot']);
    for (const e of EVENT_OPTIONS) expect(e.text.length).toBeGreaterThan(20);
  });

  it('has none by default, and suggests the second step', () => {
    expect(defaultForm().events).toEqual([]);
    expect(defaultEventStep(defaultForm())).toBe(30);
    expect(defaultEventStep({ ...defaultForm(), stepVMs: 5, maxVMs: 5 })).toBe(5);
  });

  it('refuses an event at a VM count that is not a step of the run', () => {
    expect(validateForm({ ...defaultForm(), events: [{ type: 'BootStorm', atVMs: 30 }] }).events).toBeUndefined();
    expect(validateForm({ ...defaultForm(), events: [{ type: 'BootStorm', atVMs: 31 }] }).events).toMatch(/step/);
    expect(validateForm({ ...defaultForm(), events: [{ type: 'BootStorm', atVMs: 105 }] }).events).toMatch(/step/);
  });

  it('puts the events of the form into each run, in step order', () => {
    const runs = toPoolBenches({
      ...defaultForm(),
      tiers: [3000, 10000],
      events: [{ type: 'Snapshot', atVMs: 60 }, { type: 'BootStorm', atVMs: 30 }],
    });
    for (const r of runs) {
      expect(r.spec.events).toEqual([{ type: 'BootStorm', atVMs: 30 }, { type: 'Snapshot', atVMs: 60 }]);
    }
    expect(toPoolBenches(defaultForm())[0].spec.events).toBeUndefined();
  });
});
