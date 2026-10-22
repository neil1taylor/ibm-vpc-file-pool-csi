import { FileSharePool, PoolShareStatus } from '../types';
import { phaseToPatternFlyStatus } from './resource-helpers';

export interface ShareHealth {
  label: string;
  status: 'success' | 'info' | 'warning' | 'danger' | 'custom';
}

/** A share can serve volumes only when it is stable and has a mount target IP. */
export function shareIsUsable(share: PoolShareStatus): boolean {
  return share.state === 'stable' && !!share.mountTargetIP;
}

export function hasUsableShare(pool: FileSharePool): boolean {
  return (pool.status?.shares ?? []).some(shareIsUsable);
}

export function shareHealth(share: PoolShareStatus): ShareHealth {
  if (share.state === 'stable' && !share.mountTargetIP) {
    return { label: 'No mount target', status: 'warning' };
  }
  return { label: share.state, status: phaseToPatternFlyStatus(share.state) };
}

/** Why a pool is not Ready, taken from the condition that explains its phase. */
export function poolPhaseReason(pool: FileSharePool): string | undefined {
  const phase = pool.status?.phase;
  if (!phase || phase === 'Ready') return undefined;
  const type = phase === 'Full' ? 'CapacityAvailable' : 'SharesReady';
  const condition = pool.status?.conditions?.find((c) => c.type === type);
  if (!condition || condition.status !== 'False' || !condition.message) return undefined;
  return condition.message;
}

/** One ID per line or comma-separated; blank entries are dropped. */
export function parseSecurityGroupIDs(input: string): string[] {
  return input
    .split(/[\n,]/)
    .map((id) => id.trim())
    .filter((id) => id !== '');
}

export type DrainAction = 'drain' | 'return';

/**
 * The pool's drain list after a share is drained or returned to service. The
 * list is declarative: a share stays on it until it is taken off.
 */
export function drainListAfter(current: string[] | undefined, shareID: string, action: DrainAction): string[] {
  const list = current ?? [];
  if (action === 'drain') return list.includes(shareID) ? [...list] : [...list, shareID];
  return list.filter((id) => id !== shareID);
}

export function isDraining(pool: FileSharePool, shareID: string): boolean {
  return (pool.spec?.drainShares ?? []).includes(shareID);
}

/** What the operator should know before draining a share or returning it. */
export function drainConsequences(share: PoolShareStatus, action: DrainAction): string[] {
  if (action === 'drain') {
    const lines = ['New volumes stop going to this share.'];
    if (share.pvcCount > 0) {
      const n = share.pvcCount;
      lines.push(
        `${n} existing ${n === 1 ? 'volume stays' : 'volumes stay'} on the share until ${n === 1 ? 'its PVC is' : 'their PVCs are'} deleted. ` +
          `Draining does not move any data; applications must create new PVCs to leave the share.`,
      );
    } else {
      lines.push('Draining does not move any data.');
    }
    return lines;
  }
  return [
    'If the VPC reports the share healthy, it takes new volumes again within about a minute.',
    'If you plan on deleting the share, do not return it: keep it on the drain list while deleting the share, or it takes new volumes before it is gone.',
  ];
}
