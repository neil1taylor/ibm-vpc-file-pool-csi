import { PoolBench, PoolBenchStepResult } from '../types';
import { eventRows, isActive, resultLabel, stepMarks, verdictText } from './poolbench';
import { EVENT_OPTIONS } from './poolbench-form';

export type ControlAction = 'pause' | 'resume' | 'hold' | 'release' | 'extend' | 'skip' | 'stop' | 'cleanup';

/** The controls that make sense for the run as it is now, in the order the page shows them. */
export const availableActions = (pb: PoolBench): ControlAction[] => {
  const st = pb.status || {};
  const c = pb.spec.control || {};
  if (c.stop) return [];
  if (!isActive(pb)) {
    return pb.spec.keepAfterEnd && !st.cleanedUp && st.phase !== 'Failed' ? ['cleanup'] : [];
  }
  if (st.phase === 'CleaningUp') return [];
  const out: ControlAction[] = [];
  if (st.phase === 'Paused') out.push('resume');
  else if (st.phase === 'Running') out.push('pause');
  if (st.stage === 'Measuring' && st.phase === 'Running') {
    out.push(c.hold ? 'release' : 'hold', 'extend', 'skip');
  }
  out.push('stop');
  return out;
};

/** The VM count of the step that is running now. */
const currentStepVMs = (pb: PoolBench): number => (pb.status?.step || 0) * (pb.spec.stepVMs || 0);

/** An event can be added while the VMs of a step are made, settle or are measured. */
export const canAddEvent = (pb: PoolBench): boolean => {
  const st = pb.status || {};
  return (
    !pb.spec.control?.stop &&
    (st.phase === 'Running' || st.phase === 'Paused') &&
    (st.stage === 'Creating' || st.stage === 'Settling' || st.stage === 'Measuring') &&
    currentStepVMs(pb) > 0
  );
};

/** The events that are not yet asked for at the step that is running. */
export const eventChoices = (pb: PoolBench) =>
  EVENT_OPTIONS.filter((o) => !(pb.spec.events || []).some((e) => e.type === o.type && e.atVMs === currentStepVMs(pb)));

/** The JSON patch that asks for an event at the step that is running; it runs after that step is measured. */
export const eventPatch = (
  pb: PoolBench,
  type: string,
): Array<{ op: 'add'; path: string; value: NonNullable<PoolBench['spec']['events']> }> => [
  { op: 'add', path: '/spec/events', value: [...(pb.spec.events || []), { type, atVMs: currentStepVMs(pb) }] },
];

type Control = NonNullable<PoolBench['spec']['control']>;

/** The JSON patch that asks the PoolBench service for an action. Other control fields stay as they are. */
export const controlPatch = (
  pb: PoolBench,
  action: ControlAction,
  minutes = 15,
): Array<{ op: 'add'; path: string; value: Control }> => {
  const c: Control = { ...(pb.spec.control || {}) };
  const step = pb.status?.step || 0;
  switch (action) {
    case 'pause':
      c.paused = true;
      break;
    case 'resume':
      c.paused = false;
      break;
    case 'hold':
      c.hold = true;
      break;
    case 'release':
      c.hold = false;
      break;
    case 'extend':
      c.extendMinutes = (c.extendStep === step ? c.extendMinutes || 0 : 0) + minutes;
      c.extendStep = step;
      break;
    case 'skip':
      c.skipStep = step;
      break;
    case 'stop':
    case 'cleanup':
      c.stop = true;
      break;
  }
  return [{ op: 'add', path: '/spec/control', value: c }];
};

const msText = (micros?: number): string => (micros === undefined ? '' : (micros / 1000).toFixed(1));

/** The step table as CSV, one row per step, for a spreadsheet. */
export const stepsCSV = (pb: PoolBench): string => {
  const header =
    'vms,windows_vms,share_iops,guest_iops,ibm_ops,ibm_ops_pct_of_limit,share_latency_ms,p50_ms,p95_ms,p99_ms,throughput_mb_s,slow_vms_pct,stalls,ready_s,result,mark';
  const row = (s: PoolBenchStepResult): string =>
    [
      s.vms,
      s.windowsVMs ?? 0,
      s.shareIOPS ?? '',
      s.guestIOPS ?? '',
      s.ibmOps ?? '',
      s.ibmOps !== undefined && pb.spec.iops ? Math.round((s.ibmOps / pb.spec.iops) * 100) : '',
      msText(s.shareLatencyMicros),
      msText(s.p50Micros),
      msText(s.p95Micros),
      msText(s.p99Micros),
      s.throughputKBps !== undefined ? ((s.throughputKBps * 1024) / 1e6).toFixed(1) : '',
      s.slowPercent ?? 0,
      s.stalls ?? 0,
      s.readySeconds ?? '',
      s.ended ? resultLabel(s) : 'In progress',
      stepMarks(pb, s.vms).join(' '),
    ].join(',');
  return [header, ...(pb.status?.steps || []).map(row)].join('\n') + '\n';
};

const md = (micros?: number): string => (micros === undefined ? '-' : `${(micros / 1000).toFixed(1)} ms`);

const eventsSection = (pb: PoolBench): string[] => {
  const rows = eventRows(pb);
  if (rows.length === 0) return [];
  return [
    '## Events',
    '',
    '| Event | At VMs | State | Time | What happened |',
    '|---|---|---|---|---|',
    ...rows.map((e) => `| ${e.name} | ${e.atVMs} | ${e.state} | ${e.time} | ${e.message || '-'} |`),
    '',
  ];
};

/** The run as a Markdown report: the result in one sentence, the settings, and the step table. */
export const reportMarkdown = (pb: PoolBench): string => {
  const s = pb.spec;
  const l = s.load || {};
  const k = s.knee || {};
  const st = pb.status || {};
  const lines = [
    `# PoolBench run ${pb.metadata?.name}`,
    '',
    verdictText(pb) || 'No step has been measured yet.',
    '',
    '## Settings',
    '',
    `- Share: ${s.shareSizeGB} GB at ${s.iops} IOPS (dp2)`,
    `- VMs: ${s.stepVMs} per step, up to ${s.maxVMs}; ${s.windowsEvery ? `1 in ${s.windowsEvery} Windows` : 'all Linux'}`,
    `- Load per VM: ${l.steadyIOPS} IOPS steady, ${l.burstIOPS} IOPS bursts of ${l.burstSeconds} s every ${l.periodSeconds} s, ${l.readPercent}% reads, blocks ${l.blockMix}`,
    `- Each step: ${s.settleMinutes} min to settle, ${s.measureMinutes} min of measuring`,
    `- Knee rule: p95 over ${k.latencyLimitMs} ms on more than ${k.slowVMPercent}% of the VMs, a burst p95 over 50 ms on as many, any I/O over ${k.stallSeconds} s, or the share at 95% of its IOPS limit`,
    `- Run: ${st.startTime || '-'} to ${st.endTime || 'still running'}, ${st.phase || 'Pending'}`,
    '',
    '## Steps',
    '',
    '| VMs | Share IOPS | Guest IOPS | IBM operations | Share latency | p50 | p95 | p99 | Slow VMs | Stalls | Result |',
    '|---|---|---|---|---|---|---|---|---|---|---|',
    ...(st.steps || []).map(
      (r) =>
        `| ${r.vms} (${r.windowsVMs ?? 0} Windows) | ${r.shareIOPS ?? '-'} | ${r.guestIOPS ?? '-'} | ${r.ibmOps ?? '-'} | ${md(r.shareLatencyMicros)} | ${md(r.p50Micros)} | ${md(r.p95Micros)} | ${md(r.p99Micros)} | ${r.slowPercent ?? 0}% | ${r.stalls ?? 0} | ${r.ended ? resultLabel(r) : 'In progress'}${stepMarks(pb, r.vms).length ? `, ${stepMarks(pb, r.vms).join(', ')}` : ''} |`,
    ),
    '',
    ...eventsSection(pb),
  ];
  return lines.join('\n');
};

/** Offers text as a file to save. */
export const downloadText = (name: string, type: string, text: string): void => {
  const url = URL.createObjectURL(new Blob([text], { type }));
  const a = document.createElement('a');
  a.href = url;
  a.download = name;
  a.click();
  URL.revokeObjectURL(url);
};
