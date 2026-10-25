import { PoolBench, PoolBenchStepResult } from '../types';

/** About how long the VMs of a step take to be made and send results, from the lab runs. */
const READY_MINUTES = 5;

const STAGE_WORDS: Record<string, string> = {
  Preparing: 'making the test pool',
  Creating: 'making the new VMs',
  Settling: 'letting the share settle after the disk copies',
  Measuring: 'measuring',
  Event: 'running an event',
};

/** A run that can still change: it has not ended, or it is still cleaning up. */
export const isActive = (pb: PoolBench): boolean =>
  ['Pending', 'Checking', 'Running', 'Paused', 'CleaningUp'].includes(pb.status?.phase || 'Pending');

const minutesLeft = (pb: PoolBench, now: Date): number | undefined => {
  const ends = pb.status?.stageEnds;
  if (!ends) return undefined;
  return Math.max(0, Math.ceil((new Date(ends).getTime() - now.getTime()) / 60000));
};

/** The status line at the top of the run page, in plain words. */
export const bannerText = (pb: PoolBench, now: Date): string => {
  const st = pb.status || {};
  if (st.phase !== 'Running' && st.phase !== 'Paused') {
    return st.message || (st.phase ? `${st.phase}.` : 'Waiting for the PoolBench service to start the run.');
  }
  if (st.stage === 'Preparing') return st.message || 'Making the test pool and waiting for its share';
  const head = st.phase === 'Paused' ? `Paused at step ${st.step} of ${st.totalSteps}` : `Step ${st.step} of ${st.totalSteps}`;
  const vms = `${st.vms ?? 0} VMs`;
  const win = `(${st.windowsVMs ?? 0} Windows)`;
  if (st.stage === 'Creating') {
    return `${head}: ${vms} ${win}, ${st.readyVMs ?? 0} ready, ${STAGE_WORDS.Creating}`;
  }
  const left = minutesLeft(pb, now);
  return `${head}: ${vms} running ${win}, ${STAGE_WORDS[st.stage || ''] || 'working'}${left !== undefined ? `, ${left} min left` : ''}`;
};

/**
 * When the run should end if it reaches its last step: the rest of this stage,
 * then each step to come (making the VMs, settling, measuring). Unknown for a run
 * that has ended or has not started its steps.
 */
export const expectedEnd = (pb: PoolBench, now: Date): Date | undefined => {
  const st = pb.status;
  if (!st || (st.phase !== 'Running' && st.phase !== 'Paused') || !st.step || !st.totalSteps) return undefined;
  const perStep = READY_MINUTES + (pb.spec.settleMinutes ?? 15) + (pb.spec.measureMinutes ?? 45);
  const left = minutesLeft(pb, now) ?? 0;
  return new Date(now.getTime() + (left + (st.totalSteps - st.step) * perStep) * 60000);
};

/** Microseconds as milliseconds with one decimal. */
export const ms = (micros: number | undefined): string =>
  micros === undefined || micros === null ? '-' : `${(micros / 1000).toFixed(1)} ms`;

/** IBM operations as a percent of the share's IOPS limit. */
export const percentOfLimit = (ops: number | undefined, limit: number): string =>
  ops === undefined || !limit ? '-' : `${Math.round((ops / limit) * 100)}%`;

export const resultLabel = (s: PoolBenchStepResult): string =>
  `${s.pass ? 'Pass' : 'Fail'}${s.cutShort ? ' (cut short)' : ''}`;

/** The marks of a step row: the knee and the recommended count. */
export const stepMarks = (pb: PoolBench, vms: number): string[] => {
  const marks: string[] = [];
  if (pb.status?.kneeVMs && pb.status.kneeVMs === vms) marks.push('Knee');
  if (pb.status?.recommendedVMs && pb.status.recommendedVMs === vms) marks.push('Recommended');
  return marks;
};

/** One sentence that explains the result of the steps measured so far. */
export const verdictText = (pb: PoolBench): string => {
  const st = pb.status || {};
  const measured = (st.steps || []).filter((s) => s.ended || !isActive(pb));
  if (measured.length === 0) return '';
  const last = measured[measured.length - 1].vms;
  const share = `this ${pb.spec.shareSizeGB} GB share at ${pb.spec.iops} IOPS`;
  if (!st.kneeVMs) {
    return isActive(pb)
      ? `So far no knee up to ${last} VMs.`
      : `No knee up to ${last} VMs: the share carried every step. The real limit is higher.`;
  }
  if (!st.recommendedVMs) {
    return `Users would notice already at ${st.kneeVMs} VMs, the first step. Try a faster share or smaller steps.`;
  }
  return `Users would notice at ${st.kneeVMs} VMs on ${share}. Recommended: up to ${st.recommendedVMs} VMs.`;
};
